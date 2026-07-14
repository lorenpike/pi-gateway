# One-off jobs

The container runs Ubuntu `atd` under supervisor. Use `at` for commands that
should run once, cron for recurring jobs, and supervisor for persistent services
or daemons.

## Scheduling

Submit jobs as `wall-e`; an `at` job runs as the user who submitted it. Prefer a
small executable wrapper with absolute paths and explicit environment:

```sh
printf '%s\n' '/home/wall-e/scripts/reminder.sh >>/home/wall-e/reminder.log 2>&1' \
  | at 09:30 tomorrow

printf '%s\n' '/home/wall-e/scripts/job.sh' | at now + 30 minutes
```

Do not use `sudo at` unless a root job was explicitly requested. Although `at`
captures much of the submitting process's environment, jobs should set required
values such as `HOME`, `PATH`, `PI_CODING_AGENT_DIR`, channels, and secrets
explicitly in a mode-`0600` wrapper or private env file.

For a scheduled chat message, capture `$WALLE_CHANNEL` during the live turn and
write it into the wrapper, then invoke `wall-e msg "$CHANNEL"` from that wrapper.

## Inspection and cancellation

```sh
atq
at -c JOB_ID
atrm JOB_ID
supervisorctl status atd
```

The queue lives in the container's system spool. It survives restarting the same
container, but is lost when the container is removed or recreated. Scripts and
logs that matter should live under the persisted `/home/wall-e` volume.

The container does not configure mail delivery, so redirect stdout and stderr
explicitly when output matters.

## Layout

| Path | Purpose |
|---|---|
| `/etc/supervisor/conf.d/atd.conf` | admin-managed supervisor program for `atd` |
| `/var/spool/cron/atjobs/` | runtime one-off job queue |
| `/var/spool/cron/atspool/` | runtime execution spool |
| `/var/log/wall-e/atd.out.log` | daemon stdout |
| `/var/log/wall-e/atd.err.log` | daemon stderr |
