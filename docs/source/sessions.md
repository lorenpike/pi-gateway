# Sessions

A **session** is a pi transcript: a JSONL file on disk holding the message history for one channel. The session manager (`session/manager.go`) is the durable mapping from a typed `ChannelID` to its *current* transcript file path.

## The map

`session.Manager` keeps `current map[ChannelID]string` (guarded by a `sync.RWMutex`) mapping each typed channel to its current session file. It is the single source of truth the pool consults when binding a slot to a channel: `Acquire` calls `switch_session` to point the pi process at the channel's current file.

Key operations:

- `Current(ch)` — returns the current path for `ch`, lazily generating a fresh one on first sight.
- `SetCurrent(ch, path)` / `ResyncFromState(ch, sessionFile)` — update the map.
- `ListKnownChannels()` — sorted list of known typed channel ids.
- `ListSessionFiles()` / `ResolveSessionKey(key)` — read-only introspection for the local session UI/export endpoints.

## File naming

Each transcript is named:

```text
<channel-type>--<channel-id>--<YYYYMMDDTHHMMSSZ>--<uuid>.jsonl
```

under `WALLE_SESSION_DIR` (default `/home/wall-e/sessions`). For example:

```text
http--smoke--20260702T153012Z--a1b2c3d4e5f6a7b8.jsonl
telegram--123456789--20260702T153055Z--d9876cafe1234567.jsonl
```

- `channel-type` is `http`, `telegram`, and future `discord`, `slack`, etc.
- `channel-id` is the platform/channel identifier, sanitized for filenames.
- The datestamp is UTC and lexicographically sortable.
- `<uuid>` is 8 random bytes hex-encoded (16 chars), with a time-based fallback so generation never blocks.
- Filename components are sanitized so they do not contain the `--` separator.

## Rebuild from disk on startup

There is no sidecar persistence. On startup the manager walks `WALLE_SESSION_DIR`, groups files by typed channel (`channel-type` + `channel-id`), and picks the newest `(datestamp, uuid)` per channel:

```text
http--chanA--20260702T153012Z--aaaa.jsonl
http--chanA--20260702T153013Z--bbbb.jsonl   <- Current(http/chanA)
telegram--chanB--20260702T153014Z--cccc.jsonl
README.txt                                  <- ignored
```

The rebuild replaces the in-memory map: a stale in-memory entry with no file on disk is dropped; a file the manager never saw is picked up.

## switch_session is constrained

A `switch_session` target must resolve inside `WALLE_SESSION_DIR` (after `EvalSymlinks` on the parent when it exists, with a prefix-wise fallback for not-yet-created files). Escapes via `..` or symlinks are rejected (`ErrPathOutsideSessionDir`) and the map is left untouched.

## Channel identity

Construct typed ids with `session.NewChannelID(channelType, channelID)`:

| Front-end | channel type | channel id |
|---|---|---|
| HTTP | `http` | the `channel` field from the JSON body |
| Telegram | `telegram` | the Telegram chat id, decimal string |
| Discord (planned) | `discord` | the Discord channel id |

The local debug UI displays channel type and session date, but not raw channel id or uuid.

## Config

| Var | Default | Notes |
|---|---|---|
| `WALLE_SESSION_DIR` | `/home/wall-e/sessions` | transcript dir; created on startup |

The dir is `MkdirAll`'d by `session.New`; an empty/corrupt dir just means no channels are known yet — they generate fresh paths on first sight.
