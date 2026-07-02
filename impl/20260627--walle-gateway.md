# wall-e Gateway — Implementation Plan

**Date:** 2026-06-27
**Status:** Planning → implementation
**Author:** planning session (pi-assisted)

## 1. Goal

A single Go binary (`wall-e`) that runs inside the existing Ubuntu container and
exposes a small HTTP API plus Discord and Telegram front-ends, each fronting a
**fixed pool of `pi --mode rpc` child processes**. The gateway translates
between chat-platform events and pi's JSONL RPC protocol.

### Non-goals (v1)

- Interactive extension-UI routing to chat platforms (auto-answered by policy
  instead).
- HTTP admin/control plane (list/abort/evict via API).
- Webhook ingestion.
- Multi-tenant auth (single shared bearer token only).
- Per-channel configuration UI (all config via env vars).

## 2. Architecture

```
            ┌────────────────────────────────────────────────────────────┐
            │  container (ubuntu:24.04, tini = PID 1)                    │
            │                                                            │
            │  ┌─────────────────────┐    spawn/manage    ┌────────────┐ │
            │  │ wall-e (Go)         │ ─────────────────▶ │ pi --mode  │ │
            │  │                     │ ◀── JSONL stdio ───│  rpc  (×N) │ │
            │  │  - HTTP server      │                    └────────────┘ │
            │  │  - Discord listener │                                   │
            │  │  - Telegram listener│  fixed pool, 1 live proc/channel  │
            │  │  - session manager  │                                   │
            │  │  - worker pool      │                                   │
            │  └─────────────────────┘                                   │
            │           ▲                                                │
            │           │ WALLE_* env                                    │
            └───────────┼────────────────────────────────────────────────┘
                        │
   Discord  ────  gateway:8080  ────  Telegram
   HTTP/bearer ──────────────────────────────
```

- `tini` is PID 1 (reaps zombies, forwards signals to the gateway).
- The gateway is the container entrypoint; it spawns/owns the `pi` processes.
- TUI attachment is still possible via `docker exec ... tmux` for debugging, *not* as the production agent path.

## 3. Decisions summary

| Area | Decision |
|---|---|
| Language | Go, single static binary |
| PID 1 | `tini` |
| Config | env vars only, `WALLE_` prefix |
| Agent driver | `pi --mode rpc` subprocess, JSONL over stdin/stdout |
| Concurrency | fixed pool of M `pi` processes; one live process per active channel |
| Channel identity | stable per-platform id (Discord channel id, Telegram chat id, HTTP client id) |
| Session identity | pi transcript file: `<channel-id>--<unix-ts>--<uuid>.jsonl` under session dir |
| Channel↔session map | gateway-owned; re-synced via `get_state.sessionFile` after `new_session`/`clone`/`switch_session` |
| Inbound mid-stream (chat) | `prompt` with `streamingBehavior: "steer"` |
| Inbound mid-stream (HTTP) | per-channel request serialization (2nd request blocks/queues until 1st stream ends) |
| Worker reuse / eviction | drain: wait for `agent_end` (timeout → `abort`), then `switch_session`/respawn |
| Extension UI | auto-answered by policy; `confirm` default `true` (configurable); `select`→first; `input`/`editor`→cancelled; fire-and-forget ignored |
| HTTP surface | `/health` (no auth), `/v1/prompt` (bearer, SSE/chunked streamed response) |
| HTTP auth | single bearer token (`WALLE_TOKEN`), constant-time compare |
| Chat platforms (v1) | Discord + Telegram |

## 4. Configuration (env vars)

