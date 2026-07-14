# Cron jobs

The container runs Ubuntu `cron` under supervisor for short recurring commands.
Use `at` for work that should run once, and supervisor for persistent services,
servers, daemons, or watchers.

## Persistence model

`/home/wall-e` is persisted across container recreates. Cron's live spool under
`/var/spool/cron` is not.

For jobs that should survive Docker restarts, rebuilds, and updates, edit the
persisted crontab source of truth:

```text
/home/wall-e/.config/cron/crontab
```

Supervisor starts cron through `/usr/local/bin/run-cron`. On container startup,
`run-cron` installs `/home/wall-e/.config/cron/crontab` into the live `wall-e`
user crontab before starting cron.

After changing the persisted file, install it into the live crontab:

```sh
crontab /home/wall-e/.config/cron/crontab
crontab -l
```

The `run-cron` wrapper normally creates the cron config, lock, and log
directories at startup. Create them manually only if they are unexpectedly
missing.

## Layout

| Path | Purpose |
|---|---|
| `/etc/supervisor/conf.d/cron.conf` | admin-managed supervisor program for cron |
| `/usr/local/bin/run-cron` | startup wrapper that restores persisted user cron state |
| `/home/wall-e/.config/cron/crontab` | persisted source of truth for the `wall-e` user crontab |
| `/home/wall-e/.config/cron/env` | optional private environment file for jobs |
| `/home/wall-e/.local/state/cron/locks/` | stable lock files for `flock` |
| `/var/log/wall-e/cron/` | per-job cron logs |
| `/var/spool/cron/crontabs/wall-e` | live cron spool; never edit directly |

## Recommended crontab header

Cron jobs get a small cron environment, not the full supervisor/container
environment. Set only the variables the jobs need.

```text
SHELL=/bin/bash
HOME=/home/wall-e
PATH=/usr/local/bin:/usr/bin:/bin:/home/wall-e/.local/bin
MAILTO=""
```

If a job invokes pi/wall-e tooling, add these explicitly:

```text
PI_CODING_AGENT_DIR=/opt/pi
WALLE_SESSION_DIR=/home/wall-e/sessions
```

The container timezone is the system timezone, expected to be UTC unless changed.

## Current chat/channel

Inside a live `pi` turn, wall-e sets `WALLE_CHANNEL` to the current typed channel address, for example `telegram:123456789` or `http:morning-digest`. Use it when a user says "schedule this for this chat" and you need to discover the target channel.

Cron jobs do **not** inherit the `pi` process environment. If a scheduled job needs the current channel later, capture the value while creating the job and write it explicitly into the wrapper script or a private config file under `/home/wall-e`.

Example scheduled message wrapper:

```bash
#!/usr/bin/env bash
set -euo pipefail

CHANNEL="telegram:123456789"   # captured from $WALLE_CHANNEL when the job was created
export WALLE_TOKEN="..."        # or source /home/wall-e/.config/cron/env

wall-e msg "$CHANNEL" <<'EOF'
Scheduled task: summarize today's calendar and top priorities.
EOF
```

`wall-e msg` reads stdin, posts to `/v1/prompt`, streams the assistant response to stdout, and exits non-zero on gateway errors, stream errors, timeouts, or a stream that ends before `done`.

## Job style

Use absolute paths. Prefer a small cron wrapper script so locking and log
truncation are explicit:

```text
*/15 * * * * /home/wall-e/project/scripts/my-job-cron.sh
```

If a job needs private environment variables, source `/home/wall-e/.config/cron/env`
inside the wrapper. Keep that env file mode `0600` and do not commit secrets.

## Logging

Cron jobs should be quiet and bounded.

- Do not schedule commands with large or unbounded output.
- Prefer scripts that print nothing on success.
- For any appending log, truncate it at 2 MiB before running the job.

Example `/home/wall-e/project/scripts/my-job-cron.sh`:

```bash
#!/usr/bin/env bash
set -euo pipefail

log=/var/log/wall-e/cron/my-job.log
max_bytes=$((2 * 1024 * 1024))
lock=/home/wall-e/.local/state/cron/locks/my-job.lock

if [ -f "$log" ] && [ "$(wc -c <"$log")" -gt "$max_bytes" ]; then
  : >"$log"
fi

cd /home/wall-e/project
exec flock -n "$lock" ./scripts/my-job.sh >>"$log" 2>&1
```

Logs under `/var/log/wall-e/cron/` are not persisted unless a deployment mounts
that directory. For logs that must survive container recreation, write to a
project-specific location under `/home/wall-e` or add a log volume.

## Debugging

Check supervisor and live cron state:

```sh
supervisorctl status cron
crontab -l
```

Cron itself logs primarily through syslog on Ubuntu, and this container does not
run syslog by default. The supervisor logs for cron may therefore be sparse:

```sh
tail /var/log/wall-e/cron.out.log
tail /var/log/wall-e/cron.err.log
```

Per-job redirects under `/var/log/wall-e/cron/` are the canonical debugging path:

```sh
tail /var/log/wall-e/cron/my-job.log
```

## Safety notes

- Manage only the `wall-e` user crontab unless explicitly asked for an admin/root
  cron.
- Never edit `/var/spool/cron`, `/etc/crontab`, or `/etc/cron.d` directly unless
  the user explicitly asks for admin cron.
- Use `at`, not cron, for one-off scheduled jobs.
- Use supervisor, not cron, for persistent daemons.
- Remember that `wall-e` has passwordless sudo, so cron is a scheduling
  convenience, not a privilege boundary.
