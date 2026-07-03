# User-space service supervision — Implementation Plan

**Date:** 2026-07-03

## Goal

Use `supervisord` inside the gateway container so the agent can keep project
services running without rebuilding the image or relying on `nohup`, `tmux`, or
background shell jobs. Docker remains the sandbox/persistence boundary.

System/admin programs (`wall-e`, `nginx`, optional `cloudflared`) are configured
under `/etc`. User/project services are configured from the user's home
directory. Project code can live anywhere under `/home/wall-e`; this plan does
not prescribe that layout.

## Directory layout

```text
/etc/supervisor/supervisord.conf          base supervisor config
/etc/supervisor/conf.d/*.conf             admin-managed programs
/etc/nginx/                               admin-managed nginx base config
/usr/local/bin/cloudflared-tunnel         admin-managed tunnel wrapper
/home/wall-e/.config/supervisor.d/*.conf  user/project service snippets
/home/wall-e/.config/nginx/conf.d/*.conf  user/project nginx route snippets
/home/wall-e/sessions/                    wall-e session exports
/var/log/wall-e/*.log                     persisted logs
/var/run/supervisor.sock                  supervisor control socket
```

No `enabled/` / `available/` split. Active user services are simply
`~/.config/supervisor.d/*.conf`. To disable one, stop it and rename its snippet
from `name.conf` to `name.conf.disabled`.

## Supervisor config

`/etc/supervisor/supervisord.conf`:

```ini
[unix_http_server]
file=/var/run/supervisor.sock
chmod=0700
chown=wall-e:wall-e

[supervisord]
nodaemon=true
logfile=/var/log/wall-e/supervisord.log
pidfile=/var/run/supervisord.pid
childlogdir=/var/log/wall-e

[rpcinterface:supervisor]
supervisor.rpcinterface_factory = supervisor.rpcinterface:make_main_rpcinterface

[supervisorctl]
serverurl=unix:///var/run/supervisor.sock

[include]
files = /etc/supervisor/conf.d/*.conf /home/wall-e/.config/supervisor.d/*.conf
```

`/etc/supervisor/conf.d/wall-e.conf`:

```ini
[program:wall-e]
command=/usr/local/bin/wall-e
directory=/home/wall-e
user=wall-e
autostart=true
autorestart=true
stopasgroup=true
killasgroup=true
stopsignal=TERM
stdout_logfile=/dev/stdout
stdout_logfile_maxbytes=0
stderr_logfile=/dev/stderr
stderr_logfile_maxbytes=0
```

`wall-e` receives its environment from the container. Defaults such as
`HOME`, `PI_CODING_AGENT_DIR`, `WALLE_SESSION_DIR`, and `WALLE_SITE` should be
set with Docker `ENV`, not hardcoded in the supervisor snippet.

## Nginx routing

Nginx is the in-container local router. It is supervised by `supervisord`.
The base config is admin-managed under `/etc/nginx`, listens on
`127.0.0.1:80`, and may include user/project route snippets from
`~/.config/nginx/conf.d/*.conf` inside its default `server` block. It does not
terminate public TLS in v1; Cloudflare handles external access.

`/etc/supervisor/conf.d/nginx.conf`:

```ini
[program:nginx]
command=/usr/sbin/nginx -g 'daemon off;'
autostart=true
autorestart=true
stopsignal=QUIT
stdout_logfile=/var/log/wall-e/nginx.out.log
stderr_logfile=/var/log/wall-e/nginx.err.log
```

Example user route in `~/.config/nginx/conf.d/acme-site.conf`:

```nginx
location /acme/ { proxy_pass http://127.0.0.1:3101/; }
```

Reload after routing changes:

```sh
nginx -t && supervisorctl signal HUP nginx
```

## User/project service workflow

The code location is project-specific and should be under `/home/wall-e`. This
example uses `~/services/acme-site/app`, but that is not a required layout.

`~/.config/supervisor.d/acme-site.conf`:

```ini
[program:acme-site]
directory=/home/wall-e/services/acme-site/app
command=/usr/bin/npm start
user=wall-e
autostart=true
autorestart=true
startsecs=3
stopasgroup=true
killasgroup=true
environment=PORT="3101",NODE_ENV="production",HOME="/home/wall-e"
stdout_logfile=/var/log/wall-e/acme-site.out.log
stderr_logfile=/var/log/wall-e/acme-site.err.log
```

Apply, inspect, restart, or disable:

