# Changelog

## 1.1.0 - 2026-09-16
**Contributors**: Douglas P. Fields, Jr. (`symbolics@lisp.engineer`)

### Added
- **Persistent transfer audit logging (`rsh_transfers.log`)**: new `internal/xferlog`
  package records every `exec`/`read`/`write`/`upload-file`/`download-file` call
  with ISO-8601 timestamps, target host/port, local and remote usernames, source
  port and whether it was privileged, remote/local paths, byte counts, SHA-256
  digests, duration, and status. Log directory resolves from a per-call `log_dir`
  argument, `RSH_LOG_DIR`, `FTP_LOG_DIR`/`TELNET_LOG_DIR`/`SERIAL_LOG_DIR`,
  `./logs`, or `~/.mcp-rsh-logs`.
- **Binary-safe `read`/`write`**: `read` accepts `encoding: "text"|"base64"` and
  fails clearly (rather than corrupting output) if text mode hits non-UTF-8
  content; `write` accepts `content_base64` as an alternative to `content`,
  plus an optional octal `mode`.
- **`upload-file` / `download-file` tools**: stream directly to/from disk via
  `io.Copy`, so multi-megabyte transfers never pass through the model context.
  `download-file` refuses to overwrite an existing local file unless
  `overwrite: true`, and writes to a temp file + atomic rename so a failed
  transfer never leaves a truncated file at the destination.
- **Environment-variable defaults**: `RSH_HOST`, `RSH_PORT`, `RSH_USER`,
  `RSH_LOCAL_USER`, `RSH_LOG_DIR`, `RSH_REQUIRE_PRIVPORT` let a single
  `.mcp.json` entry target one host without repeating arguments on every call.
- **Structured output** (`structuredContent`) on every tool: bytes, sha256,
  sourcePort, privilegedPort, localUser, remoteUser, logFile, durationMs.
- Test suite: `internal/rcmd`, `internal/rcp`, `internal/rsh`, `internal/xferlog`
  all covered by `go test ./...`, including an in-process fake `rshd` in
  `internal/rcp` exercising binary round trips, missing files, OpenVMS-style
  `%...-E-...` error lines, BSD warning/error status bytes, directory-entry
  rejection, and download overwrite protection.

### Fixed
- `port` tool argument now accepts a JSON string **or** number (previously a
  bare integer failed schema validation: `type: 5141 has type "integer", want
  "string"`).
- Silent ephemeral-port fallback: when no privileged source port (512-1023)
  can be bound, the server now fails fast with a `setcap` hint instead of
  quietly connecting from a high port that most `rshd` servers (including
  OpenVMS `TCPIP$RSH`) then reject with a bare EOF. Opt out with
  `RSH_REQUIRE_PRIVPORT=0` / `-require-privileged-port=false` to deliberately
  probe whether a given server enforces the reserved-port rule.
- `exec`'s `max_lines`/`tail` truncation had an off-by-one when output ended
  in a newline (the normal case for command output): `bytes.Split` turns the
  trailing `\n` into a spurious empty final "line", so `tail=true` with
  `max_lines=N` returned N-1 real lines plus that empty one. The trailing
  newline is now tracked separately and re-appended after truncation.
- OpenVMS `TCPIP$RCP` reports errors (e.g. a missing file) as a plain
  `%FACILITY-E-IDENT, message` line with no BSD-style leading status byte;
  this previously surfaced as an opaque "unexpected rcp control message" and
  now reports as a clear remote error with the original VMS text intact.
- `rcp -f`/`rcp -t` control-line parsing now transparently skips `T` (mtime,
  from `-p`) lines instead of choking on them, and rejects `D` (directory)
  entries with an explicit "recursive copy not supported" message instead of
  an opaque protocol error; an empty/EOF control line no longer risks an
  index-out-of-range panic.
- `LocalUser()` now respects `RSH_LOCAL_USER`, letting the local identity sent
  in the RSH handshake be set independently of the OS account running the
  server -- necessary when a remote proxy database is keyed to a specific name.

## 1.0.1 and earlier

See upstream [tenox7/rsh-mcp](https://github.com/tenox7/rsh-mcp) history.
