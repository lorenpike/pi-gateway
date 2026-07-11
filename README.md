# wall-e

A single Go binary (`wall-e`) that runs inside an Ubuntu container and exposes a
small HTTP API over a fixed pool of `pi --mode rpc` child processes. The gateway
translates between HTTP/chat-platform events and pi's JSONL RPC protocol.

## Run the gateway

The gateway is configured entirely via `WALLE_*` env vars. `WALLE_TOKEN` is
required (it is the HTTP bearer token); everything else has a sensible default.

```sh
# 1. Build the image and run the gateway (detached) on :6007.
export WALLE_TOKEN="$(openssl rand -hex 32)"
make docker

# 2. Open the local read-only session UI in a browser:
# http://localhost:6007/

# 3. Health check (no auth required).
curl http://localhost:6007/health
# {"status":"ok"}

# 4. Send a prompt (bearer auth, SSE stream).
curl -N -H "Authorization: Bearer $WALLE_TOKEN" \
     -d '{"channelType":"http","channel":"smoke","message":"say hi"}' \
     http://localhost:6007/v1/prompt
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

# 5. Stop (graceful drain within WALLE_DRAIN_TIMEOUT, default 30s).
make stop
```

## CLI

The binary has explicit subcommands:

```sh
wall-e run              # start the gateway server
wall-e --help           # print usage
wall-e msg <type:id>    # read a prompt from stdin and submit it
```

`wall-e msg` is intended for local automation and cron. It only talks to the
local gateway at `http://127.0.0.1:${WALLE_PORT:-6007}`, requires
`WALLE_TOKEN`, and streams assistant text deltas to stdout until the turn
completes.

Examples:

```sh
wall-e msg http:morning-digest < ~/prompts/morning.md

wall-e msg telegram:123456789 <<'EOF'
Scheduled task: summarize today's calendar and top priorities.
EOF
```

The container passes `OPENAI_API_KEY` / `OPENROUTER_API_KEY` through to pi, and
bind-mounts `build/auth.json` and `build/pi-settings.json` into `/opt/pi`. A
real model call requires those credentials to be valid.

The Docker image seeds `/opt/wall-e/SYSTEM.md`. Every spawned `pi --mode rpc`
process receives `--system-prompt /opt/wall-e/SYSTEM.md`. `/opt/pi/APPEND_SYSTEM.md`
is still loaded by pi as appended environment context.

Each pool worker `pi` process also receives `WALLE_CHANNEL=<type>:<id>` for the
channel it is currently serving, for example `telegram:123456789`. The pool keeps
same-channel reuse warm, but respawns a worker on cross-channel reuse so this env
var is never stale.

## Configuration

| Var | Required | Default | Notes |
|---|---|---|---|
| `WALLE_TOKEN` | yes | — | HTTP bearer token |
| `WALLE_PORT` | no | `6007` | HTTP listen port |
| `WALLE_POOL_SIZE` | no | `4` | max concurrent `pi` processes |
| `WALLE_DRAIN_TIMEOUT` | no | `30s` | drain grace on reuse/shutdown |
| `WALLE_HTTP_QUEUE_TIMEOUT` | no | `60s` | max wait to acquire/steer a prompt turn → 503 |
| `WALLE_SITE` | no | `/opt/wall-e/www` | static session-debug UI dir |
| `WALLE_SESSION_EXPORT_TIMEOUT` | no | `30s` | max time to export a session HTML file |
| `WALLE_SESSION_DIR` | no | `/home/wall-e/sessions` | transcript dir |
| `WALLE_PI_BIN` | no | `pi` | pi executable path |
| `WALLE_PROVIDER` | no | from pi settings | `--provider` |
| `WALLE_MODEL` | no | from pi settings | `--model` |
| `WALLE_CONFIRM_DEFAULT` | no | `true` | auto-answer `confirm` dialogs |
| `WALLE_LOG_LEVEL` | no | `info` | `debug`/`info`/`warn`/`error` |
| `WALLE_TELEGRAM_TOKEN` | no | — | Telegram bot token; if unset the Telegram front-end is skipped (HTTP still serves) |
| `WALLE_TELEGRAM_ALLOWED_CHATS` | no | — | comma-separated chat-id allowlist; unset = allow all |
| `WALLE_TELEGRAM_REGISTER_COMMANDS` | no | `true` | register Telegram command menu entries for pi RPC commands plus `/skill`, `/name`, `/session`, `/clone`, `/new`, `/compact`, `/abort` |

## Develop

```sh
make test           # go vet + go test ./...
RACE=1 make test    # with the race detector (uses MinGW gcc for cgo)
make debug          # throwaway tmux container for manual `pi` TUI access
```

The Go module lives under `src/` (`module wall-e`, stdlib-only). Packages:
`rpc/` (pi JSONL client), `session/` (channel→transcript map),
`pool/` (bounded worker pool), `httpapi/` (`/health`, `/v1/prompt` SSE,
static UI, session listing/export), `chat/` (Telegram front-end), `config/`
(env parsing), `main` (wiring).
