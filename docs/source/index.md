# wall-e gateway

`wall-e` is a single Go binary that runs inside an Ubuntu container and exposes a small HTTP API plus Telegram and Discord front-ends, each fronting a **fixed pool of `pi --mode rpc` child processes**. The gateway translates between chat-platform events and pi's JSONL RPC protocol.

```{toctree}
:maxdepth: 2
:caption: Configuration

environment
```

```{toctree}
:maxdepth: 2
:caption: Channels

channels/index
```

```{toctree}
:maxdepth: 2
:caption: Integrations

composio
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
- **Channel identity is stable per platform** — Telegram chat id, Discord channel/thread id, or HTTP client-chosen `channel` string — and maps to a pi session transcript file on disk.
- **Agents can discover the current channel** through `WALLE_CHANNEL=<type>:<id>`. Same-channel reuse stays warm; cross-channel slot reuse respawns `pi` so the env var is never stale.
- **Mid-stream messages steer**, they do not queue a new turn: a second message from chat or HTTP/CLI for a channel with an in-flight turn is forwarded as pi's `steer` command, not a new `Acquire`.
- **Small transport boundary.** Telegram is hand-rolled over `net/http`; Discord uses pinned `discordgo` for Gateway recovery and REST rate-limit handling behind a wall-e-owned fakeable interface. The binary remains statically buildable with `CGO_ENABLED=0`.

## Running it

```sh
make docker            # build + run the gateway container (tini PID 1 -> supervisord)
make stop              # docker stop (graceful drain within WALLE_DRAIN_TIMEOUT)
```

See [Environment variables](environment) for gateway, CLI, credential, container, and benchmark configuration. Front-end-specific setup lives under [Channels](channels/index). To connect email, calendars, source control, messaging, and other services, see [Composio](composio).

## Source layout

```
src/
├── main.go        wiring + signal/drain
├── rpc/           pi JSONL client (framing, commands, events, ext-ui policy)
├── session/       channel -> transcript map (rebuilt from disk on startup)
├── pool/          bounded worker pool (acquire/drain/evict, per-channel ser.)
├── turn/          shared active-turn coordinator (prompt vs steer, subscribers)
├── httpapi/       /health + /v1/prompt SSE
├── chat/          Telegram + Discord front-ends and transport seams
└── config/        WALLE_* env parsing
```

## Further reading

- `archive/20260627--walle-gateway.md` — the full implementation plan.
- `archive/20260627--log.md` — per-phase implementation log (decisions, gotchas, iteration notes).
- `README.md` — quickstart and development commands.
