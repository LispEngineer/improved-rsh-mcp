package rcp

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tenox7/rsh-mcp/internal/rcmd"
)

func init() {
	// The fake server is local and doesn't care about source ports; don't
	// require a privileged one so tests run without root/setcap.
	rcmd.RequirePrivilegedPort = false
}

func TestReadFileText(t *testing.T) {
	f := newFakeRshd(t)
	defer f.close()
	f.files["HELLO.TXT"] = []byte("hello from the fake server\n")
	go f.serveOne(t)

	host, port := f.addr()
	res, err := ReadFile(host, "USER1", "dfields", "HELLO.TXT", port)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(res.Content) != "hello from the fake server\n" {
		t.Errorf("content = %q", res.Content)
	}
	if res.SHA256 != sha256Hex(f.files["HELLO.TXT"]) {
		t.Errorf("sha256 mismatch: got %s", res.SHA256)
	}
	if res.LocalUser != "dfields" || res.RemoteUser != "USER1" {
		t.Errorf("user metadata wrong: local=%s remote=%s", res.LocalUser, res.RemoteUser)
	}
}

func TestReadFileBinaryRoundTrip(t *testing.T) {
	f := newFakeRshd(t)
	defer f.close()
	binary := make([]byte, 70000)
	for i := range binary {
		binary[i] = byte(i * 7 % 256)
	}
	f.files["RANDOM.BIN"] = binary
	go f.serveOne(t)

	host, port := f.addr()
	res, err := ReadFile(host, "USER1", "dfields", "RANDOM.BIN", port)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(res.Content, binary) {
		t.Errorf("binary content corrupted: got %d bytes, want %d", len(res.Content), len(binary))
	}
	if res.SHA256 != sha256Hex(binary) {
		t.Errorf("sha256 mismatch")
	}
}

func TestReadFileNotFound(t *testing.T) {
	f := newFakeRshd(t)
	defer f.close()
	go f.serveOne(t)

	host, port := f.addr()
	_, err := ReadFile(host, "USER1", "dfields", "NOSUCHFILE.TXT", port)
	if err == nil {
		t.Fatal("expected an error for a missing file")
	}
	if !strings.Contains(err.Error(), "no such file") {
		t.Errorf("error should surface the remote message, got: %v", err)
	}
}

func TestWriteFileThenSinkReceivesIt(t *testing.T) {
	f := newFakeRshd(t)
	defer f.close()
	go f.serveOne(t)

	host, port := f.addr()
	content := []byte("written by the test\n")
	res, err := WriteFile(host, "USER1", "dfields", "OUT.TXT", port, content, 0o644)
	if err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if res.Bytes != int64(len(content)) {
		t.Errorf("bytes = %d, want %d", res.Bytes, len(content))
	}
	if got := f.sink["OUT.TXT"]; !bytes.Equal(got, content) {
		t.Errorf("server received %q, want %q", got, content)
	}
	if res.SHA256 != sha256Hex(content) {
		t.Errorf("sha256 mismatch")
	}
}

func TestUploadDownloadStreaming(t *testing.T) {
	dir := t.TempDir()
	localIn := filepath.Join(dir, "in.bin")
	payload := bytes.Repeat([]byte{0xAB, 0xCD, 0xEF, 0x01}, 50000) // 200000 bytes
	if err := os.WriteFile(localIn, payload, 0o644); err != nil {
		t.Fatal(err)
	}

	f := newFakeRshd(t)
	defer f.close()
	go f.serveOne(t)
	host, port := f.addr()

	upRes, err := Upload(host, "USER1", "dfields", localIn, "REMOTE.BIN", port)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if upRes.Bytes != int64(len(payload)) || upRes.SHA256 != sha256Hex(payload) {
		t.Fatalf("upload result wrong: bytes=%d sha256=%s", upRes.Bytes, upRes.SHA256)
	}

	// Feed the uploaded bytes back as what a subsequent read would serve,
	// and download it to a new local path.
	f.files["REMOTE.BIN"] = f.sink["REMOTE.BIN"]
	go f.serveOne(t)

	localOut := filepath.Join(dir, "out.bin")
	downRes, err := Download(host, "USER1", "dfields", "REMOTE.BIN", port, localOut, false)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if downRes.SHA256 != sha256Hex(payload) {
		t.Fatalf("download sha256 mismatch")
	}
	got, err := os.ReadFile(localOut)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("downloaded file corrupted")
	}
}

func TestDownloadRefusesOverwriteByDefault(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "existing.txt")
	if err := os.WriteFile(existing, []byte("do not touch me"), 0o644); err != nil {
		t.Fatal(err)
	}

	f := newFakeRshd(t)
	defer f.close()
	f.files["ANY.TXT"] = []byte("new content")
	go f.serveOne(t)
	host, port := f.addr()

	_, err := Download(host, "USER1", "dfields", "ANY.TXT", port, existing, false)
	if err == nil {
		t.Fatal("expected Download to refuse overwriting an existing file")
	}
	got, _ := os.ReadFile(existing)
	if string(got) != "do not touch me" {
		t.Fatalf("existing file was modified despite overwrite=false: %q", got)
	}
}

func TestControlLineWarningIsNotFatalPanic(t *testing.T) {
	f := newFakeRshd(t)
	defer f.close()
	f.forceControlLine = "\x01a non-fatal warning from the source\n"
	go f.serveOne(t)

	host, port := f.addr()
	_, err := ReadFile(host, "USER1", "dfields", "WHATEVER.TXT", port)
	if err == nil {
		t.Fatal("expected an error surfaced from the warning byte")
	}
	if !strings.Contains(err.Error(), "warning") {
		t.Errorf("expected the error to be labeled a warning, got: %v", err)
	}
}

func TestControlLineDirectoryRejected(t *testing.T) {
	f := newFakeRshd(t)
	defer f.close()
	f.forceControlLine = "D0755 0 SOMEDIR\n"
	go f.serveOne(t)

	host, port := f.addr()
	_, err := ReadFile(host, "USER1", "dfields", "SOMEDIR", port)
	if err == nil {
		t.Fatal("expected an error for a directory entry")
	}
	if !strings.Contains(err.Error(), "directory") {
		t.Errorf("expected a directory-specific error, got: %v", err)
	}
}

func TestControlLineOpenVMSErrorRecognized(t *testing.T) {
	f := newFakeRshd(t)
	defer f.close()
	f.forceControlLine = "%RCP-E-INSEARCHFAIL, error searching input file nosuchfile99.txt\n"
	go f.serveOne(t)

	host, port := f.addr()
	_, err := ReadFile(host, "USER1", "dfields", "NOSUCHFILE99.TXT", port)
	if err == nil {
		t.Fatal("expected an error for an OpenVMS-style error line starting with a percent sign")
	}
	if !strings.Contains(err.Error(), "RCP-E-INSEARCHFAIL") {
		t.Errorf("expected the VMS error text to be surfaced verbatim, got: %v", err)
	}
	if strings.Contains(err.Error(), "unexpected rcp control message") {
		t.Errorf("VMS error should be labeled as a remote error, not an unexpected message: %v", err)
	}
}

func TestUploadRejectsDirectory(t *testing.T) {
	dir := t.TempDir()
	_, err := Upload("127.0.0.1", "USER1", "dfields", dir, "REMOTE", "1")
	if err == nil {
		t.Fatal("expected Upload to reject a directory as the local path")
	}
	if !strings.Contains(err.Error(), "directory") {
		t.Errorf("expected a directory-specific error, got: %v", err)
	}
}
