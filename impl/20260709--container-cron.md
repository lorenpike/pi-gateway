# Container cron — Implementation Plan

**Date:** 2026-07-10

## Goal

Run `cron` inside the wall-e container so the model can create, inspect, and
remove its own scheduled jobs as the `wall-e` user. Cron is for small periodic
commands only; long-running services still belong in `supervisor`.

## Current state

- `cron` is already installed in the `Dockerfile`.
- The container boots through `supervisord`, but there is no supervised cron
  daemon yet.
- `wall-e` can use the normal `crontab` command for user-level crons.
- `/home/wall-e` is persisted by the current Docker run, but cron's live spool
  under `/var/spool/cron/crontabs` is not.

## Desired behavior

- `cron` starts automatically with the container and is restarted by supervisor.
- Model-managed jobs run as `wall-e`, not root.
- The live crontab is installed with `crontab`, never by editing spool files.
- User-managed cron state persists across container rebuilds/recreates via a
  canonical crontab file under `/home/wall-e`.
- Cron job output is explicit and debuggable under `/var/log/wall-e/cron/`.
- The image ships both user docs and a short skill telling the model how to add
  safe cron jobs.

## Canonical file layout

```text
# Source-controlled image files
static/etc/supervisor/conf.d/cron.conf       admin-managed supervised cron daemon
static/bin/run-cron                          admin-managed cron startup wrapper
static/skills/cron/SKILL.md                  agent guidance for cron usage
docs/source/cron.md                          user/admin cron documentation

# Runtime files inside the container
/usr/local/bin/run-cron                      installed cron startup wrapper
/etc/supervisor/conf.d/cron.conf             installed supervisor program
/var/spool/cron/crontabs/wall-e              live cron spool; do not edit directly
/home/wall-e/.config/cron/crontab            persisted wall-e crontab source of truth
/home/wall-e/.config/cron/env                optional private environment file
/home/wall-e/.local/state/cron/locks/        stable per-job lock files
/var/log/wall-e/cron/                        per-job cron logs
```

Notes:

- The live spool path is implementation detail owned by cron. Agents must use
  `crontab` to install or inspect it.
- `/home/wall-e/.config/cron/crontab` is the persisted desired crontab because
  `/home/wall-e` is volume-backed by the current `Makefile`.
- `/var/log/wall-e/cron/` follows the existing wall-e log convention. These logs
  are not persisted unless a future Docker run mounts `/var/log/wall-e` or jobs
  choose to log under `/home/wall-e`.

## Supervisor config

Add `static/etc/supervisor/conf.d/cron.conf`:

```ini
[program:cron]
command=/usr/local/bin/run-cron
user=root
autostart=true
autorestart=true
stopsignal=TERM
stdout_logfile=/var/log/wall-e/cron.out.log
stdout_logfile_maxbytes=0
stderr_logfile=/var/log/wall-e/cron.err.log
stderr_logfile_maxbytes=0
```

Notes:

- The cron daemon must run as root so it can drop privileges to user crontabs.
- User jobs should be installed with `crontab` as `wall-e` and will execute as
  `wall-e`.
- Do not expose any cron control interface over the network.
- On Ubuntu, cron primarily logs through syslog. Because this container does not
  run syslog by default, the supervisor stdout/stderr logs may be sparse. Per-job
  redirects are the canonical debugging path.

## Cron startup wrapper

Add `static/bin/run-cron` and copy it to `/usr/local/bin/run-cron` with mode
`0555`:

```sh
#!/bin/sh
set -eu

install -d -o wall-e -g wall-e -m 0755 /var/log/wall-e/cron
install -d -o wall-e -g wall-e -m 0700 \
    /home/wall-e/.config/cron \
    /home/wall-e/.local/state/cron/locks

if [ -f /home/wall-e/.config/cron/crontab ]; then
    chown wall-e:wall-e /home/wall-e/.config/cron/crontab
    chmod 600 /home/wall-e/.config/cron/crontab
    if ! su -s /bin/sh wall-e -c 'crontab /home/wall-e/.config/cron/crontab'; then
        echo 'run-cron: failed to install /home/wall-e/.config/cron/crontab; starting cron anyway' >&2
    fi
fi

rm -f /var/run/crond.pid /run/crond.pid
exec /usr/sbin/cron -f -L 15
```

Rationale:

- Container rebuilds/recreates lose `/var/spool/cron/crontabs`, but preserve
  `/home/wall-e/.config/cron/crontab` via the home volume.
- The wrapper restores the persisted crontab on startup using the supported
  `crontab` command.
- The wrapper also ensures the canonical log and lock directories exist even if
  the home volume is old.

## Dockerfile changes

- Keep `cron` installed.
- Create `/home/wall-e/.config/cron`, `/home/wall-e/.local/state/cron/locks`,
  and `/var/log/wall-e/cron`.
