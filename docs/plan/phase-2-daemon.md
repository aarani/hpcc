## Phase 2: Daemon Architecture

Move the cache logic into a long-running daemon so multiple concurrent compiler
invocations share state efficiently.

### 2.1 Daemon Process

- `hpcc start` — run the daemon as a **foreground process**, listening on a
  **TCP socket bound to loopback** (`127.0.0.1:<port>`, default `:9080`).
  Foreground execution keeps the process model simple: the user (or a
  process supervisor like systemd, launchd, or a container entrypoint)
  owns the lifecycle. TCP keeps the daemon portable across Linux, macOS,
  and Windows — Unix domain sockets exist on Windows 10+ but
  tooling/library support is uneven, and the wrapper has to run on every
  dev machine. Loopback-only binding keeps the surface area equivalent to
  a Unix socket: no off-host reachability.
- A small **port handshake file** at `<UserConfigDir>/hpcc/daemon.json`
  (resolved via Go's `os.UserConfigDir()` —
  `~/.config/hpcc/daemon.json` on Linux,
  `~/Library/Application Support/hpcc/daemon.json` on macOS,
  `%AppData%\hpcc\daemon.json` on Windows) records `{port, pid,
  auth_token}` so the wrapper can find the daemon without a fixed port.
  Same lookup path as the config file (§5.4) — one directory per user, no
  separate runtime-vs-config split. File permissions: `0600` (Unix) /
  current-user ACL (Windows).
- Per-connection auth: wrapper reads the token from the handshake file and
  presents it on connect. Cheap defense against another local user
  connecting to the loopback port on a shared machine.
- Graceful shutdown via `SIGINT` / `SIGTERM` (Unix) or `Ctrl-C` (all
  platforms). No separate `stop` command needed — the process supervisor
  or the user's terminal handles it.

### 2.2 Client-Server Protocol

**Length-prefixed protobuf over a loopback TCP connection** — *not* gRPC. The
wrapper binary is invoked thousands of times per build and must start fast
and stay small; pulling in the gRPC runtime is overkill for local IPC. Same
`.proto` files as the Phase 4 control plane, just a thinner client.

Connection setup: wrapper reads `daemon.json`, dials `127.0.0.1:<port>`,
sends a one-byte version + the auth token as the first framed message, then
proceeds with normal request/response. Disable Nagle (`TCP_NODELAY`) — these
are short, latency-sensitive RPCs.

Messages:
- `CompileRequest` — compiler path, args, working directory, environment subset.
- `CompileResponse` — cache hit/miss, output artifact bytes (or path),
  stdout, stderr, exit code.
- `StatsRequest` / `StatsResponse`
- `CleanRequest` / `CleanResponse`

### 2.3 Deduplication

If two concurrent invocations have the same cache key, the daemon should only
run the compiler once and serve the result to both. Track in-flight
compilations by cache key; use channels/waitgroups to block duplicates until
the first completes.

### 2.4 Fallback

If the daemon is not running, the client falls back to compiling directly
(with local cache still available in-process). Never fail a build because the
daemon is down.

### Milestone ✅

`make -j16` with the daemon running deduplicates identical translation units
and reports stats from a single process.
