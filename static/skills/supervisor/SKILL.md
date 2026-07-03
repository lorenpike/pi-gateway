---
name: supervisord
description: |
    Configure and operate long-running project services in the wall-e container
    using supervisor.
---

# Supervisord Services

Use supervisor for any long-running service or server application. Do not use
`nohup`, `tmux`, or background shell jobs for persistent services.

**Project services** must listen on `127.0.0.1` (loopback) only. nginx is the
container's reverse proxy: it binds `0.0.0.0:80`, is the only listener allowed
on a non-loopback address, and reaches project services over the container's
loopback interface. Host/gateway exposure is set with Docker `-p <port>:80`
(and may sit behind Cloudflare upstream).

## Files

Agent-editable:

- User/project service snippets: `~/.config/supervisor.d/*.conf`

Do not edit unless explicitly asked:

- Base config: `/etc/supervisor/supervisord.conf`
- Admin service snippets: `/etc/supervisor/conf.d/*.conf`

Other locations:

- Logs: `/var/log/wall-e/`
- Control socket: `/var/run/supervisor.sock` via `supervisorctl`

## Add a service

Choose a loopback high port for web services, e.g. `3101`. Create or edit only
`~/.config/supervisor.d/<name>.conf`:

```ini
[program:<name>]
directory=/home/wall-e/path/to/app
command=/absolute/command arg1 arg2
user=wall-e
autostart=true
autorestart=true
startsecs=3
stopasgroup=true
killasgroup=true
environment=HOST="127.0.0.1",PORT="3101",HOME="/home/wall-e"
stdout_logfile=/var/log/wall-e/<name>.out.log
stderr_logfile=/var/log/wall-e/<name>.err.log
```

Rules:

- Run project services as `wall-e`.
- Use absolute paths in `command` when practical.
- Bind every server to `127.0.0.1` and a high port.
- Never bind project services to `0.0.0.0`, `::`, a public interface, or a
  low/public port.
- Do not expose ports from supervisor. External exposure belongs in Cloudflare
  or Docker configuration and should only be changed when explicitly asked.
- Put per-service environment in `environment=...`; keep secrets out of
  committed files.

Apply changes:

```sh
supervisorctl reread && supervisorctl update
supervisorctl status
```

## Operate a service

```sh
supervisorctl status <name>
supervisorctl restart <name>
supervisorctl stop <name>
supervisorctl tail -f <name> stdout
supervisorctl tail -f <name> stderr
```

Disable a service:

```sh
supervisorctl stop <name>
mv ~/.config/supervisor.d/<name>.conf ~/.config/supervisor.d/<name>.conf.disabled
supervisorctl reread && supervisorctl update
```

## Expose through nginx

After the service listens on `127.0.0.1:<port>`, add an nginx route in
`~/.config/nginx/conf.d/<name>.conf` and reload nginx. See the `nginx` skill.
