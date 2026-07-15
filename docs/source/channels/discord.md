# Discord channel

The Discord front-end connects through the Gateway, handles guild channels,
threads, and DMs, registers global application commands, and supports ordinary
messages, attachments, HTTP/CLI prompts, and direct sends. This page explains
how to create and install the bot and how wall-e delivers Discord turns.

## What you need

Before starting, you need:

- a Discord account;
- a Discord server where you have **Manage Server** permission;
- access to the [Discord Developer Portal](https://discord.com/developers/applications);
- one or more text-channel or thread IDs that wall-e may use.

Do not use a normal Discord user token. Wall-e connects as an official bot user
with a bot token.

## 1. Create the application

1. Open the [Discord Developer Portal](https://discord.com/developers/applications).
2. Select **New Application**.
3. Enter a name, such as `wall-e`, accept Discord's terms, and create it.
4. Optionally set an icon and description on **General Information**.

The application owns both the bot user and its slash commands.

## 2. Create the bot and copy its token

1. Open the application's **Bot** page.
2. If Discord shows an **Add Bot** button, select it and confirm. New
   applications may already have a bot user.
3. Under **Token**, select **Reset Token** (or **View Token**, when available).
4. Copy the token immediately and store it in a secret manager or the ignored
   project-root `.env` file.

The value wall-e needs is the **bot token**. It is not the Application ID,
Public Key, Client ID, or OAuth2 Client Secret.

```sh
WALLE_DISCORD_TOKEN=replace-with-the-bot-token
```

Treat the token like a password:

- never paste it into chat, logs, screenshots, source files, or commits;
- do not place it in `docs/` or another tracked file;
- rotate it with **Reset Token** immediately if it is exposed;
- remember that resetting it invalidates the old token, so the wall-e service
  must be updated and restarted.

This repository ignores its root `.env` file through `.gitignore`.

## 3. Enable the Message Content intent

Wall-e needs ordinary message text and attachment metadata. Discord protects
those fields behind the privileged **Message Content Intent** for most server
messages.

On the application's **Bot** page:

1. Scroll to **Privileged Gateway Intents**.
2. Enable **Message Content Intent**.
3. Save the change if the portal presents a save button.

Wall-e requests only Guilds, Guild Messages, Direct Messages, and Message
Content intents. It does not need **Presence Intent** or **Server Members
Intent**. Enabling extra privileged intents increases access
without providing a wall-e feature.

For a verified or sufficiently large application, Discord may require approval
before it grants Message Content access. See Discord's
[Gateway Intents documentation](https://discord.com/developers/docs/events/gateway#privileged-intents).

If this intent is missing, the bot may receive a message event while its text
and attachments are empty. Restart wall-e after changing intents so it opens a
new Gateway session with the updated configuration.

## 4. Configure installation scopes and permissions

Open the application's **Installation** page. For a server-hosted wall-e bot,
configure **Guild Install** with these scopes:

- `bot`
- `applications.commands`

The `applications.commands` scope allows wall-e to register native slash
commands. Discord normally includes it with the bot scope, but selecting it
explicitly makes the intended installation clear.

Grant only the bot permissions wall-e needs:

| Permission | Why wall-e needs it |
|---|---|
| **View Channels** | Receive and access messages in an allowed channel |
| **Send Messages** | Send completed responses and direct messages |
| **Read Message History** | Create replies and group long response chunks |
| **Attach Files** | Deliver files through `wall-e send --media` |
| **Send Messages in Threads** | Respond when a configured ID is a thread |

Do **not** grant **Administrator**. If wall-e should only operate in a few
channels, use Discord's channel permission overrides to limit the bot there.

Optional permissions:

- **Embed Links** is not required for ordinary Markdown links, but allows
  Discord to render richer URL previews.
- Private threads must include the bot as a thread member in addition to the
  relevant thread permissions.

Discord permission behavior is documented in
[Permissions](https://discord.com/developers/docs/topics/permissions).

### “Add to My Apps” is not the wall-e installation

Discord may offer **Add to My Apps** when **User Install** is enabled for the
application. That installs the application to your Discord user account rather
than adding its bot user as a member of a server.

A user installation currently grants the `applications.commands` scope. It can
make the app's supported slash commands available to that user in DMs, group
DMs, and permitted server contexts. It does **not** give the bot server roles or
channel permissions, and it does not provide the ordinary `MESSAGE_CREATE`
stream that wall-e needs to treat every channel message like Telegram. It also
cannot support proactive `wall-e send discord:<channel-id>` delivery by itself.

For wall-e, choose **Add to Server** / **Guild Install**. You can leave **User
Install** disabled unless a later wall-e feature explicitly supports a
command-only personal installation. Accidentally choosing **Add to My Apps** is
not dangerous, but it is insufficient: install the application again using
Guild Install.

## 5. Install the bot into your server

On the **Installation** page:

1. Use Discord's generated install link for **Guild Install**.
2. Open the link in a browser.
3. Select the target server.
4. Review the requested permissions and authorize the application.
5. Complete Discord's verification prompt if shown.

You must own the target server or have **Manage Server** permission.

If your Developer Portal does not show the newer Installation settings, use
**OAuth2** → **URL Generator** instead:

1. select the `bot` and `applications.commands` scopes;
2. select the bot permissions listed above;
3. open the generated URL and authorize it for the target server.

After installation, the bot appears in the server member list. It will show as
offline until wall-e connects with the bot token.

## 6. Direct messages

Yes. Once wall-e is connected, a user who shares
a server with the guild-installed bot can open its profile and send it a
one-to-one direct message. Discord must also permit the DM under that user's
privacy settings.

Wall-e will treat the **DM channel ID** as the stable channel—not the user's ID.
Each one-to-one DM channel therefore gets its own pi session and runtime address:

```text
WALLE_CHANNEL=discord:<dm-channel-id>
```

The Gateway connection requests Discord's Direct Messages intent. DM
message content is one of Discord's exceptions to the privileged Message
Content restrictions, although wall-e requests Message Content as well for its
server-channel behavior.

If `WALLE_DISCORD_ALLOWED_CHANNELS` is empty, DMs visible to the bot are allowed
along with server channels. If an allowlist is configured, add the DM channel
ID explicitly. To find it with Developer Mode enabled, try **Copy Channel ID**
on the DM. If that action is unavailable, copy a message link from the DM:

```text
https://discord.com/channels/@me/123456789012345678/987654321098765432
```

The value after `@me` is the DM channel ID (`123456789012345678` in this
example); the final value is only the individual message ID.

A user installation via **Add to My Apps** can expose supported application
commands in DMs, but that command-only installation is not a substitute for the
full guild-installed wall-e bot described here.

## 7. Restrict wall-e to specific channels

The adapter uses an exact channel-ID allowlist. This is strongly
recommended because everyone who can post in an allowed channel can interact
with that channel's shared pi session.

### Find a channel ID

1. In Discord, open **User Settings** → **Advanced**.
2. Enable **Developer Mode**.
3. Right-click the text channel and select **Copy Channel ID**.
4. Repeat for every channel wall-e should serve.

Discord IDs are decimal snowflake strings. Configure them as a comma-separated
list:

```sh
WALLE_DISCORD_ALLOWED_CHANNELS=123456789012345678,234567890123456789
```

Whitespace around values will be accepted:

```sh
WALLE_DISCORD_ALLOWED_CHANNELS=123456789012345678, 234567890123456789
```

Important details:

- unset or empty means allow every channel visible to the bot;
- channel IDs remain strings and should not be converted to signed integers;
- a category ID is not a text-channel ID and cannot receive messages;
- a Discord thread has its own ID;
- allowing a parent channel will **not** automatically allow its threads, so
  copy and add each permitted thread ID separately.

Discord channel IDs are globally unique, so a guild/server ID is not required
in `WALLE_DISCORD_ALLOWED_CHANNELS`.

## 8. Add the variables to wall-e

A typical project-root `.env` entry will be:

```sh
WALLE_DISCORD_TOKEN=replace-with-the-bot-token
WALLE_DISCORD_ALLOWED_CHANNELS=123456789012345678
```

Or export them in the service environment:

```sh
export WALLE_DISCORD_TOKEN='replace-with-the-bot-token'
export WALLE_DISCORD_ALLOWED_CHANNELS='123456789012345678'
```

`make docker` forwards both variables to the container. Wall-e will skip Discord entirely when
`WALLE_DISCORD_TOKEN` is unset.

Startup behavior is:

- a valid token opens the Discord Gateway and logs the connected bot identity;
- command registration happens automatically—do not manually create wall-e's
  slash commands in the Developer Portal;
- an invalid token or Gateway failure disables only Discord while the HTTP and
  other configured frontends continue running.

## 9. Messages, sessions, and delivery

Each allowed Discord channel ID is one shared conversation. Guild text
channels, threads, and DM channels all use the event's own `channel_id`, so a
thread does not share its parent's session. Session filenames and worker
context use the typed identity:

```text
discord--123456789012345678--<date>--<id>.jsonl
WALLE_CHANNEL=discord:123456789012345678
```

A message starts a turn when none is active and steers the active turn when one
is already streaming. Wall-e downloads every attachment, in event order, before submitting one
prompt. Downloads are HTTPS-only CDN requests with a 30-second timeout and a
32 MiB inbound safety cap; the bot Authorization header is never sent to
attachment URLs.

While pi is working, wall-e refreshes Discord's typing indicator. Discord uses
the same **buffered client-facing** contract as Telegram: assistant deltas are
not delivered or used to create previews, and the authoritative final text is
sent only after completion. A complete response whose trimmed text is exactly
`NO_REPLY` stops the typing indicator and sends no Discord message. This
whole-response, case-sensitive control is specified in
[`impl/20260714--no-reply.md`](https://github.com/millie-research-inc/wall-e/blob/main/impl/20260714--no-reply.md).

Discord Markdown is preserved. Responses longer than 2,000 characters are
split without corrupting emoji or losing whitespace, with later chunks replying
to the first final message. Every wall-e create, reply, edit, interaction, and
follow-up payload disables allowed mentions, so visible `@everyone`, role, and
user mention syntax from a model cannot notify anyone.

## 10. Slash commands

Wall-e globally synchronizes these native commands after Gateway Ready:
`/skill`, `/name`, `/session`, `/clone`, `/new`, `/compact`, and `/abort`.
Discovered pi extensions and prompt templates receive deterministic sanitized
aliases; skills are selected through `/skill name [args]`.

Allowed command interactions are deferred before pool or RPC work. The
deferred response does not receive assistant deltas; normal final text is
written after completion and uses follow-ups for additional chunks, while
`NO_REPLY` deletes the deferred original response so no persistent assistant
message remains. Discord's required initial
acknowledgement means its temporary “thinking” state cannot be avoided. A pi
command applied during an active turn uses pi's prompt-steer behavior and
receives a short acknowledgement while the existing channel stream remains
canonical. Disallowed channels receive an immediate ephemeral denial.

## 11. HTTP and CLI delivery

The Discord front-end registers both gateway adapters:

```sh
wall-e msg discord:123456789012345678 <<'EOF'
Summarize this channel.
EOF

wall-e send discord:123456789012345678 "hello"
wall-e send --media discord:123456789012345678 /home/wall-e/report.pdf \
  --caption "Here is the report"
```

`msg` starts or steers the same typed turn used by Discord and streams it to the
CLI while also delivering the assistant response in Discord. `send` bypasses
pi and delivers text or one generic Discord file attachment directly. The
channel allowlist applies to both paths.

## 12. Verify the Discord-side setup

Start wall-e and inspect its logs:

```sh
make docker
docker logs -f wall-e
```

Verify the following:

1. the bot changes from offline to online;
2. wall-e logs a Discord Ready/connected message;
3. typing `/` in an allowed channel shows wall-e's application commands;
4. an ordinary message receives a response;
5. a file attachment is downloaded and referenced in the pi prompt;
6. a message in a disallowed channel is ignored;
7. the bot can respond inside each explicitly allowed thread.

## Troubleshooting

### The bot stays offline

- Confirm `WALLE_DISCORD_TOKEN` contains the bot token, not the client secret.
- If you reset the token, replace the old value and restart wall-e.
- Confirm the application was installed with the `bot` scope.
- Check wall-e logs for authentication or Gateway errors.

### The bot is online but ignores ordinary messages

- Enable **Message Content Intent** on the Developer Portal's Bot page.
- Restart wall-e after enabling it.
- Confirm the channel ID is in `WALLE_DISCORD_ALLOWED_CHANNELS`.
- Confirm the bot has **View Channels** in that channel.

### The bot can read but cannot reply

Check channel-specific overrides for:

- **Send Messages** in normal text channels;
- **Send Messages in Threads** in threads;
- **Read Message History** when creating replies;
- **Attach Files** when sending media.

A channel override can deny a permission even when the server-level bot role
allows it.

### Slash commands do not appear

- Reinstall the application with the `applications.commands` scope.
- Confirm wall-e successfully registered commands in its logs.
- Global command changes can take time to appear in cached clients; restart the
  Discord client or try the command again after Discord refreshes it.
- Ensure application commands are permitted for the user/channel under the
  server's **Integrations** settings.

### “This application did not respond”

The bot received a slash-command interaction but did not acknowledge it within
Discord's three-second initial-response deadline. Check whether wall-e is
running and inspect its logs for interaction or permission errors.

## Discord references

- [Applications](https://discord.com/developers/applications)
- [Gateway and intents](https://discord.com/developers/docs/events/gateway)
- [Gateway events](https://discord.com/developers/docs/events/gateway-events)
- [Messages](https://discord.com/developers/docs/resources/message)
- [OAuth2 and bot installation](https://discord.com/developers/docs/topics/oauth2)
- [Application commands](https://discord.com/developers/docs/interactions/application-commands)
- [Receiving and responding to interactions](https://discord.com/developers/docs/interactions/receiving-and-responding)
- [Uploading files and API versions](https://discord.com/developers/docs/reference)
- [Permissions](https://discord.com/developers/docs/topics/permissions)
- [Rate limits](https://discord.com/developers/docs/topics/rate-limits)
