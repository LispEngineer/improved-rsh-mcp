package rcp

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/tenox7/rsh-mcp/internal/rcmd"
)

type Copied struct {
	Path string
	Size int64
}

type Report struct {
	Files  []Copied
	Errors []string

	// Connection bookkeeping for logging and diagnostics.
	SourcePort     int
	PrivilegedPort bool
	LocalUser      string
	RemoteUser     string
}

// Quoting chooses how remote paths are put on the rcp command line.
// QuoteSh (the default) quotes anything unusual for a Unix /bin/sh;
// QuoteNone passes paths as given, for remotes whose login shell is
// not sh (OpenVMS DCL, for example, where SYS$DISK:[DIR]FILE.TXT;1
// must reach rcp exactly as written).
type Quoting int

const (
	QuoteSh Quoting = iota
	QuoteNone
)

// Download copies remote files into localDir. With QuoteNone (a remote
// without /bin/sh) each path gets its own rcp session, because some rcp
// servers (OpenVMS TCPIP$RCP) take only one source spec per command;
// wildcards within one spec still work.
func Download(ctx context.Context, hostname, username, localUser, port string, paths []string, localDir string, recursive bool, q Quoting) (*Report, error) {
	if q == QuoteNone && len(paths) > 1 {
		var all *Report
		for _, p := range paths {
			rep, err := download(ctx, hostname, username, localUser, port, []string{p}, localDir, recursive, q)
			all = merge(all, rep, p, err)
			if ctx.Err() != nil {
				return all, errors.New("cancelled")
			}
		}
		return all, nil
	}
	return download(ctx, hostname, username, localUser, port, paths, localDir, recursive, q)
}

// merge adds one session's report to the running total; a session that
// failed outright becomes an error line, so the other paths still count.
func merge(all, rep *Report, path string, err error) *Report {
	if all == nil {
		all = &Report{}
	}
	if rep != nil {
		all.Files = append(all.Files, rep.Files...)
		all.Errors = append(all.Errors, rep.Errors...)
		if all.SourcePort == 0 {
			all.SourcePort, all.PrivilegedPort = rep.SourcePort, rep.PrivilegedPort
			all.LocalUser, all.RemoteUser = rep.LocalUser, rep.RemoteUser
		}
	}
	if err != nil {
		all.Errors = append(all.Errors, path+": "+err.Error())
	}
	return all
}

func download(ctx context.Context, hostname, username, localUser, port string, paths []string, localDir string, recursive bool, q Quoting) (*Report, error) {
	if err := os.MkdirAll(localDir, 0755); err != nil {
		return nil, err
	}
	command := "rcp -f " + flag(recursive, "-r ") + quoteAll(paths, q)
	conn, r, rep, err := open(hostname, username, localUser, port, command)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	defer context.AfterFunc(ctx, func() { conn.Close() })()

	dirs := []string{localDir}
	if err := ack(conn); err != nil {
		return rep, err
	}
	for {
		line, err := r.ReadString('\n')
		if err == io.EOF && line == "" {
			return rep, nil
		}
		if err != nil {
			return rep, cancelled(ctx, err)
		}
		switch line[0] {
		case 1:
			rep.Errors = append(rep.Errors, strings.TrimSpace(line[1:]))
			continue
		case 2:
			return rep, errors.New(strings.TrimSpace(line[1:]))
		case 'T':
		case 'E':
			if len(dirs) > 1 {
				dirs = dirs[:len(dirs)-1]
			}
		case 'D':
			mode, _, name, err := parseControl(line)
			if err != nil {
				return rep, err
			}
			dir := filepath.Join(dirs[len(dirs)-1], name)
			if err := os.MkdirAll(dir, mode|0700); err != nil {
				return rep, err
			}
			dirs = append(dirs, dir)
		case 'C':
			mode, size, name, err := parseControl(line)
			if err != nil {
				return rep, err
			}
			path := filepath.Join(dirs[len(dirs)-1], name)
			if err := receive(conn, r, path, mode, size); err != nil {
				return rep, cancelled(ctx, err)
			}
			rep.Files = append(rep.Files, Copied{path, size})
			continue
		default:
			return rep, fmt.Errorf("unexpected rcp message: %q", strings.TrimSpace(line))
		}
		if err := ack(conn); err != nil {
			return rep, err
		}
	}
}

