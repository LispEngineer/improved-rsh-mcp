package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/tenox7/rsh-mcp/internal/xferlog"
)

// TransferOutput is the structured result attached to every tool call
// (alongside its human-readable text content), so an agent can verify a
// transfer's integrity and diagnose connection issues without re-parsing
// text.
type TransferOutput struct {
	Bytes          int64  `json:"bytes,omitempty" jsonschema:"description:Number of bytes transferred or returned"`
	SHA256         string `json:"sha256,omitempty" jsonschema:"description:SHA-256 digest of the transferred payload, hex-encoded"`
	SourcePort     int    `json:"sourcePort,omitempty" jsonschema:"description:Local TCP port the connection used"`
	PrivilegedPort bool   `json:"privilegedPort,omitempty" jsonschema:"description:Whether the source port was in the reserved 512-1023 range, as most RSH servers require"`
	LocalUser      string `json:"localUser,omitempty" jsonschema:"description:Local username sent in the RSH handshake"`
	RemoteUser     string `json:"remoteUser,omitempty" jsonschema:"description:Remote username the operation ran as"`
	LogFile        string `json:"logFile,omitempty" jsonschema:"description:Path to the transfer log this call was recorded in"`
	DurationMs     int64  `json:"durationMs,omitempty" jsonschema:"description:Wall-clock time the operation took, in milliseconds"`
}

// firstOrEnv returns v if non-empty, else the named environment variable,
// else "".
func firstOrEnv(v, envVar string) string {
	if v != "" {
		return v
	}
	return os.Getenv(envVar)
}

// portString coerces the flexible `port` argument -- which the JSON schema
// leaves unrestricted so that both a quoted "5141" and a bare 5141 are
// accepted (some MCP clients send tool arguments as native JSON numbers) --
// into the decimal string net.Dial and the wire protocol expect.
func portString(v any) string {
	switch p := v.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(p)
	case float64:
		if p == float64(int64(p)) {
			return strconv.FormatInt(int64(p), 10)
		}
		return strconv.FormatFloat(p, 'f', -1, 64)
	case int:
		return strconv.Itoa(p)
	case int64:
		return strconv.FormatInt(p, 10)
	default:
		return fmt.Sprintf("%v", p)
	}
}

// resolvePort applies, in order: the call's own port argument, RSH_PORT, and
// finally the hardcoded RSH default port 514.
func resolvePort(v any) string {
	if s := portString(v); s != "" {
		return s
	}
	if s := os.Getenv("RSH_PORT"); s != "" {
		return s
	}
	return "514"
}

// previewLines returns at most n lines of b, for the log's Output field --
// full output already goes to the caller in the tool's text content; the
// log only needs enough to be useful without growing unbounded.
func previewLines(b []byte, n int) string {
	if n <= 0 {
		n = 20
	}
	lines := strings.Split(string(b), "\n")
	if len(lines) > n {
		lines = lines[:n]
		lines = append(lines, fmt.Sprintf("... (%d more lines, truncated for the log; full output was returned to the caller)", len(strings.Split(string(b), "\n"))-n))
	}
	return strings.Join(lines, "\n")
}

// openLogger resolves and opens a transfer logger for one tool call. On
// failure it returns nil silently (a logging failure must never fail the
// underlying RSH/RCP operation).
func openLogger(explicitDir string) *xferlog.Logger {
	l, err := xferlog.New(explicitDir)
	if err != nil {
		return nil
	}
	return l
}

// logResult writes one entry if l is non-nil (see openLogger) and returns
// the log file path for structured output, or "" if logging was skipped.
func logResult(l *xferlog.Logger, e xferlog.Entry) string {
	if l == nil {
		return ""
	}
	_ = l.Write(e)
	return l.LogFile()
}

func since(start time.Time) int64 {
	return time.Since(start).Milliseconds()
}