- Ensure `/home/wall-e/.config/cron` and `/home/wall-e/.local/state/cron` are
  owned by `wall-e`.
- Ensure `/var/log/wall-e/cron` is writable by `wall-e`.
- Copy the new supervisor snippet via the existing
  `COPY static/etc/supervisor/conf.d/ /etc/supervisor/conf.d/`.
- Copy `static/bin/run-cron` to `/usr/local/bin/run-cron` with mode `0555`.
- Copy the cron skill through the existing `static/skills` copy.

## Documentation and skill changes

Add `docs/source/cron.md` and include it from `docs/source/index.md` under the
internals or operations toctree.

The docs and `static/skills/cron/SKILL.md` should both state:

- Use cron only for short periodic commands.
- Use supervisor for long-running services.
- Edit `/home/wall-e/.config/cron/crontab` as the persisted source of truth.
- Install changes with `crontab /home/wall-e/.config/cron/crontab` as `wall-e`.
- Inspect the live crontab with `crontab -l` as `wall-e`.
- Never edit `/var/spool/cron`, `/etc/crontab`, or `/etc/cron.d` unless the user
  explicitly asks for an admin/root cron.

## Agent-managed crontab conventions

The model should manage only the `wall-e` user crontab. `/home/wall-e` is
persisted, so durable jobs should be represented in
`/home/wall-e/.config/cron/crontab`. The `run-cron` wrapper normally creates the
config, lock, and log directories at container startup.

After editing `/home/wall-e/.config/cron/crontab`, install and inspect it:

```sh
crontab /home/wall-e/.config/cron/crontab
crontab -l
```

For one-off scripted edits, avoid fixed `/tmp` filenames:

```sh
tmp="$(mktemp)"
crontab -l > "$tmp" 2>/dev/null || true
# edit "$tmp"
crontab "$tmp"
install -m 600 "$tmp" /home/wall-e/.config/cron/crontab
rm -f "$tmp"
```

Recommended crontab header:

```cron
SHELL=/bin/bash
HOME=/home/wall-e
PATH=/usr/local/bin:/usr/bin:/bin:/home/wall-e/.local/bin
MAILTO=""
# Container timezone is the system timezone, expected to be UTC unless changed.
```

Cron jobs get a small cron environment, not the full supervisor/container
environment. If a job invokes pi/wall-e tooling, add these explicitly:

```cron
PI_CODING_AGENT_DIR=/opt/pi
WALLE_SESSION_DIR=/home/wall-e/sessions
```

Recommended job style: use a small wrapper script so locking and log truncation
are explicit.

```cron
*/15 * * * * /home/wall-e/project/scripts/my-job-cron.sh
```

Example wrapper with 2 MiB log truncation:

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

If a job needs private environment variables, source `/home/wall-e/.config/cron/env`
inside the wrapper. The env file should be `0600` and must not be committed.

## Safety rules

- Prefer absolute paths or `cd` into the project before running commands.
- Redirect stdout/stderr for every job.
- Use `flock` with stable lock files under `/home/wall-e/.local/state/cron/locks`
  for jobs that may overlap.
- Keep secrets out of committed crontab examples; source a private env file only
  when explicitly needed.
- Do not edit `/var/spool/cron`, `/etc/crontab`, or `/etc/cron.d` unless the
  user explicitly asks for an admin/root cron.
- Use supervisor, not cron, for persistent daemons.
- Remember that `wall-e` has passwordless sudo, so cron is a scheduling
  convenience, not a privilege boundary.
- Cron jobs should be quiet and bounded by design. Do not schedule commands with
  large/unbounded output. For any appending log, truncate it at 2 MiB before
  running the job.

## Smoke test

Inside a rebuilt container, run explicitly as `wall-e` so the smoke job is not
installed into root's crontab:

```sh
supervisorctl status cron
sudo -u wall-e bash -lc '
  crontab -l > /home/wall-e/.config/cron/crontab 2>/dev/null || true
  cp /home/wall-e/.config/cron/crontab /tmp/run-cron-smoke
  printf "* * * * * date >>/var/log/wall-e/cron/smoke.log 2>&1\n" >> /tmp/run-cron-smoke
  crontab /tmp/run-cron-smoke
'
sleep 70
tail /var/log/wall-e/cron/smoke.log
sudo -u wall-e bash -lc '
  crontab -l | grep -v "smoke.log" | tee /home/wall-e/.config/cron/crontab | crontab -
'
```

Expected result: `cron` is `RUNNING`, `smoke.log` receives at least one
timestamp, and removing the smoke line stops future writes.

Persistence check:

```sh
# After docker rm/recreate, cron startup should reinstall this file into the
# live wall-e crontab.
sudo -u wall-e crontab -l | diff -u /home/wall-e/.config/cron/crontab -
```