```sh
supervisorctl reread && supervisorctl update
supervisorctl status
supervisorctl tail -f acme-site stderr
supervisorctl restart acme-site
supervisorctl stop acme-site
mv ~/.config/supervisor.d/acme-site.conf ~/.config/supervisor.d/acme-site.conf.disabled
supervisorctl reread && supervisorctl update
```

## Cloudflare tunnel routing

External access comes through a Cloudflare tunnel, supervised by `supervisord`
as an admin service. Cloudflare routes to nginx on `127.0.0.1:80`, and nginx
routes to project services on loopback high ports. No in-container TLS,
low-port binding, or public Docker port exposure is required for project
services.

The tunnel runs in token (remotes-managed) mode: routing rules live in the
Cloudflare dashboard, so no local `/etc/cloudflared/config.yml` is needed in
v1. The only requirement is that `CLOUDFLARE_TOKEN` is present in the
container environment.

### Conditional startup

Supervisord `[program]` blocks cannot be conditionally included based on the
environment, so the cloudflared program always loads but delegates to a
wrapper that no-ops cleanly when the token is missing:

`/usr/local/bin/cloudflared-tunnel` (copied from
`static/etc/cloudflared/run-tunnel.sh`):

```sh
#!/bin/sh
set -eu
if [ -z "${CLOUDFLARE_TOKEN:-}" ]; then
    echo "cloudflared: CLOUDFLARE_TOKEN not set; tunnel disabled." >&2
    exit 0
fi
exec cloudflared tunnel run --token "$CLOUDFLARE_TOKEN"
```

`/etc/supervisor/conf.d/cloudflared.conf`:

```ini
[program:cloudflared]
command=/usr/local/bin/cloudflared-tunnel
user=wall-e
autostart=true
autorestart=unexpected
exitcodes=0
startsecs=0
stopasgroup=true
killasgroup=true
stopsignal=TERM
stdout_logfile=/var/log/wall-e/cloudflared.out.log
stdout_logfile_maxbytes=0
stderr_logfile=/var/log/wall-e/cloudflared.err.log
stderr_logfile_maxbytes=0
```

`autorestart=unexpected` with `exitcodes=0` plus `startsecs=0` gives the
desired semantics:

- **No `CLOUDFLARE_TOKEN`:** the wrapper exits `0` immediately. Because `0`
  is a listed exit code and `autorestart=unexpected` only restarts on
  *unexpected* exits, supervisord leaves the program `EXITED` (effectively
  disabled). `startsecs=0` prevents that fast exit from being treated as a
  failed start.
- **Token present:** the wrapper `exec`s `cloudflared`, which runs forever.
  If it crashes (non-zero exit), supervisord restarts it.

At runtime you can still force the tunnel on or off regardless of the env
var, since it is just another supervised program:

```sh
supervisorctl status cloudflared
supervisorctl start cloudflared   # only useful if CLOUDFLARE_TOKEN is set
supervisorctl stop cloudflared
```

## Agent guidance skill

Ship `static/skills/supervisord/SKILL.md` explaining that agent-managed service
snippets live in `~/.config/supervisor.d/*.conf`, admin services live in
`/etc/supervisor/conf.d/*.conf`, logs are in `/var/log/wall-e/`, nginx base
config lives in `/etc/nginx/`, user routes may live in
`~/.config/nginx/conf.d/*.conf`, and project services bind to loopback high ports.

## Dockerfile changes

Install `supervisor` and `nginx`; create `/etc/supervisor/conf.d`,
`/home/wall-e/.config/supervisor.d`, `/home/wall-e/.config/nginx/conf.d`,
`/home/wall-e/sessions`, and `/var/log/wall-e`; ensure `wall-e` can use `/var/run/supervisor.sock` and write
logs; copy `supervisord.conf`, `wall-e.conf`, `nginx.conf`, and
`cloudflared.conf`; copy `run-tunnel.sh` to `/usr/local/bin/cloudflared-tunnel`
(mode `0555`); keep `tini` as entrypoint; change `CMD` to
`supervisord -c /etc/supervisor/supervisord.conf`.

## Safety rules

- Run project services as `wall-e`.
- Keep `/var/run/supervisor.sock` mode `0700`.
- Do not expose supervisor over TCP.
- Public routes must be explicit in Cloudflare/nginx config.
- User-writable nginx snippets are trusted config; keep the base include in `/etc/nginx` explicit.
