#!/usr/bin/env bash
set -euo pipefail

container="${WALLE_CONTAINER:-wall-e}"
volume="${WALLE_HOME_VOLUME:-walle--home}"
output="${1:-./backups}"
helper_image="ubuntu:24.04"

docker volume inspect "$volume" >/dev/null 2>&1 || {
    echo "backup-home: Docker volume not found: $volume" >&2
    exit 1
}
mkdir -p "$output"
output="$(cd "$output" && pwd)"
docker_output="$output"
if command -v cygpath >/dev/null 2>&1; then
    docker_output="$(cygpath -w "$output")"
fi

image="$(docker inspect --format '{{.Config.Image}}' "$container" 2>/dev/null || true)"
[[ -n "$image" ]] || {
    echo "backup-home: container not found: $container" >&2
    exit 1
}
version="$(docker exec "$container" wall-e --version 2>/dev/null || true)"
if [[ -z "$version" ]]; then
    version="$(docker run --rm "$image" wall-e --version)"
fi
was_running="$(docker inspect --format '{{.State.Running}}' "$container")"
restart() {
    if [[ "$was_running" == "true" ]]; then
        docker start "$container" >/dev/null || true
    fi
}
trap restart EXIT
if [[ "$was_running" == "true" ]]; then
    echo "Stopping $container for a consistent snapshot..."
    docker stop "$container" >/dev/null
fi

timestamp="$(date -u +%Y%m%dT%H%M%SZ)"
archive="walle-home--$timestamp.tar.gz"
echo "Archiving $volume to $output/$archive..."
MSYS_NO_PATHCONV=1 docker run --rm \
    -v "$volume:/source:ro" \
    -v "$docker_output:/backup" \
    "$helper_image" \
    tar --numeric-owner -C /source -czf "/backup/$archive" .

(
    cd "$output"
    sha256sum "$archive" > "$archive.sha256"
)
sha="$(awk '{print $1}' "$output/$archive.sha256")"
cat > "$output/$archive.manifest" <<EOF
format=1
created_utc=$timestamp
archive=$archive
archive_sha256=$sha
volume=$volume
image=$image
version=$version
EOF

echo "Verifying backup checksum..."
(cd "$output" && sha256sum -c "$archive.sha256")
echo "Backup complete. Protect the archive and separately back up .env/auth/settings."
