# aninode

English | [简体中文](README_CN.md)

aninode downloads and organizes anime, TV series, and movies into a library for Emby, Plex, or similar media servers. It connects to your existing downloader and creates hardlinks in the library, keeping the source files available for seeding.

It supports qBittorrent, Transmission, and aria2, with releases from Mikan, DMHY, Nyaa, or custom RSS and search sources. The web interface manages works, discovers releases, fills episode gaps, and migrates existing download tasks. You can also use it to organize local files without enabling content sources.

## Install

Run aninode on Linux using the included Docker Compose configuration. Downloads and library files must share a filesystem and be mounted through one common parent directory. Hardlinks cannot cross filesystems; aninode does not fall back to copying.

From the repository directory, prepare the configuration:

```sh
cp .env.example .env
mkdir -p config
```

Edit `.env`:

- `CONFIG_ROOT`: persistent application configuration, default `./config`.
- `MEDIA_ROOT`: the host media directory, such as `/srv/media`.
- `PUID`, `PGID`: the UID and GID of a user with read and write access to that directory.
- `UMASK`: permissions mask for files created by aninode, default `022` (`002` is useful for group-writable NAS shares).
- `ANINODE_BIND`: host address to publish on, default `127.0.0.1` (local host / reverse proxy only).
- `ANINODE_PORT`: the web port, default `7391`.

Start the container. On every start, aninode performs a media preflight as `PUID:PGID`. Existing source/target roots are checked for readability/writability and a real temporary hardlink is created and removed. Missing roots are **not created by preflight** and do not block the first-run WebUI, so you can correct mount paths there. Existing paths with bad permissions or a cross-filesystem layout still fail immediately:

```sh
docker compose up -d
docker compose logs -f aninode
```

The default Compose binding is `127.0.0.1`, so open `http://localhost:7391` on the Docker host or put an HTTPS reverse proxy in front of it. Before the first login, obtain the short-lived one-time setup code from the host:

```sh
docker compose exec aninode aninode setup-code
```

Enter that code and a login password of at least 8 characters in the WebUI. The setup code expires after 10 minutes; running the command again after expiry creates a fresh code. A successful setup deletes the code immediately. Later visits use the normal login page. The first start also creates a basic organizer configuration. Open **Settings** to configure the downloader and enable content sources. The interface defaults to English and loads a matching locale when available.

For direct access on a trusted LAN, set `ANINODE_BIND=0.0.0.0` (or a specific host interface address). Do not expose plain HTTP across an untrusted network; use the HTTPS reverse-proxy example in [container setup](docs/CONTAINER_DATA.md#https-reverse-proxy).

Enter the downloader password or RPC secret in the WebUI downloader settings. Saving takes effect immediately and stores the secret encrypted under `/config/secrets`; downloader JSON contains no plaintext, ciphertext, or secret reference. The same secret store is intended for future integration tokens such as TMDB. The UI only reports whether a credential is set: leave blank to preserve, enter a new value to replace, or select clear to remove it. No separate secret mount or container restart is needed. The downloader URL must be reachable from the aninode container; `localhost` usually does not reach a downloader in another container.

See [container setup](docs/CONTAINER_DATA.md) for mounts, permissions, environment variables, and backups.

## Use

The default layout is shown below. `/media` corresponds to `MEDIA_ROOT` in `.env`.

```text
/media/
├── downloads/
│   ├── TV/
│   │   └── Example (2026)/
│   │       ├── .aninode.json
│   │       └── Season 01/
│   │           └── Example S01E01.mkv
│   └── Movies/
│       └── Example Movie (2026)/
│           └── Example Movie.mkv
└── library/
    ├── TV/
    └── Movies/
```

**Organize local files:** Use a separate directory for each series, with another folder level for its media. Put movies in their own work directories. aninode creates `.aninode.json` files for titles and organization rules, then hardlinks completed, recognizable media into `library`. It assigns a target season to each first-level series folder; correct season numbers and episode offsets in the work settings if needed.

**Follow releases and fill gaps:** Browse RSS or search in **发现**, select a work, and download it. Work settings can restrict release groups, resolutions, and subtitle types. Automatic backfill handles gaps within a known episode range. Use manual range completion when you need to specify the last episode.

**Adopt existing tasks:** Open **迁移** to inspect downloader tasks and confirm their works and directories. Migration can ask the downloader to move data, so review the destination before applying it.

**Connect a media server:** Add `library/TV` and `library/Movies` as series and movie libraries in Emby or Plex.

Local files are checked every minute by default; RSS is checked every 15 minutes. A manual RSS refresh only updates discovery results. Accepting works or running automation performs the corresponding download and organization actions.

Hardlinks share file contents, so editing media through either path affects both. Changing a title or season mapping updates library paths and removes old links to the same file while preserving the source. A different file at the destination is reported as a conflict and is not overwritten.

Keep incomplete-file markers enabled in your downloader: `.!qB` for qBittorrent, `.part` for Transmission, or `.aria2` files for aria2. Otherwise an unfinished file may look ready to organize. aninode does not follow symlinks.

## Configuration and CLI

Global settings live in `config/`; each work's rules live in `.aninode.json` in its source directory. See the [configuration reference](config.example/README.md) for fields and examples.

| Command | Purpose |
|---|---|
| `aninode init` | Create missing base configuration |
| `aninode preflight` | Verify existing media path access and hardlink capability without creating missing roots |
| `aninode serve` | Start the web interface and scheduled tasks |
| `aninode check` | Validate configuration and work declarations |
| `aninode run` | Run one download and organization cycle |
| `aninode migrate` | Preview migration of existing tasks |
| `aninode version` | Print version information |

The configuration directory defaults to `/config`; override it with `--config-dir`. Use `aninode <command> --help` for flags. `migrate --apply` requires a `--group` confirmation key from the preview.

## Development

The backend is Go. HTML, CSS, and JavaScript are embedded in the binary, with no frontend build step. See [go.mod](go.mod) for the required Go version. Frontend checks require Node.js and npm.

```sh
go build -o aninode ./cmd/aninode
go test ./...
npm --prefix internal/server/web test
npm --prefix internal/server/web run check
```

Validate filesystem operations on Linux. For concurrency changes, also run `go test -race ./...`. To build and run the local container:

```sh
docker compose -f compose.yaml -f compose.dev.yaml up -d --build
```

Start with `internal/application` for workflows, `internal/organizer` for file organization, `internal/download` for downloader adapters, `internal/provider` for content sources, and `internal/server` for HTTP and UI code. The [filesystem model](docs/FILESYSTEM_MODEL.md) and [UI notes](internal/server/web/DESIGN.md) cover the constraints relevant to changes.

## License

[Apache-2.0](LICENSE)
