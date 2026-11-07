// Package rcmd implements the client side of the BSD rcmd/rshd
// protocol shared by rsh and rcp.
package rcmd

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"math/rand"
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

// maxDialAttempts bounds how many reserved source ports we actually try
// to connect through before giving up, so an unreachable host fails in
// reasonable time instead of walking all 512 ports.
const maxDialAttempts = 4

// Dial connects to rshd from a privileged source port (512-1023), as
// the protocol requires. It tries those ports in a random order and
// moves on whenever one will not bind or its connection does not come
// up, so many sessions at once and quick reconnects do not all pile
// onto the same port and collide in TIME_WAIT. If no reserved port can
// be bound at all and RequirePrivilegedPort is true, it fails fast with
// a setcap hint; otherwise it falls back to an ephemeral port. Reads on
// the returned conn time out after IdleTimeout of silence so MCP calls
// can not hang forever.
func Dial(hostname, port string) (*DialResult, error) {
	addr := net.JoinHostPort(hostname, port)
	d := net.Dialer{Timeout: dialTimeout}

	attempts := 0
	var lastErr, bindErr error
	for _, off := range rand.Perm(1024 - 512) {
		p := 512 + off
		d.LocalAddr = &net.TCPAddr{Port: p}
		conn, err := d.Dial("tcp", addr)
		if err == nil {
			return &DialResult{Conn: idleConn{conn}, SourcePort: p, PrivilegedPort: true}, nil
		}
		lastErr = err
		if portUnavailable(err) {
			// Could not bind this reserved port (busy, or not
			// permitted here); another one is free to try at no cost.
			bindErr = err
			continue
		}
		// The connection itself did not come up. That can be a
		// TIME_WAIT 4-tuple clash (seen as a timeout) or a reserved
		// source port that momentarily collided with another session
		// (seen as a refusal); either way a different source port
		// usually works, so try a few more before giving up. A host
		// that is genuinely down refuses each one instantly, so we
		// still stop quickly after maxDialAttempts.
		attempts++
		if attempts >= maxDialAttempts {
			return nil, err
		}
	}

	// No reserved port could be used.
	if RequirePrivilegedPort {
		return nil, fmt.Errorf(
			"cannot bind a reserved source port (512-1023): %w -- "+
				"this binary lacks cap_net_bind_service (a rebuild with go build removes it; "+
				"build with make install instead); run: sudo setcap cap_net_bind_service=+ep %s "+
				"(or set RSH_REQUIRE_PRIVPORT=0 to probe without one)",
			bindErr, executablePath())
	}
	d.LocalAddr = nil
	conn, err := d.Dial("tcp", addr)
	if err != nil {
		if lastErr != nil {
			return nil, fmt.Errorf("no reserved source port available (%v); ephemeral port also failed: %w", lastErr, err)
		}
		return nil, err
	}
	localPort := 0
	if ta, ok := conn.LocalAddr().(*net.TCPAddr); ok {
		localPort = ta.Port
	}
	return &DialResult{Conn: idleConn{conn}, SourcePort: localPort, PrivilegedPort: false}, nil
}

// executablePath names this binary for the setcap hint, falling back to a
// placeholder when the OS cannot say.
func executablePath() string {
	if p, err := os.Executable(); err == nil {
		return p
	}
	return "<this binary>"
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
