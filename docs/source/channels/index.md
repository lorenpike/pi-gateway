# Channels

A **channel** is a logical, platform-stable conversation: a Telegram chat, a Discord channel/thread/DM, or an HTTP client-chosen `channel` string. The gateway binds at most one live `pi --mode rpc` process to any active channel at a time, and routes that channel's messages to it.

## The channel contract

Every front-end (HTTP, Telegram, Discord) routes normal user turns through the shared turn manager and delivery adapters:

1. **Submit** a typed channel and message to the turn manager.
2. If no turn is active for the channel, the manager acquires a pool slot, sends `prompt`, consumes `Slot.Events()`, and broadcasts events to subscribers.
3. If a turn is already active for the same channel, the manager sends pi `steer` (or `prompt` with `streamingBehavior=steer` for extension slash commands) instead of acquiring again.
4. Delivery adapters subscribe to the broadcast stream according to one of two delivery contracts:
   - **raw streaming transports** such as HTTP/SSE expose assistant deltas and lifecycle events as they arrive;
   - **buffered client-facing channels** such as Telegram and Discord wait for the authoritative final text before delivering a reply.

The distinction is intentional. HTTP is an application-facing event transport used by clients such as the CLI, not a rendered chat surface. It must preserve the raw stream. User-facing chat adapters should share buffered behavior so gateway-level presentation controls can work consistently in every current and future client channel.

### Buffered `NO_REPLY` control

A buffered client-facing channel interprets a complete assistant response whose trimmed text is exactly `NO_REPLY` as a delivery control: stop the typing/activity indicator and send no assistant message. Matching is case-sensitive and whole-response only; `NO_REPLY` inside ordinary prose, Markdown, or a larger response is visible text.

This control applies only at buffered delivery. HTTP/SSE continues to expose the raw `NO_REPLY` text, direct `wall-e send` delivery is unaffected, and the underlying pi transcript retains the assistant output. If an HTTP prompt targets Telegram or Discord, the HTTP subscriber therefore sees the raw stream while the external chat adapter suppresses its buffered reply.

Telegram and Discord share the same authoritative-completion helper for this behavior. The implementation design and test requirements are recorded in [`impl/20260714--no-reply.md`](https://github.com/lorenpike/pi-gateway/blob/main/impl/20260714--no-reply.md).

Same-channel reuse stays **warm** (the process is not killed) for fast follow-up turns. Cross-channel reuse claims the LRU idle slot and respawns the process so runtime channel identity remains correct.

## Mid-stream messages: steer, don't re-acquire

A chat user or CLI/HTTP caller can send a second message while the agent is still streaming. Re-acquiring would block until the first turn finishes — the wrong UX. Instead, the shared turn manager forwards the message as pi's `steer` command on the **same** slot:

```go
// mid-stream: the channel already has an active turn
slot.Client().Steer(ctx, message)   // NOT pool.Acquire again
```

Because this active-turn state is shared, a cron/CLI prompt can steer an in-flight Telegram or Discord turn, and a human chat message can steer a turn originally started by `/v1/prompt`.

## Channel identity

`pool.ChannelID` is a named `string` type (alias of `session.ChannelID`). Each front-end supplies its platform's stable id as a string:

| Front-end | ChannelID source |
|---|---|
| HTTP | `channelType: "http"` plus client-chosen `channel` field in the JSON body |
| Telegram | `channelType: "telegram"` plus the Telegram chat id, as a decimal string |
| Discord | `channelType: "discord"` plus the Discord channel, thread, or DM channel snowflake string |

The ChannelID is also the prefix of the on-disk transcript filename — see [Sessions](../sessions).

## Runtime channel identity

Every `pi` process spawned by the pool gets a `WALLE_CHANNEL` environment variable with the current typed channel address:

```sh
echo "$WALLE_CHANNEL"
# telegram:123456789
```

This is the value agents should use when a user says "this chat" or asks to create automation that targets the current conversation.

Environment variables cannot be changed inside an already-running process, so the pool uses this rule:

- **same channel** → reuse the existing warm process;
- **different channel** → claim the idle slot but respawn its `pi` process with a new `WALLE_CHANNEL`, then `switch_session` to that channel's current transcript.

This preserves the important fast path for repeated messages in one chat while avoiding stale channel identity after cross-channel slot reuse.

```{toctree}
:maxdepth: 1

http
telegram
discord
```
