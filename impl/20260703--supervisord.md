# User-space service supervision — Implementation Plan

**Date:** 2026-07-03
**Status:** Planning

## 1. Goal

Turn the gateway container into a long-lived, agent-managed workspace where
client-specific services can be created, updated, started, stopped, and routed
without rebuilding the Docker image or restarting the container.

The outer Docker container remains the persistence/sandbox boundary. Inside it,
`supervisord` provides a small user-space service manager for `wall-e`, optional
reverse proxy, and per-client server programs under `/home/wall-e/...`.

Example target shape:

```text
container
├── tini                         PID 1, signal/zombie handling
├── supervisord                  service manager
│   ├── wall-e                   gateway HTTP/chat process
│   ├── nginx or caddy           optional reverse proxy
│   └── client services          node/python/go/etc.
└── /home/wall-e/
    ├── clients/
    │   └── acme-site/
    ├── services/
    │   ├── available/
    │   └── enabled/
    ├── proxy/
    │   └── sites-enabled/
    └── logs/
```

## 2. Non-goals

- Replace Docker/Compose/Kubernetes for normal multi-container production
  deployments.
- Add a full hosting platform or web admin UI in the first version.
- Run systemd inside the container.
- Require container restarts for normal client app deploys.
- Require root-owned service files for day-to-day agent operations.

## 3. Design principles

1. **Docker is the outer sandbox.** The container can be restarted/recreated by
   external infrastructure, but routine app changes happen inside `/home/wall-e`.
2. **Supervisor is the inner service manager.** The agent edits service config
   and calls `supervisorctl reread/update/restart`.
3. **User-space by default.** Client code, service definitions, proxy snippets,
   logs, and runtime state live under `/home/wall-e`.
4. **Hot reload over rebuild.** Updating a Node site should normally mean
   `git pull`, dependency/build steps, `supervisorctl restart <service>`, and
   proxy reload if routing changed.
5. **Keep `wall-e` simple.** Do not make the Go gateway itself responsible for
   supervising arbitrary server processes in v1.

## 4. Tool choice

Use `supervisord` for v1.

Reasons:

- Mature and widely packaged on Ubuntu.
- Simple INI config that is easy for the agent to inspect and modify.
- `supervisorctl` gives clear primitives: `status`, `start`, `stop`, `restart`,
  `reread`, and `update`.
- Works without systemd.
- Good enough for mixed Node/Python/Go/static helper processes.

Alternatives considered:

| Tool | Notes |
|---|---|
| `runit` | Smaller and cleaner, but less self-describing for dynamic agent edits. |
| `s6` / `s6-overlay` | Excellent container option, but higher complexity for this use case. |
| `pm2` | Good for Node-only apps, less appropriate as a mixed-language supervisor. |
| shell scripts / `tmux` / `nohup` | Fine for debugging, not acceptable for durable service management. |

## 5. Directory layout

Create these directories in the image and keep them writable by `wall-e`:

```text
/home/wall-e/clients/              client/application checkouts
/home/wall-e/services/             supervisor config root
/home/wall-e/services/enabled/     active supervisord program snippets
/home/wall-e/services/available/   inactive/template snippets
/home/wall-e/proxy/                reverse-proxy config root
/home/wall-e/proxy/sites-enabled/  active site snippets
/home/wall-e/logs/                 app/proxy/supervisor logs
/home/wall-e/run/                  pid/sock files
```

The service manager should include all active snippets from:

```text
/home/wall-e/services/enabled/*.conf
```

## 6. Supervisor configuration

Install `supervisor` in the runtime image and make it the main process under
`tini`.

Container process tree:

```text
tini
└── supervisord
    ├── wall-e
    ├── nginx/caddy  optional
    └── client apps  optional
```

Base config at:

```text
/etc/supervisor/supervisord.conf
```

Proposed base config:

