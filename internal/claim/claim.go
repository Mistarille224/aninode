// Package claim resolves downloader observations against the current declarations.
// Claims are rebuilt on every run; they are not serialized or persisted.
package claim

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"aninode/internal/catalog"
	"aninode/internal/configstore"
	"aninode/internal/download"
	"aninode/internal/episode"
	"aninode/internal/filesystem"
	"aninode/internal/medianame"
	"aninode/internal/observation"
	"aninode/internal/organizer"
	"aninode/internal/source"
)

type Status string

const (
	Resolved  Status = "resolved"
	Unknown   Status = "unknown"
	Ambiguous Status = "ambiguous"
)

type Claim struct {
	Client, EntryKey string
	Task             download.Task
	SourceEpisode    episode.Key
	TargetEpisode    episode.Key
	Status           Status
	Evidence         []string
	Detail           string
	Physical         filesystem.Snapshot
}
type resolution struct {
	entryKey       string
	source, target episode.Key
}
type fileObservation struct {
	paths    []string
	analysis medianame.Batch
	infos    []fs.FileInfo
	files    []download.File
}

// SourceIndex is an ephemeral inverse index from physical filesystem objects
// to the declared Entry source namespaces that currently contain them.
type SourceIndex struct {
	entries    map[string]catalog.Entry
	owners     map[filesystem.ObjectID][]string
	uncertain  map[string]bool
	evidence   map[string][]catalog.FolderProjectionEvidence
	extensions []string
}

func BuildSourceIndexFromGraph(graph observation.Graph, entries map[string]catalog.Entry, extensions []string) SourceIndex {
	index := SourceIndex{entries: make(map[string]catalog.Entry, len(entries)), owners: map[filesystem.ObjectID][]string{}, uncertain: map[string]bool{}, evidence: map[string][]catalog.FolderProjectionEvidence{}, extensions: append([]string(nil), extensions...)}
	for _, w := range sortedEntries(entries) {
		index.entries[w.Key] = w
		snapshot := graph.Sources[w.Key]
		index.evidence[w.Key] = catalog.ObserveFolderProjectionEvidenceFromSnapshot(w, snapshot, extensions)
		if len(snapshot.Issues) > 0 {
			index.uncertain[w.Key] = true
		}
		for id := range snapshot.Objects {
			index.owners[id] = append(index.owners[id], w.Key)
		}
	}
	return index
}

// RefreshEntry replaces one Entry's ownership facts after a mutation that may
// have moved downloader content into its source namespace.
func (index *SourceIndex) RefreshEntry(w catalog.Entry) error {
	for objectID, owners := range index.owners {
		next := owners[:0]
		for _, owner := range owners {
			if owner != w.Key {
				next = append(next, owner)
			}
		}
		if len(next) == 0 {
			delete(index.owners, objectID)
		} else {
			index.owners[objectID] = next
		}
	}
	delete(index.uncertain, w.Key)
	index.entries[w.Key] = w
	if strings.TrimSpace(w.DeclarationPath) == "" {
		return nil
	}
	observed, err := filesystem.ObserveTreeIfPresent(w.DeclarationPath)
	if err != nil {
		return fmt.Errorf("observe %s source %s: %w", w.Key, w.DeclarationPath, err)
	}
	index.evidence[w.Key] = catalog.ObserveFolderProjectionEvidenceFromSnapshot(w, observed, index.extensions)
	if len(observed.Issues) > 0 {
		index.uncertain[w.Key] = true
	}
	for objectID := range observed.Objects {
		index.owners[objectID] = append(index.owners[objectID], w.Key)
	}
	return nil
}

func BuildSourceIndex(entries map[string]catalog.Entry) (SourceIndex, error) {
	index := SourceIndex{entries: make(map[string]catalog.Entry, len(entries)), owners: map[filesystem.ObjectID][]string{}, uncertain: map[string]bool{}, evidence: map[string][]catalog.FolderProjectionEvidence{}}
	for _, w := range sortedEntries(entries) {
		index.entries[w.Key] = w
		if strings.TrimSpace(w.DeclarationPath) == "" {
			continue
		}
		observed, err := filesystem.ObserveTreeIfPresent(w.DeclarationPath)
		if err != nil {
			return SourceIndex{}, fmt.Errorf("observe %s source %s: %w", w.Key, w.DeclarationPath, err)
		}
		if !observed.RootExists {
			continue
		}
		index.evidence[w.Key] = catalog.ObserveFolderProjectionEvidenceFromSnapshot(w, observed, nil)
		if len(observed.Issues) > 0 {
			index.uncertain[w.Key] = true
		}
		for id := range observed.Objects {
			index.owners[id] = append(index.owners[id], w.Key)
		}
	}
	return index, nil
}

