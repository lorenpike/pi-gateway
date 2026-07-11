# Telegram channel

The Telegram front-end (`chat/telegram.go`, hand-rolled over `net/http` — no `go-telegram-bot-api`/`telebot` dependency) reads messages via long-poll `getUpdates`, routes each chat to the worker pool, and streams replies by **editing a single message in place** (throttled to ~1 edit/sec).

## Setup

### 1. Create the bot

Talk to [@BotFather](https://t.me/BotFather):

```
/newbot
```

Pick a name and username; BotFather returns a token like `123456:ABC-DEF...`. Set it:

```sh
export WALLE_TELEGRAM_TOKEN="123456:ABC-DEF..."
```

If `WALLE_TELEGRAM_TOKEN` is **unset**, the gateway skips the Telegram front-end entirely and serves HTTP alone — the gateway logs `telegram: disabled (WALLE_TELEGRAM_TOKEN unset)`. A bad token or network failure at startup is **non-fatal**: it logs and continues (HTTP still serves).

### 2. (Optional) Restrict to specific chats

By default the bot responds in every chat it can see. To lock it down, set an allowlist of chat ids:

```sh
export WALLE_TELEGRAM_ALLOWED_CHATS="123456789,-100987654321"
```

- Comma-separated integers; whitespace is trimmed (`"42, -7 , 999"` works).
- DM chat ids are positive (the user's id); group/supergroup ids are **negative** (e.g. `-1001234567890`).
- Unset/empty = allow all.
- Messages from other chats are ignored before any pool Acquire.

### 3. Find a chat id

The chat id isn't shown in the Telegram UI. The easiest way is to ask the Bot API what it just received:

```sh
curl -s "https://api.telegram.org/bot$WALLE_TELEGRAM_TOKEN/getUpdates" \
  | jq '.result[].message.chat.id'
```

So the typical first-run flow is:

1. Start the gateway **without** `WALLE_TELEGRAM_ALLOWED_CHATS` (allow all).
2. Send the bot a message in each chat (DM / group) you want to permit.
3. Run the `getUpdates` curl above to grab each chat id.
4. Set `WALLE_TELEGRAM_ALLOWED_CHATS` and restart.

## Groups

For the bot to receive group messages:

- **Add the bot as a member** (or admin) of the group. Telegram only delivers group messages to bots that are members.
- **Privacy mode** — by default, bots in groups only see messages that @-mention them or are replies. To see every message, disable privacy mode via BotFather: `/setprivacy` → `Disable` (set **before** adding the bot to the group; re-add if you change it after).

## Reply behavior

For each incoming text message:

1. **Acquire** a slot for the chat (one live `pi` per Telegram chat).
2. On the **first** assistant text delta, send an initial message (so the user sees the reply start immediately).
3. As further deltas arrive, **edit that same message** in place, throttled to ~1 edit/sec (Telegram's rate limit is ~30 edits/min). Edits are coalesced: if deltas arrive faster than the throttle, only one edit per tick fires with the accumulated text so far.
4. Assistant Markdown is rendered through Telegram's `HTML` parse mode using a conservative stdlib converter (bold/italic/code blocks/links/headings/lists; unsupported Markdown remains escaped plain text).
5. On `agent_end`, **finalize**: a last edit with the full concatenated text. If the final text exceeds Telegram's 4096-char limit, the first chunk pins the existing message and the rest are sent as replies (split on rune boundaries so multi-byte text stays valid UTF-8).

Non-text messages are ignored in v1.

## Mid-stream messages steer

While a turn is streaming for chat X, a second message from chat X is forwarded as pi's `steer` command on the **same** slot — it does **not** start a new turn. Active-turn state is shared with HTTP/CLI prompting, so a `/v1/prompt` request targeting `channelType: "telegram"` can steer an in-flight human Telegram turn, and a human Telegram message can steer a Telegram turn originally started by HTTP/CLI automation.

## HTTP/CLI prompting

The HTTP prompt endpoint can target Telegram directly when the Telegram front-end is enabled:

```json
{"channelType":"telegram","channel":"123456789","message":"Scheduled task: ..."}
```

The injected user prompt is recorded in the pi transcript but is not echoed into Telegram as a user message. The assistant response is delivered to Telegram using the same edit-in-place behavior as a human-originated Telegram turn, and the HTTP caller also receives the response as SSE.

`WALLE_TELEGRAM_ALLOWED_CHATS` is enforced for HTTP/CLI prompts too; targeting a disallowed chat fails instead of acquiring a pool slot.

## Self-message suppression

The bot ignores its own messages (`from.id == bot.id`, where the bot id is learned from `getMe` at startup) to avoid feedback loops.

## Lifecycle

- `Start` calls `getMe` (validates the token, learns the bot id), then launches a `getUpdates` long-poll loop (30s timeout) in a goroutine. Each received update is dispatched to a turn goroutine so a slow turn doesn't block polling.
- The **offset** is advanced past the last `update_id`, so a restart doesn't replay old messages (in-memory only for v1; not persisted across gateway restarts).
- `Stop` cancels the poll loop and drains in-flight turns **bounded** by the shutdown grace context — it does not block forever on a stuck pi. main runs Stop concurrently with HTTP + pool shutdown.
- An **idle watchdog** (default 5 min) finalizes a turn that has received no events for that long, guarding against a stuck pi process (the pool's `Slot.Events()` is never closed — see the Phase 5 log).

## Config

| Var | Required | Default | Notes |
|---|---|---|---|
| `WALLE_TELEGRAM_TOKEN` | no | — | bot token; unset = front-end disabled |
| `WALLE_TELEGRAM_ALLOWED_CHATS` | no | — | comma-separated chat ids; unset = allow all |

## Limitations (v1)

- No persistence of the `getUpdates` offset across restarts — the Bot API confirms the offset once you call `getUpdates` again, so only messages that arrived after the last confirmed offset are redelivered.
- No inline keyboards / buttons (extension-UI dialogs are auto-answered by policy, not surfaced as keyboards — v2).
- The ~1 edit/sec throttle may need backoff under heavy load (tracked as a plan risk).
- Emoji/non-BMP chunking may occasionally allow one fewer chunk than the 4096-char limit permits (rune-safe splitting vs UTF-16 code units); acceptable for v1.
