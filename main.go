// Command rsh-mcp is an MCP server exposing RSH command execution and RCP
// file transfer to vintage / embedded systems that speak the BSD rcmd
// protocol (OpenVMS TCPIP$RSH among them).
//
// This is an improved fork of github.com/tenox7/rsh-mcp adding: persistent
// transfer audit logging (rsh_transfers.log, modeled on the companion
// improved-mcp-server-ftp fork), binary-safe read/write via base64,
// streaming upload-file/download-file tools that never round-trip large
// files through the model context, environment-variable defaults so a
// single .mcp.json entry can be pre-pointed at one host (RSH_HOST,
// RSH_PORT, RSH_USER, RSH_LOCAL_USER, RSH_SHELL), and fail-fast
// privileged-port handling instead of a silent ephemeral-port fallback.
package main

import (
	"context"
	"encoding/base64"
	"flag"
	"fmt"
	"log"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tenox7/rsh-mcp/internal/rcmd"
	"github.com/tenox7/rsh-mcp/internal/rcp"
	"github.com/tenox7/rsh-mcp/internal/rsh"
	"github.com/tenox7/rsh-mcp/internal/xferlog"
)

// forkVersion is reported when the Go build info carries no module
// version (a local "make build"); keep it in step with CHANGELOG.md.
const forkVersion = "1.2.0"

type ExecArgs struct {
	Host      string `json:"host,omitempty" jsonschema:"description:Remote hostname or IP address (optional if RSH_HOST is set)"`
	User      string `json:"user,omitempty" jsonschema:"description:Remote username (optional, defaults to RSH_USER env var, else the local user)"`
	LocalUser string `json:"local_user,omitempty" jsonschema:"description:Local username sent in the RSH handshake (optional, defaults to RSH_LOCAL_USER env var, else the OS user); must match what the remote's proxy database trusts"`
	Command   string `json:"command" jsonschema:"description:Command to execute on the remote host. With shell sh it may have several lines"`
	Port      any    `json:"port,omitempty" jsonschema:"description:Port number, string or integer (optional, defaults to RSH_PORT env var or 514)"`
	Shell     string `json:"shell,omitempty" jsonschema:"description:sh runs the command under /bin/sh whatever the login shell is and returns its exit status; on Windows NT that needs ntrshd 1.2 or later. login hands it to the login shell as it is with no exit status (for systems without /bin/sh, such as OpenVMS). Optional: defaults to the RSH_SHELL env var, else sh"`
	Timeout   int    `json:"timeout,omitempty" jsonschema:"description:Seconds after which the call stops waiting and returns the output so far (optional). The command may go on running"`
	Grep      string `json:"grep,omitempty" jsonschema:"description:Return only the lines that match this regular expression (Go syntax). Filtered here so old remote tools don't matter (optional)"`
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

type DownloadArgs struct {
	Host      string   `json:"host,omitempty" jsonschema:"description:Remote hostname or IP address (optional if RSH_HOST is set)"`
	User      string   `json:"user,omitempty" jsonschema:"description:Remote username (optional, defaults to RSH_USER env var, else the local user)"`
	LocalUser string   `json:"local_user,omitempty" jsonschema:"description:Local username sent in the RSH handshake (optional, defaults to RSH_LOCAL_USER env var, else the OS user)"`
	Paths     []string `json:"paths" jsonschema:"description:Remote files to copy. * and ? work as in the remote shell"`
	To        string   `json:"to" jsonschema:"description:Local folder to copy them into (an absolute path; made if missing)"`
	Recursive bool     `json:"recursive,omitempty" jsonschema:"description:Copy folders with everything in them (optional)"`
	Shell     string   `json:"shell,omitempty" jsonschema:"description:sh (default, or RSH_SHELL) quotes unusual remote paths for a Unix /bin/sh; login passes them exactly as given, for remotes without /bin/sh such as OpenVMS"`
	Port      any      `json:"port,omitempty" jsonschema:"description:Port number, string or integer (optional, defaults to RSH_PORT env var or 514)"`
	LogDir    string   `json:"log_dir,omitempty" jsonschema:"description:Override the transfer log directory for this call (optional)"`
}

type UploadArgs struct {
	Host      string   `json:"host,omitempty" jsonschema:"description:Remote hostname or IP address (optional if RSH_HOST is set)"`
	User      string   `json:"user,omitempty" jsonschema:"description:Remote username (optional, defaults to RSH_USER env var, else the local user)"`
	LocalUser string   `json:"local_user,omitempty" jsonschema:"description:Local username sent in the RSH handshake (optional, defaults to RSH_LOCAL_USER env var, else the OS user)"`
	Paths     []string `json:"paths" jsonschema:"description:Local files to copy (absolute paths). Folders need recursive"`
	To        string   `json:"to" jsonschema:"description:Remote folder to copy them into. For one file it may also be the new file name"`
	Recursive bool     `json:"recursive,omitempty" jsonschema:"description:Copy folders with everything in them (optional)"`
	Shell     string   `json:"shell,omitempty" jsonschema:"description:sh (default, or RSH_SHELL) quotes unusual remote paths for a Unix /bin/sh; login passes them exactly as given, for remotes without /bin/sh such as OpenVMS"`
	Port      any      `json:"port,omitempty" jsonschema:"description:Port number, string or integer (optional, defaults to RSH_PORT env var or 514)"`
	LogDir    string   `json:"log_dir,omitempty" jsonschema:"description:Override the transfer log directory for this call (optional)"`
}

type VersionArgs struct{}

func serverVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok || info.Main.Version == "" || info.Main.Version == "(devel)" {
		return forkVersion
	}
	return info.Main.Version
}

