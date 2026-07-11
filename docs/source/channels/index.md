# Channels

A **channel** is a logical, platform-stable conversation: a Telegram chat, an HTTP client-chosen `channel` string, (later) a Discord channel. The gateway binds at most one live `pi --mode rpc` process to any active channel at a time, and routes that channel's messages to it.

## The channel contract

Every front-end (HTTP, Telegram, Discord) routes normal user turns through the shared turn manager and delivery adapters:

1. **Submit** a typed channel and message to the turn manager.
2. If no turn is active for the channel, the manager acquires a pool slot, sends `prompt`, consumes `Slot.Events()`, and broadcasts events to subscribers.
3. If a turn is already active for the same channel, the manager sends pi `steer` (or `prompt` with `streamingBehavior=steer` for extension slash commands) instead of acquiring again.
4. Delivery adapters subscribe to the broadcast stream and decide how to render the assistant response: SSE for HTTP/CLI, edit-in-place messages for Telegram, etc.

Same-channel reuse stays **warm** (the process is not killed) for fast follow-up turns. Cross-channel reuse claims the LRU idle slot and respawns the process so runtime channel identity remains correct.

## Mid-stream messages: steer, don't re-acquire

A chat user or CLI/HTTP caller can send a second message while the agent is still streaming. Re-acquiring would block until the first turn finishes — the wrong UX. Instead, the shared turn manager forwards the message as pi's `steer` command on the **same** slot:

```go
// mid-stream: the channel already has an active turn
slot.Client().Steer(ctx, message)   // NOT pool.Acquire again
```

Because this active-turn state is shared, a cron/CLI prompt can steer an in-flight Telegram turn, and a human Telegram message can steer a turn originally started by `/v1/prompt`.

## Channel identity

`pool.ChannelID` is a named `string` type (alias of `session.ChannelID`). Each front-end supplies its platform's stable id as a string:

| Front-end | ChannelID source |
|---|---|
| HTTP | `channelType: "http"` plus client-chosen `channel` field in the JSON body |
| Telegram | `channelType: "telegram"` plus the Telegram chat id, as a decimal string |
| Discord (planned) | the Discord channel id |

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
```
