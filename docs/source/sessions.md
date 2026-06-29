# Sessions

A **session** is a pi transcript: a JSONL file on disk holding the message history for one channel. The session manager (`session/manager.go`) is the durable mapping from a `ChannelID` to its *current* transcript file path.

## The map

`session.Manager` keeps `current map[ChannelID]string` (guarded by a `sync.RWMutex`) mapping each channel to its current session file. It is the single source of truth the pool consults when binding a slot to a channel: `Acquire` calls `switch_session` to point the pi process at the channel's current file.

Key operations:

- `Current(ch)` — returns the current path for `ch`, lazily generating a fresh one on first sight (re-checking under the write lock to avoid a duplicate-path race).
- `SetCurrent(ch, path)` / `ResyncFromState(ch, sessionFile)` — update the map; used after pi's `new_session` / `clone` / `switch_session` (the RPC client auto-resyncs via `get_state`).
- `ListKnownChannels()` — sorted list of known channel ids.

## File naming

Each transcript is named:

```
<channelId>--<unixSeconds>--<uuid>.jsonl
```

under `WALLE_SESSION_DIR` (default `/home/wall-e/sessions`). For example:

```
42--1719480000--a1b2c3d4e5f6a7b8.jsonl
```

- The channel id is **sanitized**: OS-unsafe chars (`/ \ : * ? " < > |`) are replaced with `_` and runs of `_` are collapsed, so the sanitized form never contains `--` (which would corrupt the rebuild parse). Empty / `.` / `..` map to `_`.
- `<unixSeconds>` is the creation time; `<uuid>` is 8 random bytes hex-encoded (16 chars), with a time-based fallback so generation never blocks. Uniqueness within a `(channel, second)` bucket is what matters for the rebuild tiebreak.

## Rebuild from disk on startup

There is **no sidecar persistence** in v1. On startup the manager walks `WALLE_SESSION_DIR`, groups files by their sanitized channel prefix, and picks the highest `(timestamp, uuid)` per channel:

```
TestManager_RebuildFromDir:
  chanA--1--u.jsonl
  chanA--2--u.jsonl   <- Current("chanA") picks this (newest ts)
  chanB--1--u.jsonl   <- Current("chanB")
  README.txt          <- ignored (doesn't match the regex)
```

The rebuild **replaces** the in-memory map: a stale in-memory entry with no file on disk is dropped; a file the manager never saw is picked up. This is why filenames are the source of truth — the map is rebuilt lazily from them.

## switch_session is constrained

A `switch_session` target **must** resolve inside `WALLE_SESSION_DIR` (after `EvalSymlinks` on the parent when it exists, with a prefix-wise fallback for not-yet-created files). Escapes via `..` or symlinks are rejected (`ErrPathOutsideSessionDir`) and the map is left untouched.

This is the §8 risk mitigation from the plan: without it, a `switch_session` to an arbitrary path outside the session dir couldn't be recovered by the on-startup rebuild (the rebuild only scans the session dir), so a gateway restart would lose the binding. Constraining targets to live under the session dir keeps the rebuild-on-startup invariant intact.

## Channel identity

`ChannelID` is a named `string` type, aliased between `session` and `pool`. Each front-end supplies its platform's stable id as a string:

| Front-end | ChannelID |
|---|---|
| HTTP | the `channel` field from the JSON body |
| Telegram | the Telegram chat id, decimal string |
| Discord (planned) | the Discord channel id |

Because the ChannelID is the filename prefix, a chat's transcripts are all grouped together on disk and survive gateway restarts.

## Config

| Var | Default | Notes |
|---|---|---|
| `WALLE_SESSION_DIR` | `/home/wall-e/sessions` | transcript dir; created on startup |

The dir is `MkdirAll`'d by `session.New`; an empty/corrupt dir just means no channels are known yet — they generate fresh paths on first sight.