```ini
[unix_http_server]
file=/home/wall-e/run/supervisor.sock
chmod=0700
chown=wall-e:wall-e

[supervisord]
nodaemon=true
logfile=/home/wall-e/logs/supervisord.log
pidfile=/home/wall-e/run/supervisord.pid
childlogdir=/home/wall-e/logs

[rpcinterface:supervisor]
supervisor.rpcinterface_factory = supervisor.rpcinterface:make_main_rpcinterface

[supervisorctl]
serverurl=unix:///home/wall-e/run/supervisor.sock

[include]
files = /home/wall-e/services/enabled/*.conf
```

Default `wall-e` program snippet:

```ini
[program:wall-e]
command=/usr/local/bin/wall-e
directory=/home/wall-e
autostart=true
autorestart=true
stopasgroup=true
killasgroup=true
stopsignal=TERM
stdout_logfile=/dev/stdout
stdout_logfile_maxbytes=0
stderr_logfile=/dev/stderr
stderr_logfile_maxbytes=0
environment=HOME="/home/wall-e",PI_CODING_AGENT_DIR="/opt/pi",WALLE_SESSION_DIR="/home/wall-e/sessions",WALLE_SITE="/opt/wall-e/www"
```

Notes:

- `stdout_logfile_maxbytes=0` avoids supervisor trying to rotate Docker stdout.
- `stopasgroup`/`killasgroup` ensure child process groups are cleaned up.
- Environment inherited from Docker should still be available; explicit values
  are only for stable defaults.

## 7. Agent service workflow

For a new Node service named `acme-site`:

```sh
mkdir -p ~/clients/acme-site
cd ~/clients/acme-site
git clone <repo> app
cd app
npm ci
npm run build
```

Create `/home/wall-e/services/enabled/acme-site.conf`:

```ini
[program:acme-site]
directory=/home/wall-e/clients/acme-site/app
command=/usr/bin/npm start
autostart=true
autorestart=true
startsecs=3
stopasgroup=true
killasgroup=true
environment=PORT="3101",NODE_ENV="production",HOME="/home/wall-e"
stdout_logfile=/home/wall-e/logs/acme-site.out.log
stderr_logfile=/home/wall-e/logs/acme-site.err.log
```

Apply changes without restarting Docker:

```sh
supervisorctl -c /etc/supervisor/supervisord.conf reread
supervisorctl -c /etc/supervisor/supervisord.conf update
supervisorctl -c /etc/supervisor/supervisord.conf status acme-site
```

Update deployment:

```sh
cd ~/clients/acme-site/app
git pull --ff-only
npm ci
npm run build
supervisorctl restart acme-site
```

Disable service:

```sh
supervisorctl stop acme-site
mv ~/services/enabled/acme-site.conf ~/services/available/acme-site.conf
supervisorctl reread
supervisorctl update
```

## 8. Reverse proxy strategy

Two viable options:

### Option A: nginx

Run nginx under supervisor with a home-rooted config. This is familiar and very
flexible, but non-root nginx should listen on high ports unless the container is
given `CAP_NET_BIND_SERVICE` or Docker maps host `80/443` to container high
ports.

Suggested config root:

```text
/home/wall-e/proxy/nginx.conf
/home/wall-e/proxy/sites-enabled/*.conf
/home/wall-e/logs/nginx-access.log
/home/wall-e/logs/nginx-error.log
```

Supervisor snippet:

```ini
[program:nginx]
command=/usr/sbin/nginx -p /home/wall-e/proxy -c nginx.conf -g 'daemon off;'
autostart=true
autorestart=true
stopsignal=QUIT
stdout_logfile=/home/wall-e/logs/nginx.out.log
stderr_logfile=/home/wall-e/logs/nginx.err.log
```

Reload after route changes:

```sh
nginx -p /home/wall-e/proxy -c nginx.conf -t
nginx -p /home/wall-e/proxy -c nginx.conf -s reload
```

Example site snippet:

```nginx
server {
    listen 8080;
    server_name _;

    location /acme/ {
        proxy_pass http://127.0.0.1:3101/;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
}
```

