# HTTP channel

The HTTP front-end exposes the prompt API over the worker pool plus a local read-only session-debug UI. It does **not** re-serialize same-channel prompt requests itself — the pool does — it only bounds the Acquire wait with a queue timeout and streams pi events back as SSE.

HTTP is intentionally a **raw streaming transport** for applications such as the `wall-e` CLI. It does not apply buffered chat presentation controls. In particular, if the assistant's complete output is `NO_REPLY`, HTTP still emits that text as ordinary `delta` events followed by `agent_end` and `done`. The calling application may interpret the text itself, but the HTTP gateway does not remove or rewrite it.

## Endpoints

### `GET /health`

No auth. Returns `200` with a JSON body:

```json
{"status":"ok"}
```

Use it for container healthchecks and smoke tests.

### `GET /`

No auth. Serves the static read-only session UI from `WALLE_SITE`.

### `GET /v1/sessions`

No auth. Returns all typed session files grouped-friendly for the UI. The response omits raw channel ids and uuids.

### `GET /v1/sessions/{key}/messages`

No auth. Returns the selected transcript's chat-visible messages as JSON for the web UI:

```json
{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"hello"}]}
```

### `GET /v1/sessions/{key}/export.html`

No auth. Exports the selected session via pi's `export_html` RPC command into a temporary file and returns it as `text/html`.

### `POST /v1/prompt`

Bearer auth (`WALLE_TOKEN`). Body:

```json
{
  "channelType": "http",
  "channel": "smoke",
  "message": "describe this image",
  "attachments": [{
    "fileName": "photo.jpg",
    "mimeType": "image/jpeg",
    "data": "<base64 data>"
  }]
}
```

- `channelType` (required) — the delivery adapter/type, for example `http` or `telegram` when Telegram is enabled.
- `channel` (required) — the stable id within that channel type. For `http`, pick anything stable per conversation. For `telegram`, use the Telegram chat id.
- `message` — the user prompt text; it may be empty when at least one attachment is present.
- `attachments` — optional inbound files with a name, optional MIME type, and standard base64 data. They are saved under the session media directory and linked into the agent prompt.

Once the prompt is accepted it returns `200` with `Content-Type: text/event-stream` and streams the turn as SSE until the terminal `agent_end`, then a `done` event. Provider failures after acceptance are returned as a terminal SSE `error` event. Non-terminal `agent_end` events produced before an automatic retry are kept internal. The target delivery adapter may also deliver the assistant response externally; for example `channelType: "telegram"` sends the assistant response to that Telegram chat while still streaming the same response to the HTTP caller. These subscribers deliberately have different presentation policies: HTTP preserves every text delta, while a buffered external channel may suppress its own delivery when the authoritative final response is exactly `NO_REPLY`.

## SSE event format

```
event: agent_start
data: {}

event: delta
data: {"text":"Hello"}

event: attachment
data: {"fileName":"report.pdf","mimeType":"application/pdf","data":"<base64 data>","caption":"Requested report"}

event: delta
data: {"text":" world"}

event: agent_end
data: {}

event: done
data: {}
```

- `agent_start` — the agent turn began.
- `delta` — an incremental assistant text chunk (`message_update` with `assistantMessageEvent.type == "text_delta"`). Concatenate the `data.text` values for the full message. Other pi delta types (thinking, tool calls) are ignored in v1.
- `message` — text delivered directly to this active HTTP channel through `/v1/send`; it is separate from assistant deltas.
- `attachment` — a file delivered directly through `/v1/send`, containing `fileName`, `mimeType`, base64 `data`, and an optional `caption`. HTTP media is limited to 32 MiB before base64 encoding.
- `agent_end` — the turn finished.
- `done` — terminal; the stream closes.
- `error` — `data: {"message":"..."}` — terminal provider/API failures and streams that end unexpectedly are surfaced here, then the stream closes without `done`.

### `POST /v1/send`

Bearer auth. Delivers text or one local media file into an active HTTP prompt
stream without creating another agent turn. This is how an agent can use
`wall-e send` to return a generated file to an HTTP caller:

```json
{
  "channelType": "http",
  "channel": "smoke",
  "mediaPath": "/home/wall-e/report.pdf",
  "caption": "Requested report"
}
```

The matching `/v1/prompt` stream receives an `attachment` event with base64
file data. Direct text uses `text` instead of `mediaPath` and produces a
`message` event. A send to an HTTP channel with no active prompt receiver
returns `409`; media paths must be absolute, clean paths to regular local files.

## Auth

`Authorization: Bearer <WALLE_TOKEN>`. Comparison is constant-time (`subtle.ConstantTimeCompare`); a missing or wrong token returns `401`. `WALLE_TOKEN` is required for the gateway to start.

## Behavior on a busy channel

A second POST to the same typed channel while a turn is streaming is treated like a chat mid-stream message: it **steers** the active turn instead of starting a separate queued turn. The HTTP caller can subscribe to the ongoing stream from that point forward.

A POST to a different channel may need to wait for a pool slot. The wait is bounded by `WALLE_HTTP_QUEUE_TIMEOUT` (default `60s`):

- If a slot is acquired in time → `200`, normal SSE stream.
- If the wait exceeds the timeout → `503` `{"error":"channel busy"}`.
- If the client disconnects during the wait → the handler returns silently.

## Client disconnect

If the HTTP client disconnects mid-stream, the handler detaches its SSE subscription and returns. It does **not** abort the underlying turn, because another subscriber may still be delivering the same assistant response to an external chat such as Telegram. Pool shutdown still aborts/drains in-flight turns during gateway stop.

## Smoke test

```sh
TOKEN="$(openssl rand -hex 32)"   # or your WALLE_TOKEN
PORT="${WALLE_PORT:-6007}"

curl -s "http://localhost:$PORT/health"

curl -N -H "Authorization: Bearer $TOKEN" \
  -d '{"channelType":"http","channel":"smoke","message":"say hi"}' \
  http://localhost:$PORT/v1/prompt
```

`-N` disables curl's output buffering so the SSE events stream as they arrive.

The CLI wraps the same endpoint for local automation:

```sh
export WALLE_TOKEN="$TOKEN"
wall-e msg http:smoke <<'EOF'
say hi
EOF
```

## Config

| Var | Default | Notes |
|---|---|---|
| `WALLE_TOKEN` | — | required bearer token |
| `WALLE_PORT` | `6007` | listen port |
| `WALLE_HTTP_QUEUE_TIMEOUT` | `60s` | max wait to acquire/steer a prompt turn → 503 |
| `WALLE_SITE` | `/opt/wall-e/www` | static session-debug UI dir |
| `WALLE_SESSION_EXPORT_TIMEOUT` | `30s` | max time to export a session HTML file |
| prompt body limit | `8 MiB` | oversized `/v1/prompt` requests return 413 |