func receive(conn net.Conn, r *bufio.Reader, path string, mode os.FileMode, size int64) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode|0600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := ack(conn); err != nil {
		return err
	}
	if _, err := io.CopyN(f, r, size); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if err := rcmd.ReadAck(r); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return ack(conn)
}

// Upload copies local files to remoteTarget. With QuoteNone (a remote
// without /bin/sh) each file gets its own rcp session with the full
// target name (remoteTarget, plus the file's name when remoteTarget
// names a folder or there are several files), because some rcp servers
// (OpenVMS TCPIP$RCP) reject the multi-file "-d" form as an ambiguous
// target. Folders are not supported that way.
func Upload(ctx context.Context, hostname, username, localUser, port string, paths []string, remoteTarget string, recursive bool, q Quoting) (*Report, error) {
	if q == QuoteNone {
		toDir := len(paths) > 1 || strings.HasSuffix(remoteTarget, "/") || strings.ContainsAny(remoteTarget[len(remoteTarget)-1:], "]>:")
		var all *Report
		for _, p := range paths {
			if st, err := os.Stat(p); err != nil {
				return nil, err
			} else if st.IsDir() {
				return nil, fmt.Errorf("%s is a folder: folders can not be copied with shell login", p)
			}
			target := remoteTarget
			if toDir {
				sep := ""
				if !strings.ContainsAny(remoteTarget[len(remoteTarget)-1:], "]>:/") {
					sep = "/"
				}
				target = remoteTarget + sep + filepath.Base(p)
			}
			rep, err := upload(ctx, hostname, username, localUser, port, []string{p}, target, false, q)
			all = merge(all, rep, p, err)
			if ctx.Err() != nil {
				return all, errors.New("cancelled")
			}
		}
		return all, nil
	}
	return upload(ctx, hostname, username, localUser, port, paths, remoteTarget, recursive, q)
}

func upload(ctx context.Context, hostname, username, localUser, port string, paths []string, remoteTarget string, recursive bool, q Quoting) (*Report, error) {
	toDir := len(paths) > 1
	for _, p := range paths {
		st, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		if st.IsDir() && !recursive {
			return nil, fmt.Errorf("%s is a folder: set recursive", p)
		}
		toDir = toDir || st.IsDir()
	}
	command := "rcp -t " + flag(toDir, "-d ") + flag(recursive, "-r ") + quote(remoteTarget, q)
	conn, r, rep, err := open(hostname, username, localUser, port, command)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	defer context.AfterFunc(ctx, func() { conn.Close() })()

	if err := rcmd.ReadAck(r); err != nil {
		return nil, fmt.Errorf("rcp: %w", err)
	}
	for _, p := range paths {
		if err := send(conn, r, p, rep); err != nil {
			return rep, cancelled(ctx, err)
		}
	}
	return rep, nil
}

func send(conn net.Conn, r *bufio.Reader, path string, rep *Report) error {
	st, err := os.Stat(path)
	if err != nil {
		rep.Errors = append(rep.Errors, err.Error())
		return nil
	}
	if st.IsDir() {
		return sendDir(conn, r, path, st, rep)
	}
	f, err := os.Open(path)
	if err != nil {
		rep.Errors = append(rep.Errors, err.Error())
		return nil
	}
	defer f.Close()

	header := fmt.Sprintf("C%04o %d %s\n", st.Mode().Perm(), st.Size(), filepath.Base(path))
	if _, err := io.WriteString(conn, header); err != nil {
		return err
	}
	if err := rcmd.ReadAck(r); err != nil {
		rep.Errors = append(rep.Errors, err.Error())
		return nil
	}
	if _, err := io.CopyN(conn, f, st.Size()); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if err := ack(conn); err != nil {
		return err
	}
	if err := rcmd.ReadAck(r); err != nil {
		rep.Errors = append(rep.Errors, err.Error())
		return nil
	}
	rep.Files = append(rep.Files, Copied{path, st.Size()})
	return nil
}

