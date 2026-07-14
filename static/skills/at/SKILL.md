---
name: at
description: |
  Schedule, inspect, and cancel jobs that should run once. Like
  reminders, messages, or delayed tasks
---

# One-off jobs with at

Use `at` for a command that should run once in the future. Use cron for recurring
jobs and supervisor for long-running services, servers, daemons, or watchers.

The `atd` daemon runs as an admin-managed supervisor service. Submit jobs as the
`wall-e` user so they also run as `wall-e`; do not use `sudo at` unless the user
explicitly requests a root job.

## Schedule a job

Prefer a small executable wrapper script with absolute paths, bounded output,
and any required environment set explicitly. Then pipe the command to `at`:

```sh
printf '%s\n' '/home/wall-e/scripts/reminder.sh >>/home/wall-e/reminder.log 2>&1' \
  | at 09:30 tomorrow
```

Other accepted time forms include:

```sh
printf '%s\n' '/home/wall-e/scripts/job.sh' | at now + 30 minutes
printf '%s\n' '/home/wall-e/scripts/job.sh' | at 14:00 Jul 20
```

`at` runs commands with `/bin/sh` and captures much of the submitter's current
environment. Do not rely on that captured environment: set `HOME`, `PATH`,
`PI_CODING_AGENT_DIR`, channels, and secrets explicitly in a mode-`0600` wrapper
or private env file when needed.

When a user schedules a message for "this chat", capture `$WALLE_CHANNEL` while
inside the live turn and write the channel explicitly into the wrapper. Use
`wall-e msg "$CHANNEL"` to deliver it.

## Inspect and cancel

```sh
atq                 # list this user's queued jobs
at -c JOB_ID        # inspect the generated job script
atrm JOB_ID         # cancel a queued job
supervisorctl status atd
```

## Persistence and logging

The queue is in the container's system spool. It survives a restart of the same
container, but not container removal or recreation. Keep important scripts and
logs under `/home/wall-e`, which is persisted, and warn the user before relying
on an `at` job across a container rebuild or update.

No mail service is configured. Redirect stdout and stderr explicitly if job
output matters, and keep logs bounded.
