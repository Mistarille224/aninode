package application

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"aninode/internal/automation"
	"aninode/internal/candidate"
	"aninode/internal/catalog"
	"aninode/internal/configstore"
	"aninode/internal/discovery"
	"aninode/internal/inventory"
	"aninode/internal/naming"
	"aninode/internal/observation"
	"aninode/internal/provider"
	"aninode/internal/release"
)

// Content-source application boundary. Remote discovery is deliberately kept
// separate from filesystem/configuration management: it supplies observations
// and acquisition candidates, but never owns local media state.

type DiscoveryResult struct {
	SourceID string            `json:"source_id"`
	Query    string            `json:"query"`
	Groups   []discovery.Group `json:"groups"`
	Issues   []OperationIssue  `json:"issues,omitempty"`
}

type RSSDiscoveryResult struct {
	Groups     []discovery.Group      `json:"groups"`
	Issues     []OperationIssue       `json:"issues,omitempty"`
	Sources    []RSSSourceObservation `json:"sources"`
	Refreshing bool                   `json:"refreshing"`
}

func (a *App) SearchDiscovery(ctx context.Context, sourceID, query string, limit int) (DiscoveryResult, error) {
	rt := a.snapshot()
	if rt == nil {
		return DiscoveryResult{}, errors.New("runtime unavailable")
	}
	src, ok := rt.sources[sourceID]
	if !ok {
		return DiscoveryResult{}, managementErr(ErrorNotFound, fmt.Errorf("content source %q not found", sourceID))
	}
	if !src.Config.Enabled {
		return DiscoveryResult{}, managementErr(ErrorInvalid, fmt.Errorf("content source %q is disabled", sourceID))
	}
	if !src.DiscoveryCapable() {
		return DiscoveryResult{}, managementErr(ErrorInvalid, fmt.Errorf("content source %q does not support discovery search", sourceID))
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return DiscoveryResult{}, managementErr(ErrorInvalid, errors.New("search query is required"))
	}
	values, err := src.Search(ctx, provider.SearchRequest{Query: query, Limit: limit})
	if err != nil && len(values) == 0 {
		return DiscoveryResult{}, managementErr(ErrorUnknown, err)
	}
	if limit <= 0 || limit > 50 {
		limit = 30
	}
	out := DiscoveryResult{SourceID: sourceID, Query: query}
	if err != nil {
		out.Issues = appendIssue(out.Issues, OperationIssue{Severity: "warning", Stage: "source", Code: "search_observation_partial", Message: err.Error(), Source: sourceID})
	}
	usable := values[:0]
	for _, value := range values {
		if strings.TrimSpace(value.MediaName) == "" {
			continue
		}
		usable = append(usable, value)
	}
	groups := discovery.GroupReleases(usable, sourceID)
	names, nameErr := runtimeNamingIndex(rt)
	if nameErr != nil {
		return DiscoveryResult{}, nameErr
	}
	resolver := discovery.Resolver{}
	for _, group := range groups {
		if resolved := resolver.Resolve(group, rt.bundle.Entries, names); resolved.Matched {
			group.EntryKey = resolved.EntryKey
		}
		out.Groups = append(out.Groups, group)
		if len(out.Groups) >= limit {
			break
		}
	}
	return out, nil
}

