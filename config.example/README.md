# Configuration reference

English | [简体中文](README_CN.md)

Most settings can be changed in the web interface. For manual configuration, use the JSON files in this directory as examples. Replace the downloader and Generic source URLs before use; example content sources are disabled by default.

`aninode init` creates missing base configuration. `run` and `serve` also initialize the base structure when needed. Configuration uses strict JSON, without comments or unknown fields.

```text
config/
├── organizer.json
├── clients/
│   └── <id>.json
└── sources/
    └── <id>.json
```

## Media directories: `organizer.json`

```json
{
  "source": "/media/downloads",
  "target": "/media/library",
  "extensions": [".mkv", ".mp4", ".avi", ".mov", ".m4v", ".ts", ".webm"]
}
```

| Field | Meaning |
|---|---|
| `source` | Absolute download root; uses `TV/` and `Movies/` below it |
| `target` | Absolute library root; uses `TV/` and `Movies/` below it |
| `extensions` | Recognized media extensions, with or without a leading dot |

The roots must be disjoint trees below a common non-root directory and support hardlinks between them. In containers, use one `/media` mount. See [container setup](../docs/CONTAINER_DATA.md).

## Downloader: `clients/*.json`

At most one downloader configuration file is allowed. Its filename must match `id`, for example `clients/qbittorrent.json`:

```json
{
  "id": "qbittorrent",
  "type": "qbittorrent",
  "url": "http://qbittorrent:8080",
  "username": "admin",
  "path_mappings": [
    {"remote": "/downloads", "local": "/media/downloads"}
  ],
  "enabled": true
}
```

| Field | Meaning |
|---|---|
| `id` | Configuration name, matching the filename |
| `type` | `qbittorrent`, `transmission`, or `aria2` |
| `url` | Downloader HTTP(S) address or RPC endpoint |
| `username` | qBittorrent or Transmission username |
| `path_mappings` | Translate downloader-visible `remote` paths to aninode-visible absolute `local` paths |
| `enabled` | Whether to enable this downloader |

The enabled downloader serves all works. Set or replace its password / RPC secret in the WebUI. Client JSON contains no secret material; recoverable integration secrets are encrypted under `/config/secrets/integrations/`.

Keep incomplete-file markers enabled: `.!qB`, `.part`, or adjacent `.aria2` files. Without these markers, an unfinished file may appear ready to organize.

## Content sources: `sources/*.json`

The filename must match `id`. Built-in sources use the same ID as their provider: `mikan`, `dmhy`, or `nyaa`.

```json
{
  "id": "mikan",
  "provider": "mikan",
  "enabled": true,
  "priority": 5
}
```

| Field | Meaning |
|---|---|
| `id` | Source name, matching the filename |
| `provider` | `mikan`, `dmhy`, `nyaa`, or `generic` |
| `rss` | List of feeds, each with `url` and an optional `name` |
| `search` | Generic only; contains `url_template` |
| `priority` | Lower numbers take precedence for releases targeting the same episode |
| `enabled` | Whether to enable this source |

Built-in sources supply their own RSS and search endpoints. Set `rss` to replace their default feeds. Generic supports RSS, search, or both:

```json
{
  "id": "custom",
  "provider": "generic",
  "rss": [{"url": "https://example.com/anime.xml"}],
  "search": {"url_template": "https://example.com/search?q={query}"},
  "enabled": false,
  "priority": 50
}
```

`url_template` must contain exactly one `{query}`. Replace the placeholder URLs above. Search responses must be supported by the Generic source parser; an arbitrary website page is not necessarily a compatible search endpoint.

## Work rules: `.aninode.json`

Rules live beside source files, outside `config/`:

- Series: `<source>/TV/<work directory>/.aninode.json`
- Movies: `<source>/Movies/<work directory>/.aninode.json`

All seasons of a series share one file at the work root. For example:

```json
{
  "title": "Example",
  "year": 2026,
  "sources": ["mikan"],
  "filters": {"groups": ["ANi"]},
  "folders": {
    "Season 01": {"season": 1}
  }
}
```

| Field | Meaning |
|---|---|
| `title` | Work title |
| `year` | Optional year |
| `enabled` | Defaults to enabled; `false` disables automatic management for this work |
| `sources` | Source IDs; omitted or empty means all enabled sources |
| `filters` | Allow-lists for `groups`, `resolutions`, and `subtitles` |
| `output.title` | Library title override; otherwise uses the work title |
| `blacklist` | Case-insensitive globs excluding source basenames or relative paths |
| `folders` | Target season and episode offset for each first-level series folder |
| `specials` | Explicit season/episode mappings for unrecognized filenames |
| `movie.classify` | Movie classification overrides keyed by source-relative path |

Each filter dimension is independent: omitted or empty arrays allow all values; nonempty arrays allow only the listed values. Existing file traits are not automatically saved as filters.

### Correcting season and episode numbers

```json
{
  "title": "Example",
  "folders": {
    "Batch A": {"season": 2, "episode_offset": -10}
  },
  "specials": [
    {"source": "OVA.mkv", "season": 0, "episode": 1}
  ]
}
```

Each `folders` key is an actual first-level directory name under the series root. `season` is the target season (0–99); the target episode is the parsed number plus `episode_offset`. Episode 13 in `Batch A` above becomes season 2, episode 3. `specials.source` must match the exact basename.

These rules change library paths without moving source files. Reconciliation adds inferred mappings for new folders and removes rules for folders that no longer exist. Correct inferred mappings manually when needed.

### Movie classification

Allowed `movie.classify` values are `version`, `extra`, `trailer`, `interview`, `featurette`, `deleted_scene`, `behind_the_scenes`, `exclude`, and `auto`. For example:

```json
{
  "title": "Example Movie",
  "movie": {
    "classify": {"preview.mkv": "trailer", "sample.mkv": "exclude"}
  }
}
```

## Validation

```sh
aninode check --config-dir ./config
```

This reads configuration and declarations, validates fields and references, and prints counts without modifying configuration. It does not test downloader authentication; check connection status in the web interface.
