# File-first media support — Implementation Plan

**Date:** 2026-07-13
**Status:** Planning

## Goal

Keep media support simple and channel-neutral by treating inbound attachments as
files first, not as typed model payloads.

1. **Incoming media:** every channel adapter downloads/saves incoming media to a
   durable session media directory, then submits a normal text prompt that points
   at the saved file path.
2. **Outgoing media/text:** provide a generic `wall-e send` command that sends
   text and/or files into a channel without starting an agent turn.
3. **Agent guidance:** add a `media` skill that teaches the agent how to send
   files/text back to the current channel and how to handle failures.

This design keeps session files small, avoids channel-specific media logic in
the agent loop, and lets pi/tools decide how to inspect files by path.

## Design decision: file-first incoming attachments

Yes: for wall-e, storing inbound media as files and adding a file link to the
prompt is the simplest robust default.

Benefits:

- **Small transcripts:** session JSONL stores text links, not base64 blobs.
- **Format agnostic:** image/audio/video/PDF/etc. all share the same channel
  contract: save a file, mention the file.
- **Tool-friendly:** the agent can use existing filesystem tools to inspect,
  convert, transcribe, summarize, or attach the file later.
- **Channel-friendly:** Telegram, future Discord, HTTP, email, etc. only need to
  implement “fetch bytes and save file”.
- **Debuggable:** operators can inspect the media directory directly.

Tradeoff:

- This does **not** automatically feed image pixels into model-native vision on
  the first LLM call. The prompt gives the agent a path. If it needs content, it
  should use tools that can read/process that file. A later optional enhancement
  can add native `images` RPC alongside file links for channels that want that,
  but v1 should stay file-first.

## Storage layout

Use a media directory under the persisted session directory:

```text
${WALLE_SESSION_DIR}/media/
```

With current defaults this is:

```text
/home/wall-e/sessions/media/
```

Each incoming file is stored as:

```text
${WALLE_SESSION_DIR}/media/<timestamp>--<safe-filename>
```

Example:

```text
/home/wall-e/sessions/media/20260713T184512Z--photo.jpg
```

If a filename collides, append a short suffix:

```text
20260713T184512Z--photo--2.jpg
20260713T184512Z--photo--3.jpg
```

Rules:

- Use UTC timestamps in the same style as session files.
- Sanitize filenames: remove path separators, control chars, shell-hostile
  chars, and leading dots; keep a reasonable length.
- Preserve extensions when possible.
- Detect MIME for validation, but do not bake format-specific behavior into the
  core path.
- Store media files with owner-only write permissions (`0600` or `0644` as
  appropriate inside the container).

## Prompt text inserted for incoming attachments

For each saved inbound file, append a normal Markdown link to the user prompt:

```md
Attached file: [photo.jpg](/home/wall-e/sessions/media/20260713T184512Z--photo.jpg)
```

If the incoming message has text/caption, keep it first:

```md
Can you look at this?

Attached file: [photo.jpg](/home/wall-e/sessions/media/20260713T184512Z--photo.jpg)
```

If the incoming message has only attachments, submit:

```md
The user attached file(s):

Attached file: [photo.jpg](/home/wall-e/sessions/media/20260713T184512Z--photo.jpg)
```

For multiple files:

```md
The user attached file(s):

Attached file: [photo.jpg](/home/wall-e/sessions/media/20260713T184512Z--photo.jpg)
Attached file: [voice.ogg](/home/wall-e/sessions/media/20260713T184530Z--voice.ogg)
```

No base64 should be written into the pi session transcript.

## Inbound batching semantics

Attachments that belong to the same user action should be delivered to pi as one
initial prompt, not as an initial prompt followed by a steering prompt.

Required behavior:

- A platform message with text/caption plus media becomes one formatted prompt.
- A platform media group/album becomes one formatted prompt containing all saved
  file links.
- The adapter must save/download the files before calling `turn.Manager.Submit`.
- Only after the complete prompt text is built should the turn begin.

For channels that deliver related content as multiple updates, the adapter may
need a small pre-turn batching window. Telegram albums, for example, use
`media_group_id`; collect updates for the same chat/group for a short debounce
period, then submit one prompt with every saved file. This batching happens
before the active-turn check. It is not steering.

If the agent is already responding when a truly new attachment arrives later,
then it follows the existing active-turn behavior and is sent as steer/prompt-
steer. That is only for later user input, not for the pieces of a single
pre-turn media message.

## Core media package

Add a small package such as `src/media` that every adapter can share:

```go
type SavedFile struct {
    OriginalName string
    StoredName   string
    Path         string
    MimeType     string
    Size         int64
}

type Store struct {
    Dir string // always ${WALLE_SESSION_DIR}/media unless explicitly injected in tests
}

func (s *Store) Save(ctx context.Context, originalName string, r io.Reader) (SavedFile, error)
func FormatAttachmentPrompt(text string, files []SavedFile) string
```

Responsibilities:

- Create `${WALLE_SESSION_DIR}/media`.
- Sanitize names and avoid collisions.
- Sniff MIME for metadata/validation.
- Return saved-file metadata.
- Generate the standard `Attached file: [...]` prompt lines.

