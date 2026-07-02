# Telegram slash-command registration for pi — Implementation Plan

**Date:** 2026-07-02
**Status:** Planning

## 1. Goal

Make the Telegram front-end expose pi's available slash commands in Telegram's
command menu and route command invocations correctly back into `pi --mode rpc`.

This is specifically about pi commands available in RPC mode: extension
commands, prompt templates, and skills. We should not advertise pi's interactive
TUI-only built-ins as if they worked over RPC.

## 2. Research notes

### Pi docs findings

From pi `README.md` and `docs/rpc.md` / `docs/prompt-templates.md` /
`docs/extensions.md`:

- In interactive mode, `/` opens slash commands.
- Extension commands are registered by extensions with `pi.registerCommand()`.
- Prompt templates expand as `/template-name`; the filename is the command name.
- Skills are invoked as `/skill:name`.
- In RPC mode, `prompt` expands skills and prompt templates before sending to
  the agent.
- In RPC mode, extension commands sent via `prompt` execute immediately, even
  while the agent is streaming, if `streamingBehavior` is supplied.
- `steer` and `follow_up` expand skills/templates, but extension commands are
  **not allowed** there; docs say to use `prompt` instead.
- RPC exposes `get_commands`, returning extension commands, prompt templates,
  and skills, with `name`, `description`, `source`, and optional path/location.
- `get_commands` deliberately excludes built-in interactive commands such as
  `/model`, `/settings`, `/hotkeys`, etc. Those are TUI-only and would not run
  if sent through `prompt`.

### Current wall-e implementation findings

- `src/rpc/client.go` already has `GetCommands(ctx)` for raw RPC
  `get_commands` data, but no typed helper and no caller.
- `src/chat/telegram.go` only wraps four Telegram Bot API methods:
  `getMe`, `getUpdates`, `sendMessage`, and `editMessageText`.
- `Bot.Start` calls `GetMe`, records `botID`, then starts long polling. It does
  not call Telegram `setMyCommands`.
- `handleMessage` treats every text message, including `/...`, as a normal
  prompt or steer.
- While a turn is active, `steer()` always sends RPC `steer`. That is wrong for
  extension slash commands per the pi RPC docs; slash commands should be routed
  through RPC `prompt` with `streamingBehavior:"steer"`.
- The fake Telegram API and tests do not model command registration yet.

### Telegram constraints relevant to pi commands

- Telegram registered bot commands must use only lowercase letters, digits, and
  underscores, and must be no longer than 32 characters.
- Telegram allows up to 100 commands in the bot command menu.
- Pi command names may contain characters Telegram cannot register, notably:
  - skills: `skill:brave-search`
  - templates/extensions with hyphens: `fix-tests`
- Therefore we need a deterministic alias layer; registering only already-valid
  pi names would omit many useful commands.

## 3. Design

### 3.1 Command discovery

Add a typed command helper in `rpc` while keeping the existing raw helper for
compatibility:

```go
type Command struct {
    Name        string `json:"name"`
    Description string `json:"description,omitempty"`
    Source      string `json:"source"` // extension | prompt | skill
    Location    string `json:"location,omitempty"`
    Path        string `json:"path,omitempty"`
}

func (c *Client) ListCommands(ctx context.Context) ([]Command, error)
```

In `main.run`, when Telegram is enabled, build a discovery function for the bot
that starts a short-lived standalone RPC client, calls `ListCommands`, then
closes it. Use a copied RPC config with session persistence disabled so command
discovery does not consume a pool slot or create a channel session:

```go
discoverCfg := cfg.RPC
discoverCfg.NoSession = true
discoverCfg.SessionDir = ""
```

If discovery fails, log and continue with only gateway-native commands (or no
registered pi commands). Telegram serving should not fail just because command
registration failed.

### 3.2 Alias registry

Create a small `telegramCommandRegistry` inside `chat`:

```go
type telegramCommand struct {
    TelegramName string // e.g. fix_tests
    PiName       string // e.g. fix-tests, no leading slash
    Source       string // extension | prompt | skill | gateway
    Description  string
}
```

Rules:

1. Start with pi `get_commands` order: extensions, prompts, skills.
2. Convert each pi command name into a Telegram-safe alias:
   - lowercase
   - replace any run of non `[a-z0-9_]` with `_`
   - trim leading/trailing `_`
   - if empty, skip
   - cap to 32 chars
3. Prefer source-specific readability for skills:
   - `skill:brave-search` -> `skill_brave_search`
4. Resolve collisions deterministically with numeric suffixes:
   - `fix-tests` -> `fix_tests`
   - `fix_tests` -> `fix_tests_2`
