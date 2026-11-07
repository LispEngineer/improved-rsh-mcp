//go:build live

// Live tests against real RSH servers, run only on request:
//
//	RSH_LIVE_TARGETS='VAX96=127.0.0.1:15144,AXP3=127.0.0.1:15153' \
//	RSH_LIVE_USER=SYSTEM RSH_LIVE_DIR='SYS$SYSDEVICE:[HOME.SYSTEM.RSHMCP]' \
//	go test -tags live -run Live -v .
//
// The targets are OpenVMS hosts (shell login, DCL commands). Each test
// target gets the directory RSH_LIVE_DIR created, used and deleted again.
// Book the machines first.
package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type liveTarget struct{ name, host, port string }

func liveTargets(t *testing.T) []liveTarget {
	spec := os.Getenv("RSH_LIVE_TARGETS")
	if spec == "" {
		t.Skip("RSH_LIVE_TARGETS not set")
	}
	var out []liveTarget
	for _, item := range strings.Split(spec, ",") {
		name, addr, ok := strings.Cut(item, "=")
		host, port, ok2 := strings.Cut(addr, ":")
		if !ok || !ok2 {
			t.Fatalf("bad target %q (want NAME=host:port)", item)
		}
		out = append(out, liveTarget{name, host, port})
	}
	return out
}

func callParams(tool string, args map[string]any) *mcp.CallToolParams {
	return &mcp.CallToolParams{Name: tool, Arguments: args}
}