The core media package should not know about Telegram, HTTP, Discord, or pi RPC.

## Configuration

Do not add new media-specific environment variables for v1.

The media directory is derived from the session directory:

```text
${WALLE_SESSION_DIR}/media
```

That keeps the persistence model simple: sessions and their attached files live
under the same tree.

Do not add configurable media size limits unless a real need appears. For v1:

- Channel APIs, such as Telegram, already impose their own media/file limits.
- Existing HTTP request body limits still protect JSON/base64 prompt uploads.
- Outgoing sends should let the target channel reject files it cannot accept and
  return that failure to `wall-e send` stdout.

Important deployment assumption: the wall-e HTTP API is intended to be strictly
local/private. Do not expose attachment-capable HTTP endpoints directly to the
public internet. If the HTTP API becomes public, large file uploads create an
obvious disk/CPU/bandwidth DoS risk and media-specific request limits, quotas,
auth hardening, and probably multipart streaming limits become required.

MIME allowlists can be channel-specific later. For the core v1, prefer broad
storage: save the file even if wall-e does not understand the format, then let
the agent/tools decide what to do with the path.

## Incoming HTTP API

Keep existing `/v1/prompt` JSON compatible. Add optional file attachments as
base64 or multipart.

### JSON/base64 v1

```json
{
  "channelType": "http",
  "channel": "smoke",
  "message": "look at this",
  "attachments": [
    {
      "fileName": "photo.jpg",
      "mimeType": "image/jpeg",
      "data": "base64..."
    }
  ]
}
```

Server behavior:

1. Decode each attachment.
2. Save it through `media.Store`.
3. Replace the submitted prompt with `FormatAttachmentPrompt(message, files)`.
4. Call the existing text prompt path.

The HTTP prompt endpoint still streams normal SSE text deltas. No inbound media
bytes are sent to pi RPC.

### Multipart phase 2

Add multipart later if JSON/base64 becomes awkward:

```http
POST /v1/prompt
Content-Type: multipart/form-data

channelType=http
channel=smoke
message=look at this
file=@photo.jpg
```

## Incoming CLI

Extend `wall-e msg` with repeatable file attachments:

```sh
wall-e msg telegram:123456789 --file ./photo.jpg <<'EOF'
Can you look at this?
EOF
```

Aliases may be useful:

```sh
wall-e msg telegram:123456789 --media ./photo.jpg
wall-e msg telegram:123456789 --image ./photo.jpg
```

CLI behavior:

- Read prompt text from stdin as today.
- Read each file from disk and POST it to `/v1/prompt` as an attachment.
- Gateway saves the files under `${WALLE_SESSION_DIR}/media` and submits links.
- Reject missing/unreadable/oversized files before POST when practical.

## Incoming Telegram

Telegram adapter changes:

- Extend message structs to include `caption`, `photo`, `document`, `voice`,
  `audio`, `video`, and other media IDs.
- Add `getFile` + file download support.
- For every incoming media item, download bytes and call `media.Store.Save`.
- Use text or caption as the prompt prefix.
- Submit only the formatted text prompt to the turn manager.

Supported in v1 because they are all just files:

- photos
- image documents
- generic documents
- voice/audio files
- short videos, if the channel delivers them successfully

The adapter does not need to transcribe audio or understand images. It simply
saves the file and tells the agent where it is.

## Turn manager and RPC changes

With file-first media, the shared turn manager can remain string-based for v1.

No required changes to:

- `turn.Manager.Submit(ctx, channel, message, opts)`
- `rpc.Client.Prompt(ctx, message, steer)`
- pi RPC image fields

Adapters transform attachments into text before calling the turn manager.

This is intentionally simpler than model-native multimodal input. If native
vision is desired later, add it as an optional parallel path rather than the
core contract.

## Outgoing: generic channel send

Add a direct delivery command that does **not** create a pi prompt/turn:

```sh
wall-e send <channelID>
wall-e send <channelID> "<text>"
wall-e send --media <channelID> <filepath>
wall-e send --media <channelID> <filepath> --caption "Here is the image"
```

Where `<channelID>` is the existing typed channel address:

```text
telegram:123456789
http:some-channel
```

CLI behavior:

- `wall-e send <channelID> "text"` sends text directly to that channel.
- `wall-e send <channelID>` reads text from stdin and sends it.
- `wall-e send --media <channelID> <filepath>` sends the file directly.
- `--caption` adds channel-native caption text when supported; otherwise send
  caption as a text message near the file.
- Validate local file existence and max size.
- Return machine-readable status on stdout so the agent can inspect failures.
- Exit non-zero on failure.

Suggested stdout format:

Success:

```json
{"ok":true,"channel":"telegram:123456789","sent":[{"type":"media","path":"/home/wall-e/out/report.pdf"}]}
```

Failure:

```json
{"ok":false,"error":"telegram: file too large"}
```

Keep stderr for debug logs only. The agent should rely on stdout + exit code.

