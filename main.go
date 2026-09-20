// Command rsh-mcp is an MCP server exposing RSH command execution and RCP
// file transfer to vintage / embedded systems that speak the BSD rcmd
// protocol (OpenVMS TCPIP$RSH among them).
//
// This is an improved fork of github.com/tenox7/rsh-mcp adding: persistent
// transfer audit logging (rsh_transfers.log, modeled on the companion
// improved-mcp-server-ftp fork), binary-safe read/write via base64,
// streaming upload-file/download-file tools that never round-trip large
// files through the model context, environment-variable defaults so a
// single .mcp.json entry can be pre-pointed at one host, and fail-fast
// privileged-port handling instead of a silent ephemeral-port fallback.
package main

import (
	"context"
	"encoding/base64"
	"flag"
	"fmt"
	"log"
	"os"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tenox7/rsh-mcp/internal/rcmd"
	"github.com/tenox7/rsh-mcp/internal/rcp"
	"github.com/tenox7/rsh-mcp/internal/rsh"
	"github.com/tenox7/rsh-mcp/internal/xferlog"
)

type ExecArgs struct {
	Host      string `json:"host,omitempty" jsonschema:"description:Remote hostname or IP address (optional if RSH_HOST is set)"`
	User      string `json:"user,omitempty" jsonschema:"description:Remote username (optional, defaults to RSH_USER env var, else the local user)"`
	LocalUser string `json:"local_user,omitempty" jsonschema:"description:Local username sent in the RSH handshake (optional, defaults to RSH_LOCAL_USER env var, else the OS user); must match what the remote's proxy database trusts"`
	Command   string `json:"command" jsonschema:"description:Command to execute on the remote host"`
	Port      any    `json:"port,omitempty" jsonschema:"description:Port number, string or integer (optional, defaults to RSH_PORT env var or 514)"`
	MaxLines  int    `json:"max_lines,omitempty" jsonschema:"description:Maximum lines of output to return (optional, defaults to 1000)"`
	MaxBytes  int    `json:"max_bytes,omitempty" jsonschema:"description:Maximum bytes of output to return (optional, defaults to 100000)"`
	Tail      bool   `json:"tail,omitempty" jsonschema:"description:Return the last N lines instead of the first N (optional)"`
	LogDir    string `json:"log_dir,omitempty" jsonschema:"description:Override the transfer log directory for this call (optional)"`
}

type ReadFileArgs struct {
	Host      string `json:"host,omitempty" jsonschema:"description:Remote hostname or IP address (optional if RSH_HOST is set)"`
	User      string `json:"user,omitempty" jsonschema:"description:Remote username (optional, defaults to RSH_USER env var, else the local user)"`
	LocalUser string `json:"local_user,omitempty" jsonschema:"description:Local username sent in the RSH handshake (optional, defaults to RSH_LOCAL_USER env var, else the OS user)"`
	Path      string `json:"path" jsonschema:"description:Path to the remote file to read"`
	Port      any    `json:"port,omitempty" jsonschema:"description:Port number, string or integer (optional, defaults to RSH_PORT env var or 514)"`
	Encoding  string `json:"encoding,omitempty" jsonschema:"description:'text' (default) or 'base64'. Text mode fails clearly instead of corrupting output if the file is not valid UTF-8 -- retry with base64 for binaries."`
	LogDir    string `json:"log_dir,omitempty" jsonschema:"description:Override the transfer log directory for this call (optional)"`
}

