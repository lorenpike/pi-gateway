# CLI message injection — Implementation Plan

**Date:** 2026-07-11
**Status:** In progress

## Goal

Allow local automation, especially cron, to inject a normal user message into an
existing wall-e channel/session without requiring a human chat event.

Example:

```sh
cat ~/prompts/morning.md | wall-e msg telegram:123456789
```

This enables jobs like "message me my daily schedule every morning" while
reusing the same model, tools, memory, and most recent session for that channel.

## CLI shape

```sh
wall-e run
wall-e msg <type:id>
```

- `wall-e run` starts the gateway server.
- `wall-e msg <type:id>` reads the entire prompt from `stdin`.
- `wall-e --help` prints help. `wall-e` with no args also prints help, not start the server.
- Prompt on stdin avoids command-line length limits and composes with files,
  heredocs, pipes, template renderers, etc.

Examples:

```sh
wall-e msg http:morning-digest < ~/prompts/morning.md

wall-e msg telegram:123456789 <<'EOF'
Good morning. Summarize today's calendar, weather, and top priorities.
EOF
```

## Channel/session behavior

Channel addresses are typed:

```text
<type>:<id>
```

Examples: `http:morning-digest`, `telegram:123456789`.

The gateway maps this to `session.NewChannelID(type, id)` and should use the
most recent session for that channel. `msg` must not create a new session unless
there is no existing session for the channel, matching normal first-message
behavior.

`http:*` channels are useful for hidden/background cron jobs. They do not need a
chat delivery adapter; results are visible through the session/debug UI and the
CLI stream.

## Current channel environment

**Implemented:** the pool now sets the channel env on `pi` spawn and respawns on
cross-channel slot reuse.

Each channel-bound pool worker `pi` process/session should know its bound
channel through a single environment variable set by the gateway before spawning
the process:

```sh
WALLE_CHANNEL=telegram:123456789
```

Because environment variables are fixed at process spawn, the pool must preserve
same-channel warm reuse but respawn a worker when an idle slot is rebound to a
different channel. This avoids a stale `WALLE_CHANNEL` while keeping the common
same-chat path warm.

The system prompt tells the model how to discover the current channel, e.g.
`echo $WALLE_CHANNEL`. The `type:id` format is simple to parse when needed. This
lets a user say "schedule this for this chat" and the agent can create cron jobs
targeting the current channel without asking for a raw chat id.

Implementation notes:

- `rpc.Config.Env` carries per-process env overrides.
- `pool.rpcConfigForChannel` sets `WALLE_CHANNEL` for each spawned worker.
- Same-channel acquire/release preserves warm reuse.
- Cross-channel idle-slot reuse respawns the `pi` process before
  `switch_session`, preventing stale `WALLE_CHANNEL`.

## Prompt visibility

Injected prompts must be written into the pi session transcript as user
messages. They do not need to be mirrored to the external chat channel.

- HTTP: prompt and response are visible through the session/debug UI and CLI.
- Telegram/future chat: send the assistant response to the channel, but do not
  send a copy of the cron/injection prompt to the chat.

No automatic prompt prefix. Add guidance in the skill and system prompt instead,
for example recommending scheduled prompts begin with context like
`Scheduled task:` when useful.

## Gateway API

Use the authenticated prompt endpoint for both HTTP callers and local CLI/cron
injection. `/v1/prompt` is strict: callers must supply a typed channel.

```http
POST /v1/prompt
Authorization: Bearer $WALLE_TOKEN
Content-Type: application/json

{
  "channelType": "telegram",
  "channel": "123456789",
  "message": "..."
}
```

The CLI still accepts `<type:id>` and splits on the first `:` before sending the
JSON request. Invalid or empty type/id is a usage error.

Return SSE so the CLI can wait for completion and stream logs/output:

```text
event: agent_start
data: {}

event: delta
data: {"text":"..."}

event: agent_end
data: {}

event: done
data: {}
```

`/v1/prompt` is the single typed prompt/injection endpoint. It routes by
`channelType` through delivery adapters. Oversized prompt bodies fail explicitly
with `413` (current limit: 8 MiB).

## Delivery

**Implemented:** `/v1/prompt` now routes through a delivery abstraction.

- `http`: submit to the shared turn manager and stream deltas to the HTTP/CLI caller.
- `telegram`: submit to the same shared turn manager, send the assistant response
  to Telegram, and stream the same response to the HTTP/CLI caller. The injected
  user prompt is recorded in the pi session but is not sent as a Telegram
  message.
- Future channel types register prompt/delivery adapters by type.

If a requested delivery adapter is unavailable, return a clear error such as
`unsupported channelType "telegram"`. Telegram adapter requests must respect
`WALLE_TELEGRAM_ALLOWED_CHATS` strictly.

Active-turn behavior is shared across adapters: if a CLI prompt arrives while a
Telegram turn is active, it steers that turn; if a Telegram message arrives while
a CLI-started Telegram turn is active, it also steers that same turn.

## CLI config

**Implemented:** `wall-e msg` loads only the env needed by the client path; it
must not call the full gateway `config.Load()`. For security, it only connects
to localhost (`http://127.0.0.1:${WALLE_PORT:-6007}`) and does not accept a
remote URL override.

| Var | Default | Notes |
|---|---|---|
| `WALLE_PORT` | `6007` | Localhost gateway port; `msg` only connects to `127.0.0.1` |
| `WALLE_TOKEN` | required | Bearer token |
| `WALLE_MSG_TIMEOUT` | `30m` | Overall wait timeout |

Exit non-zero for usage/config errors, non-2xx gateway responses, timeout,
`event: error`, or a stream ending before `done`. Current CLI stdin limit is
8 MiB to match the gateway prompt body limit.

## Implementation phases

1. **CLI skeleton** ✅
   - Add subcommands: `run`, `msg`, `--help`/`help`.
   - Change no-arg behavior to help.
   - Keep Docker/supervisor entrypoints explicit: `wall-e run`.

2. **stdin message client** ✅
   - Implement `wall-e msg <type:id>`.
   - Split on the first `:`; reject missing/empty type or id.
   - Read prompt from stdin, reject empty input.
   - POST to `/v1/prompt` and consume SSE until `done`.
   - Print `delta.text` to stdout and exit non-zero on stream errors or early close.

3. **Shared typed prompt path** ✅
   - Make `/v1/prompt` require `channelType`, `channel`, and `message`.
   - Route by channel type through prompt/delivery adapters.
   - Use a shared active-turn manager so CLI/HTTP/chat messages steer each other.
   - Reuse most recent session via existing session manager/pool behavior.
   - Enforce explicit large request-body failures.

4. **Channel env injection** ✅
   - Set `WALLE_CHANNEL=<type:id>` for the spawned/bound `pi` process.
   - Preserve same-channel warm reuse.
   - Respawn on cross-channel idle-slot reuse so env is never stale.
   - Update `SYSTEM.md` and docs/cron skill to mention `echo $WALLE_CHANNEL`.

5. **Telegram adapter** ✅
   - Extract reusable Telegram response delivery through shared turn subscriptions.
   - Add typed `/v1/prompt` support that records the user prompt in session and
     sends only the assistant response to Telegram.
   - Share steering semantics between Telegram human messages and CLI/HTTP prompts.
   - Respect Telegram allowed-chat config.

6. **Docs/skills/system prompt** ✅
   - Document heredoc/file/pipe usage.
   - Add cron examples.
   - Add skill/system guidance recommending explicit scheduled-task context when
     appropriate, without enforcing a prefix.
