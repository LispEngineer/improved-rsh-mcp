# Changelog

## 1.2.0 - 2026-11-07
**Contributors**: Douglas P. Fields, Jr. (`symbolics@lisp.engineer`); upstream changes by Antoni Sawicki

### Merged from upstream (tenox7/rsh-mcp `master` at `d9b7819`)
- `exec` runs the command as a `/bin/sh` script and returns its **exit status**; new `shell` (`sh`/`login`), `timeout`
  and `grep` arguments; CR LF output becomes LF; Windows NT via ntrshd 1.2 or later.
- Multi-file **`download` / `upload`** tools (wildcards, `recursive`, folders), and a **`version`** tool.
- `Dial` tries reserved source ports in a **random order**, with up to 4 connection attempts, so concurrent sessions
  and quick reconnects do not collide in TIME_WAIT.
- **MIT `LICENSE`**.

### Fixed
- **`exec` and `read` output was not shown in Claude Code**: they returned the text and a structured bookkeeping
  result, and Claude Code shows `structuredContent` in place of the text, so only the bookkeeping appeared. They now
  return text only; the bookkeeping still goes to `rsh_transfers.log`. Test `TestExecReturnsOutputAsTextOnly`.

### Added
- `RSH_SHELL` sets the default `shell`; `shell` on `download`/`upload` too, where `login` sends remote paths unquoted
  (OpenVMS file specs). `local_user`, `log_dir` and the env defaults on the new tools; they are logged
  (`DOWNLOAD-MANY`, `UPLOAD-MANY`), and `exec` log entries carry the exit status.
- `write` text result now gives the byte count and SHA-256.
- Tests for the MCP tools (`main_test.go`, in-memory client) and for `exec`'s sh mode, grep, timeout and CR LF.

### Changed
- **Licence: our changes are now MIT** (were Apache-2.0); `LICENSE` names both copyright holders; `NOTICE` rewritten;
  `LICENSE-APACHE-2.0.txt` removed.
- Our single-file functions are `rcp.UploadFile` / `rcp.DownloadFile` (upstream's multi-file ones keep `Upload` /
  `Download`). The tool names `upload-file` / `download-file` are unchanged.
- Version comes from the Go build info, else `1.2.0`.

## 1.1.2 - 2026-11-04
**Contributors**: Douglas P. Fields, Jr. (`symbolics@lisp.engineer`)

### Added
- **`Makefile`**: `make install` builds the binary and runs `sudo setcap cap_net_bind_service=+ep` on it, after
  explaining why sudo may ask for the user's password; `make build`, `make test`. A plain `go build` silently drops the
  capability, so `make install` is now the documented way to install and upgrade.
- **`NOTICE`** and **`LICENSE-APACHE-2.0.txt`**: our changes since upstream `31af750` are Apache-2.0.

### Changed
- The reserved-port error now says the binary lacks `cap_net_bind_service`, that a `go build` rebuild removes it, to use
  `make install`, and names the real executable path in the `setcap` command (was `<this binary>`). New test
  `TestDialWithoutCapabilityGivesSetcapHint`.
- README: install and upgrade with `make install`; corrected the symptom of a missing capability (it is the
  reserved-port error, not `server closed connection without response`).

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