type WriteFileArgs struct {
	Host          string `json:"host,omitempty" jsonschema:"description:Remote hostname or IP address (optional if RSH_HOST is set)"`
	User          string `json:"user,omitempty" jsonschema:"description:Remote username (optional, defaults to RSH_USER env var, else the local user)"`
	LocalUser     string `json:"local_user,omitempty" jsonschema:"description:Local username sent in the RSH handshake (optional, defaults to RSH_LOCAL_USER env var, else the OS user)"`
	Path          string `json:"path" jsonschema:"description:Path to the remote file to write"`
	Content       string `json:"content,omitempty" jsonschema:"description:Text content to write. Exactly one of content / content_base64 is required."`
	ContentBase64 string `json:"content_base64,omitempty" jsonschema:"description:Base64-encoded binary content to write. Exactly one of content / content_base64 is required."`
	Mode          string `json:"mode,omitempty" jsonschema:"description:Octal file mode for the new remote file, e.g. \"0644\" (optional, default 0644)"`
	Port          any    `json:"port,omitempty" jsonschema:"description:Port number, string or integer (optional, defaults to RSH_PORT env var or 514)"`
	LogDir        string `json:"log_dir,omitempty" jsonschema:"description:Override the transfer log directory for this call (optional)"`
}

type UploadFileArgs struct {
	Host       string `json:"host,omitempty" jsonschema:"description:Remote hostname or IP address (optional if RSH_HOST is set)"`
	User       string `json:"user,omitempty" jsonschema:"description:Remote username (optional, defaults to RSH_USER env var, else the local user)"`
	LocalUser  string `json:"local_user,omitempty" jsonschema:"description:Local username sent in the RSH handshake (optional, defaults to RSH_LOCAL_USER env var, else the OS user)"`
	LocalPath  string `json:"local_path" jsonschema:"description:Path to the local file to upload"`
	RemotePath string `json:"remote_path" jsonschema:"description:Destination path on the remote host"`
	Port       any    `json:"port,omitempty" jsonschema:"description:Port number, string or integer (optional, defaults to RSH_PORT env var or 514)"`
	LogDir     string `json:"log_dir,omitempty" jsonschema:"description:Override the transfer log directory for this call (optional)"`
}

type DownloadFileArgs struct {
	Host       string `json:"host,omitempty" jsonschema:"description:Remote hostname or IP address (optional if RSH_HOST is set)"`
	User       string `json:"user,omitempty" jsonschema:"description:Remote username (optional, defaults to RSH_USER env var, else the local user)"`
	LocalUser  string `json:"local_user,omitempty" jsonschema:"description:Local username sent in the RSH handshake (optional, defaults to RSH_LOCAL_USER env var, else the OS user)"`
	RemotePath string `json:"remote_path" jsonschema:"description:Path to the remote file to download"`
	LocalPath  string `json:"local_path" jsonschema:"description:Destination path on the local filesystem"`
	Overwrite  bool   `json:"overwrite,omitempty" jsonschema:"description:Allow overwriting an existing local file (optional, default false)"`
	Port       any    `json:"port,omitempty" jsonschema:"description:Port number, string or integer (optional, defaults to RSH_PORT env var or 514)"`
	LogDir     string `json:"log_dir,omitempty" jsonschema:"description:Override the transfer log directory for this call (optional)"`
}

func errResult(prefix string, err error) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: prefix + ": " + err.Error()}},
	}
}