5. Descriptions:
   - use pi's description when present
   - fallback to `Pi <source> command`
   - cap to Telegram's 256-char limit
6. Cap registered commands to Telegram's 100-command limit.
7. Keep the full alias map in memory even if only the first 100 are registered,
   so manually typed aliases can still work.

### 3.3 Telegram Bot API surface

Extend `TelegramAPI`:

```go
type BotCommand struct {
    Command     string `json:"command"`
    Description string `json:"description"`
}

SetMyCommands(ctx context.Context, commands []BotCommand) error
```

Implement `setMyCommands` in `src/chat/telegram.go` via the existing generic
`call` helper. Add fake support in tests.

Add optional config:

```text
WALLE_TELEGRAM_REGISTER_COMMANDS=true|false   default true
```

When false, skip `setMyCommands` but still parse/route commands if the registry
was built.

### 3.4 Startup flow

`Bot.Start` should become:

1. `GetMe` as today.
2. Build command registry by calling the injected discovery function.
3. If command registration is enabled, call `SetMyCommands` with the registry's
   Telegram commands.
4. Log command registration count and any non-fatal errors.
5. Start polling as today.

Do not make `setMyCommands` failure fatal. The bot can still work without a
Telegram menu.

### 3.5 Incoming command parsing and routing

Before `handleMessage` decides prompt-vs-steer, normalize Telegram command
syntax:

- Recognize the first token if it starts with `/`.
- In groups, Telegram may deliver `/cmd@botusername arg`; strip `@botusername`
  if it matches our bot username.
- If a slash command is addressed to a different bot, ignore it.
- Look up the command token in the alias registry.
- If found, rewrite the message text to the real pi form:
  - `/fix_tests please` -> `/fix-tests please`
  - `/skill_brave_search pi docs` -> `/skill:brave-search pi docs`
- If not found, either pass through unchanged or send a short unknown-command
  message. Prefer pass-through for now to preserve current behavior.

While a turn is active:

- For normal text, keep using RPC `steer` as today.
- For slash-command text, send RPC `prompt` with `streamingBehavior:"steer"`
  instead of RPC `steer`. This is required for extension commands.

Implementation shape:

```go
func (b *Bot) normalizeTelegramCommand(text string) (normalized string, isSlash bool, addressedToOtherBot bool)
func (b *Bot) sendDuringActiveTurn(ctx context.Context, slot *pool.Slot, text string, isSlash bool) error
```

### 3.6 Gateway-native commands (small optional set)

Do not register pi interactive built-ins. If we want Telegram convenience
commands, make them explicitly gateway-native and implement them with RPC APIs.
Start with only safe/helpful commands:

- `/help` — explain Telegram aliases and that pi commands come from extensions,
  prompts, and skills.
- `/commands` — list registered aliases and their underlying pi command names.

Defer `/new`, `/compact`, `/abort`, `/session`, and model switching unless we
want a broader Telegram control surface. Those require more careful active-turn
and session-state behavior.

## 4. Tests

Add/extend tests in `src/chat/telegram_test.go`:

1. `TestTelegram_StartRegistersPiCommands`
   - fake command discovery returns prompt, skill, and extension commands
   - `Start` calls fake `SetMyCommands`
   - aliases are sanitized (`fix-tests` -> `fix_tests`,
     `skill:brave-search` -> `skill_brave_search`)
2. `TestTelegram_CommandAliasRewritesToPiCommand`
   - incoming `/fix_tests arg` causes the fake pi to receive prompt text
     `/fix-tests arg`
3. `TestTelegram_SkillAliasRewritesToPiSkill`
   - incoming `/skill_brave_search query` causes prompt text
     `/skill:brave-search query`
4. `TestTelegram_GroupCommandMention`
   - `/fix_tests@wall_e_test_bot arg` is accepted and mention stripped
   - `/fix_tests@other_bot arg` is ignored
5. `TestTelegram_ActiveSlashCommandUsesPromptStreamingBehaviorSteer`
   - hold first turn streaming
   - send a slash command during the active turn
   - assert fake pi receives `type:"prompt"` with
     `streamingBehavior:"steer"`, not `type:"steer"`
6. `TestTelegram_SetMyCommandsFailureNonFatal`
   - fake `SetMyCommands` returns error
   - `Start` still starts polling
7. Unit tests for alias builder:
   - invalid characters
   - collisions
   - 32-char command limit
   - 100-command registration cap
   - description fallback/truncation

Add config tests for `WALLE_TELEGRAM_REGISTER_COMMANDS` if that env var is
implemented.

## 5. Implementation phases

### Phase 1 — Typed command discovery

- Add `rpc.Command` and `Client.ListCommands`.
- Add/adjust RPC fake test for typed parsing.
- No Telegram changes yet.

