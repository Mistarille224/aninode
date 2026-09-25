# Container setup

The [README](../README.md) covers first startup. This page covers storage, permissions, and configuration changes for the included Compose deployment.

## Mounts and permissions

| Host path | Container path | Use |
|---|---|---|
| `CONFIG_ROOT` (`./config`) | `/config` | Writable global configuration |
| `MEDIA_ROOT` | `/media` | Writable downloads, library files, and work declarations |

Mount the common media parent once. For example, `/srv/media:/media` exposes `/srv/media/downloads` as `/media/downloads` and `/srv/media/library` as `/media/library`. Both subtrees must be on the same filesystem; a nested mount can still prevent hardlinks.

The container starts as root to prepare `/config`, then runs aninode as `PUID:PGID` (default `911:911`). Both IDs must be nonzero. Use these variables instead of Compose's `user` setting. The entrypoint adjusts `/config` ownership but does not change media ownership; give the selected user access to the media directories on the host. `UMASK` defaults to `022`; use `002` when aninode and other NAS services share a writable group. Sensitive data is stored only below `/config/secrets`; no separate secret mount is used.

Login and downloader credentials are managed through the WebUI. The default deployment has no permanent API token or downloader-secret mount; first-run setup uses the short-lived `/config/secrets/auth/setup.json` bootstrap code described below.

## Startup preflight

Before the HTTP service starts, the container runs `aninode preflight` as `PUID:PGID` against the persisted organizer configuration. It creates the configured source and target roots if they are missing and the selected identity has permission, confirms both directories are readable and writable, creates one temporary file in the source, hardlinks it into the target, and removes both probes immediately.

This intentionally makes deployment errors fail fast. If the source and target are on different filesystems, a nested mount breaks the common filesystem, or `PUID:PGID` cannot write either path, the container exits with the failing path in the log instead of starting a partially usable service. You can rerun the same check manually after changing mounts or WebUI paths:

```sh
docker compose exec aninode aninode preflight
```

The included Compose service also sets `no-new-privileges:true`. aninode still starts its entrypoint as root only to prepare `/config`, then permanently drops to `PUID:PGID` for initialization, preflight, and the daemon.

## Paths seen by the downloader

When the downloader sees `/downloads` but aninode sees `/media/downloads`, set:

```json
{
  "path_mappings": [
    {"remote": "/downloads", "local": "/media/downloads"}
  ]
}
```

A mapping translates names only. Both paths must refer to the same files. It does not mount directories or make separate filesystems hardlinkable.

## Environment variables

| Variable | Default | Effect |
|---|---|---|
| `CONFIG_ROOT` | `./config` | Host path for persistent application configuration |
| `MEDIA_ROOT` | `/path/to/media` | Host media mount; replace before starting |
| `ANINODE_BIND` | `127.0.0.1` | Host interface used for the published web port |
| `ANINODE_PORT` | `7391` | Published host port |
| `ANINODE_IMAGE` | `ghcr.io/mistarille224/aninode:latest` | Stable main-line image; pin a version tag if you need reproducible rollbacks |
| `PUID`, `PGID` | `911`, `911` | Runtime user and group |
| `UMASK` | `022` | Runtime file creation mask; `002` is useful for group-writable shares |
| `ANINODE_INTERVAL` | `15m` | RSS/full cycle interval; `0` disables periodic remote checks |
| `ANINODE_INIT_SOURCE` | `/media/downloads` | Initial download root |
| `ANINODE_INIT_TARGET` | `/media/library` | Initial library root |
| `ANINODE_INIT_EXTENSIONS` | `.mkv,.mp4,.avi,.mov,.m4v,.ts,.webm` | Initial recognized media extensions |

`ANINODE_INIT_*` only applies when `organizer.json` is first created. After that, change paths and extensions in the web settings or `/config/organizer.json`. Other deployment variables take effect when the container is recreated with `docker compose up -d`. Disabling periodic remote checks does not disable local file checks.

First-run administrator setup requires two values: a one-time setup code held under `/config/secrets/auth/setup.json` and a new password of at least 8 characters. The setup code is generated automatically, is readable only by the runtime user, expires after 10 minutes, and is deleted immediately after successful setup. Retrieve it from the Docker host with:

