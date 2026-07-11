# CLI message injection — Implementation Plan

**Date:** 2026-07-11
**Status:** Planning

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
- `wall-e` with no args prints help, not start the server.
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

Add an authenticated injection endpoint used by the CLI:

```http
POST /v1/inject
Authorization: Bearer $WALLE_TOKEN
Content-Type: application/json

{
  "channelType": "telegram",
  "channel": "123456789",
  "message": "..."
}
```

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

`/v1/prompt` remains the public HTTP prompt endpoint. `/v1/inject` is the local
admin path for typed channel injection.

## Delivery

- `http`: reuse the existing pool/SSE prompt flow and print deltas to CLI.
- `telegram`: reuse Telegram turn delivery so the assistant response is sent to
  the chat, but the injected user prompt is not sent as a Telegram message.
- Future channel types register delivery adapters by type.

If a requested delivery adapter is unavailable, return a clear error such as
`telegram front-end not enabled`.

## CLI config

| Var | Default | Notes |
|---|---|---|
| `WALLE_URL` | `http://127.0.0.1:${WALLE_PORT:-6007}` | Gateway base URL |
| `WALLE_TOKEN` | required | Bearer token |
| `WALLE_MSG_TIMEOUT` | `30m` | Overall wait timeout |

Exit non-zero for usage/config errors, non-2xx gateway responses, timeout, or a
stream ending before completion.

## Implementation phases

1. **CLI skeleton**
   - Add subcommands: `run`, `msg`, `help`.
   - Change no-arg behavior to help.
   - Keep Docker/supervisor entrypoints explicit: `wall-e run`.

2. **stdin message client**
   - Implement `wall-e msg <type:id>`.
   - Read prompt from stdin, reject empty input.
   - POST to `/v1/inject` and consume SSE until `done`.

3. **Shared injection path**
   - Add `/v1/inject` with bearer auth.
   - Validate typed channel and message.
   - Reuse most recent session via existing session manager/pool behavior.
   - Support `http` first.

4. **Telegram adapter**
   - Extract reusable Telegram response delivery from inbound message handling.
   - Add injection support that records the user prompt in session and sends
     only the assistant response to Telegram.
   - Respect Telegram allowed-chat config.

5. **Docs/skills/system prompt**
   - Document heredoc/file/pipe usage.
   - Add cron examples.
   - Add skill/system guidance recommending explicit scheduled-task context when
     appropriate, without enforcing a prefix.
