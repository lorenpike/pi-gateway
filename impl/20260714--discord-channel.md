# Discord channel parity — Implementation Plan

**Date:** 2026-07-14
**Status:** Implemented

## 1. Goal

Add a Discord front-end with the same gateway behavior currently provided by
Telegram:

- optional token-based startup and a channel allowlist;
- one stable pi session per Discord channel/thread;
- normal messages start turns and same-channel messages steer active turns;
- edit-in-place streaming previews, typing state, authoritative final delivery,
  and long-response chunking;
- inbound attachments saved through the existing file-first `media.Store`;
- direct HTTP/CLI prompting with `discord:<channel-id>`;
- direct text/file delivery through `wall-e send`;
- discovered pi commands plus the existing gateway-native session commands;
- bounded startup/shutdown and fake-API unit tests with no Discord network calls.

The current Telegram **code** is the parity baseline, including functionality
that its channel documentation has not completely caught up with yet (notably
inbound attachments, direct media sends, command discovery, and gateway-native
commands).

## 2. Parity contract

| Capability | Telegram today | Discord target |
|---|---|---|
| Stable identity | Telegram chat ID | Discord channel ID; a thread ID is its own channel |
| Session/WALLE_CHANNEL | `telegram:<chat-id>` | `discord:<channel-id>` |
| Access control | optional allowed-chat list | optional allowed-channel list |
| Inbound text | `message.text` / caption | `MESSAGE_CREATE.content` |
| Inbound files | Bot API download → `media.Store` | attachment CDN URL → `media.Store` |
| New turn | shared `turn.Manager.Submit` | same |
| Mid-turn input | `steer`; pi slash commands use prompt-steer | same |
| Streaming | temporary message + throttled edits | same for normal messages |
| Final delivery | authoritative fresh chunks, then delete preview | same for normal messages |
| Typing | refreshed chat action | refreshed typing indicator |
| Formatting | Markdown converted to Telegram HTML | Discord Markdown passed through natively |
| Commands | registered command menu + pi aliases + gateway commands | native application commands/interactions |
| HTTP prompt adapter | `channelType: telegram` | `channelType: discord` |
| Direct send adapter | text/photo/document | text/file attachment |
| Lifecycle | non-fatal frontend startup; bounded stop | same |

## 3. Discord API research

Official references reviewed:

