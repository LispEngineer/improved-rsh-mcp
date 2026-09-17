package rcp

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
)

// fakeRshd is a minimal in-process rshd that speaks just enough of the
// rcmd + rcp protocol to exercise this package's client code, including
// the failure modes noted in the improvement plan (empty control lines,
// warning/error status bytes, directory entries).
type fakeRshd struct {
	ln net.Listener
	// files holds content that "rcp -f <path>" will serve.
	files map[string][]byte
	// sink collects what "rcp -t <path>" receives, keyed by remote path.
	sink map[string][]byte
	// forceControlLine, if set, is sent verbatim instead of a normal C
	// line, to test warning/error/empty/directory handling.
	forceControlLine string
}

func newFakeRshd(t *testing.T) *fakeRshd {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	return &fakeRshd{ln: ln, files: map[string][]byte{}, sink: map[string][]byte{}}
}

func (f *fakeRshd) addr() (host, port string) {
	ta := f.ln.Addr().(*net.TCPAddr)
	return "127.0.0.1", strconv.Itoa(ta.Port)
}

func (f *fakeRshd) close() { f.ln.Close() }

// serveOne accepts exactly one connection and handles it according to the
// rcmd + rcp -f / -t protocol, then closes the listener side of that
// connection. Tests call this in a goroutine before dialing.
func (f *fakeRshd) serveOne(t *testing.T) {
	t.Helper()
	conn, err := f.ln.Accept()
	if err != nil {
		return
	}
	defer conn.Close()
	r := bufio.NewReader(conn)

	// rcmd handshake: "0\0localuser\0remoteuser\0command\0"
	if _, err := r.ReadString(0); err != nil {
		t.Errorf("fake server: reading stderr port: %v", err)
		return
	}
	if _, err := r.ReadString(0); err != nil {
		t.Errorf("fake server: reading local user: %v", err)
		return
	}
	if _, err := r.ReadString(0); err != nil {
		t.Errorf("fake server: reading remote user: %v", err)
		return
	}
	cmdBytes, err := r.ReadString(0)
	if err != nil {
		t.Errorf("fake server: reading command: %v", err)
		return
	}
	command := strings.TrimSuffix(cmdBytes, "\x00")

	// ack the rsh command itself
	conn.Write([]byte{0})

	switch {
	case strings.HasPrefix(command, "rcp -f "):
		path := strings.TrimPrefix(command, "rcp -f ")
		f.serveSource(t, conn, r, path)
	case strings.HasPrefix(command, "rcp -t "):
		path := strings.TrimPrefix(command, "rcp -t ")
		f.serveSink(t, conn, r, path)
	default:
		// plain exec: echo the command back as output.
		conn.Write([]byte("exec:" + command))
	}
}

func (f *fakeRshd) serveSource(t *testing.T, conn net.Conn, r *bufio.Reader, path string) {
	t.Helper()
	// wait for the sink's readiness byte
	if _, err := r.ReadByte(); err != nil {
		t.Errorf("fake server: reading readiness byte: %v", err)
		return
	}

	if f.forceControlLine != "" {
		conn.Write([]byte(f.forceControlLine))
		if strings.HasPrefix(f.forceControlLine, "T") {
			// sink acks the T line then expects a real C line to follow;
			// keep it simple and just also send a small file after.
			if _, err := r.ReadByte(); err != nil {
				return
			}
		} else {
			return // warning/error/directory/empty cases: nothing more to send
		}
	}

	content, ok := f.files[path]
	if !ok {
		fmt.Fprintf(conn, "%c%s\n", 2, "no such file: "+path)
		return
	}
	fmt.Fprintf(conn, "C%04o %d %s\n", 0644, len(content), path)
	if _, err := r.ReadByte(); err != nil { // ack for C line
		t.Errorf("fake server: reading C-line ack: %v", err)
		return
	}
	conn.Write(content)
	conn.Write([]byte{0}) // final transfer status: success
	r.ReadByte()          // final ack from sink
}

func (f *fakeRshd) serveSink(t *testing.T, conn net.Conn, r *bufio.Reader, path string) {
	t.Helper()
	conn.Write([]byte{0}) // ready for control line
	line, err := r.ReadString('\n')
	if err != nil {
		t.Errorf("fake server: reading C line: %v", err)
		return
	}
	if !strings.HasPrefix(line, "C") {
		t.Errorf("fake server: expected C line, got %q", line)
		return
	}
	parts := strings.SplitN(strings.TrimSpace(line[1:]), " ", 3)
	size, _ := strconv.Atoi(parts[1])
	conn.Write([]byte{0}) // ack C line

	buf := make([]byte, size)
	if _, err := io.ReadFull(r, buf); err != nil {
		t.Errorf("fake server: reading file data: %v", err)
		return
	}
	if _, err := r.ReadByte(); err != nil { // trailing 0 byte after data
		t.Errorf("fake server: reading trailing byte: %v", err)
		return
	}
	f.sink[path] = buf
	conn.Write([]byte{0}) // transfer status: success

	endLine, err := r.ReadString('\n')
	if err != nil || !strings.HasPrefix(endLine, "E") {
		t.Errorf("fake server: expected E line, got %q err=%v", endLine, err)
		return
	}
	conn.Write([]byte{0})
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
