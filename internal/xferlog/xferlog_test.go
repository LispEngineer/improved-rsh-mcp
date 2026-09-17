package xferlog

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestResolveLogDirPrecedence(t *testing.T) {
	t.Run("explicit wins", func(t *testing.T) {
		t.Setenv("RSH_LOG_DIR", "/from/env")
		if got := ResolveLogDir("/explicit"); got != "/explicit" {
			t.Errorf("got %q", got)
		}
	})
	t.Run("RSH_LOG_DIR before others", func(t *testing.T) {
		t.Setenv("RSH_LOG_DIR", "/rsh")
		t.Setenv("FTP_LOG_DIR", "/ftp")
		if got := ResolveLogDir(""); got != "/rsh" {
			t.Errorf("got %q", got)
		}
	})
	t.Run("falls back through FTP/TELNET/SERIAL log dirs", func(t *testing.T) {
		os.Unsetenv("RSH_LOG_DIR")
		os.Unsetenv("FTP_LOG_DIR")
		t.Setenv("TELNET_LOG_DIR", "/telnet")
		if got := ResolveLogDir(""); got != "/telnet" {
			t.Errorf("got %q", got)
		}
	})
	t.Run("uses ./logs if present", func(t *testing.T) {
		os.Unsetenv("RSH_LOG_DIR")
		os.Unsetenv("FTP_LOG_DIR")
		os.Unsetenv("TELNET_LOG_DIR")
		os.Unsetenv("SERIAL_LOG_DIR")
		dir := t.TempDir()
		wd, _ := os.Getwd()
		defer os.Chdir(wd)
		os.Chdir(dir)
		if err := os.Mkdir("logs", 0o755); err != nil {
			t.Fatal(err)
		}
		if got := ResolveLogDir(""); got != "logs" {
			t.Errorf("got %q", got)
		}
	})
}

func TestFormatBytes(t *testing.T) {
	cases := []struct {
		n    int64
		want string
	}{
		{500, "500 B"},
		{1536, "1.50 KB"},
		{5 * 1024 * 1024, "5.00 MB"},
	}
	for _, tc := range cases {
		if got := FormatBytes(tc.n); got != tc.want {
			t.Errorf("FormatBytes(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

func TestSHA256Hex(t *testing.T) {
	got := SHA256Hex([]byte("hello"))
	want := "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	if got != want {
		t.Errorf("SHA256Hex(\"hello\") = %s, want %s", got, want)
	}
}

func TestLoggerWritesReadableBlockAndFileMatches(t *testing.T) {
	dir := t.TempDir()
	l, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if l.LogFile() != filepath.Join(dir, LogFileName) {
		t.Errorf("LogFile() = %q", l.LogFile())
	}

	err = l.Write(Entry{
		Operation: "UPLOAD", Host: "192.168.3.201", Port: "514",
		LocalUser: "dfields", RemoteUser: "USER1",
		SourcePort: 1023, PrivilegedPort: true,
		RemotePath: "DKA0:[HOME.USER1]TEST.TXT", LocalPath: "/tmp/test.txt",
		Bytes: 1234, SHA256: "abc123", DurationMs: 42, Status: "SUCCESS",
	})
	if err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(l.LogFile())
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{
		"RSH TRANSFER: UPLOAD - SUCCESS",
		"192.168.3.201:514",
		"local=dfields remote=USER1",
		"Source Port  : 1023 (privileged=true)",
		"DKA0:[HOME.USER1]TEST.TXT",
		"/tmp/test.txt",
		"1234 bytes",
		"abc123",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("log missing %q\nfull log:\n%s", want, text)
		}
	}
}

func TestLoggerAppendsRatherThanTruncates(t *testing.T) {
	dir := t.TempDir()
	l, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	l.Write(Entry{Operation: "EXEC", Host: "h", Port: "1", Status: "SUCCESS", Command: "first"})
	l.Write(Entry{Operation: "EXEC", Host: "h", Port: "1", Status: "SUCCESS", Command: "second"})

	data, err := os.ReadFile(l.LogFile())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "first") || !strings.Contains(string(data), "second") {
		t.Fatalf("expected both entries present, got:\n%s", data)
	}
}

func TestLoggerConcurrentWritesDoNotCorrupt(t *testing.T) {
	dir := t.TempDir()
	l, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			l.Write(Entry{Operation: "EXEC", Host: "h", Port: "1", Status: "SUCCESS", Command: "concurrent"})
		}(i)
	}
	wg.Wait()

	data, err := os.ReadFile(l.LogFile())
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(data), "RSH TRANSFER: EXEC"); got != 20 {
		t.Errorf("expected 20 entries, found %d (possible interleaving corruption)", got)
	}
}

func TestSanitizeStripsControlCharsButKeepsNewlines(t *testing.T) {
	dir := t.TempDir()
	l, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Write(Entry{
		Operation: "EXEC", Host: "h", Port: "1", Status: "SUCCESS",
		Command:       "line one\nline two",
		OutputPreview: "clean\x07bell and \x1b[31mansi\x1b[0m stuff",
	}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(l.LogFile())
	if strings.ContainsRune(string(data), 0x07) || strings.Contains(string(data), "\x1b[31m") {
		t.Errorf("expected control chars stripped, got:\n%s", data)
	}
	if !strings.Contains(string(data), "line one\nline two") {
		t.Error("expected embedded newline in a field to be preserved")
	}
}