- [Gateway](https://discord.com/developers/docs/events/gateway)
- [Gateway events](https://discord.com/developers/docs/events/gateway-events)
- [Message resource](https://discord.com/developers/docs/resources/message)
- [Application commands](https://discord.com/developers/docs/interactions/application-commands)
- [Receiving and responding to interactions](https://discord.com/developers/docs/interactions/receiving-and-responding)
- [API reference / uploading files](https://discord.com/developers/docs/reference#uploading-files)
- [Rate limits](https://discord.com/developers/docs/topics/rate-limits)
- [Permissions](https://discord.com/developers/docs/topics/permissions)

Relevant findings:

1. Discord delivers normal messages as `MESSAGE_CREATE` Gateway dispatches.
   Guild and DM events require the corresponding message intents.
2. `MESSAGE_CONTENT` is privileged. Without it, `content`, `attachments`,
   `embeds`, and components are empty for most guild messages. The bot must
   request this intent and the operator must enable it in the Developer Portal.
   DMs and messages mentioning the app have limited exemptions, but wall-e
   should not rely on those exemptions.
3. Gateway connections require heartbeat, reconnect, resume, sequence, and
   identify-rate-limit handling. Discord explicitly recommends a maintained
   client library rather than casually hand-rolling this lifecycle.
4. Discord snowflakes are up to 64 bits and are serialized as strings. Channel
   IDs must remain strings throughout wall-e; do not parse them as signed Go
   integers.
5. Message content is limited to 2,000 characters. Create Message currently has
   a 25 MiB maximum request size. File sends use `multipart/form-data` with
   `files[n]` and optional `payload_json`.
6. Attachments provide `filename`, `content_type`, `size`, `url`, and
   `proxy_url`. Signed CDN URLs can expire, so files must be downloaded before
   starting the pi turn.
7. Create, Edit, and Delete Message are the REST operations needed for the
   Telegram-style preview/final flow. Trigger Typing Indicator supplies the
   transient typing state.
8. REST limits are dynamic and bucketed. Clients must honor the rate-limit
   headers and `retry_after` on 429 responses rather than hard-coding route
   quotas. The documented authenticated global limit is 50 requests/second.
9. Discord uses Markdown natively. `allowed_mentions` must be set explicitly to
   prevent model-generated text such as `@everyone` or `<@id>` from causing
   notifications.
10. Native slash commands are Application Commands and arrive as
    `INTERACTION_CREATE`, not as ordinary messages. An interaction must receive
    its initial response/defer within 3 seconds; its token remains valid for 15
    minutes for edits and follow-ups.
11. Chat-input command names are 1–32 characters, descriptions are 1–100
    characters, and an app can have up to 100 global chat-input commands.
    String options may be up to 6,000 characters.
12. Guild commands update immediately and are useful during development, while
    global commands are the correct production equivalent of Telegram's bot
    command menu. This implementation will register global commands.
13. Required guild-channel permissions are at least `VIEW_CHANNEL` and
    `SEND_MESSAGES`. Replies/chunk grouping also need `READ_MESSAGE_HISTORY`,
    file sends need `ATTACH_FILES`, and threads need `SEND_MESSAGES_IN_THREADS`.

## 4. Client-library decision

Use `github.com/bwmarrin/discordgo`, pinned to a reviewed release (currently
`v0.29.0`), behind a small wall-e-owned interface.

Why:

- Go's standard library does not provide a WebSocket client.
- Discord Gateway recovery is substantially more than reading JSON from a
  socket: heartbeats, heartbeat ACK detection, resume, reconnect, close codes,
  identify concurrency, intents, and sequence tracking all matter.
- discordgo already handles Gateway lifecycle and REST rate-limit buckets and
  provides the message, interaction, command, and multipart methods wall-e
  needs.
- The original gateway plan already selected discordgo for Phase 7.

Before implementation, verify the pinned release's API version against
Discord's current supported-version table and run a real Gateway smoke test.
Discord's official reference currently lists API v9 and v10 as available;
discordgo v0.29.0 uses v9. If Discord removes v9 before merge, use a reviewed
v10-capable discordgo revision or switch the transport shim to a maintained v10
client without changing the wall-e bot logic.

This intentionally ends the repository's zero-third-party-Go-dependency
invariant. Update comments and docs that currently advertise stdlib-only Go.
The runtime remains statically buildable with `CGO_ENABLED=0`.

## 5. Configuration and setup

Add:

| Variable | Required | Default | Meaning |
|---|---|---|---|
| `WALLE_DISCORD_TOKEN` | no | — | Bot token; Discord is disabled when empty |
| `WALLE_DISCORD_ALLOWED_CHANNELS` | no | allow all | Comma-separated Discord channel/thread snowflakes |

Configuration shape:

```go
type ChatConfig struct {
    Telegram TelegramConfig
    Discord  DiscordConfig
}

type DiscordConfig struct {
    Token           string
    AllowedChannels []string
}
```

Parsing rules for the allowlist:

- trim whitespace and ignore empty elements;
- validate each element as a non-empty unsigned decimal snowflake;
- preserve the original normalized decimal string;
- deduplicate values;
- exact match only: allowing a parent channel does not implicitly allow every
  thread beneath it.

No guild ID is needed in channel identity because Discord channel and thread
snowflakes are globally unique. DMs use their DM channel snowflake.

Operator setup must document:

1. create an application and bot in the Discord Developer Portal;
2. enable the **Message Content Intent** on the Bot page;
3. install with `bot` and `applications.commands` scopes;
4. grant View Channel, Send Messages, Read Message History, Attach Files, and,
   where needed, Send Messages in Threads;
5. enable Developer Mode in Discord and use **Copy Channel ID** to build the
   allowlist;
6. keep the bot token only in `.env`/the deployment secret store.

## 6. Package and transport shape

Keep Discord-specific logic out of `main` and keep network calls mockable.
Suggested split:

```text
src/chat/
  discord.go              Discord bot/channel behavior
  discord_api.go          discordgo-backed Gateway + REST shim
  discord_commands.go     Application Command catalog and interaction parsing
  discord_test.go         fake API + fake pi tests
  commands.go             shared gateway-command metadata/execution (refactor)
```

Add an internal `DiscordAPI`/`DiscordTransport` boundary that exposes only what
wall-e uses:

```go
type DiscordHandlers struct {
    Ready             func(DiscordReady)
    MessageCreate     func(DiscordMessage)
    InteractionCreate func(DiscordInteraction)
}

type DiscordAPI interface {
    SetHandlers(DiscordHandlers)
    Open(context.Context) error
    Close() error

    BulkOverwriteGlobalCommands(context.Context, string, []DiscordCommand) error
    SendMessage(context.Context, DiscordSend) (DiscordMessage, error)
    EditMessage(context.Context, channelID, messageID, text string) error
    DeleteMessage(context.Context, channelID, messageID string) error
    TriggerTyping(context.Context, channelID string) error

    RespondInteraction(context.Context, DiscordInteraction, DiscordInteractionResponse) error
    EditInteractionResponse(context.Context, DiscordInteraction, string) error
    CreateInteractionFollowup(context.Context, DiscordInteraction, DiscordSend) error
}
```

The real shim translates these wall-e-owned values to discordgo values. Tests
inject a fake and call handlers without a Gateway connection. Avoid exposing
large discordgo structs throughout the bot logic.

`Open` registers handlers before connecting, requests only these intents:

```go
IntentsGuilds |
IntentsGuildMessages |
IntentsDirectMessages |
IntentMessageContent
```

The Ready event supplies both bot user ID and application ID. `Start` waits for
Ready with a bounded context, records those IDs, then registers commands.

Use a separately injectable attachment fetcher over `net/http`. It must require
HTTPS, stream the response into `media.Store`, close every body, reject non-2xx
responses, and never add the Discord bot Authorization header to CDN requests.

## 7. Inbound normal messages

For each `MESSAGE_CREATE`:

1. Ignore malformed events without an author/channel.
2. Ignore the bot's own user ID, all bot-authored messages, and webhook-authored
   messages to prevent feedback loops.
3. Enforce `WALLE_DISCORD_ALLOWED_CHANNELS` before downloads, command work, or
   pool acquisition.
4. Use `content` as prompt text.
5. Download every attachment in event order and save it through the shared
   `media.Store` using the Discord filename.
6. Build one prompt with `media.FormatAttachmentPrompt(content, files)`.
7. Ignore a message only when both content and saved attachments are empty.
8. Submit once through the shared turn manager.

A Discord message already contains all of its attachments, so Telegram's media
album debounce is not needed. All files must be downloaded before
`turn.Manager.Submit`; attachment pieces are never sent as later steers.

Attachment metadata is informative, not trusted:

- `filename` still goes through `media.Store` sanitization;
- detected MIME remains the stored default, with Discord `content_type` usable
  as metadata;
- errors are returned to the channel as a short warning and no pi turn starts;
- do not log CDN query strings, attachment contents, or tokens.

## 8. Turn routing and shared session identity

Use:

```go
func discordChannelID(channelID string) pool.ChannelID {
    return pool.ChannelID(session.NewChannelID("discord", channelID))
}
```

This produces transcript names such as:

```text
discord--123456789012345678--20260714T153012Z--<uuid>.jsonl
```

and worker context:

```text
WALLE_CHANNEL=discord:123456789012345678
```

Normal behavior remains entirely in `turn.Manager`:

- no active turn: acquire and prompt;
- active turn: `Steer` on the same slot;
- active pi application command: `Prompt(..., streamingBehavior="steer")`;
- Discord, Telegram, HTTP, and CLI all observe the same active-turn map.

A thread receives its own session because its `channel_id` is the thread ID.
All users in one allowed Discord channel share one pi conversation, matching a
Telegram group chat's semantics.

## 9. Normal-message response delivery

Mirror Telegram's current robust delivery flow:

1. Start a typing refresher as soon as a new turn is accepted.
2. On the first assistant text delta, send a temporary preview.
3. Coalesce further deltas and edit the preview at most once per second.
4. Stop refreshing typing after the preview becomes visible.
5. On completion, read `Subscription.FinalText`; do not trust the best-effort
   event subscription to contain every delta.
6. Send the authoritative response as fresh 2,000-character chunks.
7. Make later chunks replies to the first final chunk so a long response stays
   grouped, with `replied_user: false`.
8. Delete the temporary preview only after every final chunk succeeds.
9. If final delivery fails, retain and update the preview as a best-effort
   fallback, then attempt remaining chunks.
10. Use `(no response)` for an empty completed turn.

Chunk on rune boundaries while counting conservatively for Discord's content
limit (including non-BMP characters). Prefer a newline/whitespace boundary when
one is available near the limit, but never lose or duplicate source text.
Tests must assert that concatenating final chunks reproduces the authoritative
text exactly.

Discord renders Markdown itself, so do not run Telegram's HTML converter. Every
send/edit/follow-up must carry an empty `allowed_mentions.parse` list and
`replied_user: false`. This preserves visible Markdown/mention text without
allowing model output to ping users, roles, or everyone.

Call Trigger Typing immediately and approximately every 8 seconds until the
first visible preview or completion. The REST client, not a hard-coded delay,
handles Discord's bucket and 429 behavior.

Retain Telegram's five-minute no-event watchdog for a stuck pi process.

## 10. Application commands and interactions

### 10.1 Registration model

Discord slash commands are interactions, not text messages. Build a Discord
command registry from the same typed `rpc.Command` discovery used by Telegram.
Register globally with Bulk Overwrite after Ready supplies the application ID.
The Discord application is assumed to be dedicated to wall-e, so synchronizing
its complete chat-input command set is acceptable and removes stale aliases.

Registration is non-fatal:

- discovery failure → register gateway-native commands only;
- Discord registration failure → log and continue handling ordinary messages;
- cap the final set at 100 commands including native commands.

Use deterministic aliases:

- lowercase;
- replace runs outside `[a-z0-9_]` with `_` for consistent Telegram/Discord UX;
- trim underscores and cap at 32 characters;
- resolve collisions with `_2`, `_3`, etc.;
- reserve native command names first;
- normalize descriptions to one line, provide a fallback, and cap at 100
  characters.

As on Telegram, skills are exposed through one `/skill` command rather than one
command per skill. Non-skill pi extensions/templates get an optional string
option named `args`.

### 10.2 Native command shapes

Register the existing native set:

- `/skill [name] [args]`
- `/name [value]`
- `/session`
- `/clone`
- `/new`
- `/compact [instructions]`
- `/abort`

The underlying behavior must remain identical to Telegram:

- `/name` uses `set_session_name`;
- `/session` uses `get_state`;
- `/new` switches to a fresh wall-e typed session path;
- `/clone` clones, copies/retargets to a typed path, resyncs the manager, and
  removes the temporary pi-named source;
- `/compact` calls `compact`;
- `/abort` operates through `turn.Manager.Abort` and is allowed during a turn;
- other control commands return busy while a turn is active;
- `/skill` lists skills or rewrites to `/skill:<real-name> ...`.

To prevent parity drift, extract Telegram's current gateway command switch into
`chat/commands.go` as a platform-neutral executor that accepts a typed
`pool.ChannelID` and returns display text/errors. Keep platform registration,
argument parsing, and response rendering in each adapter. Existing Telegram
tests must stay green through this refactor.

### 10.3 Interaction timing and delivery

For every command interaction:

1. Validate the channel allowlist immediately.
2. If disallowed, send an immediate ephemeral denial so Discord does not show
   “application did not respond.”
3. If allowed, send `DEFERRED_CHANNEL_MESSAGE_WITH_SOURCE` before any pool,
   media, discovery, or RPC work and within Discord's 3-second deadline.
4. Execute the native command or rewrite the pi alias to its real slash text.
5. Edit the deferred original response with progress/final text.

For a pi command that starts a new turn, the deferred original response is the
streaming preview and canonical final response. Edit it at the normal throttle,
then finalize it from `FinalText`; send extra chunks as interaction follow-ups.
Do **not** send a fresh duplicate and delete the original interaction response.

For a pi command that prompt-steers an already active turn, the existing
channel delivery subscriber remains responsible for the assistant stream. Edit
the deferred interaction response to a short acknowledgement such as “Command
applied to the active response.” This prevents duplicate response streams while
still satisfying the interaction protocol.

The five-minute turn watchdog normally keeps work inside the 15-minute
interaction-token lifetime. If an interaction edit/follow-up fails because the
token expired, fall back to normal bot-authenticated channel messages and log a
sanitized warning.

## 11. HTTP/CLI prompt adapter

Make the Discord bot implement `httpapi.PromptAdapter`:

```go
func (b *DiscordBot) Prompt(ctx context.Context, channel, message string) (*turn.Subscription, error)
```

Behavior matches Telegram:

- validate the channel snowflake and allowlist;
- target `session.NewChannelID("discord", channel)`;
- start or steer through the shared turn manager;
- on a newly started turn, pre-attach a Discord delivery subscription before
  prompting, while returning a separate subscription to the HTTP/SSE caller;
- do not echo the injected user prompt into Discord;
- deliver the assistant response to Discord and stream the same turn to HTTP;
- a disconnecting SSE caller only detaches; it does not abort Discord delivery.

This enables:

```sh
wall-e msg discord:123456789012345678 <<'EOF'
Run the scheduled channel summary.
EOF
```

and sets the worker's `WALLE_CHANNEL` to the same typed address.

## 12. Direct text and media send adapter

Make the bot implement `httpapi.SendAdapter` so the existing CLI works without
core API changes:

```sh
wall-e send discord:123456789012345678 "hello"
wall-e send --media discord:123456789012345678 /home/wall-e/report.pdf \
  --caption "Here is the report"
```

Rules:

- validate the snowflake and enforce the same allowlist;
- send text directly without creating a pi turn;
- chunk text over 2,000 characters;
- send a local file with multipart Create Message;
- use `caption` as the file message's content;
- if a caption exceeds 2,000 characters, send leading text chunks first and
  attach the file to the final legal chunk (or send the file separately if
  needed);
- open/close the file per request and return Discord API errors to `/v1/send`;
- let Discord reject unsupported size/type/permission combinations rather than
  duplicating server limits that vary over time;
- return one `SentItem` for every delivered text chunk/file;
- disable allowed mentions on every direct send too.

There is no Discord-specific “photo then document fallback”: Discord's generic
message attachment mechanism handles images and documents through the same API.

## 13. Lifecycle and wiring

`DiscordBot.Start`:

1. install Ready, Message Create, and Interaction Create handlers;
2. configure the minimal intents;
3. open the Gateway connection;
4. wait for Ready and record bot/application IDs;
5. discover and register commands (registration errors are non-fatal);
6. return once the frontend is usable.

`DiscordBot.Stop`:

1. mark the bot stopping so new callbacks are ignored;
2. close the Gateway session to stop reconnects/events;
3. cancel typing/stream contexts;
4. wait for tracked handler/turn goroutines, bounded by the supplied shutdown
   context.

`main.run`:

- create Discord when `WALLE_DISCORD_TOKEN` is set;
- treat constructor/Open failures as non-fatal, like Telegram;
- append a successfully started bot to `frontends`;
- register it under `cfg.HTTP.PromptAdapters["discord"]` and
  `cfg.HTTP.SendAdapters["discord"]`;
- stop it concurrently with Telegram, HTTP, and pool shutdown;
- consider caching one pi command-discovery result so enabling both chat
  frontends does not unnecessarily launch two no-session pi processes.

Update `Makefile` to forward both Discord environment variables into the
container.

## 14. File changes

Expected changes:

- `src/go.mod`, new `src/go.sum`
  - add the pinned Discord client and transitive dependencies.
- `src/config/config.go`, `src/config/config_test.go`
  - Discord token/allowlist parsing.
- `src/chat/commands.go`
  - shared native command metadata and platform-neutral execution.
- `src/chat/telegram_commands.go`, `src/chat/chat.go`
  - use the shared executor without changing Telegram behavior.
- `src/chat/discord.go`
  - bot behavior, turn routing, streaming, PromptAdapter, SendAdapter.
- `src/chat/discord_api.go`
  - discordgo Gateway/REST translation.
- `src/chat/discord_commands.go`
  - aliases, global command definitions, interaction parsing.
- `src/chat/discord_test.go`
  - fake Discord API and parity tests.
- `src/main.go`, `src/main_test.go`
  - frontend wiring and env coverage.
- `Makefile`
  - forward Discord env vars.
- `README.md`, `docs/source/environment.md`, `docs/source/channels/index.md`,
  `docs/source/index.md`
  - remove “later Discord”/stdlib-only wording and document typed addressing.
- `docs/source/channels/discord.md`
  - setup, permissions, intents, allowlist, commands, attachments, and limits.
- `docs/source/channels/telegram.md`
  - separately correct stale “non-text messages are ignored” wording so the
    documented parity baseline matches the existing media implementation.

## 15. Test plan

### 15.1 Config

- Discord unset → disabled and nil allowlist.
- Token and comma-separated snowflakes parse.
- Whitespace/empty elements and duplicates normalize.
- Invalid, signed, or non-decimal IDs fail with the variable name in the error.

### 15.2 Message handling and turns

- Incoming allowed message acquires `discord--<channel>` and prompts pi.
- DM and guild messages use the same channel-ID rule.
- A thread uses its thread ID, not parent channel ID.
- Same-channel mid-stream message issues `steer`, not another acquire/prompt.
- Different channels remain independent.
- Self, bot, webhook, empty, and disallowed messages do no work.
- Discord and HTTP/CLI injection share active-turn steering.

### 15.3 Streaming

- Typing starts while waiting and stops after first preview/completion.
- First delta creates one preview; later deltas produce throttled/coalesced edits.
- Completion uses authoritative `FinalText` even when preview events overflow.
- Fresh final succeeds before preview deletion.
- Final-send failure retains/updates preview.
- More than 2,000 characters splits safely and concatenates exactly.
- Emoji/non-BMP boundaries do not produce invalid UTF-8 or an over-limit chunk.
- All sends and edits suppress allowed mentions.
- Idle watchdog exits a stuck subscription.

### 15.4 Media and direct send

- Multiple inbound attachments are downloaded in order, saved under
  `${WALLE_SESSION_DIR}/media`, and submitted in one formatted prompt.
- Attachment-only messages start a turn.
- Filename sanitization is inherited from `media.Store`.
- Download non-2xx, cancellation, and close errors do not start a turn.
- Direct text and file sends return correct `SentItem` values.
- Long text/captions chunk correctly.
- Disallowed Prompt and Send calls fail before Discord REST work.

### 15.5 Commands/interactions

- Startup registers native and discovered pi commands.
- Skill commands use `/skill name args`, not one command per skill.
- Invalid names sanitize; collisions suffix; names cap at 32; descriptions cap
  at 100; registration caps at 100 total.
- Discovery/registration failures are non-fatal.
- Every allowed interaction is deferred before blocking work.
- Disallowed interactions receive an immediate ephemeral denial.
- Pi alias and skill invocations rewrite to the real pi slash command.
- Active pi commands use prompt-steer and produce one acknowledgement, not a
  duplicate stream.
- `/name`, `/session`, `/new`, `/clone`, `/compact`, and `/abort` match Telegram
  RPC/session behavior.
- New-turn interactions edit/finalize the original response and use follow-ups
  for extra chunks.
- Expired interaction response falls back to normal channel send.

### 15.6 Lifecycle and regression

- Start opens the Gateway, waits for Ready, and records identity.
- Bad Open/Ready timeout returns a startup error without hanging.
- Stop closes Gateway and drains handlers within context.
- Telegram tests remain unchanged/green after shared-command extraction.
- `go test ./...`, `go vet ./...`, and `go test -race ./...` pass.

## 16. Implementation phases

1. **Dependency, config, and interfaces**
   - Pin the reviewed Discord library.
   - Add config parsing/tests.
   - Define wall-e-owned Discord transport values and fake API.

2. **Shared command executor refactor**
   - Extract gateway-native command execution from Telegram.
   - Keep all Telegram tests green before adding Discord behavior.

3. **Core Discord text channel**
   - Add Ready/Message Create handling, allowlist, identity, turns, typing,
     preview edits, authoritative final chunks, and lifecycle tests.

4. **Media and direct send**
   - Add attachment fetching/storage.
   - Implement PromptAdapter and SendAdapter.
   - Add media, allowlist, HTTP/CLI cross-frontend tests.

5. **Application commands**
   - Build/register the command catalog.
   - Handle deferral, native commands, pi rewrites, active prompt-steer, and
     interaction streaming/follow-ups.

6. **Main/container/docs wiring**
   - Register the frontend/adapters in `main.run`.
   - Forward env vars in `Makefile`.
   - Add Discord setup docs and update channel/index/env docs.

7. **Manual smoke**
   - Test DM, guild text channel, and thread.
   - Test text, image, PDF, voice attachment, long response, slash command,
     active-turn steer, `wall-e msg`, and `wall-e send`.
   - Revoke channel permissions temporarily and verify failures are surfaced
     without retry storms.
   - Stop the container during a turn and confirm bounded shutdown.

## 17. Acceptance checklist

- [x] Gateway requests only the documented intents (fake lifecycle validated;
      live operator smoke remains outstanding).
- [x] Ordinary text and attachment messages work in an allowed channel.
- [x] Disallowed channels never acquire pi or download files.
- [x] Session files and `WALLE_CHANNEL` use `discord:<channel-id>`.
- [x] Same-channel input steers an active response.
- [x] Streaming preview, authoritative final, typing, chunking, and failure
      fallback are implemented and tested.
- [x] Output cannot trigger Discord mentions.
- [x] Native and discovered slash commands are registered and executable.
- [x] Interactions are synchronously acknowledged before blocking work.
- [x] `/new` and `/clone` use the shared typed-session implementation.
- [x] `wall-e msg discord:<id>` streams SSE and delivers to Discord.
- [x] `wall-e send discord:<id>` sends text/files without a pi turn.
- [x] REST rate limits are delegated to discordgo and wall-e adds no 429/403
      retry loop.
- [x] Telegram behavior and tests do not regress.
- [x] Unit, vet, race, docs, and diff checks pass.
- [ ] Real Discord DM/guild/thread smoke test (operator follow-up; no secret was
      used during this implementation).

## 18. Non-goals for this change

- Voice-channel connections or live audio streaming.
- Reactions, buttons, modals, polls, or exposing pi extension UI in Discord.
- Message edit/delete synchronization back into pi after submission.
- Role/user allowlists; access control remains channel-based like Telegram.
- Per-guild command catalogs or localization.
- Automatic parent-channel allowlisting of threads.
- Sharding for very large bot deployments; discordgo can be revisited if the
  bot approaches Discord's sharding threshold.
- Persisting/replaying missed messages outside normal Gateway resume behavior.

## 19. Implementation log

Implemented on 2026-07-14.

### Files changed

- `src/chat/discord.go` — Discord lifecycle, allowlist enforcement, inbound
  attachments, turn routing, typing/preview/final delivery, UTF-16-safe
  chunking, HTTP prompt/direct-send adapters, and interaction execution.
- `src/chat/discord_api.go` — wall-e-owned transport values/interface and the
  `discordgo` Gateway/REST implementation.
- `src/chat/discord_commands.go` — deterministic application-command aliases,
  native command shapes, skill lookup, descriptions, and registration cap.
- `src/chat/discord_test.go` — network-free fake Discord API/fetcher and fake-pi
  tests for lifecycle, intents, identity, filters, steering, streaming,
  fallback, chunking, mention suppression, attachments, direct sends,
  commands, and interactions.
- `src/chat/commands.go`, `src/chat/chat.go`,
  `src/chat/telegram_commands.go` — shared platform-neutral native command
  executor and Telegram migration to it without changing Telegram behavior.
- `src/chat/telegram.go`, `src/httpapi/server.go`, `src/media/media.go` — stale
  transport/package comments corrected.
- `src/config/config.go`, `src/config/config_test.go` — Discord token and exact
  decimal-string snowflake allowlist parsing, normalization, deduplication, and
  validation tests.
- `src/main.go`, `src/main_test.go` — cached cross-frontend command discovery,
  Discord lifecycle and HTTP adapter wiring, an injectable main-level Discord
  constructor, early-failure cleanup, and Discord HTTP/CLI wiring tests.
- `src/pool/pool.go` — captured each forwarding client before launching its
  goroutine, fixing a pre-existing cross-channel respawn race exposed by the
  required full race run.
- `src/go.mod`, `src/go.sum` — pinned `github.com/bwmarrin/discordgo v0.29.0`
  and its transitive dependencies.
- `Makefile`, `Dockerfile`, `README.md` — Discord environment forwarding,
  dependency/static-build wording, and Discord CLI examples.
- `docs/source/channels/discord.md`, `docs/source/channels/index.md`,
  `docs/source/channels/telegram.md`, `docs/source/environment.md`,
  `docs/source/index.md`, `docs/source/sessions.md` — implemented setup and
  behavior, permissions/intents, typed identity, attachments, commands,
  delivery, CLI use, and removal of stale “later/planned” wording.

### Important decisions and deviations

- Rechecked Discord's official Gateway, Gateway events, Message, Application
  Commands, Interactions, Permissions, API/uploading-files, and Rate Limits
  references on 2026-07-14. The official API-version table still marks v9 and
  v10 available. The pinned discordgo v0.29.0 source sets `APIVersion = "9"`,
  so it remains on a supported version; its rate-limit buckets and 429 handling
  stay behind wall-e's `DiscordAPI` seam.
- Gateway intents are exactly Guilds, Guild Messages, Direct Messages, and
  privileged Message Content. Snowflakes never pass through a signed integer.
- Every wall-e send/edit/reply/follow-up/interaction payload constructs an
  explicit empty allowed-mentions parse/users/roles set with
  `replied_user=false`.
- Inbound CDN fetching has a 30-second HTTP timeout and a 32 MiB streaming cap,
  independently injectable in tests. It accepts HTTPS only and has no bot-token
  input, preventing Authorization forwarding. Discord remains authoritative
  for variable outbound upload limits.
- Discord content chunks conservatively count non-BMP runes as two UTF-16 code
  units. Preferred whitespace boundaries are retained in a chunk rather than
  trimmed, so concatenation exactly reproduces authoritative text.
- Global command overwrite and discovery failures remain non-fatal. Both chat
  front-ends share one cached discovery call and one native command executor.
- A live Gateway smoke test was intentionally not run: implementation and
  validation used only fake Discord APIs/fake pi and did not read or use any
  configured bot secret. DM/guild/thread behavior is covered at the transport
  boundary and still needs operator smoke testing with a dedicated test bot.

### Validation results

- `cd src && go test ./... -count=1` — passed all packages.
- `cd src && go vet ./...` — passed with no diagnostics.
- `cd src && CC=x86_64-w64-mingw32-gcc go test -race -count=1 ./...` — passed
  all packages after fixing the existing pool forwarder/respawn race.
- `cd docs && uv run --with sphinx --with myst-parser --with furo python -m sphinx -W --keep-going -b html source build/walle-discord-check`
  — succeeded with warnings treated as errors.
- `git diff --check` — passed.
