# wall-e

A single Go binary (`wall-e`) that runs inside an Ubuntu container and exposes a
small HTTP API over a fixed pool of `pi --mode rpc` child processes. The gateway
translates between HTTP/chat-platform events and pi's JSONL RPC protocol.

## Run the gateway

The gateway is configured entirely via `WALLE_*` env vars. `WALLE_TOKEN` is
required (it is the HTTP bearer token); everything else has a sensible default.

```sh
# 1. Build the image and run the gateway (detached) on :8080.
export WALLE_TOKEN="$(openssl rand -hex 32)"
make docker

# 2. Health check (no auth required).
curl http://localhost:8080/health
# {"status":"ok"}

# 3. Send a prompt (bearer auth, SSE stream).
curl -N -H "Authorization: Bearer $WALLE_TOKEN" \
     -d '{"channel":"smoke","message":"say hi"}' \
     http://localhost:8080/v1/prompt
# event: agent_start
# data: {}
#
# event: delta
# data: {"text":"..."}
# ...
# event: agent_end
# data: {}
#
# event: done
# data: {}

# 4. Stop (graceful drain within WALLE_DRAIN_TIMEOUT, default 30s).
make stop
```

The container passes `OPENAI_API_KEY` / `OPENROUTER_API_KEY` through to pi, and
bind-mounts `build/auth.json` and `build/pi-settings.json` into `/opt/pi`. A
real model call requires those credentials to be valid.

The Docker image seeds `/home/wall-e/SOUL.md`. Since the container workdir is
`/home/wall-e`, every spawned `pi --mode rpc` process receives
`--system-prompt SOUL.md`. `/opt/pi/APPEND_SYSTEM.md` is still loaded by pi as
appended environment context.

## Configuration

| Var | Required | Default | Notes |
|---|---|---|---|
| `WALLE_TOKEN` | yes | — | HTTP bearer token |
| `WALLE_PORT` | no | `8080` | HTTP listen port |
| `WALLE_POOL_SIZE` | no | `4` | max concurrent `pi` processes |
| `WALLE_DRAIN_TIMEOUT` | no | `30s` | drain grace on reuse/shutdown |
| `WALLE_HTTP_QUEUE_TIMEOUT` | no | `60s` | max wait on a busy channel → 503 |
| `WALLE_SESSION_DIR` | no | `/home/wall-e/sessions` | transcript dir |
| `WALLE_PI_BIN` | no | `pi` | pi executable path |
| `WALLE_PROVIDER` | no | from pi settings | `--provider` |
| `WALLE_MODEL` | no | from pi settings | `--model` |
| `WALLE_CONFIRM_DEFAULT` | no | `true` | auto-answer `confirm` dialogs |
| `WALLE_LOG_LEVEL` | no | `info` | `debug`/`info`/`warn`/`error` |
| `WALLE_TELEGRAM_TOKEN` | no | — | Telegram bot token; if unset the Telegram front-end is skipped (HTTP still serves) |
| `WALLE_TELEGRAM_ALLOWED_CHATS` | no | — | comma-separated chat-id allowlist; unset = allow all |

## Develop

```sh
make test           # go vet + go test ./...
RACE=1 make test    # with the race detector (uses MinGW gcc for cgo)
make debug          # throwaway tmux container for manual `pi` TUI access
```

The Go module lives under `src/` (`module wall-e`, stdlib-only). Packages:
`rpc/` (pi JSONL client), `session/` (channel→transcript map),
`pool/` (bounded worker pool), `httpapi/` (`/health` + `/v1/prompt` SSE),
`chat/` (Telegram front-end), `config/` (env parsing), `main` (wiring).