| Var | Required | Default | Notes |
|---|---|---|---|
| `WALLE_TOKEN` | yes | — | HTTP bearer token. Generate: `openssl rand -hex 32` |
| `WALLE_PORT` | no | `8080` | HTTP listen port |
| `WALLE_DISCORD_TOKEN` | no* | — | Discord bot token. *Required if Discord enabled. |
| `WALLE_TELEGRAM_TOKEN` | no* | — | Telegram bot token. *Required if Telegram enabled. |
| `WALLE_POOL_SIZE` | no | `4` | max concurrent `pi` processes |
| `WALLE_SESSION_DIR` | no | `/home/wall-e/sessions` | where transcripts live; passed to pi via `--session-dir` |
| `WALLE_PI_BIN` | no | `pi` | path to pi binary |
| `WALLE_PROVIDER` | no | from pi settings | `--provider` |
| `WALLE_MODEL` | no | from pi settings | `--model` (supports `provider/id` and `:thinking`) |
| `WALLE_CONFIRM_DEFAULT` | no | `true` | auto-answer `confirm` dialogs (`true`/`false`) |
| `WALLE_DRAIN_TIMEOUT` | no | `30s` | max wait for `agent_end` before `abort` on reuse/eviction |
| `WALLE_HTTP_QUEUE_TIMEOUT` | no | `60s` | max wait for a serialized HTTP request on a busy channel |
| `WALLE_LOG_LEVEL` | no | `info` | `debug`/`info`/`warn`/`error` |

Inherited (already passed through to the container, pi reads them):
`OPENAI_API_KEY`, `OPENROUTER_API_KEY`, `PI_CODING_AGENT_DIR` (`/opt/pi`).

> **Naming note:** earlier discussion mentioned `WALLE_GATEWAY_TOKEN`; we standardize on `WALLE_TOKEN`. The bearer token and (if ever needed) a separate "gateway control" token can be split later by adding `WALLE_ADMIN_TOKEN`.

## 5. Project layout

```
wall-e/
├── Dockerfile                 (modified: add tini, Go build, entrypoint)
├── Makefile                   (modified: docker target runs gateway, not tmux)
├── src/
│   ├── go.mod
│   ├── go.sum
│   ├── main.go                (env load, wire components, signal handling)
│   ├── rpc/
│   │   ├── client.go          (spawn pi, JSONL framing, command/event dispatch)
│   │   ├── client_test.go
│   │   ├── framing.go         (strict \n reader — NOT bufio.ReadLine semantics)
│   │   ├── framing_test.go
│   │   ├── types.go           (command/response/event structs)
│   │   └── extui.go           (auto-answer policy)
│   ├── session/
│   │   ├── manager.go         (channel→file map, naming, resync)
│   │   └── manager_test.go
│   ├── pool/
│   │   ├── pool.go            (worker slots, acquire/drain/evict)
│   │   └── pool_test.go
│   ├── httpapi/
│   │   ├── server.go          (health + prompt SSE)
│   │   ├── auth.go            (bearer, constant-time)
│   │   └── server_test.go
│   ├── chat/
│   │   ├── chat.go            (common Channel interface)
│   │   ├── discord.go
│   │   └── telegram.go
│   └── config/
│       └── config.go          (env parse, validation)
└── archive/
    └── 20260627--walle-gateway.md   (this file)
```

## 6. Implementation phases (TDD)

Each phase: **write failing tests first, implement to green, refactor.** Tests
run with `go test ./...`. Where a phase depends on a real `pi` process, gate the
integration test behind a build tag `//go:build integration` and a
`WALLE_PI_BIN` env so CI can skip it; unit tests use a fake JSONL "pi" that
replays scripted lines.

A reusable **fake pi** (`rpc/fakepi_test.go`) speaks the JSONL protocol over a
pipe, so phases 1–4 are fully testable without the real binary.

---

### Phase 0 — Skeleton & framing

**Goal:** strict JSONL line reader that does not split on `U+2028`/`U+2029` (the
bug called out in `docs/rpc.md`: Node `readline` is non-compliant; Go's
`bufio.Scanner` with `ScanLines` is fine, but we must *prove* it).

