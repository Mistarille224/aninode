# Filesystem model

This document is for changes to scanning, acquisition, organization, and migration. User configuration is described in the [configuration reference](../config.example/README.md).

## Stored data

Global settings live in `/config`. A work's `.aninode.json` stores its title, source selection, filters, and output rules next to the source media. Availability, downloader ownership, and publication status are rebuilt from the filesystem and downloader APIs.

Catalog request keys such as `series/<directory-name>` and `movie/<directory-name>` come from current directory names. Renaming a directory changes its key. Do not persist these keys as stable work IDs or add task IDs, inode values, or operation progress to declarations.

Configuration and declaration writes use atomic file replacement. Catalog discovery is read-only: invalid JSON is reported rather than silently rewritten. Reconciliation creates missing declarations and updates folder projections.

## Observation and ownership

`internal/filesystem` records objects by `(device, inode)`. `internal/observation` builds a snapshot of the common media namespace and derives source, library, and work views from it. Hardlinked paths therefore refer to one object within the snapshot.

Source and target must be disjoint trees below a common non-root directory and support hardlinks. Symlinks are not followed. An unreadable or ambiguous subtree makes the affected scope unknown; it must not be treated as empty.

Downloader tasks contribute their currently observable file objects. Ownership requires one declared source scope containing the task's objects, followed by topology and episode validation. A task ID, infohash, or matching folder name alone does not establish ownership. Files can still be organized when their downloader task no longer exists.

Re-observe affected source paths after downloader relocation. Publication must use observations taken after acquisition and backfill have finished mutating those paths.

## Episode inventory

Media basenames supply episode information. First-level series folders carry target-season projections in the root declaration; deeper grouping directories do not establish episode identity. A folder rule sets the target season and adds its episode offset to the parsed episode number.

Only configured media extensions count toward episode inventory. Auxiliary files and incomplete downloads do not establish availability. Treat both of these cases as conflicts:

- One physical object is mapped to different episodes.
- Different physical objects are mapped to one episode.

Multiple hardlinks to the same object with the same episode meaning count once. Automatic backfill requires a known missing episode. An unknown tail, unreadable subtree, or ambiguous mapping is not proof of a gap. Manual range completion supplies a temporary episode boundary.

## Hardlink publication

For each canonical library path:

| Destination | Result |
|---|---|
| Missing | Create a hardlink |
| Same device and inode as source | Already organized |
| Different object | Report a conflict |

The Linux implementation walks parent directories with no-follow semantics and links through directory file descriptors. It verifies the planned source identity around the operation. Keep these checks when changing mutation code: a source path may have been replaced since planning.

After proving the canonical link, reconciliation can remove other library links to the same source object within the managed TV/Movies roots and prune emptied directories. This allows title and projection changes to converge without deleting source media. A stale pathname alone is insufficient evidence for removal.

Movie organization also supports versions, extras, ISO files, and opaque `BDMV`/`VIDEO_TS` trees. Disc trees are preserved as hardlinks; no playlist selection or remuxing occurs.

## Migration

An existing declaration fixes the work's source location. A matching task outside that location must be relocated through its downloader. For a new work, persist the declaration after re-observing and placing the task.

Compare file objects before and after relocation. Explicit migration requires the same object set; automatic topology repair may accept newly completed files but must preserve all previously observed objects. A copy-and-delete relocation fails this check because existing hardlinks still point to the old objects.

If migration paused a running task and then failed, attempt to resume it. Declaration relocation uses no-overwrite rename operations; an existing destination is a conflict.

## Coordination and caches

Mutations acquire a nonblocking `flock` on `.aninode.lock` under the config root. A competing writer fails instead of running concurrently. Lock ownership ends when its file description closes; the file's presence does not mean a process is running.

RSS results, parser results, torrent metadata, and rate-limit cooldowns are held in memory. Restarting may require new scans or network requests, but must not lose user settings or alter local availability.

Discovery reads cached RSS results and matches them against the current catalog. A failed refresh may retain old results with their original time and an error; automation must not acquire from that stale snapshot. Manual RSS refresh observes sources only and shares the serialized runner with reconciliation.

## Code and checks

| Area | Package |
|---|---|
| Observation and safe file operations | `internal/filesystem`, `internal/observation` |
| Declarations and configuration | `internal/catalog`, `internal/configstore` |
| Ownership and inventory | `internal/claim`, `internal/inventory` |
| Publication | `internal/organizer`, `internal/moviebundle` |
| Relocation | `internal/migration` |
| Operation ordering | `internal/application` |

Run `go test ./...` on Linux for filesystem changes. Include cases for conflicting targets, replaced paths, symlinks, incomplete files, and inode preservation when changing the corresponding operations. Use `go test -race ./...` for changes to writer coordination or shared caches.
