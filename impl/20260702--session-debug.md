# Session debug UI / export plan

Date: 2026-07-02

## Confirmed decisions

- Ignore TLS for now. The local UI will be served over plain `http://localhost:6007/`.
- Static UI and session-debug endpoints do **not** require bearer auth. This is local, read-only introspection.
- Existing write/agent-driving endpoint `/v1/prompt` keeps bearer auth.
- List **all** sessions, not just the latest/current session per channel.
- Organize the UI by channel type and date.
- New session filenames should include channel type and a human-readable UTC datestamp.

## Goal

Add a small browser UI for inspecting wall-e/pi sessions while the gateway is running in the container.

The UI assets will live in the repo under:

```text
static/www/
```

and will be copied into the image at:

```text
/opt/wall-e/www
```

The runtime path is configurable with:

```sh
WALLE_SITE=/some/path
```

## New session filename format

Use this format for all generated session files:

```text
<channel-type>--<channel-id>--<datestamp>--<uuid>.jsonl
```

Example:

```text
http--smoke--20260702T153012Z--a1b2c3d4e5f67890.jsonl
telegram--123456789--20260702T153055Z--d9876cafe1234567.jsonl
```

Rules:

- `channel-type`: `http`, `telegram`, future `discord`, `slack`, etc.
- `channel-id`: the platform/channel identifier, sanitized for filenames.
- `datestamp`: UTC, format `YYYYMMDDTHHMMSSZ`, lexicographically sortable.
- `uuid`: existing random suffix to avoid collisions.
- Filename components must not contain the `--` separator after sanitization.

The user-facing UI should **not** display raw channel id or uuid. It should display channel type and date/time. The internal API may still include an opaque session key used by the Export button.

## Migration/backward compatibility

No migration/backward compatibility is needed because the system is not deployed yet. Replace the old filename scheme outright: generation, rebuild, listing, and resolving should only support the new typed format.

## Channel typing in code

Currently most code passes a plain `pool.ChannelID` string:

- HTTP: JSON body `channel`
- Telegram: numeric chat id string

Plan:

- Introduce a typed channel identity, either:

```go
type ChannelRef struct {
    Type string
    ID   string
}
```

or keep the pool key string but construct it via a helper:

```go
session.ChannelKey("http", req.Channel)
session.ChannelKey("telegram", strconv.FormatInt(chatID, 10))
```

Preferred implementation: add `session.ChannelRef` and thread it through `session.Manager`/`pool` so filename generation does not depend on parsing an ad-hoc string.

HTTP mapping:

```text
channel-type = http
channel-id   = request body channel
```

Telegram mapping:

```text
channel-type = telegram
channel-id   = Telegram chat id
```

Future front-ends can provide their own type.

## Pi export behavior from docs

Pi exposes session export in three related ways:

- TUI command: `/export [file]` — exports the current session to HTML.
- CLI option: `pi --export <in> [out]` — exports a session file to HTML.
- RPC command: `export_html` — exports the RPC client's current session to an HTML file:

```json
{"type":"export_html","outputPath":"/tmp/session.html"}
```

Response:

```json
{"type":"response","command":"export_html","success":true,"data":{"path":"/tmp/session.html"}}
```

For the HTTP endpoint, use a dedicated short-lived pi RPC client, switch it to the requested session file, call `export_html` into a temporary file, then stream that file back to the browser. This avoids disturbing any warm pool slot that is serving real HTTP/chat traffic.

## Static site

### Files

Create:

```text
static/www/index.html
```

For the first version, keep it as one small self-contained HTML file.

### Docker

Update `Dockerfile`:

```dockerfile
COPY --chown=wall-e:wall-e static/www/ /opt/wall-e/www/
ENV WALLE_SITE=/opt/wall-e/www
```

### Config

Add `WALLE_SITE` parsing:

- Default: `/opt/wall-e/www`
- Stored on `httpapi.Config` as `SiteDir string`.

Update docs/README config tables.

### HTTP serving behavior

Add static file serving to the HTTP mux:

- Existing API routes keep priority:
  - `/health`
  - `/v1/prompt`
  - new session-debug endpoints below
- `/` serves files from `WALLE_SITE` via `http.FileServer`.
- `/` returns `index.html`.

If `WALLE_SITE` is empty or missing, static serving returns `404` for `/`.

## Browser UI behavior

`index.html` will provide:

1. A “Load sessions” button.
2. Sessions grouped by channel type, then date.
3. Rows/cards showing:
   - channel type
   - date/time
   - optional session name
   - message count
4. A per-session “Export HTML” button.
   - The browser fetches the export endpoint.
   - It creates a `Blob` URL from the returned HTML.
   - It opens the exported HTML in a new tab or displays it in an iframe.

No bearer-token input is needed for this read-only debug UI.