func textResult(text string, isError bool) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: isError,
		Content: []mcp.Content{&mcp.TextContent{Text: text}},
	}
}

func errResult(prefix string, err error) *mcp.CallToolResult {
	return textResult(prefix+": "+err.Error(), true)
}

// resolveShell picks the call's shell argument, then RSH_SHELL, then sh,
// and rejects anything else.
func resolveShell(arg string) (string, error) {
	s := strings.ToLower(strings.TrimSpace(firstOrEnv(arg, "RSH_SHELL")))
	switch s {
	case "":
		return "sh", nil
	case "sh", "login":
		return s, nil
	}
	return "", fmt.Errorf("invalid shell %q: use \"sh\" or \"login\"", s)
}

func quoting(shell string) rcp.Quoting {
	if shell == "login" {
		return rcp.QuoteNone
	}
	return rcp.QuoteSh
}

func execText(shell string, timeout int, res *rsh.Result) string {
	var b strings.Builder
	b.Write(res.Output)
	if res.Matched < res.Lines {
		fmt.Fprintf(&b, "[%d of %d lines match the grep]\n", res.Matched, res.Lines)
	}
	if res.Cut {
		fmt.Fprintf(&b, "[cut to %d lines, use max_lines, max_bytes, tail or grep]\n", res.Shown)
	}
	if res.TimedOut {
		fmt.Fprintf(&b, "[timed out after %d s, the command may still be running]\n", timeout)
	}
	if res.ExitStatus != rsh.ExitUnknown {
		fmt.Fprintf(&b, "exit status %d\n", res.ExitStatus)
		return b.String()
	}
	if shell != "login" && !res.TimedOut {
		b.WriteString("[no exit status: the remote shell did not finish the command]\n")
	}
	return b.String()
}

func exitText(shell string, res *rsh.Result) string {
	switch {
	case res.ExitStatus != rsh.ExitUnknown:
		return strconv.Itoa(res.ExitStatus)
	case res.TimedOut:
		return "unknown (timed out)"
	case shell == "login":
		return "" // login shells give none; nothing to log
	}
	return "unknown (the remote shell did not finish the command)"
}

func copyText(verb string, rep *rcp.Report) string {
	var b strings.Builder
	files := "files"
	if len(rep.Files) == 1 {
		files = "file"
	}
	fmt.Fprintf(&b, "%s %d %s, %d bytes\n", verb, len(rep.Files), files, copiedBytes(rep))
	for _, f := range rep.Files {
		fmt.Fprintf(&b, "%s (%d bytes)\n", f.Path, f.Size)
	}
	for _, e := range rep.Errors {
		fmt.Fprintf(&b, "error: %s\n", e)
	}
	return b.String()
}

func copiedBytes(rep *rcp.Report) int64 {
	var total int64
	for _, f := range rep.Files {
		total += f.Size
	}
	return total
}

func copyResult(tool, verb string, rep *rcp.Report, err error) *mcp.CallToolResult {
	if rep == nil {
		return textResult("RCP "+tool+" failed: "+err.Error(), true)
	}
	text := copyText(verb, rep)
	if err != nil {
		text += "stopped: " + err.Error() + "\n"
	}
	return textResult(text, err != nil || len(rep.Files) == 0)
}

// logCopy records one multi-file download or upload.
func logCopy(l *xferlog.Logger, op, host, port, localUser, user, remote, local string, rep *rcp.Report, err error, dur int64) {
	e := xferlog.Entry{
		Operation: op, Host: host, Port: port, LocalUser: localUser, RemoteUser: user,
		RemotePath: remote, LocalPath: local, DurationMs: dur, Status: "SUCCESS",
	}
	if rep != nil {
		e.LocalUser, e.RemoteUser = rep.LocalUser, rep.RemoteUser
		e.SourcePort, e.PrivilegedPort = rep.SourcePort, rep.PrivilegedPort
		e.Bytes = copiedBytes(rep)
		e.OutputPreview = strings.TrimSuffix(copyText("copied", rep), "\n")
	}
	switch {
	case err != nil:
		e.Status, e.Error = "FAILED", err.Error()
	case rep == nil || len(rep.Files) == 0:
		e.Status = "FAILED"
	}
	logResult(l, e)
}

