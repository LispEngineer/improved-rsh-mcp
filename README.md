# Improved rsh-mcp

A fork of [tenox7/rsh-mcp](https://github.com/tenox7/rsh-mcp), merged with
upstream `master` at `d9b7819` (7-NOV-2026), with these additions:

* Persistent transfer audit logging (`rsh_transfers.log`) with timestamps, source
  ports, byte counts, durations, exit status and SHA-256 integrity hashes
* Binary-safe `read`/`write` via base64 (`encoding`/`content_base64`), instead of
  corrupting non-UTF-8 files as text
* `upload-file` / `download-file` tools that stream one file directly to/from
  disk with a SHA-256 and overwrite protection, so large transfers never
  round-trip through the model context (alongside upstream's multi-file
  `upload` / `download`)
* Environment-variable defaults (`RSH_HOST`, `RSH_PORT`, `RSH_USER`,
  `RSH_LOCAL_USER`, `RSH_SHELL`, `RSH_LOG_DIR`) so a single `.mcp.json` entry
  can be pre-pointed at one host; `local_user` on every tool
* `shell` `login` also for `upload` / `download`, sending remote paths
  unquoted, so OpenVMS file specs (`SYS$SYSDEVICE:[DIR]FILE.TXT;1`) work
* `port` accepts a JSON string or number (some MCP clients send tool arguments
  as native numbers, which the upstream schema rejected)
* Fail-fast privileged-port handling: refuses with a `setcap` hint instead of
  silently falling back to an ephemeral source port most `rshd` servers reject
  anyway (`RSH_REQUIRE_PRIVPORT=0` / `-require-privileged-port=false` to
  probe deliberately)
* Correctly reports OpenVMS `TCPIP$RCP` error lines (`%FACILITY-E-IDENT, ...`,
  sent without the BSD status-byte prefix) as clear remote errors
  instead of "unexpected control message"
* `exec` and `read` return their text only, so clients that prefer structured
  results (Claude Code) show the output (see "What each tool returns")
* `make install` (build + `setcap`) and a test suite (`go test ./...`) covering
  the protocol client and the MCP tools against an in-process fake `rshd`

Improver: Douglas P. Fields, Jr. (`symbolics@lisp.engineer`), for the
[VAXstation lab project](https://github.com/LispEngineer/vms-exploration)

---

# rsh-mcp

RSH/RCP MCP server. Execute commands and transfer files on remote hosts via RSH/RCP protocols for vintage systems.

## Usage with Claude Code

```bash
make install        # builds ~/.local/bin/rsh-mcp and runs setcap (sudo may ask for your password)
claude mcp add -s user rsh-mcp -- ~/.local/bin/rsh-mcp
```

rshd almost always requires a source port below 1024, so the binary needs the
`cap_net_bind_service` capability. `make install` builds it and grants that
capability with `sudo setcap`, saying first why sudo may ask for your login
password. Install somewhere else with `make install BIN=/path/to/rsh-mcp`.

(A prebuilt binary must be used rather than `go run .../rsh-mcp@latest` if you
need the privileged-port capability -- `setcap` attaches to a specific file,
and `go run` compiles a fresh temporary binary on every invocation.)

## Upgrading

**`go build` silently removes the capability. Upgrade with `make install`.**

```bash
git pull
make install                  # rebuilds and re-runs setcap
getcap ~/.local/bin/rsh-mcp   # confirm: cap_net_bind_service=ep
```

`setcap` attaches to a specific file, and a rebuild replaces that file.
Observed directly across the v1.1.0 -> v1.1.1 upgrade:

```
getcap before go build  ->  cap_net_bind_service=ep
getcap after  go build  ->  (empty)
getcap after  setcap    ->  cap_net_bind_service=ep
```

A binary without the capability fails every call with (v1.1.2 and later):

```
cannot bind a reserved source port (512-1023): ... bind: permission denied --
this binary lacks cap_net_bind_service (a rebuild with go build removes it;
build with make install instead); run: sudo setcap cap_net_bind_service=+ep /path/to/rsh-mcp
```

(Earlier versions of this README said the symptom was `server closed connection
without response`. Tested 4-NOV-2026, that is wrong: a binary without the
capability gives the reserved-port error above. `server closed connection` means
the server accepted and then dropped the connection -- for example rshd not
running yet, or no proxy entry for this client.)

## Testing with Inspector

```bash
npx @modelcontextprotocol/inspector --cli ~/.local/bin/rsh-mcp \
  -e RSH_HOST=192.168.3.201 -e RSH_USER=USER1 \
  --method tools/call --tool-name exec --tool-arg 'command=SHOW TIME'
```

Note: `-e KEY=VALUE` must come **after** the server command and **before**
`--method`; environment variables are not otherwise inherited from your shell.

## Tools

All tools accept `host`, `user`, `local_user`, and `port` as optional
arguments that fall back to `RSH_HOST`, `RSH_USER`, `RSH_LOCAL_USER`, and
`RSH_PORT` (default `514`) respectively, plus an optional `log_dir` to
override the transfer log location for that call.

### exec

Execute a command on a remote host via RSH (single TCP connection). Returns its output (stdout and stderr) and, with `shell` `sh`, its exit status.

- `command` - Command to execute; with `shell` `sh` it may have several lines
- `shell` - `sh` or `login` (optional; defaults to `RSH_SHELL`, else `sh`; see below). **OpenVMS needs `login`**: set `RSH_SHELL=login` in the server's environment
- `timeout` - Seconds before the call returns the output so far (optional); the command may go on running
- `grep` - Return only the lines that match this regular expression, Go syntax (optional)
- `max_lines` - Maximum lines of output (optional, defaults to 1000)
- `max_bytes` - Maximum bytes of output (optional, defaults to 100000)
- `tail` - Return last N lines instead of first N (optional)

With `shell` `sh`, the command is sent as a script to `/bin/sh` on its standard input, so the login shell (sh, ksh, csh) never parses it. Quotes, `!`, comments and here-documents work the same everywhere, the command reads `/dev/null` as its input, and the exit status comes back. Windows NT has no `/bin/sh`, but [ntrshd](https://github.com/tenox7/ntrshd) 1.2 and later read that script themselves: they run the command with `cmd /c` (several lines become one command line, joined with `&`) and send the exit status back the same way, so `exec` needs no options there either. An older NT rshd answers that it doesn't know `sh`; then nothing runs and `exec` says so. `login` passes the command to the login shell as it is, with no exit status: use it for other hosts without `/bin/sh`, or an older NT rshd.

Output comes back with plain newlines: CRLF line ends become LF, so `grep` works the same on NT output.

`grep` filters here, not on the remote host, so it works with old tools and saves output.

### download

Copy files from a remote host to a local folder via RCP: many at once, binary files too.

- `paths` - Remote files; `*` and `?` work as in the remote shell
- `to` - Local folder (absolute path), made if missing
- `recursive` - Copy folders with everything in them (optional)
- `shell` - `sh` (default, or `RSH_SHELL`) quotes unusual paths for `/bin/sh`; `login` sends them exactly as given, as OpenVMS needs (`SYS$SYSDEVICE:[DIR]FILE.TXT;1`)

### upload

Copy local files to a remote host via RCP: many at once, binary files too. File modes are kept.

- `paths` - Local files (absolute paths); folders need `recursive`
- `to` - Remote folder; for one file it may also be the new file name
- `recursive` - Copy folders with everything in them (optional)
- `shell` - as for `download`

### read

Read a whole file from a remote host via RCP into text or base64.

- `path` - Path to remote file
- `encoding` - `"text"` (default) or `"base64"`. Text mode fails with a clear
  message instead of corrupting output if the file is not valid UTF-8.

### write

Write text or base64 content to a remote file via RCP in one transfer.

- `path` - Path to remote file
- `content` - Text content (exactly one of `content` / `content_base64` required)
- `content_base64` - Base64-encoded binary content
- `mode` - Octal file mode for the new file, e.g. `"0644"` (optional)

### upload-file

Stream a local file to the remote host, without holding it in memory.

- `local_path` - Path to the local file
- `remote_path` - Destination path on the remote host

### download-file

Stream a remote file to a local path, without holding it in memory. Refuses
to overwrite an existing local file unless `overwrite` is `true`.

- `remote_path` - Path to the remote file
- `local_path` - Destination path on the local filesystem
- `overwrite` - Allow overwriting an existing local file (optional, default `false`)

### version

Report the server version and Go build info (module, Go version, VCS revision).

### What each tool returns

`exec` and `read` return only text: the output or file contents (plus, for
`exec`, the grep/cut/timeout notes and `exit status N`). The connection
details (source port, users, byte count, SHA-256, duration) go to the transfer
log. They used to come back as a structured result too, but MCP clients that
show `structuredContent` in place of the text, Claude Code among them, then
showed only that bookkeeping and never the command output (fixed in 1.2.0).
`write`, `upload-file` and `download-file` still return a structured result
(bytes, SHA-256, source port, log file), since their text is only a summary.

## Environment variables

| Variable | Purpose |
| :--- | :--- |
| `RSH_HOST` | Default remote host, used when a call omits `host`. **Fixed in v1.1.1** - see "RSH_HOST was unusable before v1.1.1" below. |
| `RSH_PORT` | Default port (514, or a tunnel's local port e.g. 5141) |
| `RSH_USER` | Default remote username |
| `RSH_SHELL` | Default `shell` for `exec`, `download` and `upload`: `sh` (default) or `login`. Use `login` for OpenVMS and other hosts without `/bin/sh` |
| `RSH_LOCAL_USER` | Local username sent in the RSH handshake (must match what the remote's proxy database trusts); defaults to the OS user |
| `RSH_LOG_DIR` | Transfer log directory; falls back to `FTP_LOG_DIR`, `TELNET_LOG_DIR`, `SERIAL_LOG_DIR`, `./logs`, or `~/.mcp-rsh-logs` |
| `RSH_REQUIRE_PRIVPORT` | Disables the client's own fail-fast check ONLY. It does **not** make the client bind an ephemeral port, so with `cap_net_bind_service` still attached a call sets this and succeeds anyway - see "Do not use RSH_REQUIRE_PRIVPORT to test the privileged-port requirement" below. |

## Flags

- `-idle-timeout` - drop the connection after this much silence from the remote (default `5m`)
- `-require-privileged-port` - same as `RSH_REQUIRE_PRIVPORT` (flag wins if both are set)

## `RSH_HOST` was unusable before v1.1.1

Every tool documented `host` as "optional if `RSH_HOST` is set", but the
generated JSON schema marked it **required**, so a call omitting it was
rejected by schema validation before the server ever read the environment:

```
validating "arguments": validating root: required: missing properties: ["host"]
```

`RSH_HOST` was therefore effectively dead for the host field, and a pinned
client entry like

```json
"rsh-vax60": { "command": "rsh-mcp", "env": { "RSH_HOST": "192.168.3.201", ... } }
```

silently did not work the way it reads - every call still had to pass `host`
explicitly.

The cause was the Go struct tag: `json:"host"` without `,omitempty` marks a
field required in the generated schema. Every other env-backed field
(`user`, `local_user`, `port`, `log_dir`) already had `,omitempty`; `host`
alone did not. Fixed by adding it to all five tool argument structs.

Note the env-handling code itself was always correct - `firstOrEnv(args.Host,
"RSH_HOST")` did the right thing and had simply never been reachable. A fix
that only touched the env code would have looked right and changed nothing.

Reported by the `vintagate` session, 2026-09-20.

## Do not use `RSH_REQUIRE_PRIVPORT` to test the privileged-port requirement

RSH requires the CLIENT to bind a source port below 1024, which is why this
binary needs `cap_net_bind_service` (see above). `RSH_REQUIRE_PRIVPORT=0`
disables **this program's own fail-fast check** and nothing else - it does not
make the client bind an ephemeral port. So its observable effect depends
entirely on whether the capability is attached:

| Capability | `RSH_REQUIRE_PRIVPORT=0` | Result |
| :--- | :--- | :--- |
| attached | set | **succeeds** - a low port is still bound (1021 observed) |
| removed | set | fails: `server closed connection without response` |

It is neither a workaround for a missing capability nor a way to demonstrate
that the source port matters. Setting it and observing success is a real,
correct answer to a different question, and leads to the conclusion that the
port is irrelevant - which is wrong.

The comparison that does demonstrate the requirement is removing the
capability: same binary, host, user and command, `sudo setcap -r`, and the
call fails byte-identically to an ephemeral-port rejection. Measured
2026-09-20.

## Notes

**File editing:** rsh-mcp provides read/write via RCP copy in/out. For effective editing, work locally then copy over with upload, or mount workspace via NFS/SMB.

**Privileged port:** Most RSH/RCP servers require a source port below 1024. Grant
the built binary `cap_net_bind_service` (see above) rather than running as root.

**Tunnels:** because RSH is a single TCP connection, it can be forwarded through
an SSH tunnel -- but the *server* the tunnel lands on must itself connect out to
the RSH host from a privileged port (an `ssh -L` forward re-originates from an
ephemeral port on the far end, which most `rshd` servers reject). A small
`socat ... ,lowport` relay on the tunnel's far end solves this; see
[`RCP_FILE_TRANSFER_SETUP_AND_TESTING.md`](https://github.com/LispEngineer/vms-exploration/blob/main/RCP_FILE_TRANSFER_SETUP_AND_TESTING.md)
in the companion lab repo for a worked example.

**Windows NT:** If you need an RSH/RCP server for Windows NT check out [NTRSHD](https://github.com/tenox7/ntrshd)

## License

MIT, see [LICENSE](LICENSE).