```sh
docker compose exec aninode aninode setup-code
```

If it has expired, running the same command creates a fresh 10-minute code. The server stores only the salted PBKDF2-HMAC-SHA256 password hash (600,000 iterations) in `/config/secrets/auth/login.json` with mode `0600`; it does not store the login password. Browser sessions use HttpOnly, SameSite=Strict cookies and expire after seven days. Restarting the server or changing the password invalidates existing sessions; logout invalidates the current session. HTTPS logins use Secure cookies, including TLS reverse proxies that preserve the public Host and Origin headers.

The old versioned API, Bearer token authentication, `--api-token-file`, and `--allow-unauthenticated-management` are removed. `/ui/*` endpoints exist only to serve the WebUI and require its login session; writes also require same-origin requests and the WebUI request header.

Downloader credentials are entered through the WebUI, take effect immediately, and are never returned by the API. Omitting `password` preserves the existing value; a nonempty value replaces it; an empty string clears it. File references and ciphertext are not accepted as API input. Downloader URLs must not contain embedded `user:password@host` credentials; use the separate username and password fields instead.

All sensitive state is kept under `/config/secrets`. WebUI login uses a non-recoverable PBKDF2 verifier under `secrets/auth/`; recoverable integration secrets use AES-256-GCM and a shared `/config/secrets/master.key` (0600). Downloader passwords are stored as encrypted envelopes under `secrets/integrations/downloader/<client>/password.json`; client JSON never contains plaintext, ciphertext, or a secret reference. The authenticated encryption identity includes the secret namespace, name, and field, so ciphertext cannot be moved between downloader passwords, future TMDB tokens, or other integrations.

This layout is intentionally generic: future API tokens can use the same recoverable store, for example `secrets/integrations/tmdb/default/token.json`, without adding another key-management system. Back up the entire `/config` directory. Losing `secrets/master.key` prevents recovery of integration secrets; encryption does not protect against an attacker who can read both the key and encrypted secret files or control the running application.

## HTTPS reverse proxy

The included Compose file publishes aninode on `127.0.0.1:7391` by default. This keeps plain HTTP off the LAN while allowing a reverse proxy on the Docker host to reach it. A complete Caddy example for `aninode.example.com` is:

```text
aninode.example.com {
    reverse_proxy 127.0.0.1:7391
}
```

Point the hostname at the server, install Caddy on the host, save the block in the Caddyfile, and reload Caddy. With normal public DNS and reachable ports 80/443, Caddy obtains and renews the TLS certificate automatically. Keep `ANINODE_BIND=127.0.0.1`; only the reverse proxy should be exposed. Visit `https://aninode.example.com` and complete first-run setup there.

If a reverse proxy runs on another machine, bind aninode only to the specific trusted LAN address when possible (for example `ANINODE_BIND=192.0.2.10`) and firewall port 7391 so only the proxy can reach it. Setting `ANINODE_BIND=0.0.0.0` publishes plain HTTP on every host interface and is intended only for trusted networks.

## Backups and upgrades

Back up all of `CONFIG_ROOT` (including the complete `secrets/` directory) and the media tree (including `.aninode.json` files). Use a backup tool that preserves hardlinks if you want to preserve their shared storage. There is no separate application database or cache volume to restore.

To update the image selected in `.env`:

```sh
docker compose pull
docker compose up -d
```

## Troubleshooting

| Symptom | Check |
|---|---|
| Login fails | Five failures from the same source trigger a 30-second cooldown; other sources are not globally blocked |
| Container exits during preflight | Read the failing source/target path in `docker compose logs aninode`; verify `PUID:PGID`, mount paths, and that both sides share one filesystem |
| Permission denied | `PUID:PGID` can traverse and write the media paths |
| Hardlink or cross-device error | Downloads and library share one filesystem, including any nested mounts |
| Downloader paths cannot be found | Path mappings and the `/media` mount refer to the actual downloaded files |
| Changes to `ANINODE_INIT_*` have no effect | Edit the existing `organizer.json` instead |

Use `docker compose logs aninode` for startup and operation errors. `/healthz` checks the HTTP service; `/readyz` checks readiness. Use a TLS reverse proxy when accessing it over an untrusted network.
