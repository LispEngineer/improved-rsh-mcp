package rsh

import (
	"bufio"
	"context"
	"net"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

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

// fakeRshd accepts one rcmd exec request and answers it with reply(cmd,
// stdin), where cmd is the requested command and stdin whatever the client
// wrote after the ack (the script, in sh mode). It sends the reply and
// closes, which is all Execute needs (dial, handshake, ack, output,
// source-port reporting).
func fakeRshd(t *testing.T, reply func(cmd string, stdin *bufio.Reader) string) (host, port string, gotCmd chan string) {
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
		r.ReadString(0) // stderr port
		r.ReadString(0) // local user
		r.ReadString(0) // remote user
		cmd, _ := r.ReadString(0)
		cmd = strings.TrimSuffix(cmd, "\x00")
		gotCmd <- cmd
		conn.Write([]byte{0}) // ack
		if out := reply(cmd, r); out != "" {
			conn.Write([]byte(out))
		}
	}()
	return "127.0.0.1", strconv.Itoa(ln.Addr().(*net.TCPAddr).Port), gotCmd
}

func fixed(out string) func(string, *bufio.Reader) string {
	return func(string, *bufio.Reader) string { return out }
}

// shReply plays /bin/sh running Execute's script: it finds the marker on the
// script's echo line and answers with out, then the marker and status.
var markerLine = regexp.MustCompile(`echo (__rshmcp_[0-9a-f]+_)\$\?__`)

func shReply(out string, status int) func(string, *bufio.Reader) string {
	return func(cmd string, stdin *bufio.Reader) string {
		script := ""
		for {
			line, err := stdin.ReadString('\n')
			script += line
			if err != nil || line == "exit\n" {
				break
			}
		}
		m := markerLine.FindStringSubmatch(script)
		if m == nil {
			return "no marker in script: " + script
		}
		return out + m[1] + strconv.Itoa(status) + "__\n"
	}
}

func TestExecuteLoginReturnsOutputAndSourcePort(t *testing.T) {
	host, port, got := fakeRshd(t, func(cmd string, _ *bufio.Reader) string { return "echo:" + cmd + "\n" })
	res, err := Execute(context.Background(), Options{Host: host, User: "USER1", LocalUser: "dfields", Port: port,
		Command: "SHOW TIME", Shell: "login"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if c := <-got; c != "SHOW TIME" {
		t.Errorf("login mode sent %q, want the command as given", c)
	}
	if string(res.Output) != "echo:SHOW TIME\n" {
		t.Errorf("Output = %q", res.Output)
	}
	if res.ExitStatus != ExitUnknown {
		t.Errorf("login mode ExitStatus = %d, want ExitUnknown", res.ExitStatus)
	}
	if res.SourcePort == 0 {
		t.Error("expected a nonzero source port")
	}
	if res.LocalUser != "dfields" || res.RemoteUser != "USER1" {
		t.Errorf("user metadata: local=%s remote=%s", res.LocalUser, res.RemoteUser)
	}
}

func TestExecuteShReturnsExitStatus(t *testing.T) {
	host, port, got := fakeRshd(t, shReply("hello\n", 3))
	res, err := Execute(context.Background(), Options{Host: host, User: "u", LocalUser: "l", Port: port, Command: "false"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if c := <-got; c != scriptCommand {
		t.Errorf("sh mode sent %q, want %q", c, scriptCommand)
	}
	if string(res.Output) != "hello\n" || res.ExitStatus != 3 {
		t.Errorf("Output = %q, ExitStatus = %d; want \"hello\\n\", 3", res.Output, res.ExitStatus)
	}
}

func TestExecuteCRLFNormalised(t *testing.T) {
	host, port, _ := fakeRshd(t, fixed("a\r\nb\r\n"))
	res, err := Execute(context.Background(), Options{Host: host, User: "u", LocalUser: "l", Port: port, Command: "x", Shell: "login"})
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Output) != "a\nb\n" {
		t.Errorf("Output = %q, want CR LF turned into LF", res.Output)
	}
}

func TestExecuteMaxLinesTail(t *testing.T) {
	host, port, _ := fakeRshd(t, fixed("l1\nl2\nl3\nl4\nl5\n"))
	res, err := Execute(context.Background(), Options{Host: host, User: "u", LocalUser: "l", Port: port, Command: "x", Shell: "login", MaxLines: 2, Tail: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(res.Output); got != "l4\nl5\n" || !res.Cut || res.Lines != 5 {
		t.Errorf("tail output = %q (cut %v, lines %d), want \"l4\\nl5\\n\", cut, 5 lines", got, res.Cut, res.Lines)
	}
}

func TestExecuteMaxLinesHead(t *testing.T) {
	host, port, _ := fakeRshd(t, fixed("l1\nl2\nl3\nl4\nl5\n"))
	res, err := Execute(context.Background(), Options{Host: host, User: "u", LocalUser: "l", Port: port, Command: "x", Shell: "login", MaxLines: 2})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(res.Output); got != "l1\nl2\n" {
		t.Errorf("head output = %q, want %q", got, "l1\nl2\n")
	}
}

func TestExecuteGrep(t *testing.T) {
	host, port, _ := fakeRshd(t, fixed("alpha\nbeta\ngamma\nalphabet\n"))
	res, err := Execute(context.Background(), Options{Host: host, User: "u", LocalUser: "l", Port: port, Command: "x", Shell: "login", Grep: "^alpha"})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(res.Output); got != "alpha\nalphabet\n" || res.Matched != 2 || res.Lines != 4 {
		t.Errorf("grep output = %q (matched %d of %d)", got, res.Matched, res.Lines)
	}
}

func TestExecuteBadGrep(t *testing.T) {
	_, err := Execute(context.Background(), Options{Host: "127.0.0.1", Port: "1", Command: "x", Grep: "("})
	if err == nil || !strings.Contains(err.Error(), "grep") {
		t.Errorf("bad grep: err = %v, want a grep error", err)
	}
}

func TestExecuteNoTrailingNewline(t *testing.T) {
	host, port, _ := fakeRshd(t, fixed("no newline at all"))
	res, err := Execute(context.Background(), Options{Host: host, User: "u", LocalUser: "l", Port: port, Command: "x", Shell: "login"})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(res.Output); got != "no newline at all\n" {
		t.Errorf("output = %q, want the text with one newline added", got)
	}
}

func TestExecuteTimeout(t *testing.T) {
	host, port, _ := fakeRshd(t, func(string, *bufio.Reader) string { time.Sleep(3 * time.Second); return "" })
	start := time.Now()
	res, err := Execute(context.Background(), Options{Host: host, User: "u", LocalUser: "l", Port: port, Command: "x", Shell: "login", Timeout: 300 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if !res.TimedOut || time.Since(start) > 2*time.Second {
		t.Errorf("TimedOut = %v after %v, want a timeout near 300 ms", res.TimedOut, time.Since(start))
	}
}

// The marker must be found even when the command's own output is large.
func TestCutMarker(t *testing.T) {
	m := newMarker()
	out, st := cutMarker([]byte(strings.Repeat("x\n", 1000)+m+"0__\n"), m)
	if st != 0 || len(out) != 2000 {
		t.Errorf("cutMarker: status %d, %d bytes left", st, len(out))
	}
	if _, st := cutMarker([]byte("nothing here"), m); st != ExitUnknown {
		t.Errorf("no marker: status %d, want ExitUnknown", st)
	}
}
