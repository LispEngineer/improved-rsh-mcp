package rsh

import (
	"bufio"
	"net"
	"strconv"
	"strings"
	"testing"

	"github.com/tenox7/rsh-mcp/internal/rcmd"
)

func init() {
	rcmd.RequirePrivilegedPort = false
}

func TestParseUserHost(t *testing.T) {
	cases := []struct {
		in       string
		wantUser string
		wantHost string
		wantErr  bool
	}{
		{"192.168.3.201", "", "192.168.3.201", false}, // wantUser filled from OS user, just check host
		{"USER1@192.168.3.201", "USER1", "192.168.3.201", false},
		{"a@b@c", "", "", true},
	}
	for _, tc := range cases {
		user, host, err := ParseUserHost(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseUserHost(%q): expected error", tc.in)
			}
			continue
		}
		if err != nil {
			t.Fatalf("ParseUserHost(%q): %v", tc.in, err)
		}
		if host != tc.wantHost {
			t.Errorf("ParseUserHost(%q) host = %q, want %q", tc.in, host, tc.wantHost)
		}
		if tc.wantUser != "" && user != tc.wantUser {
			t.Errorf("ParseUserHost(%q) user = %q, want %q", tc.in, user, tc.wantUser)
		}
	}
}

// fakeExecServer accepts one rcmd exec request and echoes the command back
// as output, then closes -- just enough to exercise Execute end to end
// (dial, handshake, ack, read output, source-port reporting).
func fakeExecServer(t *testing.T) (host, port string, done chan struct{}) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done = make(chan struct{})
	go func() {
		defer close(done)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		defer ln.Close()
		r := bufio.NewReader(conn)
		r.ReadString(0) // stderr port
		r.ReadString(0) // local user
		r.ReadString(0) // remote user
		cmd, _ := r.ReadString(0)
		conn.Write([]byte{0}) // ack
		conn.Write([]byte("echo:" + strings.TrimSuffix(cmd, "\x00")))
	}()
	ta := ln.Addr().(*net.TCPAddr)
	return "127.0.0.1", strconv.Itoa(ta.Port), done
}

func TestExecuteReturnsOutputAndSourcePort(t *testing.T) {
	host, port, done := fakeExecServer(t)
	res, err := Execute(host, "USER1", "dfields", "SHOW TIME", port, 0, 0, false)
	<-done
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if string(res.Output) != "echo:SHOW TIME" {
		t.Errorf("Output = %q", res.Output)
	}
	if res.SourcePort == 0 {
		t.Error("expected a nonzero source port")
	}
	if res.LocalUser != "dfields" || res.RemoteUser != "USER1" {
		t.Errorf("user metadata: local=%s remote=%s", res.LocalUser, res.RemoteUser)
	}
}

func TestExecuteMaxLinesTail(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		defer ln.Close()
		r := bufio.NewReader(conn)
		r.ReadString(0)
		r.ReadString(0)
		r.ReadString(0)
		r.ReadString(0)
		conn.Write([]byte{0})
		conn.Write([]byte("l1\nl2\nl3\nl4\nl5\n"))
	}()
	ta := ln.Addr().(*net.TCPAddr)

	res, err := Execute("127.0.0.1", "USER1", "dfields", "cmd", strconv.Itoa(ta.Port), 2, 0, true)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	got := string(res.Output)
	if got != "l4\nl5\n" {
		t.Errorf("tail-limited output = %q, want %q", got, "l4\nl5\n")
	}
}

func TestExecuteMaxLinesHead(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		defer ln.Close()
		r := bufio.NewReader(conn)
		r.ReadString(0)
		r.ReadString(0)
		r.ReadString(0)
		r.ReadString(0)
		conn.Write([]byte{0})
		conn.Write([]byte("l1\nl2\nl3\nl4\nl5\n"))
	}()
	ta := ln.Addr().(*net.TCPAddr)

	res, err := Execute("127.0.0.1", "USER1", "dfields", "cmd", strconv.Itoa(ta.Port), 2, 0, false)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	got := string(res.Output)
	if got != "l1\nl2\n" {
		t.Errorf("head-limited output = %q, want %q", got, "l1\nl2\n")
	}
}

func TestExecuteNoTrailingNewlinePreserved(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		defer ln.Close()
		r := bufio.NewReader(conn)
		r.ReadString(0)
		r.ReadString(0)
		r.ReadString(0)
		r.ReadString(0)
		conn.Write([]byte{0})
		conn.Write([]byte("no newline at all"))
	}()
	ta := ln.Addr().(*net.TCPAddr)

	res, err := Execute("127.0.0.1", "USER1", "dfields", "cmd", strconv.Itoa(ta.Port), 0, 0, false)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := string(res.Output); got != "no newline at all" {
		t.Errorf("output = %q, want unchanged %q", got, "no newline at all")
	}
}