**Tests (write first):**
- `framing_test.go`:
  - `TestReader_SplitsOnLF` — `{"a":1}\n{"b":2}\n` → 2 records.
  - `TestReader_AcceptsCRLF` — `{"a":1}\r\n` → 1 record, no `\r` in payload.
  - `TestReader_KeepsUnicodeLineSeparators` — a record containing `U+2028`
    *inside a JSON string* is not split (verify the decoded string contains it).
  - `TestReader_PartialThenRest` — half a record, then the rest arrives in a
    later `Read` → one record.
  - `TestReader_HugeLine` — 1 MiB single line → one record.

**Implement:** `framing.go` with a hand-rolled `\n`-delimited reader backed by
`bufio.Reader` (index `\n`, strip optional trailing `\r`). Do **not** use any
generic "line reader" that treats Unicode separators as newlines.

**Done when:** `go test ./rpc` green.

---

### Phase 1 — RPC client

**Goal:** one Go type that owns a `pi --mode rpc` process and exposes typed
methods + an event channel.

**Tests (fake pi):**
- `TestClient_SpawnAndPrompt` — fake pi, send
  `{"type":"prompt","message":"hi"}`, assert a `response` with `success:true`
  and an `agent_end` event arrive.
- `TestClient_IDCorrelation` — command with `id:"req-1"` → response carries same
  `id`.
- `TestClient_SteerWhileStreaming` — when streaming, a `prompt` without
  `streamingBehavior` returns `success:false`; with `"steer"` returns
  `success:true`.
- `TestClient_Abort` — `abort` → `response success:true`.
- `TestClient_NewSessionResync` — after `new_session` response, client
  auto-calls `get_state` and updates its known `sessionFile`; test asserts the
  post-condition file path equals what fake pi reported.
- `TestClient_GetState` — fields parse into struct.
- `TestClient_ExtensionUIConfirm` — fake pi emits
  `extension_ui_request{method:"confirm"}`; client replies
  `extension_ui_response{confirmed:<default>}` within 50ms; default read from
  config.
- `TestClient_ExtensionUISelect` — replies `value` = first option.
- `TestClient_ExtensionUIInput_Editor_Cancelled` — replies `cancelled:true`.
- `TestClient_ExtensionUIFireAndForget_Ignored` — `notify`/`setStatus`/etc.
  produce no response, don't block.
- `TestClient_ProcessExit_ReturnsError` — fake pi closes stdout → `Client`
  returns a typed `ErrPiExit`.

**Implement:**
- `rpc/client.go`: `Client` struct holding `cmd`, stdin pipe, framing reader, a
  `sync.Map[id]chan response`, an event channel `Events <-chan Event`.
- Goroutine: read frames, parse `type`; route `response` to the matching request
  channel; route everything else (`agent_start`, `message_update`,
  `tool_execution_*`, `extension_ui_request`, …) onto `Events`.
- A second goroutine consumes `extension_ui_request` and applies the auto-answer
  policy from `config`.
- `Client` methods: `Prompt`, `Steer`, `FollowUp`, `Abort`, `NewSession`,
  `SwitchSession(path)`, `GetState`, `GetMessages`, `Compact`, `Bash`,
  `GetLastAssistantText`, `GetSessionStats`, `GetCommands`, `SetModel`, … (only
  the ones we need; add lazily).
- After any session-mutating call (`NewSession`, `SwitchSession`, `Clone`),
  automatically `GetState` and store `sessionFile`.

**Done when:** all green; `go vet ./...` clean.

---

### Phase 2 — Session manager

**Goal:** durable, stable mapping from `ChannelID` to the *current* session file path.

**Tests:**
- `TestManager_NewChannel_GeneratesPath` — first time a channel is seen, returns
  `WALLE_SESSION_DIR/<channelid>-<ts>-<uuid>.jsonl` with the timestamp/uuid
  shape (regex-assert).
- `TestManager_Roundtrip` — `SetCurrent(channel, path)` then `Current(channel)`
  returns same path.
