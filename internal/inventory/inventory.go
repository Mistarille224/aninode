package inventory

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"aninode/internal/catalog"
	"aninode/internal/episode"
	"aninode/internal/filesystem"
	"aninode/internal/mediafile"
	"aninode/internal/medianame"
	"aninode/internal/moviebundle"
	"aninode/internal/observation"
)

type Availability uint8

const (
	Unknown Availability = iota
	Absent
	Present
)

type SeasonInventory struct {
	Known    bool
	Episodes map[int]struct{}
	Err      error
}
type EntryInventory struct {
	Known     bool
	Available bool
	Episodes  map[string]struct{}
	Seasons   map[int]SeasonInventory
	Err       error
}
type Snapshot struct{ Entries map[string]EntryInventory }

var defaultMediaExtensions = mediafile.ExtensionSet([]string{"mkv", "mp4", "avi", "mov", "m4v", "ts", "webm"})

// Scan inventories only media files. Any media file that cannot be assigned to
// one unambiguous episode makes its season (or, when no season context exists,
// the whole Entry) unknown. Non-media sidecars/subtitles/artwork are ignored.
func Scan(entries map[string]catalog.Entry) Snapshot { return ScanWithExtensions(entries, nil) }
func ScanWithExtensions(entries map[string]catalog.Entry, extensions []string) Snapshot {
	return scan(entries, nil, extensions)
}

// ScanGraph interprets a shared physical observation. It performs no tree walk
// for series Entries, so availability is derived from the same filesystem facts
// used by claim and publication planning in the reconciliation pass.
func ScanGraph(entries map[string]catalog.Entry, graph observation.Graph, extensions []string) Snapshot {
	if !graph.LibraryRoot.RootExists && len(entries) > 0 {
		out := Snapshot{Entries: make(map[string]EntryInventory, len(entries))}
		for id := range entries {
			out.Entries[id] = EntryInventory{Known: false, Err: fmt.Errorf("library namespace is unavailable: %s", graph.LibraryRoot.Root)}
		}
		return out
	}
	return scan(entries, graph.Libraries, extensions)
}

