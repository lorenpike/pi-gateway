# Environment variables

The gateway reads its configuration from environment variables. The standard
`make docker` target loads the ignored project-root `.env` file and forwards the
gateway, CLI, provider, and tool variables documented below when they are
present. The image supplies the container defaults. Secrets should remain in
`.env` or another private deployment secret store and must not be committed.

## Gateway configuration

These variables are read by `wall-e run`.

| Variable | Required | Default | Purpose |
|---|---|---|---|
| `WALLE_TOKEN` | yes | — | Bearer token for authenticated HTTP endpoints. The local `msg` and `send` commands use the same token. |
| `WALLE_PORT` | no | `6007` | HTTP listen port. |
| `WALLE_POOL_SIZE` | no | `4` | Maximum number of concurrent pi worker processes. Must be an integer greater than zero. |
| `WALLE_DRAIN_TIMEOUT` | no | `30s` | Grace period when draining a worker for reuse or shutting down. |
| `WALLE_HTTP_QUEUE_TIMEOUT` | no | `60s` | Maximum time an HTTP prompt waits to acquire or steer a turn before returning 503. |
| `WALLE_SESSION_DIR` | no | `/home/wall-e/sessions` | Transcript directory. Incoming media is stored in its `media` subdirectory. |
| `WALLE_SESSION_EXPORT_TIMEOUT` | no | `30s` | Maximum time allowed to export a session as HTML. |
| `WALLE_SITE` | no | `/opt/wall-e/www` | Directory containing the static session UI. |
| `WALLE_PROVIDER` | no | pi settings default | Passed to pi as `--provider`. Primarily useful when benchmarking another provider. |
| `WALLE_MODEL` | no | pi settings default | Passed to pi as `--model`. Primarily useful when benchmarking another model. |
| `WALLE_TELEGRAM_TOKEN` | no | — | Telegram bot token. Telegram is disabled when unset. |
| `WALLE_TELEGRAM_ALLOWED_CHATS` | no | allow all | Comma-separated signed Telegram chat IDs. Whitespace is ignored. |
| `WALLE_DISCORD_TOKEN` | no | — | Discord bot token. Discord is disabled when unset. |
| `WALLE_DISCORD_ALLOWED_CHANNELS` | no | allow all | Comma-separated Discord channel/thread snowflake strings. Whitespace and duplicates are ignored; signs and non-decimal values are rejected. |

Durations use Go duration syntax, for example `30s`, `5m`, or `1h`. Duration
values must be positive.

Telegram command registration and Discord global application-command
registration are enabled automatically. Confirmation dialogs from pi
extensions use pi's default gateway policy, which confirms them. The pi binary
is always resolved as `pi` from `PATH`.

## Local CLI configuration

These variables are read by `wall-e msg` and `wall-e send`.

| Variable | Required | Default | Purpose |
|---|---|---|---|
| `WALLE_TOKEN` | yes | — | Bearer token used to call the local gateway. |
| `WALLE_PORT` | no | `6007` | Local gateway port at `127.0.0.1`. |
| `WALLE_MSG_TIMEOUT` | no | `30m` | Overall timeout for a message or direct-send request. |

## Generated worker context

`WALLE_CHANNEL` is set by the worker pool; operators should not configure it.
Each pi process receives the typed address of the channel it currently serves:

```sh
echo "$WALLE_CHANNEL"
# telegram:123456789
# or discord:123456789012345678
```

The pool respawns pi when a slot changes channels so this value cannot become
stale.

## Provider and tool credentials

These variables are not parsed by the Go gateway. Docker places them in the
container, and pi or an installed tool consumes them.

| Variable | Required | Purpose |
|---|---|---|
| `OPENAI_API_KEY` | provider-dependent | OpenAI credential used by pi. It is also required directly by the benchmark evaluator. |
| `OPENROUTER_API_KEY` | provider-dependent | OpenRouter credential used by pi. |
| `BRAVE_API_KEY` | no | Required when the bundled Brave Search skill is used. |
| `CLOUDFLARE_TOKEN` | no | Starts the bundled Cloudflare tunnel. Without it, the supervised tunnel process exits cleanly and remains disabled. |

The normal container also bind-mounts `build/auth.json` and
`build/pi-settings.json` into `/opt/pi`. Provider authentication may therefore
come from pi's auth file rather than an API-key environment variable. Composio
uses its own browser authorization flow rather than a gateway environment
variable; see [Composio](composio).

## Container environment

| Variable | Image default | Purpose |
|---|---|---|
| `HOME` | `/home/wall-e` | Home directory for the gateway, pi, persisted user configuration, and projects. |
| `PI_CODING_AGENT_DIR` | `/opt/pi` | pi configuration, auth, appended system prompt, and skills directory. |
| `TZ` | image/system default | Container timezone. `make docker` forwards the host value when set. |
| `PATH` | image-defined | Resolves `pi` and other installed tools. Cron and at wrappers should set an explicit minimal path. |

Cron jobs do not inherit the full container or pi-worker environment. See
[Cron jobs](cron) for the recommended job environment and private env-file
pattern.

## Development and benchmark variables

| Variable | Default | Purpose |
|---|---|---|
| `RACE` | unset | Set to `1` when running `make test` to enable Go's race detector. |
| `WALLE_PROVIDER` | pi settings default | Forwarded by the benchmark harness to select the provider under test. |
| `WALLE_MODEL` | pi settings default | Forwarded by the benchmark harness to select the model under test. |

The benchmark creates its own `WALLE_TOKEN` and `WALLE_PORT`; they do not need
to be supplied by the caller.

### Image-build internals

These are fixed by build recipes rather than user configuration and should not
be placed in `.env`.

| Variable | Fixed value | Purpose |
|---|---|---|
| `CGO_ENABLED` | `0` | Builds a static gateway binary without cgo. |
| `GOOS` | `linux` | Targets the container operating system. |
| `COMPOSIO_INSTALL_DIR` | `/opt/composio` | Selects the installation directory while the image installs [Composio](composio). It is not a runtime variable. |
| `CC` | `x86_64-w64-mingw32-gcc` | Selected by `make test` only when `RACE=1` on Windows. |

## Environment inheritance and secrets

Pi workers inherit the gateway process environment, with `WALLE_CHANNEL` added
or replaced for the active channel. Consequently, credentials available in the
container are also available to pi and commands it runs. Only pass credentials
that the agent or its installed tools need, and never print or return raw
environment output containing secrets.
