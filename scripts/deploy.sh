#!/usr/bin/env bash
set -euo pipefail

usage() {
    echo "Usage: $0 containers.metrized.com/wall-e:vX.Y.Z [ENV_FILE] [CONFIG_DIR]" >&2
    exit 2
}

image="${1:-}"
env_file="${2:-.env}"
config_dir="${3:-$(dirname "$env_file")}"
container="${WALLE_CONTAINER:-wall-e}"
home_volume="${WALLE_HOME_VOLUME:-walle--home}"

[[ -n "$image" ]] || usage
[[ "${image##*:}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || {
    echo "deploy: use an immutable vX.Y.Z image tag, not latest" >&2
    exit 2
}
[[ -f "$env_file" ]] || { echo "deploy: missing env file: $env_file" >&2; exit 1; }

absolute_path() {
    local path="$1"
    local dir
    dir="$(cd "$(dirname "$path")" && pwd)"
    printf '%s/%s\n' "$dir" "$(basename "$path")"
}

docker_path() {
    if command -v cygpath >/dev/null 2>&1; then
        cygpath -w "$1"
    else
        printf '%s\n' "$1"
    fi
}

env_file="$(absolute_path "$env_file")"
config_dir="$(cd "$config_dir" && pwd)"
env_mount="$(docker_path "$env_file")"

run_args=(
    run -d
    --name "$container"
    --env-file "$env_mount"
    --restart unless-stopped
    -v "$home_volume:/home/wall-e"
    -p 6007:6007
    -p 6080:80
)
for pair in "auth.json:/opt/pi/auth.json" "settings.json:/opt/pi/settings.json"; do
    host_name="${pair%%:*}"
    container_path="${pair#*:}"
    if [[ -f "$config_dir/$host_name" ]]; then
        host_path="$(docker_path "$(absolute_path "$config_dir/$host_name")")"
        run_args+=( -v "$host_path:$container_path:ro" )
    fi
done
run_args+=( "$image" )

echo "Pulling $image before replacing $container..."
docker pull "$image"
previous_image="$(docker inspect --format '{{.Config.Image}}' "$container" 2>/dev/null || true)"
if [[ -n "$previous_image" ]]; then
    echo "Replacing $container (previous image: $previous_image)..."
    docker rm -f "$container" >/dev/null
fi
MSYS_NO_PATHCONV=1 docker "${run_args[@]}" >/dev/null

for _ in $(seq 1 30); do
    if docker exec "$container" curl -fsS http://127.0.0.1:6007/health >/dev/null 2>&1; then
        echo "Started $container with $image"
        docker exec "$container" wall-e --version
        [[ -z "$previous_image" ]] || echo "Rollback image: $previous_image"
        exit 0
    fi
    sleep 1
done

echo "deploy: health check failed; inspect with: docker logs $container" >&2
[[ -z "$previous_image" ]] || echo "deploy: previous image was $previous_image" >&2
exit 1
