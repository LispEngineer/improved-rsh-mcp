package main

import (
	"bufio"
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tenox7/rsh-mcp/internal/rcmd"
)

func init() {
	rcmd.RequirePrivilegedPort = false
}

// fakeRshd answers one rcmd request: it records the command and lets
// serve(cmd, r, conn) play the remote side after the ack.
func fakeRshd(t *testing.T, serve func(cmd string, r *bufio.Reader, conn net.Conn)) (port string, gotCmd chan string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gotCmd = make(chan string, 1)
	go func() {
		defer ln.Close()
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		r := bufio.NewReader(conn)
		for i := 0; i < 3; i++ {
			r.ReadString(0)
		}
		cmd, _ := r.ReadString(0)
		cmd = strings.TrimSuffix(cmd, "\x00")
		gotCmd <- cmd
		conn.Write([]byte{0})
		serve(cmd, r, conn)
	}()
	return strconv.Itoa(ln.Addr().(*net.TCPAddr).Port), gotCmd
}

func connect(t *testing.T) *mcp.ClientSession {
	t.Helper()
	t.Setenv("RSH_LOG_DIR", t.TempDir())
	ct, st := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := newServer().Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func call(t *testing.T, cs *mcp.ClientSession, tool string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	return res
}

func text(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

// The bug this guards against: exec returned its output as text AND a
// structured bookkeeping result, and clients that show structuredContent
// instead of the text (Claude Code) never showed the output.
func TestExecReturnsOutputAsTextOnly(t *testing.T) {
	port, got := fakeRshd(t, func(cmd string, _ *bufio.Reader, conn net.Conn) {
		for i := 1; i <= 30; i++ {
			conn.Write([]byte("line " + strconv.Itoa(i) + "\r\n"))
		}
	})
	cs := connect(t)
	res := call(t, cs, "exec", map[string]any{"host": "127.0.0.1", "port": port, "command": "DIR", "shell": "login", "user": "U", "local_user": "L"})
	if c := <-got; c != "DIR" {
		t.Errorf("login mode sent %q", c)
	}
	if res.IsError {
		t.Fatalf("exec error: %s", text(res))
	}
	if res.StructuredContent != nil {
		t.Errorf("exec returned structuredContent %v; it would hide the output", res.StructuredContent)
	}
	out := text(res)
	if !strings.Contains(out, "line 1\n") || !strings.Contains(out, "line 30\n") || strings.Contains(out, "\r") {
		t.Errorf("exec text = %q, want all 30 lines with LF endings", out)
	}
	if strings.Contains(out, "no exit status") {
		t.Errorf("login mode should not complain about a missing exit status: %q", out)
	}
}

func TestExecShellFromEnv(t *testing.T) {
	t.Setenv("RSH_SHELL", "login")
	port, got := fakeRshd(t, func(string, *bufio.Reader, net.Conn) {})
	cs := connect(t)
	call(t, cs, "exec", map[string]any{"host": "127.0.0.1", "port": port, "command": "SHOW TIME", "user": "U", "local_user": "L"})
	if c := <-got; c != "SHOW TIME" {
		t.Errorf("RSH_SHELL=login: sent %q, want the command as given", c)
	}
}

func TestExecRejectsUnknownShell(t *testing.T) {
	cs := connect(t)
	res := call(t, cs, "exec", map[string]any{"host": "127.0.0.1", "port": "1", "command": "x", "shell": "dcl"})
	if !res.IsError || !strings.Contains(text(res), "invalid shell") {
		t.Errorf("shell dcl: %s", text(res))
	}
}

func TestReadReturnsContentAsTextOnly(t *testing.T) {
	port, _ := fakeRshd(t, func(cmd string, r *bufio.Reader, conn net.Conn) {
		r.ReadByte() // sink's ready ack
		conn.Write([]byte("C0644 6 F.TXT\n"))
		r.ReadByte()
		conn.Write([]byte("hello\n\x00"))
		r.ReadByte()
	})
	cs := connect(t)
	res := call(t, cs, "read", map[string]any{"host": "127.0.0.1", "port": port, "path": "F.TXT", "user": "U", "local_user": "L"})
	if res.IsError || res.StructuredContent != nil || text(res) != "hello\n" {
		t.Errorf("read: error %v, structured %v, text %q", res.IsError, res.StructuredContent, text(res))
	}
}

// With shell login (OpenVMS), a VMS file spec must reach rcp exactly as
// written; with sh it is quoted for /bin/sh.
func TestDownloadQuoting(t *testing.T) {
	for _, tc := range []struct{ shell, want string }{
		{"login", "rcp -f SYS$SYSDEVICE:[HOME.SYSTEM]A.TXT;1"},
		{"sh", "rcp -f 'SYS$SYSDEVICE:[HOME.SYSTEM]A.TXT;1'"},
	} {
		port, got := fakeRshd(t, func(cmd string, r *bufio.Reader, conn net.Conn) {
			r.ReadByte()
			conn.Write([]byte("C0644 3 A.TXT\n"))
			r.ReadByte()
			conn.Write([]byte("abc\x00"))
			r.ReadByte()
		})
		dir := t.TempDir()
		cs := connect(t)
		res := call(t, cs, "download", map[string]any{"host": "127.0.0.1", "port": port, "user": "U", "local_user": "L",
			"paths": []string{"SYS$SYSDEVICE:[HOME.SYSTEM]A.TXT;1"}, "to": dir, "shell": tc.shell})
		if c := <-got; c != tc.want {
			t.Errorf("shell %s sent %q, want %q", tc.shell, c, tc.want)
		}
		if res.IsError {
			t.Errorf("shell %s: %s", tc.shell, text(res))
		}
		if b, err := os.ReadFile(filepath.Join(dir, "A.TXT")); err != nil || string(b) != "abc" {
			t.Errorf("shell %s: file %q, %v", tc.shell, b, err)
		}
	}
}

func TestVersionTool(t *testing.T) {
	cs := connect(t)
	if out := text(call(t, cs, "version", map[string]any{})); !strings.HasPrefix(out, "rsh-mcp "+forkVersion) {
		t.Errorf("version = %q", out)
	}
}