func main() {
	flag.DurationVar(&rcmd.IdleTimeout, "idle-timeout", rcmd.IdleTimeout, "close connection after this much remote silence")
	requirePriv := flag.Bool("require-privileged-port", rcmd.RequirePrivilegedPort,
		"fail fast if a reserved source port (512-1023) cannot be bound, instead of silently using an ephemeral one (env: RSH_REQUIRE_PRIVPORT)")
	flag.Parse()
	rcmd.RequirePrivilegedPort = *requirePriv

	server := mcp.NewServer(&mcp.Implementation{
		Name:    "rsh-mcp",
		Version: "1.1.1",
	}, nil)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "exec",
		Description: "Execute a command remotely via the RSH protocol (single TCP connection, tunnel-friendly). Logged to rsh_transfers.log.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args ExecArgs) (*mcp.CallToolResult, TransferOutput, error) {
		start := time.Now()
		host := firstOrEnv(args.Host, "RSH_HOST")
		user := firstOrEnv(args.User, "RSH_USER")
		localUser := firstOrEnv(args.LocalUser, "RSH_LOCAL_USER")
		port := resolvePort(args.Port)
		logger := openLogger(args.LogDir)
		maxLines, maxBytes := args.MaxLines, args.MaxBytes

		res, err := rsh.Execute(host, user, localUser, args.Command, port, maxLines, maxBytes, args.Tail)
		dur := since(start)
		if err != nil {
			lf := logResult(logger, xferlog.Entry{
				Operation: "EXEC", Host: host, Port: port, LocalUser: localUser, RemoteUser: user,
				Command: args.Command, DurationMs: dur, Status: "FAILED", Error: err.Error(),
			})
			return errResult("RSH execution failed", err), TransferOutput{LogFile: lf, DurationMs: dur}, nil
		}

		out := TransferOutput{
			Bytes: int64(len(res.Output)), SHA256: xferlog.SHA256Hex(res.Output),
			SourcePort: res.SourcePort, PrivilegedPort: res.PrivilegedPort,
			LocalUser: res.LocalUser, RemoteUser: res.RemoteUser, DurationMs: dur,
		}
		out.LogFile = logResult(logger, xferlog.Entry{
			Operation: "EXEC", Host: host, Port: port, LocalUser: res.LocalUser, RemoteUser: res.RemoteUser,
			SourcePort: res.SourcePort, PrivilegedPort: res.PrivilegedPort, Command: args.Command,
			Bytes: out.Bytes, SHA256: out.SHA256, DurationMs: dur, Status: "SUCCESS",
			OutputPreview: previewLines(res.Output, 20),
		})

		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(res.Output)}}}, out, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "read",
		Description: "Read a whole remote file via RCP into text or base64 (binary-safe). For large files prefer download-file. Logged to rsh_transfers.log.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args ReadFileArgs) (*mcp.CallToolResult, TransferOutput, error) {
		start := time.Now()
		host := firstOrEnv(args.Host, "RSH_HOST")
		user := firstOrEnv(args.User, "RSH_USER")
		localUser := firstOrEnv(args.LocalUser, "RSH_LOCAL_USER")
		port := resolvePort(args.Port)
		logger := openLogger(args.LogDir)
		encoding := args.Encoding
		if encoding == "" {
			encoding = "text"
		}
		if encoding != "text" && encoding != "base64" {
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{
				&mcp.TextContent{Text: fmt.Sprintf("invalid encoding %q: use \"text\" or \"base64\"", encoding)},
			}}, TransferOutput{}, nil
		}

		res, err := rcp.ReadFile(host, user, localUser, args.Path, port)
		dur := since(start)
		if err != nil {
			lf := logResult(logger, xferlog.Entry{
				Operation: "READ", Host: host, Port: port, LocalUser: localUser, RemoteUser: user,
				RemotePath: args.Path, DurationMs: dur, Status: "FAILED", Error: err.Error(),
			})
			return errResult("RCP read failed", err), TransferOutput{LogFile: lf, DurationMs: dur}, nil
		}

		out := TransferOutput{
			Bytes: res.Bytes, SHA256: res.SHA256, SourcePort: res.SourcePort, PrivilegedPort: res.PrivilegedPort,
			LocalUser: res.LocalUser, RemoteUser: res.RemoteUser, DurationMs: dur,
		}

		if encoding == "text" && !utf8.Valid(res.Content) {
			errMsg := fmt.Sprintf("remote file is %d bytes but is not valid UTF-8 text; retry with encoding=\"base64\" to read it safely", len(res.Content))
			out.LogFile = logResult(logger, xferlog.Entry{
				Operation: "READ", Host: host, Port: port, LocalUser: res.LocalUser, RemoteUser: res.RemoteUser,
				SourcePort: res.SourcePort, PrivilegedPort: res.PrivilegedPort, RemotePath: args.Path,
				Bytes: out.Bytes, SHA256: out.SHA256, DurationMs: dur, Status: "FAILED", Error: errMsg,
			})
			return errResult("RCP read", fmt.Errorf("%s", errMsg)), out, nil
		}

		text := string(res.Content)
		if encoding == "base64" {
			text = base64.StdEncoding.EncodeToString(res.Content)
		}

		out.LogFile = logResult(logger, xferlog.Entry{
			Operation: "READ", Host: host, Port: port, LocalUser: res.LocalUser, RemoteUser: res.RemoteUser,
			SourcePort: res.SourcePort, PrivilegedPort: res.PrivilegedPort, RemotePath: args.Path,
			Bytes: out.Bytes, SHA256: out.SHA256, DurationMs: dur, Status: "SUCCESS",
		})

		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, out, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "write",
		Description: "Write text or base64 content to a remote file via RCP in one transfer (binary-safe). For large files prefer upload-file. Logged to rsh_transfers.log.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args WriteFileArgs) (*mcp.CallToolResult, TransferOutput, error) {
		start := time.Now()
		host := firstOrEnv(args.Host, "RSH_HOST")
		user := firstOrEnv(args.User, "RSH_USER")
		localUser := firstOrEnv(args.LocalUser, "RSH_LOCAL_USER")
		port := resolvePort(args.Port)
		logger := openLogger(args.LogDir)

		if (args.Content == "") == (args.ContentBase64 == "") {
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{
				&mcp.TextContent{Text: "specify exactly one of content or content_base64"},
			}}, TransferOutput{}, nil
		}
		var content []byte
		if args.ContentBase64 != "" {
			decoded, err := base64.StdEncoding.DecodeString(args.ContentBase64)
			if err != nil {
				return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{
					&mcp.TextContent{Text: "invalid content_base64: " + err.Error()},
				}}, TransferOutput{}, nil
			}
			content = decoded
		} else {
			content = []byte(args.Content)
		}

		modeStr := args.Mode
		if modeStr == "" {
			modeStr = "0644"
		}
		modeVal, err := strconv.ParseUint(modeStr, 8, 32)
		if err != nil {
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{
				&mcp.TextContent{Text: fmt.Sprintf("invalid mode %q (expected octal, e.g. 0644): %v", modeStr, err)},
			}}, TransferOutput{}, nil
		}

		res, err := rcp.WriteFile(host, user, localUser, args.Path, port, content, os.FileMode(modeVal))
		dur := since(start)
		if err != nil {
			lf := logResult(logger, xferlog.Entry{
				Operation: "WRITE", Host: host, Port: port, LocalUser: localUser, RemoteUser: user,
				RemotePath: args.Path, Bytes: int64(len(content)), DurationMs: dur, Status: "FAILED", Error: err.Error(),
			})
			return errResult("RCP write failed", err), TransferOutput{LogFile: lf, DurationMs: dur}, nil
		}

		out := TransferOutput{
			Bytes: res.Bytes, SHA256: res.SHA256, SourcePort: res.SourcePort, PrivilegedPort: res.PrivilegedPort,
			LocalUser: res.LocalUser, RemoteUser: res.RemoteUser, DurationMs: dur,
		}
		out.LogFile = logResult(logger, xferlog.Entry{
			Operation: "WRITE", Host: host, Port: port, LocalUser: res.LocalUser, RemoteUser: res.RemoteUser,
			SourcePort: res.SourcePort, PrivilegedPort: res.PrivilegedPort, RemotePath: args.Path,
			Bytes: out.Bytes, SHA256: out.SHA256, DurationMs: dur, Status: "SUCCESS",
		})

		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "File written successfully"}}}, out, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "upload-file",
		Description: "Upload a local file to the remote host via RCP, streaming directly from disk (no size limit from model-context overhead). Logged to rsh_transfers.log with a SHA-256 of the local file.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args UploadFileArgs) (*mcp.CallToolResult, TransferOutput, error) {
		start := time.Now()
		host := firstOrEnv(args.Host, "RSH_HOST")
		user := firstOrEnv(args.User, "RSH_USER")
		localUser := firstOrEnv(args.LocalUser, "RSH_LOCAL_USER")
		port := resolvePort(args.Port)
		logger := openLogger(args.LogDir)

		res, err := rcp.Upload(host, user, localUser, args.LocalPath, args.RemotePath, port)
		dur := since(start)
		if err != nil {
			lf := logResult(logger, xferlog.Entry{
				Operation: "UPLOAD", Host: host, Port: port, LocalUser: localUser, RemoteUser: user,
				RemotePath: args.RemotePath, LocalPath: args.LocalPath, DurationMs: dur, Status: "FAILED", Error: err.Error(),
			})
			return errResult("RCP upload failed", err), TransferOutput{LogFile: lf, DurationMs: dur}, nil
		}

		out := TransferOutput{
			Bytes: res.Bytes, SHA256: res.SHA256, SourcePort: res.SourcePort, PrivilegedPort: res.PrivilegedPort,
			LocalUser: res.LocalUser, RemoteUser: res.RemoteUser, DurationMs: dur,
		}
		out.LogFile = logResult(logger, xferlog.Entry{
			Operation: "UPLOAD", Host: host, Port: port, LocalUser: res.LocalUser, RemoteUser: res.RemoteUser,
			SourcePort: res.SourcePort, PrivilegedPort: res.PrivilegedPort,
			RemotePath: args.RemotePath, LocalPath: args.LocalPath,
			Bytes: out.Bytes, SHA256: out.SHA256, DurationMs: dur, Status: "SUCCESS",
		})

		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{
			Text: fmt.Sprintf("Uploaded %d bytes to %s (sha256 %s)", out.Bytes, args.RemotePath, out.SHA256),
		}}}, out, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "download-file",
		Description: "Download a remote file to a local path via RCP, streaming directly to disk (no size limit from model-context overhead). Refuses to overwrite an existing local file unless overwrite=true. Logged to rsh_transfers.log.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args DownloadFileArgs) (*mcp.CallToolResult, TransferOutput, error) {
		start := time.Now()
		host := firstOrEnv(args.Host, "RSH_HOST")
		user := firstOrEnv(args.User, "RSH_USER")
		localUser := firstOrEnv(args.LocalUser, "RSH_LOCAL_USER")
		port := resolvePort(args.Port)
		logger := openLogger(args.LogDir)

		res, err := rcp.Download(host, user, localUser, args.RemotePath, port, args.LocalPath, args.Overwrite)
		dur := since(start)
		if err != nil {
			lf := logResult(logger, xferlog.Entry{
				Operation: "DOWNLOAD", Host: host, Port: port, LocalUser: localUser, RemoteUser: user,
				RemotePath: args.RemotePath, LocalPath: args.LocalPath, DurationMs: dur, Status: "FAILED", Error: err.Error(),
			})
			return errResult("RCP download failed", err), TransferOutput{LogFile: lf, DurationMs: dur}, nil
		}

		out := TransferOutput{
			Bytes: res.Bytes, SHA256: res.SHA256, SourcePort: res.SourcePort, PrivilegedPort: res.PrivilegedPort,
			LocalUser: res.LocalUser, RemoteUser: res.RemoteUser, DurationMs: dur,
		}
		out.LogFile = logResult(logger, xferlog.Entry{
			Operation: "DOWNLOAD", Host: host, Port: port, LocalUser: res.LocalUser, RemoteUser: res.RemoteUser,
			SourcePort: res.SourcePort, PrivilegedPort: res.PrivilegedPort,
			RemotePath: args.RemotePath, LocalPath: args.LocalPath,
			Bytes: out.Bytes, SHA256: out.SHA256, DurationMs: dur, Status: "SUCCESS",
		})

		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{
			Text: fmt.Sprintf("Downloaded %d bytes from %s to %s (sha256 %s)", out.Bytes, args.RemotePath, args.LocalPath, out.SHA256),
		}}}, out, nil
	})

	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Fatalf("Server error: %v", err)
	}
}
