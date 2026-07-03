#!/bin/sh
# Cloudflare tunnel admin service wrapper.
#
# Runs `cloudflared tunnel run` only when CLOUDFLARE_TOKEN is set in the
# environment. When the token is absent this script exits 0 immediately so
# supervisord (configured with autorestart=unexpected, exitcodes=0, startsecs=0)
# leaves the program in the EXITED state instead of restarting it.
set -eu

if [ -z "${CLOUDFLARE_TOKEN:-}" ]; then
    echo "cloudflared: CLOUDFLARE_TOKEN not set; tunnel disabled." >&2
    exit 0
fi

exec cloudflared tunnel run --token "$CLOUDFLARE_TOKEN"
