# Channels

A **channel** is a logical, platform-stable conversation: a Telegram chat, an HTTP client-chosen `channel` string, (later) a Discord channel. The gateway binds at most one live `pi --mode rpc` process to any active channel at a time, and routes that channel's messages to it.

## The channel contract

Every front-end (HTTP, Telegram, Discord) follows the same shape against the worker pool:

1. **Acquire** a slot for the channel (`pool.Acquire(ctx, channelID)`). The pool serializes same-channel access — a second Acquire for a busy channel blocks until the first Releases. Under capacity it spawns a fresh `pi`; at capacity it reuses the LRU idle slot (draining + `switch_session`).
2. **Drive** the slot: `Slot.Client().Prompt(ctx, message, steer=false)` to start a turn, then range `Slot.Events()` for `agent_start` / `message_update` (`text_delta`) / `agent_end`.
3. **Release** the slot (`pool.Release(channelID)`) when the turn is done. The process stays **warm** (not killed) for fast reuse.

## Mid-stream messages: steer, don't re-acquire

A chat user can send a second message while the agent is still streaming. Re-acquiring would block (per-channel serialization) until the first turn finishes — the wrong UX. Instead, the front-end keeps a small per-chat map of in-flight turns and forwards a mid-stream message as pi's `steer` command on the **same** slot:

```go
// mid-stream: the chat already holds a slot
slot.Client().Steer(ctx, message)   // NOT pool.Acquire again
```

This is the **one piece of per-channel state** a chat front-end owns; everything else (serialization, drain, eviction) lives in the pool.

## Channel identity

`pool.ChannelID` is a named `string` type (alias of `session.ChannelID`). Each front-end supplies its platform's stable id as a string:

| Front-end | ChannelID source |
|---|---|
| HTTP | client-chosen `channel` field in the JSON body |
| Telegram | the Telegram chat id, as a decimal string |
| Discord (planned) | the Discord channel id |

The ChannelID is also the prefix of the on-disk transcript filename — see [Sessions](../sessions).

```{toctree}
:maxdepth: 1

http
telegram
```
