---
name: cron
description: Create, inspect, and maintain short periodic jobs.
---

# Cron Jobs

Use cron only for small periodic commands. Use supervisor for long-running
services, servers, daemons, watchers, or anything that should stay running.

## Persistence model

`/home/wall-e` is persisted across container recreates. Cron's live spool under
`/var/spool/cron` is not.

For jobs that should survive Docker restarts, rebuilds, and updates, edit the
persisted crontab source of truth:

- `/home/wall-e/.config/cron/crontab`

Supervisor starts cron through `/usr/local/bin/run-cron`. On container startup,
`run-cron` installs `/home/wall-e/.config/cron/crontab` into the live `wall-e`
user crontab before starting cron.

After changing the persisted file, install it into the live crontab:

```sh
crontab /home/wall-e/.config/cron/crontab
crontab -l
```

The `run-cron` wrapper normally creates these directories at startup:

- `/home/wall-e/.config/cron`
- `/home/wall-e/.local/state/cron/locks`
- `/var/log/wall-e/cron`

## Files

Persisted files:

- `/home/wall-e/.config/cron/crontab`
- `/home/wall-e/.config/cron/env` for optional private environment variables

Runtime / system locations:

- Live crontab: managed with `crontab`; do **not** edit `/var/spool/cron/crontabs/wall-e`
- Logs: `/var/log/wall-e/cron/*.log`
- Locks: `/home/wall-e/.local/state/cron/locks/*.lock`

## Recommended crontab header

Cron jobs get a small cron environment, not the full supervisor/container
environment. Set only the variables the jobs need.

```cron
SHELL=/bin/bash
HOME=/home/wall-e
PATH=/usr/local/bin:/usr/bin:/bin:/home/wall-e/.local/bin
MAILTO=""
```

If a job invokes pi, add these explicitly:

```cron
PI_CODING_AGENT_DIR=/opt/pi
```

The container timezone is the system timezone, expected to be UTC unless changed.

## Job pattern

Prefer a small cron wrapper script so locking and log truncation are explicit:

```cron
*/15 * * * * /home/wall-e/scripts/my-job-cron.sh
```

Rules:

- Run jobs as the `wall-e` user by editing only the `wall-e` crontab.
- Prefer absolute paths or `cd` into the project before running commands.
- Use `flock` for jobs that may overlap.
- Keep secrets out of committed crontab examples.
- For secrets, source `/home/wall-e/.config/cron/env` explicitly and keep it
  mode `0600`.

## Logging rules

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
