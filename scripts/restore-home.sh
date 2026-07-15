#!/usr/bin/env bash
set -euo pipefail

archive="${1:-}"
volume="${2:-walle--home}"
helper_image="ubuntu:24.04"
[[ -n "$archive" && -f "$archive" ]] || {
    echo "Usage: $0 BACKUP.tar.gz [DESTINATION_VOLUME]" >&2
    exit 2
}
archive="$(cd "$(dirname "$archive")" && pwd)/$(basename "$archive")"
dir="$(dirname "$archive")"
name="$(basename "$archive")"
docker_dir="$dir"
if command -v cygpath >/dev/null 2>&1; then
    docker_dir="$(cygpath -w "$dir")"
fi

checksum="$archive.sha256"
[[ -f "$checksum" ]] || {
    echo "restore-home: missing checksum file: $checksum" >&2
    exit 1
}
echo "Verifying backup checksum..."
(cd "$dir" && sha256sum -c "$(basename "$checksum")")

if MSYS_NO_PATHCONV=1 docker run --rm -v "$docker_dir:/backup:ro" "$helper_image" \
    sh -c "tar -tzf '/backup/$name' | grep -Eq '(^/|(^|/)\.\.(/|$))'"; then
    echo "restore-home: archive contains an unsafe path" >&2
    exit 1
fi

docker volume inspect "$volume" >/dev/null 2>&1 || docker volume create "$volume" >/dev/null
if [[ -n "$(docker ps -aq --filter "volume=$volume")" ]]; then
    echo "restore-home: destination volume $volume is attached to a container" >&2
    echo "Stop and remove that test container, or choose a new volume." >&2
    exit 1
fi
if ! docker run --rm -v "$volume:/target" "$helper_image" \
    sh -c 'test -z "$(find /target -mindepth 1 -maxdepth 1 -print -quit)"'; then
    if [[ "${FORCE_RESTORE:-0}" != "1" ]]; then
        echo "restore-home: destination volume $volume is not empty" >&2
        echo "Use a new volume, or set FORCE_RESTORE=1 to erase it explicitly." >&2
        exit 1
    fi
    echo "Erasing existing contents of $volume..."
    docker run --rm -v "$volume:/target" "$helper_image" \
        sh -c 'find /target -mindepth 1 -delete'
fi

echo "Restoring $name into $volume..."
MSYS_NO_PATHCONV=1 docker run --rm \
    -v "$volume:/target" \
    -v "$docker_dir:/backup:ro" \
    "$helper_image" \
    tar --numeric-owner -C /target -xzf "/backup/$name"
echo "Restore complete. Start the image recorded in $name.manifest and verify it before upgrading."
