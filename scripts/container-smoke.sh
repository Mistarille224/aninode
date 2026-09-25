#!/usr/bin/env bash
set -euo pipefail

image="${1:-aninode:smoke}"
root="$(mktemp -d)"
topology="$root/topology"
config="$topology/config"
media="$root/media"
logs="$root/container.log"
uid="${SMOKE_PUID:-12345}"
gid="${SMOKE_PGID:-12346}"
cookie_jar="$root/cookies.txt"

cleanup() {
  status=$?
  trap - EXIT
  set +e

  (cd "$topology" 2>/dev/null && docker compose down -v --remove-orphans >/dev/null 2>&1) || true

  # The entrypoint intentionally chowns bind-mounted config to PUID:PGID.
  # Restore ownership of this smoke test's private temp tree before the host
  # runner removes it; otherwise a non-root runner cannot unlink files from
  # directories that are now owned by the runtime UID with mode 0755.
  if [ -d "$root" ]; then
    host_uid="$(id -u)"
    host_gid="$(id -g)"
    docker run --rm \
      --entrypoint /bin/sh \
      -v "$root:/cleanup" \
      "$image" \
      -c 'chown -R "$1:$2" /cleanup' -- "$host_uid" "$host_gid" \
      >/dev/null 2>&1 || true
    rm -rf "$root"
    cleanup_status=$?
    if [ "$cleanup_status" -ne 0 ]; then
      echo "container smoke cleanup failed: $root" >&2
      if [ "$status" -eq 0 ]; then
        status=$cleanup_status
      fi
    fi
  fi

  exit "$status"
}
trap cleanup EXIT

mkdir -p "$config" "$media/downloads" "$media/library/TV"
cp compose.yaml "$topology/compose.yaml"
cp -R config.example/. "$config/"
chmod -R a+rwX "$media"

printf 'hardlink-smoke\n' > "$media/downloads/hardlink-source"
ln "$media/downloads/hardlink-source" "$media/library/TV/hardlink-target"
test "$(stat -c %i "$media/downloads/hardlink-source")" = "$(stat -c %i "$media/library/TV/hardlink-target")"

port="$(python3 - <<'PY'
import socket
s=socket.socket(); s.bind(('127.0.0.1', 0)); print(s.getsockname()[1]); s.close()
PY
)"
export ANINODE_IMAGE="$image" ANINODE_PORT="$port" MEDIA_ROOT="$media" PUID="$uid" PGID="$gid"

(cd "$topology" && docker compose config -q)
(cd "$topology" && docker compose up -d)
container="$(cd "$topology" && docker compose ps -q aninode)"
test -n "$container"
base="http://127.0.0.1:${port}"

wait_ready() {
  for _ in $(seq 1 60); do
    if curl -fsS "$base/readyz" >/dev/null 2>&1; then
      return 0
    fi

    if ! docker inspect -f '{{.State.Running}}' "$container" 2>/dev/null | grep -qx true; then
      echo "aninode container exited before readiness" >&2
      (cd "$topology" && docker compose ps -a) >&2 || true
      docker logs "$container" >&2 || true
      return 1
    fi
    sleep 0.25
  done

  echo "aninode container did not become ready" >&2
  (cd "$topology" && docker compose ps -a) >&2 || true
  docker logs "$container" >&2 || true
  return 1
}

wait_ready
curl -fsS "$base/healthz" | grep -q '"status":"live"'
curl -fsS "$base/readyz" | grep -q '"ready":true'
curl -fsS "$base/" | grep -qi '<!doctype html'

test "$(curl -sS -o /dev/null -w '%{http_code}' "$base/ui/diagnostics")" = 401

# First-run setup must not be claimable by a network-only caller. Verify the
# host-held one-time setup code is required, then retrieve it from inside the
# container exactly as an operator would.
test "$(curl -sS -o /dev/null -w '%{http_code}' \
  -H 'Content-Type: application/json' -H 'X-Aninode-Request: 1' \
  -d '{"password":"smoke-login-password"}' "$base/auth/setup")" = 401
setup_json="$(docker exec "$container" aninode setup-code)"
setup_code="$(printf '%s' "$setup_json" | python3 -c 'import json, sys; print(json.load(sys.stdin)["setup_code"])')"
test -n "$setup_code"
setup_body="$(printf '{"password":"smoke-login-password","setup_code":"%s"}' "$setup_code")"
curl -fsS -c "$cookie_jar" -H 'Content-Type: application/json' -H 'X-Aninode-Request: 1' \
  -d "$setup_body" "$base/auth/setup" >/dev/null
curl -fsS -b "$cookie_jar" "$base/ui/diagnostics" >/dev/null
curl -fsS -b "$cookie_jar" -H 'X-Aninode-Request: 1' -X POST "$base/auth/logout" >/dev/null
test "$(curl -sS -b "$cookie_jar" -o /dev/null -w '%{http_code}' "$base/ui/diagnostics")" = 401
curl -fsS -c "$cookie_jar" -H 'Content-Type: application/json' -H 'X-Aninode-Request: 1' \
  -d '{"password":"smoke-login-password"}' "$base/auth/login" >/dev/null

pid_uid="$(docker exec "$container" awk '/^Uid:/{print $2}' /proc/1/status)"
pid_gid="$(docker exec "$container" awk '/^Gid:/{print $2}' /proc/1/status)"
test "$pid_uid" = "$uid"
test "$pid_gid" = "$gid"
test "$(stat -c %u "$config")" = "$uid"
test "$(stat -c %g "$config")" = "$gid"

docker exec -u "$uid:$gid" "$container" sh -c 'printf mounted > /media/downloads/runtime-hardlink && ln /media/downloads/runtime-hardlink /media/library/TV/runtime-hardlink'
test "$(docker exec "$container" stat -c %i /media/downloads/runtime-hardlink)" = "$(docker exec "$container" stat -c %i /media/library/TV/runtime-hardlink)"

(cd "$topology" && docker compose stop -t 10 aninode)
docker logs "$container" >"$logs" 2>&1
grep -q 'shutdown requested' "$logs"
grep -q 'daemon stopped' "$logs"

(cd "$topology" && docker compose start aninode)
wait_ready
test "$(curl -sS -b "$cookie_jar" -o /dev/null -w '%{http_code}' "$base/ui/diagnostics")" = 401
curl -fsS -c "$cookie_jar" -H 'Content-Type: application/json' -H 'X-Aninode-Request: 1' \
  -d '{"password":"smoke-login-password"}' "$base/auth/login" >/dev/null
curl -fsS -b "$cookie_jar" "$base/ui/diagnostics" >/dev/null
echo "container/compose smoke PASS: image=$image uid=$uid gid=$gid liveness/readiness/auth/webui/config-mount/hardlink/SIGTERM"
