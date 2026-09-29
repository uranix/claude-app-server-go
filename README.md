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
| `--permission-timeout <seconds>` | `300` | auto-deny an unanswered permission prompt after this long |
| `--key-file <path>` | none | keep the auth key in this file (created with mode 600 if missing) so it survives restarts, e.g. under systemd; the banner then does not print it |
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
| `thread/start`, `thread/resume`, `thread/fork` | manage threads (`thread/start` and `thread/attach` accept `model` and `append_system_prompt`, extra instructions passed to every spawn of that thread's process) |
| `thread/close` | kill the thread's process and free its slot; the session stays resumable via `thread/attach` |
| `thread/attach` | bind a new thread to an existing CLI session (`cli_session_id`, `cwd`), to continue after a restart |
| `turn/start`, `turn/steer`, `turn/interrupt` | drive a thread's turns; `turn/start` and `turn/steer` accept an optional `message_id` (UUID), reported back by `message/consumed` |
| `approval/respond` | change permission mode (real, not simulated). Modes: `default`, `plan`, `acceptEdits`, `dontAsk`, `auto` (the CLI approves what it judges safe and asks about the rest), and `bypassPermissions` only with the server flag |
| `permission/respond` | answer a live permission prompt (see below) |
| `thread/set_model` | switch a thread's model; live on a running process (`set_model` control request) |
| `model/list` | models the CLI offers (`models` names, `model_info` details, `live`); cached 10 min, static fallback if the CLI cannot be asked |
| `skills/list`, `app/list` | static discovery |

Notifications pushed by the server: `initialized`, `item/progress`,
`item/created`, `turn/completed`, `turn/error`, `turn/permission_denied`,
`approval/requested`, `approval/cancelled`, `message/consumed`
(`{thread_id, message_id}`: the CLI has taken that user message off its queue,
i.e. the agent now sees it; a steered message is consumed only after the
current tool call or step finishes).

### Live permission prompts

By default a tool call that needs permission is denied silently and reported
afterwards (`turn/permission_denied`). A thread started with
`"permission_prompts": true` (`thread/start` or `thread/attach`) instead asks
the client, using the CLI's `--permission-prompt-tool stdio`:

1. The server pushes `approval/requested`:
   `{thread_id, turn_id, request_id, tool_name, tool_use_id, description, input, suggestions, expires_at}`.
   `suggestions` are the CLI's own offers, such as
   `{"type":"setMode","mode":"acceptEdits","destination":"session"}`.
2. The client answers with `permission/respond`:
   `{thread_id, request_id, behavior: "allow"|"deny", message?, apply_suggestions?}`.
   `message` is shown to the model on deny. `apply_suggestions` also applies the
   suggestions on allow ("and stop asking"); note that a Bash suggestion can be
   a persistent rule written to the project's `.claude/settings.local.json`.
3. `approval/cancelled` `{thread_id, request_id, reason}` says the request is
   gone: `"cancelled"` (the turn was interrupted) or `"timeout"`.

The CLI itself waits for an answer indefinitely, so the server denies a request
nobody answers after `--permission-timeout`. Only clients that opt in receive
these messages; the CLI blocks on each request, so an opted-in client that
never answers stalls its turn until the timeout. Requests still pending when a
thread is closed or its process exits are dropped. A turn interrupted while a
tool or prompt is pending ends as `interrupted`.

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

## License

MIT, see [LICENSE](LICENSE).