// Resolve uses only the current task, its files, path mappings and declarations.
func Resolve(ctx context.Context, b configstore.Bundle, client string, task download.Task, files []download.File) Claim {
	index, err := BuildSourceIndex(b.Entries)
	if err != nil {
		return Claim{Client: client, Task: task, Status: Unknown, Detail: "Entry source observation failed: " + err.Error()}
	}
	return ResolveWithIndex(ctx, b, index, client, task, files)
}

func ResolveWithIndex(ctx context.Context, b configstore.Bundle, index SourceIndex, client string, task download.Task, files []download.File) Claim {
	c := Claim{Client: client, Task: task, Status: Unknown}
	maps := mappings(b, client)
	observed := observeFiles(files, maps)
	localTask := task
	localTask.SavePath = download.MapPath(task.SavePath, maps)
	localTask.ContentPath = download.MapPath(task.ContentPath, maps)
	sourceObservation, observationErr := source.Observe(localTask, observed.files, nil)
	if observationErr != nil {
		c.Detail = "source observation failed: " + observationErr.Error()
		return c
	}
	taskName := medianame.AnalyzeMany([]string{task.Name}).Items[0]
	taskObjects, objectErr := filesystem.ObservePaths(observed.paths)
	if objectErr != nil {
		c.Detail = "filesystem observation failed: " + objectErr.Error()
		return c
	}
	c.Physical = taskObjects
	owners, ownershipUncertain := index.physicalOwners(taskObjects)
	if ownershipUncertain {
		c.Detail = "source namespace observation is incomplete, so unique filesystem ownership cannot be proven"
		return c
	}
	if len(owners) == 0 {
		c.Detail = "task filesystem objects are not present under any declared Entry source namespace"
		return c
	}
	if len(owners) > 1 {
		c.Status, c.Detail = Ambiguous, fmt.Sprintf("task filesystem objects are present under %d Entry source namespaces", len(owners))
		return c
	}
	ownedWork := owners[0]
	c.EntryKey = ownedWork.Key
	var matches []resolution
	var pathEntries []string
	for _, w := range []catalog.Entry{ownedWork} {
		if w.MediaType == catalog.MediaMovie {
			topology := source.ValidateMovie(b.Organizer.MovieSource(), sourceObservation.ContentRoot, observed.paths)
			if !topology.Valid {
				continue
			}
			cfg, err := b.OrganizerForPublication(w.Key, 0, 0, sourceObservation.ContentRoot)
			if err != nil {
				continue
			}
			pathEntries = append(pathEntries, w.Key)
			if validPlan(ctx, cfg, observed) {
				matches = append(matches, resolution{entryKey: w.Key, source: episode.Key{EntryKey: w.Key}, target: episode.Key{EntryKey: w.Key}})
			}
			continue
		}
		topology := source.ValidateSeriesAtSource(b.Organizer.SeriesSource(), sourceObservation.ContentRoot, observed.paths)
		if !topology.Valid {
			continue
		}
		pathEntries = append(pathEntries, w.Key)
		// If the task is physically inside a declared first-level source folder,
		// that folder projection is the strongest available season-routing fact.
		// Do not enumerate every logical season for a seasonless filename: doing so
		// creates multiple interpretations for absolute-numbered releases such as
		// BLEACH 41..47 stored under the Season 02 source folder.
		if m, ok := resolveFolderProjectedSeriesClaim(ctx, b, w, index.evidence[w.Key], sourceObservation, observed, taskName); ok {
			matches = append(matches, m)
			continue
		}

		for _, season := range catalog.SourceSeasons(w) {
			targetSeason := season
			cfg, err := b.OrganizerForPublication(w.Key, season, targetSeason, sourceObservation.ContentRoot)
			if err != nil {
				continue
			}
			source, ok := sourceEpisode(w.Key, season, taskName, observed.analysis)
			if !ok || !validPlan(ctx, cfg, observed) {
				continue
			}
			target, ok := targetEpisode(w.Key, source, observed.analysis)
			if !ok || target.Season != targetSeason {
				continue
			}
			matches = append(matches, resolution{w.Key, source, target})
		}
	}
	pathEntries = unique(pathEntries)
	matches = uniqueMatches(matches)
	if len(matches) == 1 {
		m := matches[0]
		c.EntryKey, c.SourceEpisode, c.TargetEpisode, c.Status = m.entryKey, m.source, m.target, Resolved
		c.Evidence = []string{"current downloader observation", "filesystem object identity", "declared source namespace", "unique current media declaration"}
		return c
	}
	if len(matches) > 1 || len(pathEntries) > 1 {
		c.Status, c.Detail = Ambiguous, fmt.Sprintf("task has %d declaration interpretations", len(matches))
		return c
	}
	if len(pathEntries) == 1 {
		c.EntryKey = pathEntries[0]
		c.Detail = "enabled Entry path is known but episode identity is not uniquely observable"
	} else {
		c.Detail = "task is outside a uniquely valid declared source topology"
	}
	return c
}