// RSSDiscoveries reads backend observations without fetching remote resources.
// Entry matching still uses current filesystem evidence, never cached availability.
// A non-positive limit returns all candidates so display sorting sees every source.
func (a *App) RSSDiscoveries(ctx context.Context, sourceID string, limit int) (RSSDiscoveryResult, error) {
	if err := ctx.Err(); err != nil {
		return RSSDiscoveryResult{}, err
	}
	rt := a.snapshot()
	if rt == nil {
		return RSSDiscoveryResult{}, errors.New("runtime unavailable")
	}
	ids := configstore.EnabledSourceIDs(rt.bundle)
	if strings.TrimSpace(sourceID) != "" {
		ids = []string{sourceID}
	}
	out := RSSDiscoveryResult{Groups: []discovery.Group{}, Sources: []RSSSourceObservation{}}
	var observed []discovery.Group
	a.rssMu.RLock()
	if a.rssRefreshErr != nil {
		out.Issues = appendIssue(out.Issues, OperationIssue{Severity: "warning", Stage: "source", Code: "rss_refresh_failed", Message: a.rssRefreshErr.Error()})
	}
	for _, id := range ids {
		src, ok := rt.sources[id]
		if !ok {
			a.rssMu.RUnlock()
			return RSSDiscoveryResult{}, managementErr(ErrorNotFound, fmt.Errorf("content source %q not found", id))
		}
		if !src.Config.Enabled || !src.Capabilities().RSS {
			continue
		}
		observation, ok := a.rssObservations[id]
		if !ok || !sameRSSConfig(observation.config, src.Config) {
			out.Sources = append(out.Sources, RSSSourceObservation{SourceID: id, Status: "pending"})
			continue
		}
		out.Sources = append(out.Sources, observation.state)
		if observation.err != nil {
			out.Issues = appendIssue(out.Issues, OperationIssue{Severity: "warning", Stage: "source", Code: "rss_observation_failed", Message: observation.err.Error(), Source: id})
		}
		observed = append(observed, observation.groups...)
	}
	a.rssMu.RUnlock()
	if len(observed) == 0 {
		return out, nil
	}

	groups := discovery.MergeGroups(observed)
	names, nameErr := runtimeNamingIndex(rt)
	if nameErr != nil {
		return RSSDiscoveryResult{}, nameErr
	}
	resolver := discovery.Resolver{}
	for _, group := range groups {
		// RSS discovery is intentionally series-only: it is a feed for finding
		// shows the user may want to follow, not a generic release browser.
		// Filter movies before applying the response limit so movie-shaped noise
		// cannot crowd real series out of the discovery window.
		if group.MediaType != catalog.MediaSeries || strings.TrimSpace(group.Title) == "" || resolver.Resolve(group, rt.bundle.Entries, names).Matched {
			continue
		}
		group.Releases = cloneRSSReleases(group.Releases)
		out.Groups = append(out.Groups, group)
	}
	if limit > 0 && len(out.Groups) > limit {
		out.Groups = out.Groups[:limit]
	}
	return out, nil
}

func runtimeNamingIndex(rt *runtimeSnapshot) (naming.Index, error) {
	graph, err := observation.Build(rt.bundle.Organizer.Source, rt.bundle.Organizer.Target, rt.bundle.Entries)
	if err != nil {
		return nil, fmt.Errorf("observe filesystem naming evidence: %w", err)
	}
	return naming.BuildIndex(rt.bundle.Entries, graph.Sources, graph.Libraries, rt.bundle.Organizer.Extensions), nil
}

type DiscoveryAcquireRequest struct {
	EntryKey string          `json:"entry_key"`
	SourceID string          `json:"source_id"`
	Query    string          `json:"query"`
	Release  release.Release `json:"release"`
}

type DiscoveryAcquireResult struct {
	Candidate   candidate.Candidate      `json:"candidate"`
	Acquisition automation.AcquireResult `json:"acquisition"`
}