func sha(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func TestLiveVMS(t *testing.T) {
	user := os.Getenv("RSH_LIVE_USER")
	dir := os.Getenv("RSH_LIVE_DIR")
	if user == "" || dir == "" {
		t.Skip("RSH_LIVE_USER and RSH_LIVE_DIR must be set")
	}
	logDir := os.Getenv("RSH_LIVE_LOG_DIR")
	for _, tg := range liveTargets(t) {
		t.Run(tg.name, func(t *testing.T) {
			cs := connect(t)
			if logDir != "" {
				os.Setenv("RSH_LOG_DIR", logDir)
			}
			base := map[string]any{"host": tg.host, "port": tg.port, "user": user}
			with := func(kv ...any) map[string]any {
				m := map[string]any{}
				for k, v := range base {
					m[k] = v
				}
				for i := 0; i < len(kv); i += 2 {
					m[kv[i].(string)] = kv[i+1]
				}
				return m
			}
			execOK := func(cmd string, kv ...any) string {
				t.Helper()
				res := call(t, cs, "exec", with(append([]any{"command", cmd, "shell", "login"}, kv...)...))
				if res.IsError || res.StructuredContent != nil {
					t.Fatalf("exec %q: error=%v structured=%v: %s", cmd, res.IsError, res.StructuredContent, text(res))
				}
				return text(res)
			}

			// 1. Short output, as text (the 1.1.2 bug: it never showed).
			out := execOK("SHOW TIME")
			if !strings.Contains(out, "-20") {
				t.Errorf("SHOW TIME: %q", out)
			}
			t.Logf("SHOW TIME -> %q", strings.TrimSpace(out))

			// 2. Long output: well over the old 20-line log preview.
			long := execOK("DIRECTORY/NOHEAD/NOTRAIL SYS$SYSTEM:*.EXE")
			n := strings.Count(long, "\n")
			if i := strings.Index(long, "\r"); i >= 0 {
				lo := max(0, i-40)
				t.Logf("CR at byte %d: %q", i, long[lo:min(len(long), i+40)])
			}
			if n < 50 || strings.Contains(long, "\r") {
				t.Errorf("long output: %d lines (want >= 50), CR present %v", n, strings.Contains(long, "\r"))
			}
			t.Logf("DIRECTORY SYS$SYSTEM:*.EXE -> %d lines", n)
			tail := execOK("DIRECTORY/NOHEAD/NOTRAIL SYS$SYSTEM:*.EXE", "max_lines", 3, "tail", true)
			if !strings.Contains(tail, "[cut to 3 lines") {
				t.Errorf("tail: %q", tail)
			}
			g := execOK("DIRECTORY/NOHEAD/NOTRAIL SYS$SYSTEM:*.EXE", "grep", "LOGINOUT")
			if !strings.Contains(g, "LOGINOUT.EXE") || !strings.Contains(g, "lines match the grep") {
				t.Errorf("grep: %q", g)
			}

			// 3. RSH_SHELL=login as the default (no shell argument).
			os.Setenv("RSH_SHELL", "login")
			res := call(t, cs, "exec", map[string]any{"host": tg.host, "port": tg.port, "user": user, "command": "SHOW TIME"})
			os.Unsetenv("RSH_SHELL")
			if res.IsError || !strings.Contains(text(res), "-20") {
				t.Errorf("RSH_SHELL=login default: %s", text(res))
			}

			// 4. Four calls at once.
			var wg sync.WaitGroup
			errs := make(chan string, 4)
			for i := 0; i < 4; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					r, err := cs.CallTool(t.Context(), callParams("exec", with("command", "SHOW TIME", "shell", "login")))
					if err != nil || r.IsError || !strings.Contains(text(r), "-20") {
						errs <- fmt.Sprintf("%v %s", err, text(r))
					}
				}()
			}
			wg.Wait()
			close(errs)
			for e := range errs {
				// A remote rshd may cap simultaneous sessions; report, not fail.
				t.Logf("concurrent exec (not counted as a failure): %s", e)
			}

			// 5. Transfers in a scratch directory.
			execOK("CREATE/DIRECTORY " + dir)
			defer func() {
				execOK("DELETE " + dir + "*.*;*")
				parent := dir[:strings.LastIndex(dir, ".")] + "]"
				leaf := dir[strings.LastIndex(dir, ".")+1 : len(dir)-1]
				execOK("SET SECURITY/PROTECTION=(O:RWED) " + parent + leaf + ".DIR;1")
				execOK("DELETE " + parent + leaf + ".DIR;1")
				if left := execOK("DIRECTORY " + parent + leaf + ".DIR"); !strings.Contains(left, "no files found") && !strings.Contains(left, "FNF") {
					t.Errorf("cleanup: %s still there: %q", leaf, left)
				}
			}()

			local := t.TempDir()
			bin := make([]byte, 1<<20)
			rand.Read(bin)
			binPath := filepath.Join(local, "LIVE.BIN")
			os.WriteFile(binPath, bin, 0o644)

			// upload-file / download-file (ours), binary, 1 MiB.
			start := time.Now()
			up := call(t, cs, "upload-file", with("local_path", binPath, "remote_path", dir+"LIVE.BIN"))
			if up.IsError {
				t.Fatalf("upload-file: %s", text(up))
			}
			t.Logf("upload-file 1 MiB: %v", time.Since(start).Round(time.Millisecond))
			back := filepath.Join(local, "BACK.BIN")
			start = time.Now()
			down := call(t, cs, "download-file", with("remote_path", dir+"LIVE.BIN", "local_path", back))
			if down.IsError {
				t.Fatalf("download-file: %s", text(down))
			}
			t.Logf("download-file 1 MiB: %v", time.Since(start).Round(time.Millisecond))
			if b, _ := os.ReadFile(back); sha(b) != sha(bin) {
				t.Errorf("binary round trip differs: %d bytes back", len(b))
			}

			// write / read (text and base64).
			if r := call(t, cs, "write", with("path", dir+"HELLO.TXT", "content", "hello from rsh-mcp\n")); r.IsError {
				t.Errorf("write: %s", text(r))
			}
			rt := call(t, cs, "read", with("path", dir+"HELLO.TXT"))
			if rt.IsError || rt.StructuredContent != nil || !strings.Contains(text(rt), "hello from rsh-mcp") {
				t.Errorf("read text: err=%v structured=%v %q", rt.IsError, rt.StructuredContent, text(rt))
			}
			if rb := call(t, cs, "read", with("path", dir+"LIVE.BIN", "encoding", "base64")); rb.IsError || len(text(rb)) < 1<<20 {
				t.Errorf("read base64: err=%v len %d", rb.IsError, len(text(rb)))
			}

			// upload / download (upstream's multi-file tools) with VMS paths.
			a, b2 := filepath.Join(local, "A.BIN"), filepath.Join(local, "B.BIN")
			os.WriteFile(a, bin[:5120], 0o644)
			os.WriteFile(b2, bin[5120:10240], 0o644)
			if r := call(t, cs, "upload", with("paths", []string{a, b2}, "to", dir, "shell", "login")); r.IsError {
				t.Errorf("upload: %s", text(r))
			}
			got := t.TempDir()
			r := call(t, cs, "download", with("paths", []string{dir + "A.BIN", dir + "B.BIN"}, "to", got, "shell", "login"))
			if r.IsError {
				t.Errorf("download: %s", text(r))
			}
			t.Logf("download -> %s", strings.TrimSpace(text(r)))
			for _, f := range []string{"A.BIN", "B.BIN"} {
				want, _ := os.ReadFile(filepath.Join(local, f))
				have, err := os.ReadFile(filepath.Join(got, strings.ToLower(f)))
				if err != nil {
					have, err = os.ReadFile(filepath.Join(got, f))
				}
				if err != nil || sha(have) != sha(want) {
					entries, _ := os.ReadDir(got)
					t.Errorf("download %s: err %v, match %v; folder has %v", f, err, sha(have) == sha(want), entries)
				}
			}
			r = call(t, cs, "download", with("paths", []string{dir + "*.BIN"}, "to", t.TempDir(), "shell", "login"))
			t.Logf("download wildcard -> %s", strings.TrimSpace(text(r)))
		})
	}
}