func (index SourceIndex) physicalOwners(task filesystem.Snapshot) ([]catalog.Entry, bool) {
	var candidates map[string]bool
	for objectID := range task.Objects {
		next := map[string]bool{}
		for _, entryKey := range index.owners[objectID] {
			if candidates == nil || candidates[entryKey] {
				next[entryKey] = true
			}
		}
		candidates = next
		if len(candidates) == 0 {
			// An unobserved object could still live below a Entry with an
			// observation issue, so absence is not conclusive.
			return nil, len(index.uncertain) > 0
		}
	}
	// A positive owner is not unique if another Entry was incompletely
	// observed; the hidden subtree could contain another hardlink name.
	for id := range index.uncertain {
		if !candidates[id] {
			return nil, true
		}
	}
	ids := make([]string, 0, len(candidates))
	for id := range candidates {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]catalog.Entry, 0, len(ids))
	for _, id := range ids {
		out = append(out, index.entries[id])
	}
	return out, false
}

func observeFiles(files []download.File, maps []download.PathMapping) fileObservation {
	var paths, names []string
	var infos []fs.FileInfo
	mappedFiles := make([]download.File, 0, len(files))
	for _, f := range files {
		if f.Wanted {
			path := download.MapPath(f.Path, maps)
			if info, err := os.Lstat(path); err == nil && info.Mode().IsRegular() && (f.Size <= 0 || info.Size() == f.Size) {
				paths = append(paths, path)
				names = append(names, filepath.Base(path))
				infos = append(infos, info)
				mapped := f
				mapped.Path = path
				mappedFiles = append(mappedFiles, mapped)
			}
		}
	}
	return fileObservation{paths: paths, analysis: medianame.AnalyzeFiles(names), infos: infos, files: mappedFiles}
}

func validPlan(ctx context.Context, cfg organizer.Config, observed fileObservation) bool {
	if len(observed.paths) == 0 {
		return false
	}
	p, err := organizer.BuildPlanForObservedFiles(ctx, cfg, observed.paths, observed.analysis.Items, observed.infos)
	if err != nil || len(p.Items) == 0 {
		return false
	}
	for _, item := range p.Items {
		if item.Action == organizer.ActionConflict || item.Action == organizer.ActionUnmatched {
			return false
		}
	}
	return true
}

func resolveFolderProjectedSeriesClaim(ctx context.Context, b configstore.Bundle, w catalog.Entry, evidence []catalog.FolderProjectionEvidence, sourceObservation source.Observation, observed fileObservation, taskName medianame.Name) (resolution, bool) {
	if strings.TrimSpace(w.DeclarationPath) == "" || len(w.FolderProjections) == 0 {
		return resolution{}, false
	}
	rel, err := filepath.Rel(filepath.Clean(w.DeclarationPath), filepath.Clean(sourceObservation.ContentRoot))
	if err != nil || rel == "." || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return resolution{}, false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) == 0 || strings.TrimSpace(parts[0]) == "" {
		return resolution{}, false
	}
	folder := parts[0]
	rule, ok := w.FolderProjections[folder]
	if !ok {
		return resolution{}, false
	}

	start, end := 0, 0
	parsedSeason := 0
	seasonKnown := false
	consume := func(n medianame.Name) bool {
		c := n.Components
		if c.EpisodeStart <= 0 {
			return false
		}
		if c.SeasonExplicit || c.Season > 0 {
			if seasonKnown && parsedSeason != c.Season {
				return false
			}
			parsedSeason = c.Season
			seasonKnown = true
		}
		e := c.EpisodeEnd
		if e < c.EpisodeStart {
			e = c.EpisodeStart
		}
		if start == 0 || c.EpisodeStart < start {
			start = c.EpisodeStart
		}
		if e > end {
			end = e
		}
		return true
	}
	seen := false
	for _, n := range observed.analysis.Items {
		if n.Components.EpisodeStart <= 0 {
			continue
		}
		if !consume(n) {
			return resolution{}, false
		}
		seen = true
	}
	if !seen {
		if !consume(taskName) {
			return resolution{}, false
		}
	}

	targetSeason, targetStart, _, ok := catalog.ProjectObservedEpisode(w, evidence, parsedSeason, seasonKnown, start)
	if !ok || targetSeason != rule.Season {
		return resolution{}, false
	}
	endSeason, targetEnd, _, ok := catalog.ProjectObservedEpisode(w, evidence, parsedSeason, seasonKnown, end)
	if !ok || endSeason != targetSeason {
		return resolution{}, false
	}

	sourceSeason := parsedSeason
	if sourceSeason <= 0 && !seasonKnown {
		sourceSeason = 1
	}
	sourceKey := episode.Key{EntryKey: w.Key, Season: sourceSeason, EpisodeStart: start, EpisodeEnd: end, Special: sourceSeason == 0}
	targetKey := episode.Key{EntryKey: w.Key, Season: targetSeason, EpisodeStart: targetStart, EpisodeEnd: targetEnd, Special: targetSeason == 0}

	cfg, err := b.OrganizerForFolderPublication(w.Key, sourceSeason, rule.Season, rule.EpisodeOffset, sourceObservation.ContentRoot)
	if err != nil || !validPlan(ctx, cfg, observed) {
		return resolution{}, false
	}
	return resolution{entryKey: w.Key, source: sourceKey, target: targetKey}, true
}

