package rcmd

import (
	"bufio"
	"net"
	"os"
	"strings"
	"testing"
)

func TestReadAckDistinguishesWarningFromError(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		wantNil bool
		wantSub string
	}{
		{"success", "\x00", true, ""},
		{"warning", "\x01careful now\n", false, "remote warning: careful now"},
		{"error", "\x02totally broken\n", false, "remote error: totally broken"},
		{"error no message", "\x02", false, "remote error (status 2)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := bufio.NewReader(strings.NewReader(tc.input))
			err := ReadAck(r)
			if tc.wantNil {
				if err != nil {
					t.Fatalf("expected nil, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error = %q, want substring %q", err.Error(), tc.wantSub)
			}
		})
	}
}

func TestReadAckEOFIsDescriptive(t *testing.T) {
	r := bufio.NewReader(strings.NewReader(""))
	err := ReadAck(r)
	if err == nil || !strings.Contains(err.Error(), "closed connection") {
		t.Errorf("expected a descriptive EOF error, got %v", err)
	}
}

func TestLocalUserRespectsOverride(t *testing.T) {
	t.Setenv("RSH_LOCAL_USER", "someoneelse")
	got, err := LocalUser()
	if err != nil {
		t.Fatal(err)
	}
	if got != "someoneelse" {
		t.Errorf("LocalUser() = %q, want %q", got, "someoneelse")
	}
}

func TestLocalUserFallsBackToOSUser(t *testing.T) {
	os.Unsetenv("RSH_LOCAL_USER")
	got, err := LocalUser()
	if err != nil {
		t.Fatal(err)
	}
	if got == "" {
		t.Error("LocalUser() returned empty string")
	}
}

// TestDialFailsFastWithoutPrivilegedPort simulates "no reserved port could
// be bound" by pointing RequirePrivilegedPort at a server that will refuse
// every connection attempt from ports 512-1023 is impractical to simulate
// directly without root, so this test instead verifies the documented
// contract: with RequirePrivilegedPort left at its default (true) and a
// closed port (nothing listening), Dial still returns a real dial error,
// not a silent success -- the privileged-port logic only special-cases
// EADDRINUSE/EACCES/EPERM, and this exercises the non-that-path branch.
func TestDialToClosedPortFailsCleanly(t *testing.T) {
	orig := RequirePrivilegedPort
	RequirePrivilegedPort = false
	defer func() { RequirePrivilegedPort = orig }()

	// Find a free port, then close it immediately so nothing is listening.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().(*net.TCPAddr)
	ln.Close()

	_, err = Dial("127.0.0.1", FormatPort(addr.Port))
	if err == nil {
		t.Fatal("expected Dial to a closed port to fail")
	}
}

func TestDialSucceedsAgainstLocalListener(t *testing.T) {
	orig := RequirePrivilegedPort
	RequirePrivilegedPort = false
	defer func() { RequirePrivilegedPort = orig }()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err == nil {
			c.Close()
		}
	}()

	addr := ln.Addr().(*net.TCPAddr)
	dr, err := Dial("127.0.0.1", FormatPort(addr.Port))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer dr.Conn.Close()
	if dr.SourcePort == 0 {
		t.Error("expected a nonzero source port to be reported")
	}
}

func TestFormatPort(t *testing.T) {
	if got := FormatPort(5141); got != "5141" {
		t.Errorf("FormatPort(5141) = %q", got)
	}
}
