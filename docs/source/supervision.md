# Service supervision

The container uses `tini` as PID 1 and starts `supervisord` as the main command:

```text
/usr/bin/supervisord -c /etc/supervisor/supervisord.conf
```

Supervisor keeps the gateway and in-container local routing processes alive, and also gives agents a standard place to add project services without rebuilding the image.

## Layout

| Path | Purpose |
|---|---|
| `/etc/supervisor/supervisord.conf` | base supervisor config |
| `/etc/supervisor/conf.d/*.conf` | admin-managed programs such as `wall-e` and `nginx` |
| `/home/wall-e/.config/supervisor.d/*.conf` | user/project service snippets |
| `/etc/nginx/` | admin-managed nginx base config (listener `0.0.0.0:80`) |
| `/home/wall-e/.config/nginx/conf.d/*.conf` | user/project nginx route snippets |
| `/home/wall-e/sessions/` | pi transcript/session exports |
| `/var/log/wall-e/` | supervisor, nginx, gateway, and project-service logs |
| `/var/run/supervisor.sock` | local `supervisorctl` socket; no TCP listener |

The base supervisor config includes both admin and user snippet directories:

```ini
[include]
files = /etc/supervisor/conf.d/*.conf /home/wall-e/.config/supervisor.d/*.conf
```

## Admin-managed programs

`wall-e` is managed by `/etc/supervisor/conf.d/wall-e.conf`. The snippet runs `/usr/local/bin/wall-e` as the `wall-e` user and inherits the container environment. Defaults such as `HOME`, `PI_CODING_AGENT_DIR`, `WALLE_SESSION_DIR`, and `WALLE_SITE` come from Docker `ENV`, not from the supervisor snippet.

`nginx` is managed by `/etc/supervisor/conf.d/nginx.conf` and runs in the foreground under supervisor. Nginx is the container's reverse proxy and its only non-loopback listener (`0.0.0.0:80`). It fronts **project services**, which must bind `127.0.0.1` high ports; nginx reaches them over the container's loopback interface. The host (or an upstream gateway such as Cloudflare) reaches nginx via Docker `-p <host-port>:80`. Project services are never bound to `0.0.0.0` and never exposed directly by `-p`.

## Project services

Agents can add services by writing supervisor snippets to:

```text
/home/wall-e/.config/supervisor.d/name.conf
```

Project services should run as `wall-e`, bind only to loopback high ports, and write logs under `/var/log/wall-e/`.

Example:

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

Apply or inspect service changes with:

```sh
supervisorctl reread && supervisorctl update
supervisorctl status
supervisorctl tail -f acme-site stderr
supervisorctl restart acme-site
```

To disable a user service, stop it and rename the snippet so it no longer ends in `.conf`:

```sh
supervisorctl stop acme-site
mv ~/.config/supervisor.d/acme-site.conf ~/.config/supervisor.d/acme-site.conf.disabled
supervisorctl reread && supervisorctl update
```

## Nginx route snippets

The admin nginx config under `/etc/nginx/` explicitly includes project route snippets from:

```text
/home/wall-e/.config/nginx/conf.d/*.conf
```

A project route should proxy to a loopback/high-port service only. The snippet
is included inside the base `server` block, so define `location` blocks, not a
`server` block:

```nginx
location /acme/ { proxy_pass http://127.0.0.1:3101/; }
```

Validate and reload nginx after route changes:

```sh
nginx -t && supervisorctl signal HUP nginx
```

## Safety notes

- Do not expose supervisor over TCP; use the local unix socket only.
- Keep project services as the `wall-e` user and bound to `127.0.0.1` high ports.
- nginx is the only listener allowed on `0.0.0.0`; everything else stays loopback.
- Host/gateway exposure is set with Docker `-p <host-port>:80` (optionally behind Cloudflare); never `-p`-publish a project service's own port.
- Treat user-writable nginx snippets as trusted container-local config.