- `TestManager_ResyncFromState` — given a `get_state` result with `sessionFile`,
  updates the map (used after `new_session`/`clone`/`switch_session`).
- `TestManager_ListKnownChannels` — returns all known channel ids.
- `TestManager_PersistOptional` — (skip in v1? decide) if persistence file is
  configured, map survives restart. **Decision: do NOT persist the map in v1** —
  it is rebuilt lazily from the session dir + the channel id prefix on the
  filename. Test instead:
- `TestManager_RebuildFromDir` — given a dir containing `chanA--1--u.jsonl` and
  `chanA--2--u.jsonl` and `chanB--1--u.jsonl`, `Current("chanA")` returns the
  newest (`--2--`) file, `Current("chanB")` returns the `chanB` file.

**Naming scheme:** `<channelId>--<unixSeconds>--<uuid>`. Channel ids are already
unique and filesystem-safe-ish; sanitize by replacing `/` and other unsafe chars
with `_`.

**Implement:** `session/manager.go`. On startup, walk `WALLE_SESSION_DIR`, group
by channel prefix, pick max timestamp per channel.

**Done when:** green.

---

### Phase 3 — Worker pool

**Goal:** bounded M processes, one per active channel, with drain-on-reuse.

**Tests (fake pi per slot):**
- `TestPool_Acquire_IdleChannel` — fresh channel → slot spawned, `prompt` works.
- `TestPool_Acquire_BusySameChannel` — second `Acquire` for the same channel
  while first is streaming returns the **same** slot (per-channel serialization
  is enforced here, not in the HTTP layer).
- `TestPool_Acquire_DifferentChannel_DrainsAndSwitches` — slot busy on chanA,
  `Acquire(chanB)`:
  - sends `abort` only after `WALLE_DRAIN_TIMEOUT` if no `agent_end` arrives;
    **test injects `agent_end` within timeout → no abort sent**.
  - then asserts `switch_session` to chanB's file was sent.
  - variant: `agent_end` never comes → `abort` is sent, then `switch_session`.
- `TestPool_AllSlotsBusy_BlocksThenAcquires` — M slots busy, `(M+1)`th `Acquire`
  blocks until a slot drains; assert ordering.
- `TestPool_Release_KeepsProcessAlive` — `Release(chan)` does not kill the
  process (it stays warm for reuse).
- `TestPool_Shutdown_DrainsAll` — `Shutdown(ctx)` sends `abort` to streaming
  slots, waits up to `DRAIN_TIMEOUT`, kills processes, returns.

**Implement:** `pool/pool.go`. A slot = `{ rpc.Client; channel ChannelID; busy
bool; mu sync.Mutex }`. `Pool` keeps `map[ChannelID]*slot` plus a
free-list/semaphore of size M. `Acquire(channel)`:
1. If a slot for `channel` exists and is free → reuse.
2. If a slot exists but is busy → block until it frees (per-channel serialization).
3. Else acquire a free slot from the semaphore (possibly evict LRU free slot's
   process by killing it — cheap, transcript on disk); spawn `pi`,
   `switch_session` to the channel's current file.

LRU eviction only kills *idle* processes; a busy process is never killed, only
drained.

**Done when:** green; pool semantics documented in code.

---

### Phase 4 — HTTP API

**Goal:** `/health` (no auth) and `/v1/prompt` (bearer, SSE stream).

**Tests (httptest + fake pi via pool):**
- `TestHealth_NoAuth_Returns200` — `GET /health` → 200, body `{"status":"ok"}`.
- `TestPrompt_NoToken_401` — `POST /v1/prompt` without `Authorization` → 401.
- `TestPrompt_WrongToken_401` — bearer mismatch → 401 (assert constant-time:
  timing of wrong-prefix token ≈ correct token within jitter).