func sendDir(conn net.Conn, r *bufio.Reader, path string, st os.FileInfo, rep *Report) error {
	entries, err := os.ReadDir(path)
	if err != nil {
		rep.Errors = append(rep.Errors, err.Error())
		return nil
	}
	header := fmt.Sprintf("D%04o 0 %s\n", st.Mode().Perm(), filepath.Base(path))
	if _, err := io.WriteString(conn, header); err != nil {
		return err
	}
	if err := rcmd.ReadAck(r); err != nil {
		rep.Errors = append(rep.Errors, err.Error())
		return nil
	}
	for _, e := range entries {
		if err := send(conn, r, filepath.Join(path, e.Name()), rep); err != nil {
			return err
		}
	}
	if _, err := io.WriteString(conn, "E\n"); err != nil {
		return err
	}
	return rcmd.ReadAck(r)
}

func open(hostname, username, localUser, port, command string) (net.Conn, *bufio.Reader, *Report, error) {
	username, localUser, err := resolveUsers(username, localUser)
	if err != nil {
		return nil, nil, nil, err
	}
	dr, err := rcmd.Dial(hostname, port)
	if err != nil {
		return nil, nil, nil, err
	}
	conn := dr.Conn
	if err := rcmd.SendRequest(conn, localUser, username, command); err != nil {
		conn.Close()
		return nil, nil, nil, err
	}
	r := bufio.NewReader(conn)
	if err := rcmd.ReadAck(r); err != nil {
		conn.Close()
		return nil, nil, nil, fmt.Errorf("rsh %s: %w", hostname, err)
	}
	rep := &Report{SourcePort: dr.SourcePort, PrivilegedPort: dr.PrivilegedPort, LocalUser: localUser, RemoteUser: username}
	return conn, r, rep, nil
}

func parseControl(line string) (os.FileMode, int64, string, error) {
	parts := strings.SplitN(strings.TrimSpace(line[1:]), " ", 3)
	if len(parts) != 3 {
		return 0, 0, "", fmt.Errorf("invalid rcp message: %q", strings.TrimSpace(line))
	}
	mode, err := strconv.ParseUint(parts[0], 8, 32)
	if err != nil {
		return 0, 0, "", fmt.Errorf("invalid mode in %q", strings.TrimSpace(line))
	}
	size, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return 0, 0, "", fmt.Errorf("invalid size in %q", strings.TrimSpace(line))
	}
	name := parts[2]
	if name == "." || name == ".." || strings.ContainsAny(name, "/\x00") {
		return 0, 0, "", fmt.Errorf("refusing remote file name %q", name)
	}
	return os.FileMode(mode).Perm(), size, name, nil
}

func ack(conn net.Conn) error {
	_, err := conn.Write([]byte{0})
	return err
}

func cancelled(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return errors.New("cancelled")
	}
	return err
}

func flag(on bool, f string) string {
	if on {
		return f
	}
	return ""
}

var plainPath = regexp.MustCompile(`^[A-Za-z0-9_./*?~+,:=@%-]+$`)

func quote(path string, q Quoting) string {
	if q == QuoteNone || plainPath.MatchString(path) {
		return path
	}
	return "'" + strings.ReplaceAll(path, "'", `'\''`) + "'"
}

func quoteAll(paths []string, q Quoting) string {
	quoted := make([]string, len(paths))
	for i, p := range paths {
		quoted[i] = quote(p, q)
	}
	return strings.Join(quoted, " ")
}
