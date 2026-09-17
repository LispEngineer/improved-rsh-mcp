package rsh

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"strings"

	"github.com/tenox7/rsh-mcp/internal/rcmd"
)

// Result carries Execute's output plus the connection bookkeeping callers
// need for logging and diagnostics.
type Result struct {
	Output         []byte
	SourcePort     int
	PrivilegedPort bool
	LocalUser      string
	RemoteUser     string
}

// Execute runs command on hostname via RSH and returns its output (stdout;
// stderr is not requested -- see rcmd.SendRequest, which is what keeps this
// single-connection and tunnel-capable, unlike GNU rsh).
func Execute(hostname, username, localUser, command, port string, maxLines, maxBytes int, tail bool) (*Result, error) {
	var err error
	if localUser == "" {
		localUser, err = rcmd.LocalUser()
		if err != nil {
			return nil, err
		}
	}
	if username == "" {
		username = localUser
	}

	dr, err := rcmd.Dial(hostname, port)
	if err != nil {
		return nil, err
	}
	conn := dr.Conn
	defer conn.Close()

	if err := rcmd.SendRequest(conn, localUser, username, command); err != nil {
		return nil, err
	}

	r := bufio.NewReader(conn)
	if err := rcmd.ReadAck(r); err != nil {
		return nil, fmt.Errorf("rsh %s: %w", hostname, err)
	}

	if maxLines <= 0 {
		maxLines = 1000
	}
	if maxBytes <= 0 {
		maxBytes = 100000
	}

	var result []byte
	buffer := make([]byte, 4096)
	for len(result) < maxBytes {
		n, err := r.Read(buffer)
		if n > maxBytes-len(result) {
			n = maxBytes - len(result)
		}
		result = append(result, buffer[:n]...)
		if err != nil {
			break
		}
	}

	// Command output almost always ends in a trailing newline, which
	// bytes.Split turns into a spurious final empty "line"; without
	// dropping it, /max_lines=N/tail=true would return N-1 real lines
	// plus that empty one instead of the last N real lines.
	trailingNewline := bytes.HasSuffix(result, []byte{'\n'})
	lines := bytes.Split(result, []byte{'\n'})
	if trailingNewline && len(lines) > 0 {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > maxLines {
		if tail {
			lines = lines[len(lines)-maxLines:]
		} else {
			lines = lines[:maxLines]
		}
	}
	result = bytes.Join(lines, []byte{'\n'})
	if trailingNewline {
		result = append(result, '\n')
	}

	return &Result{
		Output:         result,
		SourcePort:     dr.SourcePort,
		PrivilegedPort: dr.PrivilegedPort,
		LocalUser:      localUser,
		RemoteUser:     username,
	}, nil
}

func ParseUserHost(userHost string) (username, hostname string, err error) {
	parts := strings.Split(userHost, "@")

	switch len(parts) {
	case 1:
		username, err = rcmd.LocalUser()
		if err != nil {
			return "", "", err
		}
		hostname = parts[0]
	case 2:
		username = parts[0]
		hostname = parts[1]
	default:
		return "", "", errors.New("invalid username@hostname format")
	}

	return username, hostname, nil
}