- `TestPrompt_NoBody_400`.
- `TestPrompt_MissingChannel_400`.
- `TestPrompt_OK_StreamsSSE` — body `{"channel":"c1","message":"hi"}` → 200,
  `Content-Type: text/event-stream`, events: `agent_start`, `message_update`
  (text deltas), `agent_end`, `done`. Assert text deltas concatenate to the
  assistant text.
- `TestPrompt_BusyChannel_QueueUntilFree` — two concurrent POSTs to same
  channel: second waits for first's `done`, then streams its own. Assert second
  response status is 200 (not 503) and total wall-clock ≥ first's duration.
- `TestPrompt_BusyChannel_QueueTimeout_503` — second request exceeds
  `WALLE_HTTP_QUEUE_TIMEOUT` → 503 `{"error":"channel busy"}`.
- `TestPrompt_AbortViaClientDisconnect` — client closes the request → server
  sends `abort` to the slot and releases it.

**SSE event format (v1):**
```
event: agent_start
data: {}

event: delta
data: {"text":"Hello"}

event: agent_end
data: {}

event: done
data: {}
```
Errors mid-stream: `event: error\ndata: {"message":"..."}\n\n` then close.

**Auth:** `auth.go` — `func authorize(r *http.Request, token string) bool` using
`subtle.ConstantTimeCompare`.

**Done when:** green; `curl` smoke test in README.

---

### Phase 5 — Container wiring

**Goal:** `tini` as PID 1, gateway as entrypoint, build the Go binary inside the
image (multi-stage) so the runtime image stays small.

**Changes:**
- `Dockerfile`: add a `golang:24` builder stage that `go build -o /out/wall-e
  ./gateway`; runtime stage `apt-get install tini`, `ENTRYPOINT
  ["/usr/bin/tini","--"]`, `CMD ["/usr/local/bin/wall-e"]`. Keep the existing
  toolchain & `pi` install. Drop the interactive `tmux` default (still available
  via `docker exec`).
- `Makefile`: `docker` target no longer runs `tmux`; it runs the gateway with
  `WALLE_*` env passed through (`-e WALLE_TOKEN ...`). Add a `debug` target that
  execs `tmux` for manual TUI.
- `static/APPEND_SYSTEM.md`: note that the agent may be driven headlessly via
  RPC; keep existing environment notes.

**Tests (manual + a docker-level smoke):**
- `docker compose up` → `/health` returns 200.
- Send `curl -H "Authorization: Bearer $WALLE_TOKEN" -d
  '{"channel":"smoke","message":"say hi"}'
  http://localhost:$WALLE_PORT/v1/prompt` → SSE stream.
- `docker exec` into container, `pi` still works interactively.
- `docker stop` → gateway drains (within `WALLE_DRAIN_TIMEOUT`) and exits cleanly (tini reaps).

---

### Phase 6 — Telegram channel

**Goal:** bot reads messages, routes to pool, streams replies (edit-in-place).

**Tests:**
- `TestTelegram_OnMessage_AcquiresAndReplies`
- `TestTelegram_Streaming_EditsSingleMessage` — Telegram `editMessageText`,
  throttle to 1 edit/s (Telegram rate limit ~30 edits/min).
- `TestTelegram_MidStreamUserMessage_Steers`
- `TestTelegram_Over4096Chars_Splits`
- `TestTelegram_IgnoresSelf`

**Implement:** `chat/telegram.go` using `gopkg.in/telebot.v3` (or `go-telegram-bot-api`).

**Done when:** green + manual smoke.

---

### Phase 7 — Discord channel

**Goal:** bot reads messages, routes to pool, streams replies (edit-in-place).

**Tests (discordmock or a stub Gateway interface):**
- `TestDiscord_OnMessage_AcquiresAndReplies` — inject a fake message → assert
  `Acquire(channelID)` called, and when `agent_end` arrives the bot sends a
  message with the concatenated assistant text.
