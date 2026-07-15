# Composio

wall-e includes the [Composio CLI](https://docs.composio.dev), which lets the
agent find and use tools for external services such as email, calendars, source
control, and messaging. The integration is available from every wall-e channel;
no gateway plug-in or per-app environment variable is required.

## Ask wall-e to use an app

Describe the result you want and identify the relevant account or workspace when
it matters. For example:

- “List today's calendar events.”
- “Create a GitHub issue in `acme/web` from this bug report.”
- “Post this summary to the project Slack channel.”

The agent searches Composio for an appropriate tool, tries the action, and asks
you to connect an account only if the tool requires one. It does not need every
app to be connected in advance. For an action with important side effects,
state the intended recipients, repository, channel, or other target clearly;
you can also ask wall-e to preview the action without executing it.

## How the wall-e knows

The system prompt gives every pi worker this guidance in
[`APPEND_SYSTEM.md`](https://github.com/millie-research-inc/wall-e/blob/main/static/APPEND_SYSTEM.md):

> Composio lets you interact with external apps and services. When the user asks
> you to do something involving an external app, use the `composio` CLI rather
> than assuming no integration exists.
>
> Typical workflow:
>
> 1. `composio search "<what the user wants done>"`
> 2. `composio execute <slug from search> -d '<params>'`
> 3. If authentication is required, run `composio link <toolkit>`, give the user
>    the generated authorization URL, then retry step 2.
>
> Run `composio --help` for full usage and available commands.
>
> Do not assume Composio lacks coverage—search first. Do not preemptively link
> accounts or ask which accounts to connect. Try the requested action;
> authentication and validation errors are self-descriptive.

## First-time setup and authorization

There are two separate authorization steps. Either step is skipped when it has
already been completed:

1. **Sign the CLI in to Composio.** wall-e starts the headless login flow and
   sends you a Composio authorization URL. Open it as the Composio account that
   should own the integrations, complete login, and tell wall-e when it is done.
2. **Connect the requested app.** wall-e runs `composio link` for the app's
   toolkit and sends you its authorization URL. Before approving, verify the
   service, account, workspace, and requested permissions. Complete the app's
   OAuth flow, then ask wall-e to retry the original action.

Authorization URLs are temporary credentials. Send them only through the
intended private conversation and do not copy them into source files, docs, or
shared logs. wall-e never needs the app's password; authentication happens on
the app or Composio authorization page.

## Connection scope and persistence

All pi workers run as the same `wall-e` operating-system user and share one
Composio CLI session. Consequently, an app connected from one HTTP, Telegram,
or Discord channel is available to the agent in the other authorized channels
as well. Connections are not isolated per chat.

The CLI keeps its login state and cache under `/home/wall-e/.composio`. The
standard container mounts `/home/wall-e` from the `walle--home` Docker volume,
so this state survives image rebuilds and container replacement. Deleting that
volume removes the local CLI state.

Treat access to wall-e as access to every connected app:

- set the Telegram and Discord channel allowlists described in
  [Environment variables](environment), and protect `WALLE_TOKEN`;
- grant only the app permissions and workspace access that wall-e needs;
- use a dedicated service account when personal-account access is unnecessary;
- review and revoke connections in Composio and in the external app when they
  are no longer needed.

Logging the CLI out or deleting its local state is not a substitute for revoking
an OAuth grant at the provider.

## Inspect or troubleshoot

The recommended interface is a request to wall-e, but an operator can inspect
the same CLI session from the container:

```sh
make attach

composio whoami
composio link gmail --list
composio search "list today's calendar events" --human
```

Useful checks:

- `composio whoami` fails: ask wall-e to sign in to Composio again.
- No account appears for an app: ask wall-e to reconnect it, or run
  `composio link <toolkit>` in the attached session.
- A tool rejects its inputs: use `composio execute <tool-slug> --get-schema` to
  inspect the required fields.
- The CLI is missing after a custom image change: `command -v composio` should
  report `/usr/local/bin/composio`; rebuild the image otherwise.

See the [Composio documentation](https://docs.composio.dev) for the complete CLI
and supported-toolkit reference.
