// Package rcp implements the client side of the rcp-over-rsh file transfer
// protocol (the "source"/"sink" exchange BSD rcp uses on top of rcmd/rshd).
//
// All transfers are treated as opaque bytes end to end -- there is no text
// decoding anywhere in this package, so binary files (executables, archives,
// OpenVMS savesets) survive a round trip unchanged.
package rcp

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/tenox7/rsh-mcp/internal/rcmd"
)

// Result carries a transfer's outcome plus the connection bookkeeping
// callers need for logging: which local port was used, whether it was a
// reserved (privileged) port, and (for reads) the file's mode as reported
// by the remote source.
type Result struct {
	Content        []byte // populated by ReadFile only
	Bytes          int64
	SHA256         string
	SourcePort     int
	PrivilegedPort bool
	LocalUser      string
	RemoteUser     string
	RemoteMode     string // e.g. "0644", from the remote C-line (ReadFile/Download only)
}

func resolveUsers(username, localUser string) (string, string, error) {
	var err error
	if localUser == "" {
		localUser, err = rcmd.LocalUser()
		if err != nil {
			return "", "", err
		}
	}
	if username == "" {
		username = localUser
	}
	return username, localUser, nil
}

// controlLine reads one rcp control line from the source side of a
// transfer, transparently acking and skipping T (mtime, from "-p") lines,
// and rejecting D (directory) lines with a clear "not supported" error
// instead of the raw protocol byte. The caller acks the returned C line
// itself, once it has decided how to receive the payload.
func controlLine(conn io.Writer, r *bufio.Reader) (mode string, size int64, name string, err error) {
	for {
		line, rerr := r.ReadString('\n')
		if line == "" {
			if rerr != nil {
				return "", 0, "", fmt.Errorf("reading control line: %w", rerr)
			}
			return "", 0, "", errors.New("empty control line from remote")
		}
		switch line[0] {
		case 1, 2:
			prefix := "remote error"
			if line[0] == 1 {
				prefix = "remote warning"
			}
			return "", 0, "", fmt.Errorf("%s: %s", prefix, strings.TrimSpace(line[1:]))
		case 'T':
			// Modification-time line from "rcp -p"; we don't preserve
			// mtimes, so ack it and move on to the real C line.
			if _, werr := conn.Write([]byte{0}); werr != nil {
				return "", 0, "", werr
			}
			continue
		case 'D':
			return "", 0, "", errors.New("remote sent a directory entry; recursive copy is not supported")
		case '%':
			// OpenVMS TCPIP$RCP reports errors (e.g. a missing file) as a
			// plain "%FACILITY-E-IDENT, message" line with no leading
			// status byte, unlike BSD rcp's 1/2-prefixed convention above.
			return "", 0, "", fmt.Errorf("remote error: %s", strings.TrimSpace(line))
		case 'C':
			parts := strings.SplitN(strings.TrimSpace(line[1:]), " ", 3)
			if len(parts) != 3 {
				return "", 0, "", fmt.Errorf("invalid file info line: %q", strings.TrimSpace(line))
			}
			size, err = strconv.ParseInt(parts[1], 10, 64)
			if err != nil {
				return "", 0, "", fmt.Errorf("parsing file size: %w", err)
			}
			return parts[0], size, parts[2], nil
		default:
			return "", 0, "", fmt.Errorf("unexpected rcp control message: %q", strings.TrimSpace(line))
		}
	}
}