- `TestDiscord_Streaming_EditsSingleMessage` — for a long turn, the bot creates
  one message and edits it as deltas arrive (throttled to e.g. 1 edit/1s,
  Discord rate-limit aware).
- `TestDiscord_MidStreamUserMessage_Steers` — while streaming, a second message
  from the same channel issues `Steer`, not a new `Acquire`.
- `TestDiscord_Over2kChars_Splits` — assistant text > 2000 chars → split into
  multiple messages on `agent_end`.
- `TestDiscord_IgnoresSelf` — bot ignores its own messages.
- `TestDiscord_OnlyRespondsInAllowedChannels` — if
  `WALLE_DISCORD_ALLOWED_CHANNELS` set (optional), ignore others.

**Implement:** `chat/discord.go` using `discordgo`. Channel id = Discord channel
id (or thread id). The "reply" is a single message edited as deltas stream,
final state = full concatenated text, split if > 2000 chars.

**Done when:** green + manual smoke against a test Discord server.

---

## 7. Acceptance / validation checklist

Run through this end-to-end before merging v1:

1. `go test ./...` — all unit + fake-pi tests green.
2. `go test -tags=integration ./...` — with a real `pi` binary on `PATH`.
3. `docker build .` succeeds; image contains `tini`, `wall-e`, `pi`.
4. `docker run` with `WALLE_TOKEN`, `WALLE_DISCORD_TOKEN`,
   `WALLE_TELEGRAM_TOKEN` → gateway starts, logs `listening :8080`.
5. `GET /health` → 200 without auth.
6. Bad bearer → 401.
7. Valid `POST /v1/prompt` → SSE stream with deltas; final concatenated text
   matches `pi`'s actual response.
8. Two concurrent prompts to the same HTTP channel → serialized, both 200.
9. Discord message → bot replies, long replies edited-in-place then finalized.
10. Telegram message → same.
11. Mid-stream message in Discord → bot steers (verify via logs: `steer` command
    sent, `success:true`).
12. Pool saturation (send to `M+1` distinct channels concurrently) → `M+1`th
    waits, then runs; no process leak (`pgrep -f "pi --mode rpc"` ≤ M).
13. `docker stop` → graceful drain within `WALLE_DRAIN_TIMEOUT`; `docker exec
    pgrep pi` empty afterward.
14. `docker exec -it <c> tmux` → can still drive `pi` interactively for debugging.

## 8. Open questions / risks

- **`confirm` default = `true`.** Flipping to `false` only needs
  `WALLE_CONFIRM_DEFAULT=false`. Revisit if extensions start doing destructive
  things without asking.
- **Session-dir rebuild on startup** assumes filenames are the source of truth;
  if a user `switch_session`s to an arbitrary path not under
  `WALLE_SESSION_DIR`, the rebuild can't recover it after a gateway restart.
  Mitigation: constrain `switch_session` targets to live under the session dir;
  reject others. (Add to Phase 2 tests.)
- **Discord/Telegram rate limits on streaming edits.** v1 throttle = 1 edit/sec;
  may need backoff. Track in chat phase tests.
- **Multi-message assistant turns.** A single `agent_end` may carry multiple
  messages; v1 concatenates all text blocks. Confirm acceptable for chat UX.
- **No persistence of the channel→session map.** Rebuilt from dir on startup. If
  a channel's newest file was created by `new_session` but the gateway died
  before `get_state` resync, the rebuild picks it up by filename mtime/uuid
  anyway — verify with a test in Phase 2.
- **Go version.** Builder stage `golang:24` (matches Node 24 already in the
  image). Confirm available.

## 9. Out of scope, deferred

- Interactive extension-UI routing (buttons/inline keyboards) — v2.
- HTTP admin endpoints (evict/abort/list sessions) — v2.
- Webhook ingestion — v2.
- Per-channel config overrides — v2.
- Metrics/prometheus endpoint — v2 (health only for v1).
- Session compaction triggers from the gateway — rely on pi's auto-compaction for v1.