func sourceEpisode(entryKey string, season int, taskName medianame.Name, analysis medianame.Batch) (episode.Key, bool) {
	start, end := 0, 0
	for _, n := range analysis.Items {
		p, ok := medianame.EpisodeWithSeasonHint(n, season)
		if !ok || p.EpisodeStart <= 0 || ((p.SeasonExplicit || p.Season > 0) && p.Season != season) {
			return episode.Key{}, false
		}
		if start == 0 || p.EpisodeStart < start {
			start = p.EpisodeStart
		}
		if p.EpisodeEnd > end {
			end = p.EpisodeEnd
		}
	}
	if start == 0 {
		p, ok := medianame.EpisodeWithSeasonHint(taskName, season)
		if !ok {
			return episode.Key{}, false
		}
		start, end = p.EpisodeStart, p.EpisodeEnd
	}
	return episode.Key{EntryKey: entryKey, Season: season, EpisodeStart: start, EpisodeEnd: end, Special: season == 0}, end >= start
}

// targetEpisode verifies every observed media mapping, not merely the aggregate
// endpoints. This prevents one season-level source container from claiming
// files whose episode projections cross target seasons.
func targetEpisode(entryKey string, source episode.Key, analysis medianame.Batch) (episode.Key, bool) {
	targetSeason, targetStart, targetEnd := -1, 0, 0
	mapRange := func(start, end int) bool {
		for ep := start; ep <= end; ep++ {
			season, target := source.Season, ep
			if targetSeason < 0 {
				targetSeason = season
			} else if targetSeason != season {
				return false
			}
			if targetStart == 0 || target < targetStart {
				targetStart = target
			}
			if target > targetEnd {
				targetEnd = target
			}
		}
		return true
	}
	seen := false
	for _, name := range analysis.Items {
		parsed, ok := medianame.EpisodeWithSeasonHint(name, source.Season)
		if !ok || parsed.EpisodeStart <= 0 {
			continue
		}
		if !mapRange(parsed.EpisodeStart, parsed.EpisodeEnd) {
			return episode.Key{}, false
		}
		seen = true
	}
	if !seen && !mapRange(source.EpisodeStart, source.EpisodeEnd) {
		return episode.Key{}, false
	}
	return episode.Key{EntryKey: entryKey, Season: targetSeason, EpisodeStart: targetStart, EpisodeEnd: targetEnd, Special: targetSeason == 0}, targetSeason >= 0 && targetStart > 0 && targetEnd >= targetStart
}

func mappings(b configstore.Bundle, client string) []download.PathMapping {
	var out []download.PathMapping
	for _, m := range b.Clients[client].PathMappings {
		out = append(out, download.PathMapping{Remote: m.Remote, Local: m.Local})
	}
	return out
}
func sortedEntries(m map[string]catalog.Entry) []catalog.Entry {
	out := make([]catalog.Entry, 0, len(m))
	for _, w := range m {
		out = append(out, w)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}
func unique(v []string) []string {
	sort.Strings(v)
	out := v[:0]
	for _, x := range v {
		if len(out) == 0 || out[len(out)-1] != x {
			out = append(out, x)
		}
	}
	return out
}
func uniqueMatches(v []resolution) []resolution {
	seen := map[string]bool{}
	out := v[:0]
	for _, m := range v {
		k := fmt.Sprintf("%s/%d/%d/%d/%d/%d/%d", m.entryKey, m.source.Season, m.source.EpisodeStart, m.source.EpisodeEnd, m.target.Season, m.target.EpisodeStart, m.target.EpisodeEnd)
		if !seen[k] {
			seen[k] = true
			out = append(out, m)
		}
	}
	return out
}
