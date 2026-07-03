---
name: nginx
description: |
    Configure local nginx routes for project services and static files
    in the wall-e container.
---

# Nginx Routes

Nginx is the container's front door and reverse proxy. The base config listens
on `0.0.0.0:80` so it can be exposed to the host (or an upstream gateway) via
Docker `-p <host-port>:80`. nginx is the *only* thing that should bind a
non-loopback address. Project services must listen on `127.0.0.1` (loopback)
only and are reached by nginx over the container's loopback interface.

## Files

Agent-editable:

- User/project route snippets: `~/.config/nginx/conf.d/*.conf`

Read but, do not edit:

- Base config: `/etc/nginx/nginx.conf`
- Admin routes: `/etc/nginx/conf.d/*.conf`

Other locations:

- Logs: `/var/log/wall-e/nginx.*.log`

Reload after any route change:

```sh
nginx -t && supervisorctl signal HUP nginx
```

## Expose a service

First run the application under supervisor on `127.0.0.1:<app-port>` only. Then
create or edit only `~/.config/nginx/conf.d/<name>.conf`:

```nginx
location /<name>/ {
    proxy_pass http://127.0.0.1:3101/;
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
}
```

Notes:

- User snippets are included inside the base `server` block; define `location`
  blocks, not `server` blocks.
- The base nginx listener is `0.0.0.0:80`; host/gateway exposure is set with
  Docker `-p <host-port>:80` (and may sit behind Cloudflare upstream).
- The trailing slash on `proxy_pass .../` strips the matched location prefix.
- Never bind **project services** to `0.0.0.0`, `::`, a public interface, or a
  low/public port. nginx itself is the only listener allowed on `0.0.0.0`.
- Do not configure public TLS or Cloudflare directly unless explicitly asked;
  if Cloudflare fronts the container it talks to nginx's `0.0.0.0:80`.

## Serve static files

Use `alias` for a URL prefix mapped to a directory:

```nginx
location /assets/ {
    alias /home/wall-e/path/to/assets/;
    try_files $uri =404;
}
```

Use `root` for a whole static site:

```nginx
location /site/ {
    root /home/wall-e/path/to/public-parent;
    try_files $uri $uri/ /site/index.html;
}
```

Rules:

- Ensure files are readable by the `wall-e` user.
- Use `alias` with matching trailing slashes for prefix-to-directory mappings.
- Use `try_files` so missing paths return 404 or the intended SPA fallback.