func main() {
	flag.DurationVar(&rcmd.IdleTimeout, "idle-timeout", rcmd.IdleTimeout, "close connection after this much remote silence")
	requirePriv := flag.Bool("require-privileged-port", rcmd.RequirePrivilegedPort,
		"fail fast if a reserved source port (512-1023) cannot be bound, instead of silently using an ephemeral one (env: RSH_REQUIRE_PRIVPORT)")
	flag.Parse()
	rcmd.RequirePrivilegedPort = *requirePriv

	if err := newServer().Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Fatalf("Server error: %v", err)
	}
}

// newServer builds the MCP server with all its tools.
func newServer() *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "rsh-mcp",
		Version: serverVersion(),
	}, nil)

	// exec and read return their text only, with no structured result:
	// MCP clients that show structuredContent in place of the text (Claude
	// Code does) would otherwise hide the command output or file contents
	// behind the bookkeeping. The bookkeeping still goes to the log.
	mcp.AddTool(server, &mcp.Tool{
		Name:        "exec",
		Description: "Execute a command remotely via the RSH protocol (single TCP connection, tunnel-friendly). Returns its output (stdout and stderr) and, with shell sh, its exit status. Logged to rsh_transfers.log.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args ExecArgs) (*mcp.CallToolResult, any, error) {
		start := time.Now()
		host := firstOrEnv(args.Host, "RSH_HOST")
		user := firstOrEnv(args.User, "RSH_USER")
		localUser := firstOrEnv(args.LocalUser, "RSH_LOCAL_USER")
		port := resolvePort(args.Port)
		logger := openLogger(args.LogDir)
		shell, err := resolveShell(args.Shell)
		if err != nil {
			return errResult("RSH execution failed", err), nil, nil
		}
		if args.MaxLines <= 0 {
			args.MaxLines = 1000
		}
		if args.MaxBytes <= 0 {
			args.MaxBytes = 100000
		}

		res, err := rsh.Execute(ctx, rsh.Options{
			Host: host, User: user, LocalUser: localUser, Port: port, Command: args.Command,
			Shell: shell, Timeout: time.Duration(args.Timeout) * time.Second, Grep: args.Grep,
			MaxLines: args.MaxLines, MaxBytes: args.MaxBytes, Tail: args.Tail,
		})
		dur := since(start)
		if err != nil {
			logResult(logger, xferlog.Entry{
				Operation: "EXEC", Host: host, Port: port, LocalUser: localUser, RemoteUser: user,
				Command: args.Command, DurationMs: dur, Status: "FAILED", Error: err.Error(),
			})
			return errResult("RSH execution failed", err), nil, nil
		}

		logResult(logger, xferlog.Entry{
			Operation: "EXEC", Host: host, Port: port, LocalUser: res.LocalUser, RemoteUser: res.RemoteUser,
			SourcePort: res.SourcePort, PrivilegedPort: res.PrivilegedPort, Command: args.Command,
			Bytes: int64(len(res.Output)), SHA256: xferlog.SHA256Hex(res.Output), DurationMs: dur, Status: "SUCCESS",
			Exit: exitText(shell, res), OutputPreview: previewLines(res.Output, 20),
		})
		return textResult(execText(shell, args.Timeout, res), false), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "read",
		Description: "Read a whole remote file via RCP into text or base64 (binary-safe). For large files prefer download-file. Logged to rsh_transfers.log.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args ReadFileArgs) (*mcp.CallToolResult, any, error) {
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
			return textResult(fmt.Sprintf("invalid encoding %q: use \"text\" or \"base64\"", encoding), true), nil, nil
		}

		res, err := rcp.ReadFile(host, user, localUser, args.Path, port)
		dur := since(start)
		if err != nil {
			logResult(logger, xferlog.Entry{
				Operation: "READ", Host: host, Port: port, LocalUser: localUser, RemoteUser: user,
				RemotePath: args.Path, DurationMs: dur, Status: "FAILED", Error: err.Error(),
			})
			return errResult("RCP read failed", err), nil, nil
		}

		entry := xferlog.Entry{
			Operation: "READ", Host: host, Port: port, LocalUser: res.LocalUser, RemoteUser: res.RemoteUser,
			SourcePort: res.SourcePort, PrivilegedPort: res.PrivilegedPort, RemotePath: args.Path,
			Bytes: res.Bytes, SHA256: res.SHA256, DurationMs: dur, Status: "SUCCESS",
		}
		if encoding == "text" && !utf8.Valid(res.Content) {
			errMsg := fmt.Sprintf("remote file is %d bytes but is not valid UTF-8 text; retry with encoding=\"base64\" to read it safely", len(res.Content))
			entry.Status, entry.Error = "FAILED", errMsg
			logResult(logger, entry)
			return errResult("RCP read", fmt.Errorf("%s", errMsg)), nil, nil
		}

		text := string(res.Content)
		if encoding == "base64" {
			text = base64.StdEncoding.EncodeToString(res.Content)
		}
		logResult(logger, entry)
		return textResult(text, false), nil, nil
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
			return textResult("specify exactly one of content or content_base64", true), TransferOutput{}, nil
		}
		var content []byte
		if args.ContentBase64 != "" {
			decoded, err := base64.StdEncoding.DecodeString(args.ContentBase64)
			if err != nil {
				return textResult("invalid content_base64: "+err.Error(), true), TransferOutput{}, nil
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
			return textResult(fmt.Sprintf("invalid mode %q (expected octal, e.g. 0644): %v", modeStr, err), true), TransferOutput{}, nil
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

		return textResult(fmt.Sprintf("Wrote %d bytes to %s (sha256 %s)", out.Bytes, args.Path, out.SHA256), false), out, nil
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

		res, err := rcp.UploadFile(host, user, localUser, args.LocalPath, args.RemotePath, port)
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

		return textResult(fmt.Sprintf("Uploaded %d bytes to %s (sha256 %s)", out.Bytes, args.RemotePath, out.SHA256), false), out, nil
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

		res, err := rcp.DownloadFile(host, user, localUser, args.RemotePath, port, args.LocalPath, args.Overwrite)
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

		return textResult(fmt.Sprintf("Downloaded %d bytes from %s to %s (sha256 %s)", out.Bytes, args.RemotePath, args.LocalPath, out.SHA256), false), out, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "download",
		Description: "Copy files from a remote host to a local folder via RCP protocol: many at once, binary files too. Logged to rsh_transfers.log.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args DownloadArgs) (*mcp.CallToolResult, any, error) {
		start := time.Now()
		host := firstOrEnv(args.Host, "RSH_HOST")
		user := firstOrEnv(args.User, "RSH_USER")
		localUser := firstOrEnv(args.LocalUser, "RSH_LOCAL_USER")
		port := resolvePort(args.Port)
		logger := openLogger(args.LogDir)
		if len(args.Paths) == 0 || args.To == "" {
			return textResult("download needs paths and to", true), nil, nil
		}
		shell, err := resolveShell(args.Shell)
		if err != nil {
			return errResult("RCP download failed", err), nil, nil
		}

		rep, err := rcp.Download(ctx, host, user, localUser, port, args.Paths, args.To, args.Recursive, quoting(shell))
		logCopy(logger, "DOWNLOAD-MANY", host, port, localUser, user, strings.Join(args.Paths, " "), args.To, rep, err, since(start))
		return copyResult("download", "downloaded", rep, err), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "upload",
		Description: "Copy local files to a remote host via RCP protocol: many at once, binary files too. Logged to rsh_transfers.log.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args UploadArgs) (*mcp.CallToolResult, any, error) {
		start := time.Now()
		host := firstOrEnv(args.Host, "RSH_HOST")
		user := firstOrEnv(args.User, "RSH_USER")
		localUser := firstOrEnv(args.LocalUser, "RSH_LOCAL_USER")
		port := resolvePort(args.Port)
		logger := openLogger(args.LogDir)
		if len(args.Paths) == 0 || args.To == "" {
			return textResult("upload needs paths and to", true), nil, nil
		}
		shell, err := resolveShell(args.Shell)
		if err != nil {
			return errResult("RCP upload failed", err), nil, nil
		}

		rep, err := rcp.Upload(ctx, host, user, localUser, port, args.Paths, args.To, args.Recursive, quoting(shell))
		logCopy(logger, "UPLOAD-MANY", host, port, localUser, user, args.To, strings.Join(args.Paths, " "), rep, err, since(start))
		return copyResult("upload", "uploaded", rep, err), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "version",
		Description: "Report the rsh-mcp server version and Go build info",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args VersionArgs) (*mcp.CallToolResult, any, error) {
		text := "rsh-mcp " + serverVersion() + " (improved fork of github.com/tenox7/rsh-mcp)"
		if info, ok := debug.ReadBuildInfo(); ok {
			text += "\nmodule " + info.Path + "\n" + info.GoVersion
			for _, s := range info.Settings {
				if s.Key == "vcs.revision" || s.Key == "vcs.time" || s.Key == "vcs.modified" {
					text += "\n" + s.Key + " " + s.Value
				}
			}
		}
		return textResult(text, false), nil, nil
	})

	return server
}