func scan(entries map[string]catalog.Entry, physical map[string]filesystem.Snapshot, extensions []string) Snapshot {
	allowed := defaultMediaExtensions
	if len(extensions) > 0 {
		allowed = mediafile.ExtensionSet(extensions)
	}
	out := Snapshot{Entries: make(map[string]EntryInventory, len(entries))}
	for id, w := range entries {
		wi := EntryInventory{Known: true, Episodes: map[string]struct{}{}, Seasons: map[int]SeasonInventory{}}
		if snapshot, ok := physical[id]; ok && len(snapshot.Issues) > 0 {
			// Keep positive observations, but absence below an unsafe path is not
			// evidence. Series issues are narrowed to a season when the path gives
			// an unambiguous Season NN context; otherwise the Entry is unknown.
			if w.MediaType == catalog.MediaMovie {
				wi.Known = false
				wi.Err = fmt.Errorf("movie filesystem observation is incomplete: %s: %v", snapshot.Issues[0].Path, snapshot.Issues[0].Err)
				out.Entries[id] = wi
				continue
			}
		}
		if w.MediaType == catalog.MediaMovie {
			var bundle moviebundle.Bundle
			var err error
			if snapshot, ok := physical[id]; ok {
				if !snapshot.RootExists {
					out.Entries[id] = wi
					continue
				}
				paths := make([]string, 0, len(snapshot.Paths))
				for path := range snapshot.Paths {
					if mediafile.IsIncompleteArtifact(path) {
						continue
					}
					if mediafile.HasAllowedExtension(path, allowed) {
						meta := snapshot.Objects[snapshot.Paths[path]].Metadata
						if meta.Size <= 0 {
							out.Entries[id] = wi
							goto movieDone
						}
						if _, incomplete := snapshot.Paths[path+".aria2"]; incomplete {
							out.Entries[id] = wi
							goto movieDone
						}
					}
					paths = append(paths, path)
				}
				sort.Strings(paths)
				bundle, err = moviebundle.AnalyzePaths(w.Path, paths, moviebundle.Options{Title: catalog.OutputTitle(w), Year: w.Year, Overrides: w.Movie.Classify})
			} else {
				if _, statErr := os.Stat(w.Path); errors.Is(statErr, os.ErrNotExist) {
					out.Entries[id] = wi
					continue
				} else if statErr != nil {
					wi.Known = false
					wi.Err = statErr
					out.Entries[id] = wi
					continue
				}
				bundle, err = moviebundle.Observe(w.Path, moviebundle.Options{Title: catalog.OutputTitle(w), Year: w.Year, Overrides: w.Movie.Classify})
			}
			if err != nil {
				wi.Known = false
				wi.Err = fmt.Errorf("scan movie entry %s: %w", id, err)
			} else if err = bundle.Validate(); err != nil {
				wi.Known = false
				wi.Err = fmt.Errorf("scan movie entry %s: %w", id, err)
			} else {
				wi.Available = true
			}
			out.Entries[id] = wi
		movieDone:
			continue
		}
		for _, sn := range w.Seasons {
			wi.Seasons[sn] = SeasonInventory{Known: true, Episodes: map[int]struct{}{}}
		}
		containerHints := map[string]int{}
		if snapshot, ok := physical[id]; ok {
			for _, value := range catalog.InferSeriesContainersFromSnapshot(w.Path, snapshot, extensions) {
				containerHints[filepath.Clean(value.Path)] = value.SuggestedSeason
			}
		} else if inferred, inferErr := catalog.InferSeriesContainersWithExtensions(w.Path, extensions); inferErr == nil {
			for _, value := range inferred {
				containerHints[filepath.Clean(value.Path)] = value.SuggestedSeason
			}
		}
		if snapshot, ok := physical[id]; ok && len(snapshot.Issues) > 0 {
			for _, issue := range snapshot.Issues {
				hint, hasHint := seasonFromPath(w.Path, issue.Path, containerHints)
				if !hasHint {
					wi.Known = false
					wi.Err = fmt.Errorf("filesystem observation is incomplete: %s: %v", issue.Path, issue.Err)
					break
				}
				si := wi.Seasons[hint]
				si.Known = false
				si.Episodes = nil
				si.Err = fmt.Errorf("season filesystem observation is incomplete: %s: %v", issue.Path, issue.Err)
				wi.Seasons[hint] = si
			}
			if !wi.Known {
				out.Entries[id] = wi
				continue
			}
		}
		type observedMedia struct {
			path    string
			name    string
			id      filesystem.ObjectID
			hint    int
			hasHint bool
		}
		var observed []observedMedia
		var err error
		if snapshot, ok := physical[id]; ok {
			paths := make([]string, 0, len(snapshot.Paths))
			for path := range snapshot.Paths {
				paths = append(paths, path)
			}
			sort.Strings(paths)
			for _, path := range paths {
				if mediafile.IsIncompleteArtifact(path) || !mediafile.HasAllowedExtension(path, allowed) {
					continue
				}
				if _, incomplete := snapshot.Paths[path+".aria2"]; incomplete {
					continue
				}
				objectID := snapshot.Paths[path]
				meta := snapshot.Objects[objectID].Metadata
				if !meta.IsRegular() || meta.Size <= 0 {
					continue
				}
				hint, hasHint := seasonFromPath(w.Path, path, containerHints)
				observed = append(observed, observedMedia{path: path, name: filepath.Base(path), id: objectID, hint: hint, hasHint: hasHint})
			}
		} else {
			err = filepath.WalkDir(w.Path, func(path string, entry fs.DirEntry, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				if path == w.Path {
					return nil
				}
				if entry.Type()&os.ModeSymlink != 0 {
					return fmt.Errorf("symlink is not a reliable inventory input: %s", path)
				}
				if entry.IsDir() {
					return nil
				}
				ready, readyErr := mediafile.ReadyPath(path, allowed)
				if readyErr != nil {
					return fmt.Errorf("inspect media readiness %s: %w", path, readyErr)
				}
				if !ready {
					return nil
				}
				info, infoErr := os.Lstat(path)
				if infoErr != nil {
					return fmt.Errorf("observe filesystem identity %s: %w", path, infoErr)
				}
				objectID, ok := filesystem.Identity(info)
				if !ok {
					return fmt.Errorf("filesystem identity unavailable: %s", path)
				}
				hint, hasHint := seasonFromPath(w.Path, path, containerHints)
				observed = append(observed, observedMedia{path: path, name: entry.Name(), id: objectID, hint: hint, hasHint: hasHint})
				return nil
			})
		}
		if err == nil && len(observed) > 0 {
			names := make([]string, len(observed))
			for i := range observed {
				names[i] = observed[i].name
			}
			analysis := medianame.AnalyzeFiles(names)
			physicalMeaning := map[filesystem.ObjectID]string{}
			logicalObject := map[string]filesystem.ObjectID{}
			for i, media := range observed {
				hint := -1
				if media.hasHint {
					hint = media.hint
				}
				p, ok := medianame.EpisodeWithSeasonHint(analysis.Items[i], hint)
				if !ok || p.EpisodeStart <= 0 || p.EpisodeEnd < p.EpisodeStart {
					e := fmt.Errorf("media file has ambiguous episode identity: %s", media.path)
					if media.hasHint {
						si := wi.Seasons[media.hint]
						si.Known = false
						si.Episodes = nil
						si.Err = e
						wi.Seasons[media.hint] = si
						continue
					}
					err = e
					break
				}
				season := p.Season
				if season < 0 {
					err = fmt.Errorf("media file has unknown season: %s", media.path)
					break
				}
				meaning := fmt.Sprintf("season=%d,episodes=%d-%d", season, p.EpisodeStart, p.EpisodeEnd)
				if previous, exists := physicalMeaning[media.id]; exists && previous != meaning {
					si := wi.Seasons[season]
					si.Known = false
					si.Episodes = nil
					si.Err = fmt.Errorf("one filesystem object has conflicting episode meanings: %s (%s vs %s)", media.path, previous, meaning)
					wi.Seasons[season] = si
					continue
				}
				physicalMeaning[media.id] = meaning
				conflict := false
				for n := p.EpisodeStart; n <= p.EpisodeEnd; n++ {
					logical := episode.Logical(episode.Key{EntryKey: id, Season: season, EpisodeStart: n, EpisodeEnd: n, Special: season == 0})
					if previous, exists := logicalObject[logical]; exists && previous != media.id {
						si := wi.Seasons[season]
						si.Known = false
						si.Episodes = nil
						si.Err = fmt.Errorf("episode %s has multiple physical filesystem objects", logical)
						wi.Seasons[season] = si
						conflict = true
						break
					}
					logicalObject[logical] = media.id
				}
				if conflict {
					continue
				}
				si := wi.Seasons[season]
				if si.Err != nil {
					continue
				}
				si.Known = true
				if si.Episodes == nil {
					si.Episodes = map[int]struct{}{}
				}
				for n := p.EpisodeStart; n <= p.EpisodeEnd; n++ {
					si.Episodes[n] = struct{}{}
					wi.Episodes[episode.Logical(episode.Key{EntryKey: id, Season: season, EpisodeStart: n, EpisodeEnd: n, Special: season == 0})] = struct{}{}
				}
				wi.Seasons[season] = si
			}
		}
		if err != nil {
			wi.Known = false
			wi.Episodes = nil
			wi.Seasons = nil
			wi.Err = fmt.Errorf("scan entry %s: %w", id, err)
		}
		out.Entries[id] = wi
	}
	return out
}
func seasonFromPath(root, path string, inferred map[string]int) (int, bool) {
	rel, err := filepath.Rel(root, filepath.Dir(path))
	if err != nil {
		return 0, false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) > 0 && parts[0] != "." {
		if season, ok := inferred[filepath.Clean(filepath.Join(root, filepath.FromSlash(parts[0])))]; ok {
			return season, true
		}
	}
	return 0, false
}
func (s Snapshot) Availability(k episode.Key) Availability {
	si := s.Season(k.EntryKey, k.Season)
	if !si.Known {
		return Unknown
	}
	for n := k.EpisodeStart; n <= k.End(); n++ {
		if _, ok := si.Episodes[n]; !ok {
			return Absent
		}
	}
	return Present
}
func (s Snapshot) Season(entryKey string, season int) SeasonInventory {
	wi, ok := s.Entries[entryKey]
	if !ok || !wi.Known {
		return SeasonInventory{Known: false, Err: wi.Err}
	}
	si, ok := wi.Seasons[season]
	if !ok {
		return SeasonInventory{Known: true, Episodes: map[int]struct{}{}}
	}
	return si
}
func (s Snapshot) Missing(entryKey string, season, through int) ([]int, Availability) {
	si := s.Season(entryKey, season)
	if !si.Known {
		return nil, Unknown
	}
	var v []int
	for n := 1; n <= through; n++ {
		if _, ok := si.Episodes[n]; !ok {
			v = append(v, n)
		}
	}
	if len(v) == 0 {
		return v, Present
	}
	return v, Absent
}
func (s Snapshot) RequireKnown() error {
	var es []error
	for _, wi := range s.Entries {
		if !wi.Known {
			if wi.Err != nil {
				es = append(es, wi.Err)
			} else {
				es = append(es, errors.New("filesystem inventory is unknown"))
			}
			continue
		}
		for _, si := range wi.Seasons {
			if !si.Known {
				if si.Err != nil {
					es = append(es, si.Err)
				} else {
					es = append(es, errors.New("season inventory is unknown"))
				}
			}
		}
	}
	return errors.Join(es...)
}
func (a Availability) String() string {
	switch a {
	case Present:
		return "present"
	case Absent:
		return "absent"
	default:
		return "unknown"
	}
}
