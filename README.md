# claude-app-server-go

A JSON-RPC 2.0 server that wraps the [Claude Code](https://claude.com/claude-code)
CLI, exposed over stdio or WebSocket, so a UI (mobile app, web client, custom
frontend) can drive Claude Code without shelling out to the CLI itself.

One pure-Go dependency for the WebSocket transport. Builds to a single static binary.

## Why this exists

A naive wrapper spawns a fresh `claude` process for every turn. That makes
some operations impossible to do for real:

- **Steering** a turn that's already running (you can only queue text for the
  *next* turn).
- **Interrupting** a turn (all you can do is `SIGTERM` the process).
- **Live permission-mode changes** (you can raise the mode and ask the client
  to retry, but you can't answer a prompt that's already in flight).

It also re-pays Claude Code's full startup cost (settings, `CLAUDE.md`, every
MCP server) on every single turn.

This server instead holds **one persistent `claude` subprocess per thread**,
fed over `--input-format stream-json`, and talks to it via the CLI's real
control protocol. That makes steering, interrupt, and permission-mode changes
actual operations instead of approximations:

- `turn/steer` sends a second `user` message while a turn is in flight; the
  CLI queues it and runs it as the next turn.
- `turn/interrupt` sends a real `control_request{subtype:"interrupt"}` and
  waits for the CLI's ack; the in-flight turn ends with a genuine
  `aborted_streaming` result and the process survives for the next turn.
- `approval/respond` issues a real `control_request{subtype:"set_permission_mode"}`
  against the running process.

An idle thread's process is killed after a configurable timeout and
transparently respawned with `--resume <session_id>` on its next turn, so
conversation context survives the reap.

## Build

```sh
CGO_ENABLED=0 go build -ldflags="-s -w" -o claude-app-server ./cmd/claude-app-server
```

Only dependency is [`github.com/coder/websocket`](https://github.com/coder/websocket)
(pure Go, no cgo), used for the WebSocket transport; everything else
(JSON-RPC, the Claude CLI control protocol, threading) is standard library
only. `CGO_ENABLED=0` still yields a fully static binary.

## Usage

```sh
# stdio transport (default): reads/writes NDJSON on stdin/stdout, no listener
claude-app-server

# WebSocket transport
claude-app-server start [flags]
```

Flags:

| Flag | Default | Meaning |
|---|---|---|
| `--host <addr>` | `127.0.0.1` | bind address |
| `--port <n>` | `3284` | listen port |
| `--allow-origin <origin>` | none | allow this exact browser `Origin` (repeatable) |
| `--idle-timeout <seconds>` | `600` | idle time before a thread's process is reaped |
| `--dangerously-allow-bypass-permissions` | off | allow clients to request `bypassPermissions` |
| `--debug` | off | verbose logging to stderr |

On WebSocket startup the server prints a `ws://host:port?key=...` connect URL
with a freshly generated 128-bit auth key.

`CLAUDE_PATH` overrides the `claude` binary used (default: first `claude` on
`PATH`).

## Protocol

JSON-RPC 2.0 over newline-delimited JSON, one line per message, both on
stdio and inside WebSocket text frames.

| Method | Purpose |
|---|---|
| `initialize` | handshake |
| `thread/start`, `thread/resume`, `thread/fork` | manage threads |
| `thread/attach` | bind a new thread to an existing CLI session (`cli_session_id`, `cwd`), to continue after a restart |
| `turn/start`, `turn/steer`, `turn/interrupt` | drive a thread's turns |
| `approval/respond` | change permission mode (real, not simulated) |
| `model/list`, `skills/list`, `app/list` | static discovery |

Notifications pushed by the server: `initialized`, `item/progress`,
`item/created`, `turn/completed`, `turn/error`, `turn/permission_denied`.

## Security

Anyone who can reach the port and present the auth key can run commands as
you, in any directory the server's threads are pointed at. Treat the connect
URL as a root password.

- Default bind is loopback only (`127.0.0.1`); pass `--host 0.0.0.0` to expose it.
- Browser-originated WebSocket connections are rejected unless their exact
  `Origin` is allowlisted with `--allow-origin`.
- `bypassPermissions` is refused unless `--dangerously-allow-bypass-permissions`
  is set.
- The 128-bit auth key is compared in constant time.
- **There is no TLS.** This binary only speaks plain `ws://`. Put it behind an
  SSH tunnel or a TLS-terminating reverse proxy if you need to cross an
  untrusted network.

## Known limitation

`can_use_tool` (live, per-tool host-routed permission prompts) is not wired
up yet. What enables dispatching that control request to a non-SDK host
hasn't been pinned down. Until then, denied tool calls are reported via
`turn/permission_denied` after the fact rather than prompted for live.

## License

MIT, see [LICENSE](LICENSE).
