package application

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"time"

	"aninode/internal/catalog"
	"aninode/internal/configstore"
	"aninode/internal/discovery"
	"aninode/internal/filesystem"
	"aninode/internal/mediafile"
	"aninode/internal/medianame"
	"aninode/internal/naming"
	"aninode/internal/release"
	"aninode/internal/releasefilter"
)

// RSSSourceObservation describes the age and completeness of a disposable source
// snapshot. UpdatedAt is the time of the displayed data, not proof of availability.
type RSSSourceObservation struct {
	SourceID    string    `json:"source_id"`
	Status      string    `json:"status"`
	UpdatedAt   time.Time `json:"updated_at,omitzero"`
	AttemptedAt time.Time `json:"attempted_at,omitzero"`
	Stale       bool      `json:"stale"`
}

type rssObservation struct {
	config configstore.ContentSource
	groups []discovery.Group
	state  RSSSourceObservation
	err    error
}

func sameRSSConfig(a, b configstore.ContentSource) bool {
	return a.Provider == b.Provider && slices.Equal(a.RSS, b.RSS)
}

// discardRSSObservations runs only after a validated configuration replacement.
// Removed, disabled or changed feeds must never surface data from the old source.
func (a *App) discardRSSObservations(rt *runtimeSnapshot) {
	a.rssMu.Lock()
	defer a.rssMu.Unlock()
	for id, observation := range a.rssObservations {
		src, ok := rt.sources[id]
		if !ok || !src.Config.Enabled || !src.Capabilities().RSS || !sameRSSConfig(observation.config, src.Config) {
			delete(a.rssObservations, id)
		}
	}
}

// recordRSSObservation publishes each completed source independently, so a slow
// provider never prevents readers from seeing other providers' new results.
// Snapshots are immutable after publication and are never acquisition input.
func (a *App) recordRSSObservation(cfg configstore.ContentSource, values []release.Release, err error) {
	groups := discovery.GroupReleases(cloneRSSReleases(values), cfg.ID)
	now := a.clock()
	a.rssMu.Lock()
	defer a.rssMu.Unlock()
	if a.rssObservations == nil {
		a.rssObservations = make(map[string]rssObservation)
	}
	previous := a.rssObservations[cfg.ID]
	if !sameRSSConfig(previous.config, cfg) {
		previous = rssObservation{}
	}
	observation := rssObservation{config: cfg, groups: groups, err: err, state: RSSSourceObservation{SourceID: cfg.ID, Status: "ready", AttemptedAt: now, UpdatedAt: now}}
	if err != nil {
		observation.state.Status = "partial"
		if len(values) == 0 {
			observation.groups = previous.groups
			observation.state.UpdatedAt = previous.state.UpdatedAt
			observation.state.Stale = !previous.state.UpdatedAt.IsZero()
			observation.state.Status = "error"
		}
	}
	a.rssObservations[cfg.ID] = observation
}

// RefreshRSS updates only remote observations under the existing writer boundary.
// The server's serialized runner owns its lifetime; HTTP reads never call it.
func (a *App) RefreshRSS(ctx context.Context) (finalErr error) {
	observed := false
	defer func() {
		if !observed {
			a.rssMu.Lock()
			a.rssRefreshErr = finalErr
			a.rssMu.Unlock()
		}
	}()
	return a.withOperation(ctx, func() error {
		if err := a.reloadRuntime(); err != nil {
			return err
		}
		rt := a.snapshot()
		if rt == nil {
			return errors.New("runtime unavailable")
		}
		observed = true
		_, warnings := a.ingestSources(ctx, rt)
		// Preserve typed source errors for cancellation/deadline inspection. The
		// per-source snapshots carry the same errors for the discovery projection.
		var errs []error
		if len(warnings) > 0 {
			a.rssMu.RLock()
			for _, id := range configstore.EnabledSourceIDs(rt.bundle) {
				if observation, ok := a.rssObservations[id]; ok && observation.err != nil {
					errs = append(errs, fmt.Errorf("read %s RSS: %w", id, observation.err))
				}
			}
			a.rssMu.RUnlock()
		}
		return errors.Join(append(errs, ctx.Err())...)
	})
}

// Release projections contain nested slices; isolate both producers and readers
// from the shared snapshot so neither can mutate a cached observation.
func cloneRSSReleases(values []release.Release) []release.Release {
	out := slices.Clone(values)
	for i := range out {
		out[i].Components.Metadata = slices.Clone(out[i].Components.Metadata)
		out[i].Differences = slices.Clone(out[i].Differences)
		for j := range out[i].Differences {
			out[i].Differences[j].Values = slices.Clone(out[i].Differences[j].Values)
		}
	}
	return out
}

func (a *App) entryFilterOptionsObserved(rt *runtimeSnapshot, w catalog.Entry, source filesystem.Snapshot, names naming.Index) releasefilter.Options {
	extset := mediafile.ExtensionSet(rt.bundle.Organizer.Extensions)
	paths := make([]string, 0, len(source.Paths))
	for path := range source.Paths {
		if mediafile.HasAllowedExtension(path, extset) {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	mediaNames := make([]string, len(paths))
	for i, path := range paths {
		mediaNames[i] = filepath.Base(path)
	}
	out := releasefilter.OptionsFromNames(medianame.AnalyzeFiles(mediaNames).Items).WithFilters(w.Filters)
	resolver := discovery.Resolver{}
	a.rssMu.RLock()
	defer a.rssMu.RUnlock()
	for _, observation := range a.rssObservations {
		for _, group := range observation.groups {
			resolved := resolver.Resolve(group, rt.bundle.Entries, names)
			if resolved.Matched && resolved.EntryKey == w.Key {
				out = out.Merge(group.Options)
			}
		}
	}
	return out
}

// entryFilterOptions projects disposable observations into UI choices without
// turning them into durable state. The user's explicit filters are always kept,
// existing media contributes everything the parser can observe, and current RSS
// snapshots can only add choices because they are read before candidate filters.
func (a *App) entryFilterOptions(rt *runtimeSnapshot, w catalog.Entry) releasefilter.Options {
	out := catalog.FilterOptionsFromEvidence(catalog.ObserveFolderProjectionEvidence(w, rt.bundle.Organizer.Extensions)).WithFilters(w.Filters)
	if rt == nil {
		return out
	}
	names, err := runtimeNamingIndex(rt)
	if err != nil {
		return out
	}
	resolver := discovery.Resolver{}
	a.rssMu.RLock()
	defer a.rssMu.RUnlock()
	for _, observation := range a.rssObservations {
		for _, group := range observation.groups {
			resolved := resolver.Resolve(group, rt.bundle.Entries, names)
			if !resolved.Matched || resolved.EntryKey != w.Key {
				continue
			}
			out = out.Merge(group.Options)
		}
	}
	return out
}