// receive runs the "sink" side of an rcp transfer for exactly one file,
// copying its bytes into dest and returning its reported mode, the byte
// count, and a SHA-256 digest computed while streaming (never buffering the
// file twice).
func receive(dr *rcmd.DialResult, dest io.Writer) (mode string, n int64, sum string, err error) {
	conn := dr.Conn
	r := bufio.NewReader(conn)
	if err = rcmd.ReadAck(r); err != nil {
		return "", 0, "", err
	}
	// Announce readiness; the source then sends one control line per file.
	if _, err = conn.Write([]byte{0}); err != nil {
		return "", 0, "", err
	}

	mode, size, _, err := controlLine(conn, r)
	if err != nil {
		return "", 0, "", err
	}
	if _, err = conn.Write([]byte{0}); err != nil {
		return "", 0, "", err
	}

	h := sha256.New()
	n, err = io.CopyN(io.MultiWriter(dest, h), r, size)
	if err != nil {
		return "", n, "", fmt.Errorf("reading file data: %w", err)
	}

	if err = rcmd.ReadAck(r); err != nil {
		return "", n, "", fmt.Errorf("transfer status: %w", err)
	}
	if _, err = conn.Write([]byte{0}); err != nil {
		return "", n, "", err
	}
	return mode, n, hex.EncodeToString(h.Sum(nil)), nil
}

// send runs the "source" side of an rcp transfer for exactly one file of
// known size, streaming from src (never loading the whole file into memory
// twice) and returning the byte count and a SHA-256 digest of what was sent.
func send(dr *rcmd.DialResult, remotePath string, mode os.FileMode, size int64, src io.Reader) (n int64, sum string, err error) {
	conn := dr.Conn
	r := bufio.NewReader(conn)
	if err = rcmd.ReadAck(r); err != nil {
		return 0, "", err
	}
	if err = rcmd.ReadAck(r); err != nil {
		return 0, "", fmt.Errorf("rcp: %w", err)
	}

	perm := mode.Perm()
	if perm == 0 {
		perm = 0o644
	}
	fileInfo := fmt.Sprintf("C%04o %d %s\n", perm, size, filepath.Base(remotePath))
	if _, err = conn.Write([]byte(fileInfo)); err != nil {
		return 0, "", err
	}
	if err = rcmd.ReadAck(r); err != nil {
		return 0, "", fmt.Errorf("file info rejected: %w", err)
	}

	h := sha256.New()
	n, err = io.CopyN(io.MultiWriter(conn, h), src, size)
	if err != nil && err != io.EOF {
		return n, "", fmt.Errorf("writing file data: %w", err)
	}
	if _, err = conn.Write([]byte{0}); err != nil {
		return n, "", err
	}
	if err = rcmd.ReadAck(r); err != nil {
		return n, "", fmt.Errorf("transfer failed: %w", err)
	}

	if _, err = conn.Write([]byte("E\n")); err != nil {
		return n, "", err
	}
	if err = rcmd.ReadAck(r); err != nil {
		return n, "", fmt.Errorf("end of transfer rejected: %w", err)
	}
	return n, hex.EncodeToString(h.Sum(nil)), nil
}

// ReadFile reads remotePath in full into memory and returns it, along with
// connection/logging metadata. Suitable for text or modestly sized binaries;
// for large files prefer Download, which streams straight to disk.
func ReadFile(hostname, username, localUser, remotePath, port string) (*Result, error) {
	username, localUser, err := resolveUsers(username, localUser)
	if err != nil {
		return nil, err
	}
	dr, err := rcmd.Dial(hostname, port)
	if err != nil {
		return nil, err
	}
	defer dr.Conn.Close()

	if err := rcmd.SendRequest(dr.Conn, localUser, username, "rcp -f "+remotePath); err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	mode, n, sum, err := receive(dr, &buf)
	if err != nil {
		return nil, fmt.Errorf("rcp read %s: %w", hostname, err)
	}
	return &Result{
		Content: buf.Bytes(), Bytes: n, SHA256: sum,
		SourcePort: dr.SourcePort, PrivilegedPort: dr.PrivilegedPort,
		LocalUser: localUser, RemoteUser: username, RemoteMode: mode,
	}, nil
}