### Option B: Caddy

Caddy may be simpler for agent-managed dynamic routing, especially if HTTPS is
needed later. It supports concise config and hot reload:

```sh
caddy reload --config /home/wall-e/proxy/Caddyfile
```

For v1, choose one proxy. If nginx is selected, keep the config entirely under
`/home/wall-e/proxy` and expose only a high container port by default.

## 9. Dockerfile changes

- Install `supervisor`.
- Optionally install `nginx` if chosen for v1 proxy support.
- Create writable user-space directories.
- Copy default supervisor snippets.
- Change `CMD` from `wall-e` to `supervisord`.
- Keep `tini` as entrypoint.

Sketch:

```dockerfile
RUN apt update && apt install -y supervisor nginx ...

RUN mkdir -p \
      /home/wall-e/clients \
      /home/wall-e/services/enabled \
      /home/wall-e/services/available \
      /home/wall-e/proxy/sites-enabled \
      /home/wall-e/logs \
      /home/wall-e/run \
    && chown -R wall-e:wall-e /home/wall-e

COPY config/supervisord.conf /etc/supervisor/supervisord.conf
COPY --chown=wall-e:wall-e config/services/wall-e.conf /home/wall-e/services/enabled/wall-e.conf

ENTRYPOINT ["/usr/bin/tini", "--"]
CMD ["/usr/bin/supervisord", "-c", "/etc/supervisor/supervisord.conf"]
```

## 10. Permissions and security

- Prefer running client services as `wall-e`.
- Do not require root for normal service edits.
- Keep supervisor control socket at `/home/wall-e/run/supervisor.sock` with mode
  `0700`.
- Avoid exposing supervisor's HTTP/TCP control interface.
- Keep public reverse-proxy routes explicit; no automatic exposure of every
  process listening on localhost.
- Store per-client secrets in files under `/home/wall-e/clients/<name>/.env` or
  `/home/wall-e/secrets/<name>.env` with restrictive permissions.
- Treat any agent able to edit supervisor configs as privileged within the
  container.

## 11. Observability

Minimum useful commands:

```sh
supervisorctl status
supervisorctl tail -f wall-e stderr
supervisorctl tail -f acme-site stdout
tail -f ~/logs/acme-site.err.log
```

Potential future HTTP/admin helpers in `wall-e`:

- `GET /v1/services` → wrapper around `supervisorctl status`
- `POST /v1/services/{name}/restart`
- `GET /v1/services/{name}/logs`

Do not add these in v1 unless needed; shell access through the agent is enough.

## 12. Testing plan

1. Build image.
2. Run container with existing gateway env vars.
3. Verify `wall-e` starts under supervisor:

   ```sh
   supervisorctl status wall-e
   curl http://localhost:6007/health
   ```

4. Add a toy Node service under `/home/wall-e/clients/hello`.
5. Add a supervisor snippet under `/home/wall-e/services/enabled/hello.conf`.
6. Run `supervisorctl reread && supervisorctl update`.
7. Verify process restart behavior by killing the Node process.
8. If nginx is included, add a route and verify reload without container restart.
9. Stop/restart the container and confirm enabled services come back.

## 13. Open decisions

- Should v1 include nginx, Caddy, or no proxy by default?
- Should client service definitions live only under `/home/wall-e`, or should the
  repo ship templates under `static/services/`?
- Should `wall-e` expose a tiny service-control API later, or should service
  management remain shell-only through the agent?
- Should the image grant low-port bind capability, or should all user-space
  proxies listen on high ports with Docker/host mapping externally?

## 14. Recommended v1 scope

1. Add `supervisor` and run `wall-e` under it.
2. Create the user-space directory layout.
3. Ship a documented example Node service snippet.
4. Defer proxy choice until a concrete site deployment needs it, or include
   nginx only if we know the first deployment requires HTTP routing.

This gives the agent durable service-management primitives immediately while
keeping the change small and reversible.
