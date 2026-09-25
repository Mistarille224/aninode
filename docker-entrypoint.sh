#!/bin/sh
set -eu

runtime_uid="${PUID:-911}"
runtime_gid="${PGID:-911}"
runtime_umask="${UMASK:-022}"

for runtime_id in "$runtime_uid" "$runtime_gid"; do
  case "$runtime_id" in
    ''|*[!0-9]*)
      echo "PUID and PGID must be positive numeric IDs" >&2
      exit 64
      ;;
  esac
done

if [ "$runtime_uid" -eq 0 ] || [ "$runtime_gid" -eq 0 ]; then
  echo "PUID and PGID must not be 0" >&2
  exit 64
fi

case "$runtime_umask" in
  [0-7][0-7][0-7]|[0-7][0-7][0-7][0-7]) ;;
  *)
    echo "UMASK must be a 3- or 4-digit octal value (for example 022 or 002)" >&2
    exit 64
    ;;
esac
umask "$runtime_umask"

if [ "$(id -u)" -ne 0 ]; then
  echo "Container must start as root; use PUID/PGID for the runtime identity." >&2
  exit 77
fi

# Docker may create a missing bind-mount source as root on first start.
mkdir -p /config
chown -R "$runtime_uid:$runtime_gid" /config
chmod u+rwx /config

if [ "${1:-}" = "serve" ]; then
  # These variables are bootstrap defaults only. `aninode init` uses them when
  # organizer.json is first created and never overwrites persisted /config.
  su-exec "$runtime_uid:$runtime_gid" /usr/local/bin/aninode init \
    --config-dir /config \
    --source "${ANINODE_INIT_SOURCE:-/media/downloads}" \
    --target "${ANINODE_INIT_TARGET:-/media/library}" \
    --extensions "${ANINODE_INIT_EXTENSIONS:-.mkv,.mp4,.avi,.mov,.m4v,.ts,.webm}" \
    >/dev/null

  # Validate existing persisted media roots before HTTP starts. Missing roots are
  # deferred so first-run setup can stay reachable; invalid existing roots fail.
  su-exec "$runtime_uid:$runtime_gid" /usr/local/bin/aninode preflight --config-dir /config >/dev/null

  shift
  if [ -n "${ANINODE_INTERVAL:-}" ]; then
    # Explicit command-line flags come later and therefore can override the
    # container defaults when a one-off run needs to differ.
    exec su-exec "$runtime_uid:$runtime_gid" /usr/local/bin/aninode serve --listen 0.0.0.0:7391 --interval "$ANINODE_INTERVAL" "$@"
  fi
  exec su-exec "$runtime_uid:$runtime_gid" /usr/local/bin/aninode serve --listen 0.0.0.0:7391 "$@"
fi

exec su-exec "$runtime_uid:$runtime_gid" /usr/local/bin/aninode "$@"
