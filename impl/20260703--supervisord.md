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
/etc/cloudflared/                         admin-managed tunnel config, if used
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
The base config is admin-managed under `/etc/nginx`, and may include
user/project route snippets from `~/.config/nginx/conf.d/*.conf`. It does not
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
server {
    listen 127.0.0.1:8080;
    location /acme/ { proxy_pass http://127.0.0.1:3101/; }
}
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

External access comes through a gateway tunnel / Cloudflare process. Cloudflare
routes to nginx, and nginx routes to project services on loopback high ports.

Do not require in-container TLS, low-port binding, or public Docker port exposure
for project services.

Example `/etc/cloudflared/config.yml` route:

```yaml
ingress:
  - hostname: acme.example.com
    service: http://127.0.0.1:8080
  - service: http_status:404
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
logs; copy `supervisord.conf`, `wall-e.conf`, and `nginx.conf`; keep `tini` as
entrypoint; change `CMD` to `supervisord -c /etc/supervisor/supervisord.conf`.

## Safety rules

- Run project services as `wall-e`.
- Keep `/var/run/supervisor.sock` mode `0700`.
- Do not expose supervisor over TCP.
- Public routes must be explicit in Cloudflare/nginx config.
- User-writable nginx snippets are trusted config; keep the base include in `/etc/nginx` explicit.
