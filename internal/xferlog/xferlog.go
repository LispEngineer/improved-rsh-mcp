// Package xferlog provides persistent, human-readable audit logging for
// rsh-mcp operations, modeled on the TransferLogger in the companion
// improved-mcp-server-ftp fork (ftp_transfers.log) and the session logs of
// chuk-mcp-telnet-client.
package xferlog

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// LogFileName is the file all entries are appended to within the resolved
// log directory.
const LogFileName = "rsh_transfers.log"

// ResolveLogDir picks the log directory in priority order: an explicit
// argument (e.g. a per-call tool parameter), then RSH_LOG_DIR, then the
// FTP/Telnet/Serial MCP servers' own log-dir variables (so all our lab MCP
// tools land in the same place by default), then ./logs if it exists in the
// current directory, then a global fallback under the home directory.
func ResolveLogDir(explicit string) string {
	if s := strings.TrimSpace(explicit); s != "" {
		return s
	}
	for _, envVar := range []string{"RSH_LOG_DIR", "FTP_LOG_DIR", "TELNET_LOG_DIR", "SERIAL_LOG_DIR"} {
		if v := strings.TrimSpace(os.Getenv(envVar)); v != "" {
			return v
		}
	}
	if info, err := os.Stat("logs"); err == nil && info.IsDir() {
		return "logs"
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".mcp-rsh-logs")
	}
	return ".mcp-rsh-logs"
}

var writeMu sync.Mutex

// Entry is one logged operation. Fields are optional except Operation,
// Host, and Status; zero values are omitted from the rendered block.
type Entry struct {
	Operation      string // EXEC, READ, WRITE, UPLOAD, DOWNLOAD
	Host           string
	Port           string
	LocalUser      string
	RemoteUser     string
	SourcePort     int
	PrivilegedPort bool
	Command        string // exec only
	RemotePath     string // read/write/upload/download
	LocalPath      string // upload/download only
	Bytes          int64
	SHA256         string
	DurationMs     int64
	Status         string // SUCCESS or FAILED
	Error          string
	OutputPreview  string // exec only: first N lines of output
}

// FormatBytes renders a byte count the way a human would read it.
func FormatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	units := "KMGT"[exp : exp+1]
	return fmt.Sprintf("%.2f %sB", float64(n)/float64(div), units)
}

// SHA256Hex returns the hex-encoded SHA-256 digest of b.
func SHA256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// sanitize strips non-printable / non-ASCII bytes from text that came from
// (or is about to go to) the remote host, so the log file itself always
// stays ASCII-clean and one operation stays on a bounded number of lines.
func sanitize(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case r == '\r':
			// drop bare CR, callers already use \n
		case r >= 0x20 && r < 0x7f:
			b.WriteRune(r)
		default:
			b.WriteRune('.')
		}
	}
	return b.String()
}

// Logger appends Entry records to <dir>/rsh_transfers.log.
type Logger struct {
	dir     string
	logPath string
}

// New resolves the log directory (see ResolveLogDir), creates it if
// necessary, and returns a Logger ready to write.
func New(explicitDir string) (*Logger, error) {
	dir := ResolveLogDir(explicitDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("creating log directory %s: %w", dir, err)
	}
	return &Logger{dir: dir, logPath: filepath.Join(dir, LogFileName)}, nil
}

// LogFile returns the full path entries are written to, for returning to
// callers in structured tool results.
func (l *Logger) LogFile() string { return l.logPath }

// Write appends one entry as a human-readable block, matching the style of
// ftp_transfers.log: a delimiter line, one "Key : value" line per non-empty
// field, and a trailing delimiter. Safe for concurrent use.
func (l *Logger) Write(e Entry) error {
	writeMu.Lock()
	defer writeMu.Unlock()

	f, err := os.OpenFile(l.logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("opening %s: %w", l.logPath, err)
	}
	defer f.Close()

	var b strings.Builder
	sep := strings.Repeat("=", 80)
	ts := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")

	fmt.Fprintf(&b, "%s\n", sep)
	fmt.Fprintf(&b, "[%s] RSH TRANSFER: %s - %s\n", ts, e.Operation, e.Status)
	fmt.Fprintf(&b, "Target       : %s:%s\n", e.Host, e.Port)
	if e.LocalUser != "" || e.RemoteUser != "" {
		fmt.Fprintf(&b, "Users        : local=%s remote=%s\n", e.LocalUser, e.RemoteUser)
	}
	if e.SourcePort != 0 {
		fmt.Fprintf(&b, "Source Port  : %d (privileged=%t)\n", e.SourcePort, e.PrivilegedPort)
	}
	if e.Command != "" {
		fmt.Fprintf(&b, "Command      : %s\n", sanitize(e.Command))
	}
	if e.RemotePath != "" {
		fmt.Fprintf(&b, "Remote Path  : %s\n", sanitize(e.RemotePath))
	}
	if e.LocalPath != "" {
		fmt.Fprintf(&b, "Local Path   : %s\n", e.LocalPath)
	}
	if e.Bytes > 0 {
		fmt.Fprintf(&b, "Size         : %d bytes (%s)\n", e.Bytes, FormatBytes(e.Bytes))
	}
	if e.SHA256 != "" {
		fmt.Fprintf(&b, "SHA-256      : %s\n", e.SHA256)
	}
	if e.DurationMs > 0 {
		rate := ""
		if e.Bytes > 0 {
			bps := float64(e.Bytes) / (float64(e.DurationMs) / 1000.0)
			rate = " (" + FormatBytes(int64(bps)) + "/s)"
		}
		fmt.Fprintf(&b, "Duration     : %s ms%s\n", strconv.FormatInt(e.DurationMs, 10), rate)
	}
	if e.OutputPreview != "" {
		fmt.Fprintf(&b, "Output       :\n%s\n", sanitize(e.OutputPreview))
	}
	if e.Error != "" {
		fmt.Fprintf(&b, "Error        : %s\n", sanitize(e.Error))
	}
	fmt.Fprintf(&b, "%s\n\n", sep)

	_, err = f.WriteString(b.String())
	return err
}