## Outgoing HTTP endpoint

Implement `wall-e send` over a local authenticated HTTP API, similar to
`wall-e msg`:

```http
POST /v1/send
Authorization: Bearer $WALLE_TOKEN
Content-Type: application/json

{
  "channelType": "telegram",
  "channel": "123456789",
  "text": "hello"
}
```

For media, either:

1. JSON with a local path (simple, because CLI and agent run inside container):

   ```json
   {
     "channelType": "telegram",
     "channel": "123456789",
     "mediaPath": "/home/wall-e/out/report.pdf",
     "caption": "Here is the report"
   }
   ```

2. Multipart upload (later, useful for remote HTTP clients).

Security checks:

- Require bearer auth.
- Parse typed channel exactly as `/v1/prompt` does.
- Enforce Telegram allowed chats and future channel allowlists.
- Require `mediaPath` to be absolute, clean, existing, and a regular file.
- Let the target channel enforce media size/type constraints and return clear delivery errors.
- Do not log file contents.

## Delivery adapter interface

Split prompt adapters from direct send adapters:

```go
type SendRequest struct {
    Channel string
    Text    string
    MediaPath string
    Caption string
}

type SendAdapter interface {
    Send(ctx context.Context, req SendRequest) (SendResult, error)
}
```

Register adapters by channel type:

- `telegram` sends text with `sendMessage`; media with `sendPhoto` for images
  and `sendDocument` as fallback.
- `http` can record sends in the session/debug UI or return unsupported until a
  consumer exists.
- future Discord/email adapters implement the same interface.

## Telegram outgoing media

Extend `TelegramAPI` with multipart methods:

```go
SendPhoto(ctx context.Context, chatID int64, path string, caption string) (Message, error)
SendDocument(ctx context.Context, chatID int64, path string, caption string) (Message, error)
```

Rules:

- If MIME starts with `image/`, try `sendPhoto`.
- If `sendPhoto` fails because Telegram rejects the file, fall back to
  `sendDocument`.
- For all other files, use `sendDocument`.
- Respect Telegram caption limits. If caption is too long, send it as a separate
  text message.
- Return errors to `wall-e send` as JSON on stdout.

## Media skill

Add a bundled skill at:

```text
static/skills/media/SKILL.md
```

Skill frontmatter:

```yaml
---
name: media
description: Send text, images, audio, and other files back to the current wall-e channel.
---
```

The skill should teach the agent:

- Determine the current channel with `$WALLE_CHANNEL`.
- Use `wall-e send "$WALLE_CHANNEL" "text"` for direct text delivery.
- Use `wall-e send --media "$WALLE_CHANNEL" /path/to/file --caption "..."` for
  attachments.
- Check stdout JSON and exit code.
- If delivery fails, tell the user and offer an alternative.
- Prefer absolute paths.
- Keep generated files under `/home/wall-e` or another accessible path.
- Do not assume all channels support every media type; rely on `wall-e send` to
  report channel-specific failures.

## Implementation phases

1. **Media store**
   - Add `src/media` with safe filename generation, save logic, and prompt
     formatting.
   - Derive the media directory from `WALLE_SESSION_DIR` as
     `${WALLE_SESSION_DIR}/media`; do not add new media env vars.

2. **HTTP inbound files**
   - Add JSON attachment support to `/v1/prompt`.
   - Save attachments to media store.
   - Submit generated text links to the existing turn path.
   - Add tests for base64, filename sanitization, and prompt text.

3. **Telegram inbound files**
   - Extend Telegram structs and fake API.
   - Implement `getFile`/download.
   - Save photos/documents/voice/audio/etc. as files.
   - Submit text/caption plus attachment links.

4. **CLI inbound files**
   - Add `wall-e msg --file/--media`.
   - Reuse HTTP attachment JSON.

5. **Direct send API**
   - Add `POST /v1/send` and send adapter registry.
   - Add `wall-e send` CLI.
   - Return stdout JSON and non-zero failures.

6. **Telegram outgoing send**
   - Implement `sendMessage`, `sendPhoto`, and `sendDocument` in the send
     adapter.
   - Add tests for success/failure and allowed-chat enforcement.

7. **Media skill**
   - Add `static/skills/media/SKILL.md`.
   - Document examples and failure handling.

8. **Docs/session UI**
   - Document incoming media storage and `wall-e send`.
   - Optionally show linked attachments in the session UI.

## Test checklist

- Incoming attachment saves under `${WALLE_SESSION_DIR}/media`.
- Filenames are sanitized and collision-safe.
- Prompt text includes exactly `Attached file: [name](path)` links.
- No base64 media appears in session messages.
- HTTP rejects oversized attachments.
- Telegram downloads and saves photos/documents/voice/audio that the channel delivers.
- Text-only behavior remains unchanged.
- `wall-e send <channel> "text"` sends text without creating a prompt turn.
- `wall-e send --media <channel> <path>` sends files and returns JSON stdout.
- Send failures are visible in stdout and exit non-zero.
- Telegram allowed-chat checks apply to both prompt and send paths.
