# wall-e gateway

`wall-e` is a single Go binary that runs inside an Ubuntu container and exposes a small HTTP API plus chat-platform front-ends (Telegram, and later Discord), each fronting a **fixed pool of `pi --mode rpc` child processes**. The gateway translates between chat-platform events and pi's JSONL RPC protocol.

```{toctree}
:maxdepth: 2
:caption: Channels

channels/index
```

```{toctree}
:maxdepth: 2
:caption: Internals

sessions
supervision
at
cron
```

## At a glance

- **One live `pi` process per active chat.** A bounded pool (`WALLE_POOL_SIZE`, default 4) binds at most one process to any active channel; per-channel serialization is enforced **in the pool**, not in each front-end.
- **Channel identity is stable per platform** — Telegram chat id, HTTP client-chosen `channel` string, (later) Discord channel id — and maps to a pi session transcript file on disk.
- **Agents can discover the current channel** through `WALLE_CHANNEL=<type>:<id>`. Same-channel reuse stays warm; cross-channel slot reuse respawns `pi` so the env var is never stale.
- **Mid-stream messages steer**, they do not queue a new turn: a second message from chat or HTTP/CLI for a channel with an in-flight turn is forwarded as pi's `steer` command, not a new `Acquire`.
- **Stdlib-only Go.** The whole module (`module wall-e`, `go 1.26`) has zero third-party dependencies — even the Telegram Bot API is hand-rolled over `net/http`.

## Running it

```sh
make docker            # build + run the gateway container (tini PID 1 -> supervisord)
make stop              # docker stop (graceful drain within WALLE_DRAIN_TIMEOUT)
```

See the config table in `README.md` for the full `WALLE_*` env var list. The front-end-specific setup lives under [Channels](channels/index).

## Source layout

```
src/
├── main.go        wiring + signal/drain
├── rpc/           pi JSONL client (framing, commands, events, ext-ui policy)
├── session/       channel -> transcript map (rebuilt from disk on startup)
├── pool/          bounded worker pool (acquire/drain/evict, per-channel ser.)
├── turn/          shared active-turn coordinator (prompt vs steer, subscribers)
├── httpapi/       /health + /v1/prompt SSE
├── chat/          Telegram front-end (telegram.go is the net/http adapter)
└── config/        WALLE_* env parsing
```

## Further reading

- `archive/20260627--walle-gateway.md` — the full implementation plan.
- `archive/20260627--log.md` — per-phase implementation log (decisions, gotchas, iteration notes).
- `README.md` — quickstart smoke + config table.
