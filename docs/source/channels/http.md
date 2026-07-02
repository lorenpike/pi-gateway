# HTTP channel

The HTTP front-end exposes the prompt API over the worker pool plus a local read-only session-debug UI. It does **not** re-serialize same-channel prompt requests itself — the pool does — it only bounds the Acquire wait with a queue timeout and streams pi events back as SSE.

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

### `GET /v1/sessions/{key}/export.html`

No auth. Exports the selected session via pi's `export_html` RPC command into a temporary file and returns it as `text/html`.

### `POST /v1/prompt`

Bearer auth (`WALLE_TOKEN`). Body:

```json
{"channel": "smoke", "message": "say hi"}
```

- `channel` (required) — the ChannelID string. Pick anything stable per conversation; the pool binds one pi process to it.
- `message` (required) — the user prompt text.

On success it returns `200` with `Content-Type: text/event-stream` and streams the turn as SSE until `agent_end`, then a terminal `done` event.

## SSE event format

```
event: agent_start
data: {}

event: delta
data: {"text":"Hello"}

event: delta
data: {"text":" world"}

event: agent_end
data: {}

event: done
data: {}
```

- `agent_start` — the agent turn began.
- `delta` — an incremental assistant text chunk (`message_update` with `assistantMessageEvent.type == "text_delta"`). Concatenate the `data.text` values for the full message. Other delta types (thinking, tool calls) are ignored in v1.
- `agent_end` — the turn finished.
- `done` — terminal; the stream closes.
- `error` — `data: {"message":"..."}` — emitted if the stream ends without `agent_end` (e.g. the process died), then the stream closes.

## Auth

`Authorization: Bearer <WALLE_TOKEN>`. Comparison is constant-time (`subtle.ConstantTimeCompare`); a missing or wrong token returns `401`. `WALLE_TOKEN` is required for the gateway to start.

## Behavior on a busy channel

The pool serializes same-channel requests: a second POST to the same `channel` while the first is streaming **blocks** (not 503) until the slot frees, then streams its own turn. The wait is bounded by `WALLE_HTTP_QUEUE_TIMEOUT` (default `60s`):

- If the slot frees in time → `200`, normal SSE stream.
- If the wait exceeds the timeout → `503` `{"error":"channel busy"}`.
- If the client disconnects during the wait → the handler returns silently.

## Client disconnect → abort

If the client disconnects mid-stream, the handler sends `abort` to the slot's pi process (best-effort) so the pool's drain-on-reuse for the next Acquire completes promptly, then Releases the slot. This is why `docker stop` (which aborts in-flight streams via pool shutdown) lets the SSE handlers unblock and return.

## Smoke test

```sh
TOKEN="$(openssl rand -hex 32)"   # or your WALLE_TOKEN
PORT="${WALLE_PORT:-6007}"

curl -s "http://localhost:$PORT/health"

curl -N -H "Authorization: Bearer $TOKEN" \
  -d '{"channel":"smoke","message":"say hi"}' \
  http://localhost:$PORT/v1/prompt
```

`-N` disables curl's output buffering so the SSE events stream as they arrive.

## Config

| Var | Default | Notes |
|---|---|---|
| `WALLE_TOKEN` | — | required bearer token |
| `WALLE_PORT` | `6007` | listen port |
| `WALLE_HTTP_QUEUE_TIMEOUT` | `60s` | max wait on a busy channel → 503 |
| `WALLE_SITE` | `/opt/wall-e/www` | static session-debug UI dir |
| `WALLE_SESSION_EXPORT_TIMEOUT` | `30s` | max time to export a session HTML file |