// WriteFile writes content to remotePath in one rcp transfer.
func WriteFile(hostname, username, localUser, remotePath, port string, content []byte, mode os.FileMode) (*Result, error) {
	username, localUser, err := resolveUsers(username, localUser)
	if err != nil {
		return nil, err
	}
	dr, err := rcmd.Dial(hostname, port)
	if err != nil {
		return nil, err
	}
	defer dr.Conn.Close()

	if err := rcmd.SendRequest(dr.Conn, localUser, username, "rcp -t "+remotePath); err != nil {
		return nil, err
	}

	n, sum, err := send(dr, remotePath, mode, int64(len(content)), bytes.NewReader(content))
	if err != nil {
		return nil, fmt.Errorf("rcp write %s: %w", hostname, err)
	}
	return &Result{
		Bytes: n, SHA256: sum,
		SourcePort: dr.SourcePort, PrivilegedPort: dr.PrivilegedPort,
		LocalUser: localUser, RemoteUser: username,
	}, nil
}

// Download reads remotePath and streams it directly to localPath, without
// holding the whole file in memory. It refuses to overwrite an existing
// local file unless overwrite is true, and writes to a temporary file in
// the same directory first, renaming into place only on success, so a
// failed transfer never leaves a truncated file at localPath.
func Download(hostname, username, localUser, remotePath, port, localPath string, overwrite bool) (*Result, error) {
	username, localUser, err := resolveUsers(username, localUser)
	if err != nil {
		return nil, err
	}
	if !overwrite {
		if _, statErr := os.Stat(localPath); statErr == nil {
			return nil, fmt.Errorf("local file %s already exists (pass overwrite=true to replace it)", localPath)
		}
	}

	dr, err := rcmd.Dial(hostname, port)
	if err != nil {
		return nil, err
	}
	defer dr.Conn.Close()

	if err := rcmd.SendRequest(dr.Conn, localUser, username, "rcp -f "+remotePath); err != nil {
		return nil, err
	}

	dir := filepath.Dir(localPath)
	if dir == "" {
		dir = "."
	}
	tmp, err := os.CreateTemp(dir, ".rsh-mcp-download-*")
	if err != nil {
		return nil, fmt.Errorf("creating temp file: %w", err)
	}
	tmpName := tmp.Name()
	success := false
	defer func() {
		tmp.Close()
		if !success {
			os.Remove(tmpName)
		}
	}()

	_, n, sum, err := receive(dr, tmp)
	if err != nil {
		return nil, fmt.Errorf("rcp download %s: %w", hostname, err)
	}
	if err := tmp.Close(); err != nil {
		return nil, fmt.Errorf("closing temp file: %w", err)
	}
	if err := os.Rename(tmpName, localPath); err != nil {
		return nil, fmt.Errorf("renaming into place: %w", err)
	}
	success = true

	return &Result{
		Bytes: n, SHA256: sum,
		SourcePort: dr.SourcePort, PrivilegedPort: dr.PrivilegedPort,
		LocalUser: localUser, RemoteUser: username,
	}, nil
}

// Upload streams localPath to remotePath without holding the whole file in
// memory.
func Upload(hostname, username, localUser, localPath, remotePath, port string) (*Result, error) {
	username, localUser, err := resolveUsers(username, localUser)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(localPath)
	if err != nil {
		return nil, fmt.Errorf("opening local file: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat local file: %w", err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("%s is a directory; recursive upload is not supported", localPath)
	}

	dr, err := rcmd.Dial(hostname, port)
	if err != nil {
		return nil, err
	}
	defer dr.Conn.Close()

	if err := rcmd.SendRequest(dr.Conn, localUser, username, "rcp -t "+remotePath); err != nil {
		return nil, err
	}

	n, sum, err := send(dr, remotePath, info.Mode(), info.Size(), f)
	if err != nil {
		return nil, fmt.Errorf("rcp upload %s: %w", hostname, err)
	}
	return &Result{
		Bytes: n, SHA256: sum,
		SourcePort: dr.SourcePort, PrivilegedPort: dr.PrivilegedPort,
		LocalUser: localUser, RemoteUser: username,
	}, nil
}
