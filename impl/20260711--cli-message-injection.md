# CLI message injection — Implementation Plan

**Date:** 2026-07-11
**Status:** Planning

## Goal

Let scheduled jobs and local automation inject a normal user message into the
running gateway without going through a human chat client. The primary use case
is cron-driven agent activity, for example:

```cron
0 7 * * * /usr/local/bin/wall-e msg telegram:123456789 \
  "Good morning. Check my calendar and message me today's schedule."
```

This should reuse the same channel/session machinery as ordinary user messages
so the model has the right history, tools, memory, and chat delivery behavior.

## User-facing CLI

Extend the `wall-e` executable from "server only" into a small CLI:

```sh
wall-e run
wall-e msg <channel> "<prompt>"
```

Compatibility:

- `wall-e` with no args remains an alias for `wall-e run` for current Docker and
  supervisor configs.
- Unknown subcommands print usage and exit non-zero.
- `wall-e run` is the existing server path: load `WALLE_*`, start HTTP/chat
  front-ends, drain on SIGTERM.

`wall-e msg` is a client-mode command. It contacts the already-running gateway,
submits an injected message, waits until `agent_end`, and exits non-zero on
transport, auth, queue, prompt, or gateway errors.

## Channel syntax

Use typed channel addresses:

```text
<type>:<id>
```

Examples:

```sh
wall-e msg telegram:123456789 "send me my daily schedule"
wall-e msg http:morning-digest "prepare the daily digest"
```

Rules:

- `telegram:<chat-id>` targets the existing Telegram chat/session and sends the
  assistant response back to that chat.
- `http:<channel>` targets the existing HTTP-style channel and streams the
  response to stdout for scripts.
- Future types can include `discord:<channel-id>` and `slack:<channel-id>`.
- If no type is supplied, fail with a helpful error rather than guessing.

This maps directly onto `session.NewChannelID(type, id)`, avoiding parallel
`http--123` and `telegram--123` histories.

## Gateway API

Keep `/v1/prompt` for external HTTP clients. Add a local/admin injection API for
CLI use:

```http
POST /v1/inject
Authorization: Bearer $WALLE_TOKEN
Content-Type: application/json

{
  "channelType": "telegram",
  "channel": "123456789",
  "message": "Good morning...",
  "delivery": "default"
}
```

Response is SSE, matching `/v1/prompt` enough for the CLI to wait and report
progress:

```text
event: agent_start
data: {}

event: delta
data: {"text":"..."}

event: delivered
data: {"channelType":"telegram","channel":"123456789"}

event: agent_end
data: {}

event: done
data: {}
```

`delivery` values:

- `default`: deliver through the platform for chat-backed channels; print/stream
  for HTTP channels.
- `none`: run the agent and only return SSE to the caller. Useful for tests.

The endpoint should share auth, request size limits, queue timeout behavior, and
SSE helpers with `/v1/prompt`.

## Delivery model

Today Telegram inbound handling owns both sides of a turn: it acquires a pool
slot, sends the prompt, streams deltas, and edits/sends Telegram messages.
Injection needs to reuse that delivery code without pretending the message came
from Telegram polling.

Introduce a gateway-level message injector:

```go
type InjectRequest struct {
    ChannelType string
    ChannelID   string
    Message     string
    Delivery    DeliveryMode
}

type Delivery interface {
    CanDeliver(channelType string) bool
    Deliver(ctx context.Context, channelID string, events <-chan rpc.Event) error
}
```

Practical first step:

- Extract Telegram's `runTurn`/`streamTurn` logic into a method callable from
  both polling and injection, e.g. `Bot.Inject(ctx, chatID, text) error`.
- Register started chat front-ends in a small router keyed by channel type.
- `/v1/inject` routes `telegram` to `Bot.Inject` when Telegram is enabled.
- `http` uses the existing HTTP SSE streaming implementation and does not need
  a chat front-end.

If the requested channel type has no delivery adapter running, return `404` or
`409` with a clear message such as `telegram front-end not enabled`.

## CLI behavior

Configuration for `wall-e msg`:

| Var | Default | Notes |
|---|---|---|
| `WALLE_URL` | `http://127.0.0.1:${WALLE_PORT:-6007}` | Gateway base URL |
| `WALLE_TOKEN` | required | Bearer token for `/v1/inject` |
| `WALLE_MSG_TIMEOUT` | `30m` | Overall client wait timeout |

CLI semantics:

- Parse args with the stdlib `flag` package; no new dependency required.
- Send JSON to `/v1/inject` and consume SSE until `done` or error.
- For `http:` channels, print text deltas to stdout as they arrive.
- For chat-delivered channels, print concise status to stderr/stdout, e.g.
  `delivered telegram:123456789`.
- Exit codes:
  - `0`: injected, agent completed, and delivery succeeded.
  - `2`: usage/config error.
  - `3`: gateway returned non-2xx.
  - `4`: stream ended without `done`/`agent_end`.

## Cron usage

Cron jobs should call the CLI rather than `curl` directly so auth, SSE parsing,
exit codes, and future endpoint changes stay centralized:

```sh
/usr/local/bin/wall-e msg telegram:$NOAH_TELEGRAM_CHAT_ID \
  "Good morning. Summarize today's calendar, weather, and top priorities."
```

Recommended cron pattern:

```cron
0 7 * * * flock -n /home/wall-e/.local/state/cron/locks/morning.lock \
  /usr/local/bin/wall-e msg telegram:123456789 "Good morning..." \
  >>/var/log/wall-e/cron/morning.log 2>&1
```

## Implementation phases

### Phase 1 — CLI skeleton

- Move current `main()` server startup into `runCommand(ctx, args)` or similar.
- Add subcommands: default/`run`, `msg`, and `help`.
- Preserve current no-arg behavior.
- Add unit tests for arg parsing and missing config.

### Phase 2 — Shared prompt streaming helper

- Factor `/v1/prompt`'s acquire/prompt/event loop into reusable code that can
  accept a typed `session.ChannelID` and stream callbacks.
- Keep `/v1/prompt` behavior byte-compatible where possible.
- Add tests for typed channel construction and queue timeout preservation.

### Phase 3 — `/v1/inject` endpoint

- Add request/response handling and auth.
- Support `http` delivery first by reusing the existing SSE stream.
- Validate `channelType`, `channel`, and `message` explicitly.
- Add tests for auth, validation, successful injection, and unsupported type.

### Phase 4 — Telegram delivery adapter

- Extract Telegram turn execution so polling and injection share the same code.
- Add `Bot.Inject(ctx, chatID, text) error`.
- Register Telegram with the injection router when `WALLE_TELEGRAM_TOKEN` is set.
- Add fake Telegram tests proving injected turns send/edit messages in the same
  chat and use the existing `telegram--<chat-id>` session.

### Phase 5 — CLI `msg` client

- Implement `wall-e msg <type:id> <prompt...>`.
- Parse simple SSE from the gateway.
- Add integration-ish tests with `httptest.Server` for success, gateway error,
  malformed SSE, and timeout.
- Document cron examples in README and `docs/source/cron.md`.

## Open questions

- Should `wall-e msg telegram:<id>` require the chat id to be in
  `WALLE_TELEGRAM_ALLOWED_CHATS`? Recommended: yes, reuse the same allowlist for
  both inbound and injected outbound messages.
- Should injection prompts be visibly prefixed in chat, e.g. `Scheduled task:`?
  Recommended: no automatic prefix; let the cron prompt decide wording.
- Do we need `--no-wait`? Not for v1. Waiting gives cron reliable logs and
  failure semantics.