## New HTTP API endpoints

These session/debug endpoints are local read-only introspection and do **not** require bearer auth.

### `GET /v1/sessions`

Return all known session files from `WALLE_SESSION_DIR`, grouped-friendly for the UI.

Proposed response:

```json
{
  "sessions": [
    {
      "key": "opaque-export-key",
      "channelType": "http",
      "datestamp": "20260702T153012Z",
      "createdAt": "2026-07-02T15:30:12Z",
      "modifiedAt": "2026-07-02T15:45:00Z",
      "sessionId": "pi-session-uuid-from-header",
      "name": "optional session name",
      "cwd": "/home/wall-e",
      "messageCount": 12
    }
  ]
}
```

Implementation details:

- Scan `SessionDir` for `.jsonl` files matching the new typed filename format only.
- Parse the first JSONL line for the pi session header:

```json
{"type":"session","version":3,"id":"...","timestamp":"...","cwd":"..."}
```

- Scan entries lightly for:
  - latest `session_info.name`
  - count of `message` entries
- Do not return raw channel id, uuid, or absolute session file path in the API response unless needed later.
- Sort server-side by `channelType`, then descending `createdAt`.

### `GET /v1/sessions/{key}/export.html`

Export one session to HTML and return it.

Behavior:

1. Resolve `{key}` to a session file under `WALLE_SESSION_DIR`.
2. Reject path traversal and unknown keys.
3. Create a temp output file, e.g. `/tmp/walle-session-*.html`.
4. Spawn a short-lived pi RPC client.
5. `switch_session` to the resolved session path.
6. Call `export_html` with the temp output path.
7. Send the temp file back with:

```http
Content-Type: text/html; charset=utf-8
Content-Disposition: inline; filename="session-<datestamp>.html"
```

8. Delete the temp file after the response is sent.

Timeouts:

- Add `WALLE_SESSION_EXPORT_TIMEOUT`, default `30s`, or use a hardcoded 30s context for v1.

Concurrency:

- Export should not acquire a normal pool slot, so it does not queue behind active chat/prompt work and does not mutate warm pooled clients.
- It temporarily spawns one pi process per export request.
- If this needs bounding later, add a small semaphore.

## RPC changes

Add a typed client method:

```go
func (c *Client) ExportHTML(ctx context.Context, outputPath string) (string, error)
```

It sends:

```json
{"type":"export_html","outputPath":"..."}
```

and decodes `data.path`.

## HTTP implementation shape

Likely changes:

- `src/config/config.go`
  - parse `WALLE_SITE`
  - maybe parse `WALLE_SESSION_EXPORT_TIMEOUT`
- `src/httpapi/server.go`
  - add static handler
  - add `/v1/sessions`
  - add `/v1/sessions/{key}/export.html`
- `src/session/manager.go`
  - add typed session filename generation
  - add safe session listing/resolution helpers
  - support the new typed filename parsing only
- `src/rpc/client.go`
  - add `ExportHTML`
- `src/chat/chat.go`
  - pass `telegram` channel type for Telegram chats
- `src/httpapi/server.go`
  - pass `http` channel type for prompt body channels
- `src/main.go`
  - pass session manager/export config into `httpapi.Server`
- `Dockerfile`
  - copy static site and set `WALLE_SITE`
- `README.md` and `docs/source/channels/http.md`
  - document UI + endpoints + env vars

To keep tests easy, make the export operation injectable behind a small interface, for example:

```go
type SessionExporter interface {
    ExportHTML(ctx context.Context, sessionPath string, outputPath string) error
}
```

Production implementation uses pi RPC. Tests use a fake exporter that writes a known HTML file.

## Tests

Add/extend tests for:

1. Config
   - `WALLE_SITE` default is `/opt/wall-e/www`.
   - override works.
2. Static files
   - `/` serves `index.html` from a temp site dir.
   - existing `/health` and `/v1/prompt` still route correctly.
3. Session filename generation
   - HTTP uses `http--<channel-id>--<datestamp>--<uuid>.jsonl`.
   - Telegram uses `telegram--<chat-id>--<datestamp>--<uuid>.jsonl`.
   - unsafe chars and `--` separators are sanitized out of components.
4. Rebuild/listing
   - new-format files parse correctly.
   - old-format/non-matching files are ignored.
   - all matching files are listed, not only latest per channel.
   - list is sorted/groupable by channel type/date.
   - UI/API metadata excludes raw channel id and uuid.
5. Export endpoint
   - no bearer token required.
   - unknown session returns `404`.
   - path traversal attempts return `400`/`404`.
   - known session calls fake exporter, writes temp HTML, returns `text/html`.
   - temp file is removed after response.
6. RPC client
   - fake pi handles `export_html`; client decodes returned path.