// AcquireDiscovery is the explicit search -> local Entry -> downloader boundary.
// Search results are transient; the durable fact is the selected Entry and the
// downloader task created for it. The runner derives and passes the Entry's
// source directory to the downloader, preserving the normal ownership path.
func (a *App) AcquireDiscovery(ctx context.Context, req DiscoveryAcquireRequest) (DiscoveryAcquireResult, error) {
	var out DiscoveryAcquireResult
	err := a.withOperation(ctx, func() error {
		if err := a.reloadRuntime(); err != nil {
			return err
		}
		rt := a.snapshot()
		if rt == nil {
			return errors.New("runtime unavailable")
		}
		w, ok := rt.bundle.Entries[req.EntryKey]
		if !ok {
			return managementErr(ErrorNotFound, fmt.Errorf("entry %q not found", req.EntryKey))
		}
		f, ok := rt.bundle.Sources[req.SourceID]
		if !ok {
			return managementErr(ErrorNotFound, fmt.Errorf("content source %q not found", req.SourceID))
		}
		if !f.Enabled {
			return managementErr(ErrorInvalid, fmt.Errorf("content source %q is disabled", req.SourceID))
		}
		query := strings.TrimSpace(req.Query)
		if query == "" {
			return managementErr(ErrorInvalid, errors.New("search query is required for acquisition verification"))
		}
		if !rt.sources[req.SourceID].DiscoveryCapable() {
			return managementErr(ErrorInvalid, fmt.Errorf("content source %q does not support discovery search", req.SourceID))
		}
		if strings.TrimSpace(req.Release.DownloadURL) == "" {
			return managementErr(ErrorInvalid, errors.New("selected release has no download URL"))
		}
		values, err := rt.sources[req.SourceID].Search(ctx, provider.SearchRequest{Query: query, Limit: 50})
		if err != nil {
			return managementErr(ErrorUnknown, fmt.Errorf("verify search selection: %w", err))
		}
		selected, ok := verifiedRelease(values, req.Release)
		if !ok {
			return managementErr(ErrorConflict, errors.New("selected release is no longer present in the search result; search again"))
		}
		if selected.MediaType != "" && selected.MediaType != w.MediaType {
			return managementErr(ErrorInvalid, fmt.Errorf("selected %s release cannot target %s entry", selected.MediaType, w.MediaType))
		}

		// Explicit search selection bypasses work-title guessing only after the
		// provider has re-confirmed that the selected release belongs to the query.
		// Filters, episode mapping and acquisition identity stay shared with RSS.
		bound, err := candidate.BuildBound(ctx, rt.bundle, req.EntryKey, req.SourceID, selected)
		if err != nil {
			return managementErr(ErrorInvalid, err)
		}
		out.Candidate = bound
		if out.Candidate.Decision != candidate.DecisionSelected {
			return managementErr(ErrorInvalid, fmt.Errorf("selected release is %s: %s", out.Candidate.Decision, out.Candidate.Reason))
		}
		physical, err := observation.Build(rt.bundle.Organizer.Source, rt.bundle.Organizer.Target, rt.bundle.Entries)
		if err != nil {
			return managementErr(ErrorUnknown, fmt.Errorf("observe filesystem graph: %w", err))
		}
		plan := candidate.Plan{Candidates: []candidate.Candidate{out.Candidate}}
		gateAvailability(&plan, inventory.ScanGraph(rt.bundle.Entries, physical, rt.bundle.Organizer.Extensions))
		out.Candidate = plan.Candidates[0]
		if out.Candidate.Decision != candidate.DecisionSelected {
			return managementErr(ErrorConflict, fmt.Errorf("selected release is not acquirable: %s", out.Candidate.Reason))
		}
		runner := automation.Runner{Bundle: rt.bundle, Clients: rt.backends, Now: a.now}
		acquisitions, err := runner.Acquire(ctx, plan, physical)
		if err != nil && len(acquisitions) == 0 {
			return managementErr(ErrorUnknown, err)
		}
		if len(acquisitions) != 1 {
			return managementErr(ErrorUnknown, errors.New("downloader did not return one acquisition result"))
		}
		out.Acquisition = acquisitions[0]
		if out.Acquisition.Decision == automation.AcquireFailed {
			return managementErr(ErrorUnknown, errors.New(out.Acquisition.Error))
		}
		return nil
	})
	return out, err
}

func verifiedRelease(values []release.Release, selected release.Release) (release.Release, bool) {
	for _, value := range values {
		if value.GUID == selected.GUID && value.DownloadURL == selected.DownloadURL && value.Title == selected.Title {
			return value, true
		}
	}
	return release.Release{}, false
}
