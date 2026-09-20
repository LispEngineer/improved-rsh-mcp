# Improved rsh-mcp

Improvements:

* Persistent transfer audit logging (`rsh_transfers.log`) with timestamps, source
  ports, byte counts, durations, and SHA-256 integrity hashes
* Binary-safe `read`/`write` via base64 (`encoding`/`content_base64`), instead of
  corrupting non-UTF-8 files as text
* New `upload-file` / `download-file` tools that stream directly to/from disk,
  so large transfers never round-trip through the model context
* Environment-variable defaults (`RSH_HOST`, `RSH_PORT`, `RSH_USER`,
  `RSH_LOCAL_USER`, `RSH_LOG_DIR`) so a single `.mcp.json` entry can be
  pre-pointed at one host
* `port` accepts a JSON string or number (some MCP clients send tool arguments
  as native numbers, which the upstream schema rejected)
* Fail-fast privileged-port handling: refuses with a `setcap` hint instead of
  silently falling back to an ephemeral source port most `rshd` servers reject
  anyway (`RSH_REQUIRE_PRIVPORT=0` / `-require-privileged-port=false` to
  probe deliberately)
* Correctly reports OpenVMS `TCPIP$RCP` error lines (`%FACILITY-E-IDENT, ...`,
  sent without the BSD status-byte prefix) as clear remote errors
  instead of "unexpected control message"
* Fixed an off-by-one in `exec`'s `tail`/`max_lines` truncation when output
  ends in a newline (the common case), which previously dropped one real
  line in favor of a spurious empty one
* Test suite (`go test ./...`) covering the protocol client against an
  in-process fake `rshd`, including binary round trips, warning/error/
  directory control lines, and overwrite protection

Improver: Douglas P. Fields, Jr. (`symbolics@lisp.engineer`), for the
[VAXstation lab project](https://github.com/LispEngineer/vms-exploration)

**Upstream license note**: as of this writing, [tenox7/rsh-mcp](https://github.com/tenox7/rsh-mcp)
carries no `LICENSE` file. This fork is kept private pending that.

---

# rsh-mcp

RSH/RCP MCP server. Execute commands and transfer files on remote hosts via RSH/RCP protocols for vintage systems.

## Usage with Claude Code

```bash
go build -o ~/.local/bin/rsh-mcp .
# rshd almost always requires a source port below 1024:
sudo setcap cap_net_bind_service=+ep ~/.local/bin/rsh-mcp
claude mcp add -s user rsh-mcp -- ~/.local/bin/rsh-mcp
```

(A prebuilt binary must be used rather than `go run .../rsh-mcp@latest` if you
need the privileged-port capability -- `setcap` attaches to a specific file,
and `go run` compiles a fresh temporary binary on every invocation.)

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

Execute a command on a remote host via RSH (single TCP connection).

- `command` - Command to execute
- `max_lines` - Maximum lines of output (optional, defaults to 1000)
- `max_bytes` - Maximum bytes of output (optional, defaults to 100000)
- `tail` - Return last N lines instead of first N (optional)

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

## Environment variables

| Variable | Purpose |
| :--- | :--- |
| `RSH_HOST` | Default remote host, used when a call omits `host`. **Fixed in v1.1.1** - see "RSH_HOST was unusable before v1.1.1" below. |
| `RSH_PORT` | Default port (514, or a tunnel's local port e.g. 5141) |
| `RSH_USER` | Default remote username |
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

**File editing:** rsh-mcp provides read/write via RCP copy in/out. For effective editing, work locally then copy over, or mount workspace via NFS/SMB.

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