### Phase 2 — Telegram command registration API

- Add `BotCommand` and `SetMyCommands` to `TelegramAPI`.
- Implement real API call in `telegram.go`.
- Extend fake Telegram API.

### Phase 3 — Alias registry

- Implement sanitizer, collision handling, Telegram command conversion, and
  lookup.
- Unit test thoroughly.

### Phase 4 — Wire startup registration

- Extend `chat.Config` with command discovery and registration flag.
- Wire from `main.run` using a short-lived RPC client.
- Update README config table.

### Phase 5 — Route incoming slash commands correctly

- Normalize `/cmd@bot` syntax.
- Rewrite Telegram aliases back to pi command names before prompt/steer.
- For active slash commands, use `Prompt(..., steer=true)` instead of `Steer`.

### Phase 6 — Manual smoke

- Run `go test ./...`.
- Start a test Telegram bot.
- Verify Telegram command menu shows aliases.
- Invoke a prompt template, a skill command, and an extension command.
- Verify mid-stream extension command does not fail due to RPC `steer`.

## 6. Non-goals for this change

- Full Telegram admin/control plane.
- Per-chat command scopes.
- Webhook mode.
- Registering pi TUI-only built-ins such as `/settings`, `/model`, `/hotkeys`.
- Dynamic automatic refresh after pi `/reload`; add a manual refresh command
  later if needed.

## 7. Implementation log

Implemented on 2026-07-02.

### Files changed

- `src/rpc/types.go`, `src/rpc/client.go`
  - Added typed `rpc.Command` and `Client.ListCommands` over existing
    `get_commands`.
- `src/chat/chat.go`
  - Extended Telegram API with `SetMyCommands`.
  - Added command discovery/registration config to Telegram `Config`.
  - Added startup command discovery/registration.
  - Rewrites Telegram aliases back to pi slash commands.
  - Routes active slash commands through RPC `prompt` with
    `streamingBehavior:"steer"` instead of RPC `steer`.
- `src/chat/telegram.go`
  - Added real `setMyCommands` Bot API call.
- `src/chat/telegram_commands.go`
  - Added Telegram-safe alias registry with sanitization, collision handling,
    description fallback/truncation, and 100-command registration cap.
- `src/config/config.go`
  - Added `WALLE_TELEGRAM_REGISTER_COMMANDS` parsing, default `true`.
- `src/main.go`
  - Wires Telegram command discovery via a short-lived `pi --mode rpc
    --no-session` client, so the pool is not consumed.
- `README.md`
  - Documented `WALLE_TELEGRAM_REGISTER_COMMANDS`.

### Tests added/updated

- `src/rpc/client_test.go`
  - `TestClient_ListCommands`.
- `src/chat/telegram_test.go`
  - Telegram command registration.
  - `setMyCommands` failure is non-fatal.
  - Alias rewrite for prompt templates and skills.
  - Group command mentions (`/cmd@bot`).
  - Active slash command uses `prompt` + `streamingBehavior:"steer"`.
  - Alias sanitizer/collision/cap behavior.
- `src/config/config_test.go`
  - Register-commands bool default, override, and parse failure.

### Validation

```sh
cd src && gofmt -w main.go chat/chat.go chat/telegram.go chat/telegram_commands.go chat/telegram_test.go config/config.go config/config_test.go rpc/types.go rpc/client.go rpc/client_test.go main_test.go
cd src && go test ./... && go vet ./...
```

Both commands passed.

## 8. Follow-up: gateway-native commands and `/skill <name>`

Implemented after manual review feedback:

- Changed skill UX:
  - Telegram registers `/skill`, not one command per skill.
  - `/skill` lists discovered pi skills.
  - `/skill <skill-name> [args]` rewrites to pi's `/skill:<skill-name> [args]`.
  - Skill lookup accepts the real skill name (e.g. `brave-search`) and a
    Telegram-safe alias form (e.g. `brave_search`).
- Added gateway-native Telegram commands to the command menu:
  - `/name [name]` -> RPC `set_session_name`.
  - `/session` -> RPC `get_state` summary.
  - `/clone` -> RPC `clone` and resyncs the pool session mapping.
  - `/new` -> RPC `new_session` and resyncs the pool session mapping.
  - `/compact [instructions]` -> RPC `compact`.
- Gateway-native commands are handled by wall-e instead of being forwarded to pi
  as prompts. If a turn is active, these control commands return a short busy
  message rather than racing the active stream.
- Added `Pool.ResyncFromState` so `/new` and `/clone` update the session
  manager, preventing later slot reuse from switching back to a stale session
  file.

Validation:

```sh
cd src && go test ./... && go vet ./...
```

Passed.
