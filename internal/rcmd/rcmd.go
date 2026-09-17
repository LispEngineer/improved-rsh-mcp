// Package rcmd implements the client side of the BSD rcmd/rshd
// protocol shared by rsh and rcp.
package rcmd

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/user"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const dialTimeout = 10 * time.Second

// IdleTimeout bounds silence from the remote, settable via -idle-timeout.
var IdleTimeout = 5 * time.Minute

// RequirePrivilegedPort controls what happens when no port in 512-1023 can
// be bound. When true (the default), Dial fails fast with a clear message
// instead of silently connecting from an ephemeral port, which most rshd
// servers (including OpenVMS TCPIP$RSH) reject anyway with a confusing EOF.
// Set from the RSH_REQUIRE_PRIVPORT environment variable ("0"/"false"/"no"/
// "off" disables it) -- disabling it is occasionally useful to probe whether
// a given server actually enforces the reserved-port rule.
var RequirePrivilegedPort = envBool("RSH_REQUIRE_PRIVPORT", true)

func envBool(name string, def bool) bool {
	v, ok := os.LookupEnv(name)
	if !ok {
		return def
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "0", "false", "no", "off":
		return false
	}
	return true
}

// DialResult carries the connection plus the source-port bookkeeping that
// callers need for logging and for diagnosing "connection refused" errors.
type DialResult struct {
	Conn           net.Conn
	SourcePort     int
	PrivilegedPort bool
}

// Dial prefers privileged source ports 512-1023 as rshd requires. If none
// can be bound and RequirePrivilegedPort is true, it fails fast with a
// setcap hint. Reads on the returned conn time out after IdleTimeout of
// silence so MCP calls cannot hang forever.
func Dial(hostname, port string) (*DialResult, error) {
	addr := net.JoinHostPort(hostname, port)
	d := net.Dialer{Timeout: dialTimeout}
	var lastErr error
	for p := 1023; p >= 512; p-- {
		d.LocalAddr = &net.TCPAddr{Port: p}
		conn, err := d.Dial("tcp", addr)
		if err == nil {
			return &DialResult{Conn: idleConn{conn}, SourcePort: p, PrivilegedPort: true}, nil
		}
		if portUnavailable(err) {
			lastErr = err
			continue
		}
		return nil, err
	}
	if RequirePrivilegedPort {
		return nil, fmt.Errorf(
			"cannot bind a reserved source port (512-1023): %w -- "+
				"run: sudo setcap cap_net_bind_service=+ep <this binary> "+
				"(or set RSH_REQUIRE_PRIVPORT=0 to probe without one)",
			lastErr)
	}
	d.LocalAddr = nil
	conn, err := d.Dial("tcp", addr)
	if err != nil {
		return nil, err
	}
	localPort := 0
	if ta, ok := conn.LocalAddr().(*net.TCPAddr); ok {
		localPort = ta.Port
	}
	return &DialResult{Conn: idleConn{conn}, SourcePort: localPort, PrivilegedPort: false}, nil
}

func portUnavailable(err error) bool {
	return errors.Is(err, syscall.EADDRINUSE) ||
		errors.Is(err, syscall.EACCES) ||
		errors.Is(err, syscall.EPERM)
}

type idleConn struct{ net.Conn }

func (c idleConn) Read(p []byte) (int, error) {
	c.SetReadDeadline(time.Now().Add(IdleTimeout))
	return c.Conn.Read(p)
}

// SendRequest sends the rshd handshake: stderr port (none), client
// user, server user, command. A stderr port of "0" is what keeps rsh-mcp
// (unlike GNU rsh) single-connection and tunnel-capable: it never asks the
// server to open a second connection back to us for stderr.
func SendRequest(conn net.Conn, localUser, remoteUser, command string) error {
	req := []byte("0\x00" + localUser + "\x00" + remoteUser + "\x00" + command + "\x00")
	_, err := conn.Write(req)
	return err
}

// ReadAck reads one rcmd status byte: 0 is success, 1 is a warning (message
// follows), 2 is a fatal error (message follows). ReadAck returns nil only
// for 0, and labels the message distinctly for 1 vs 2 so callers and logs
// can tell "the server complained but may still have worked" apart from
// "the server refused/aborted".
func ReadAck(r *bufio.Reader) error {
	b, err := r.ReadByte()
	if err == io.EOF {
		return errors.New("server closed connection without response (source port or host rejected?)")
	}
	if err != nil {
		return err
	}
	if b == 0 {
		return nil
	}
	msg, _ := r.ReadString('\n')
	msg = strings.TrimSpace(msg)
	prefix := "remote error"
	if b == 1 {
		prefix = "remote warning"
	}
	if msg == "" {
		return fmt.Errorf("%s (status %d)", prefix, b)
	}
	return fmt.Errorf("%s: %s", prefix, msg)
}

// LocalUser returns the name sent to the server as the requesting user.
// RSH_LOCAL_USER overrides the OS-reported user, which matters when the
// remote proxy database (e.g. OpenVMS TCPIP$RSH /REMOTE_USER) is keyed to a
// specific name that differs from the OS account running this server.
func LocalUser() (string, error) {
	if v := os.Getenv("RSH_LOCAL_USER"); v != "" {
		return v, nil
	}
	u, err := user.Current()
	if err != nil {
		return "", err
	}
	return u.Username, nil
}

// FormatPort renders an int as the decimal string the wire protocol and
// net.Dial expect.
func FormatPort(p int) string {
	return strconv.Itoa(p)
}
