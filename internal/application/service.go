package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"sort"
	"strings"
	"sync"
	"time"

	"aninode/internal/automation"
	"aninode/internal/backfill"
	"aninode/internal/candidate"
	"aninode/internal/catalog"
	"aninode/internal/completeness"
	"aninode/internal/configstore"
	"aninode/internal/discovery"
	"aninode/internal/download"
	"aninode/internal/downloadfactory"
	"aninode/internal/filesystem"
	"aninode/internal/inventory"
	"aninode/internal/migration"
	"aninode/internal/naming"
	"aninode/internal/observation"
	"aninode/internal/provider"
	"aninode/internal/release"
	"aninode/internal/releasefilter"
	"aninode/internal/rss"
	"aninode/internal/secretstore"
)

type RSSFetcher interface {
	Fetch(context.Context, string) ([]rss.Entry, error)
}

type Options struct {
	ConfigRoot string
	Fetcher    RSSFetcher
	History    backfill.Searcher
	Backends   map[string]download.Backend
	Now        func() time.Time
}

// App owns one validated configuration snapshot and the long-lived runtime
// resources built from it. Mutating operations are coordinated here so HTTP,
// the scheduler and CLI cannot accidentally bypass serialization.
type runtimeSnapshot struct {
	bundle   configstore.Bundle
	backends map[string]download.Backend
	sources  map[string]provider.Source
}

type App struct {
	operationMu     sync.Mutex
	rssMu           sync.RWMutex
	rssObservations map[string]rssObservation
	rssRefreshErr   error
	repairMu        sync.RWMutex
	repairReviews   map[string]RepairReview
	runtimeMu       sync.RWMutex
	runtime         *runtimeSnapshot
	reloadFailed    bool

	fetcher         RSSFetcher
	history         backfill.Searcher
	backendOverride map[string]download.Backend
	configRoot      string
	now             func() time.Time
	ops             operationsState
}

func Open(options Options) (*App, error) {
	if _, err := configstore.Bootstrap(options.ConfigRoot, configstore.BootstrapOptions{}); err != nil {
		return nil, fmt.Errorf("initialize configuration: %w", err)
	}
	fetcher := options.Fetcher
	if fetcher == nil {
		fetcher = rss.NewFetcher()
	}
	a := &App{fetcher: fetcher, history: options.History, configRoot: options.ConfigRoot, now: options.Now, ops: operationsState{startedAt: time.Now().UTC()}}
	if options.Backends != nil {
		a.backendOverride = maps.Clone(options.Backends)
	}
	if err := a.reloadRuntime(); err != nil {
		return nil, err
	}
	return a, nil
}

func buildBackends(bundle configstore.Bundle, override map[string]download.Backend, secrets secretstore.Store) (map[string]download.Backend, error) {
	if override != nil {
		return maps.Clone(override), nil
	}
	list, err := downloadfactory.EnabledFromConfig(bundle.Clients, func(id string) (string, error) {
		value, _, err := secrets.Lookup(secretstore.DownloaderPassword(id))
		return value, err
	})
	if err != nil {
		return nil, err
	}
	out := map[string]download.Backend{}
	for _, b := range list {
		out[b.Name()] = b
	}
	return out, nil
}
func (a *App) reloadRuntime() error {
	b, err := configstore.Load(a.configRoot)
	if err != nil {
		a.markReloadFailed()
		return err
	}
	// Resolve configured backends without writing runtime observations.
	backends, err := buildBackends(b, a.backendOverride, secretstore.Store{Root: a.configRoot})
	if err != nil {
		a.markReloadFailed()
		return err
	}
	sources := make(map[string]provider.Source, len(b.Sources))
	for id, cfg := range b.Sources {
		src, sourceErr := provider.NewSource(cfg, a.fetcher)
		if sourceErr != nil {
			a.markReloadFailed()
			return sourceErr
		}
		sources[id] = src
	}
	rt := &runtimeSnapshot{bundle: cloneBundle(b), backends: backends, sources: sources}
	a.runtimeMu.Lock()
	a.runtime = rt
	a.reloadFailed = false
	a.discardRSSObservations(rt)
	a.runtimeMu.Unlock()
	return nil
}

func (a *App) markReloadFailed() {
	a.runtimeMu.Lock()
	a.reloadFailed = true
	a.runtimeMu.Unlock()
}
func (a *App) snapshot() *runtimeSnapshot {
	a.runtimeMu.RLock()
	defer a.runtimeMu.RUnlock()
	if a.runtime == nil {
		return nil
	}
	r := *a.runtime
	return &r
}

func cloneBundle(in configstore.Bundle) configstore.Bundle {
	out := in
	out.Clients = maps.Clone(in.Clients)
	out.Sources = maps.Clone(in.Sources)
	out.Entries = maps.Clone(in.Entries)
	return out
}

type CycleOptions struct {
	// LocalOnly skips broad remote RSS observation/discovery. It may still be
	// paired with RepairGaps to perform narrowly targeted historical searches for
	// filesystem-proven internal episode holes.
	LocalOnly  bool
	Acquire    bool
	RepairGaps bool
	Reconcile  bool
}

type CycleResult struct {
	SourceReleases    map[string]int               `json:"source_releases,omitempty"`
	DiscoveredEntries []string                     `json:"discovered_works,omitempty"`
	CreatedEntries    []string                     `json:"created_works,omitempty"`
	CreatedSeasons    []string                     `json:"created_seasons,omitempty"`
	SelectedReleases  []candidate.Candidate        `json:"selected_releases,omitempty"`
	SkippedReleases   []candidate.Candidate        `json:"skipped_releases,omitempty"`
	DuplicateReleases []candidate.Candidate        `json:"duplicate_releases,omitempty"`
	Acquisitions      []automation.AcquireResult   `json:"acquisitions,omitempty"`
	CompletedTasks    []automation.ReconcileResult `json:"completed_tasks,omitempty"`
	Completeness      []completeness.SeasonState   `json:"completeness,omitempty"`
	OrganizedFiles    int                          `json:"organized_files,omitempty"`
	Issues            []OperationIssue             `json:"issues,omitempty"`
}

// EnsureSourceDeclarations performs only the cheap filesystem-to-declaration
// convergence needed for source ownership. It deliberately does not fetch
// content sources, inspect downloaders, build inventory, or publish media.
func (a *App) EnsureSourceDeclarations(ctx context.Context) ([]string, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	a.operationMu.Lock()
	defer a.operationMu.Unlock()
	lock, err := filesystem.AcquireWriterLock(a.configRoot)
	if err != nil {
		return nil, fmt.Errorf("acquire writer ownership: %w", err)
	}
	defer lock.Close()
	rt := a.snapshot()
	if rt == nil {
		if err := a.reloadRuntime(); err != nil {
			return nil, fmt.Errorf("reload runtime: %w", err)
		}
		rt = a.snapshot()
		if rt == nil {
			return nil, errors.New("runtime unavailable")
		}
	}
	changes, err := a.ensureSourceDeclarations(rt)
	return changes.created, err
}

type sourceDeclarationChanges struct {
	created []string
	changed bool
}

func (a *App) ensureSourceDeclarations(rt *runtimeSnapshot) (sourceDeclarationChanges, error) {
	adoptedSeries, err := catalog.AdoptStructuredSources(rt.bundle.Organizer.SeriesSource())
	if err != nil {
		return sourceDeclarationChanges{}, fmt.Errorf("adopt structured series source namespace: %w", err)
	}
	adoptedMovies, err := catalog.AdoptStructuredMovies(rt.bundle.Organizer.MovieSource())
	if err != nil {
		return sourceDeclarationChanges{}, fmt.Errorf("adopt structured movie source namespace: %w", err)
	}
	syncedFolders, err := catalog.SyncFolderProjections(rt.bundle.Organizer.SeriesSource())
	if err != nil {
		return sourceDeclarationChanges{}, fmt.Errorf("sync series folder projections: %w", err)
	}
	created := append(adoptedSeries, adoptedMovies...)
	sort.Strings(created)
	changes := sourceDeclarationChanges{created: created, changed: len(created) > 0 || len(syncedFolders) > 0}
	if changes.changed {
		if err := a.reloadRuntime(); err != nil {
			return changes, fmt.Errorf("reload runtime after source adoption: %w", err)
		}
	}
	return changes, nil
}

func (a *App) Cycle(ctx context.Context, options CycleOptions) (result CycleResult, finalErr error) {
	started := a.clock()
	defer func() {
		result.Issues = cycleIssues(result, finalErr)
		a.recordCycle(started, result)
	}()
	a.operationMu.Lock()
	defer a.operationMu.Unlock()
	lock, err := filesystem.AcquireWriterLock(a.configRoot)
	if err != nil {
		return result, fmt.Errorf("acquire writer ownership: %w", err)
	}
	defer lock.Close()
	return a.cycle(ctx, options)
}

func (a *App) cycle(ctx context.Context, options CycleOptions) (CycleResult, error) {
	var out CycleResult
	if err := a.reloadRuntime(); err != nil {
		return out, fmt.Errorf("reload runtime: %w", err)
	}
	rt := a.snapshot()
	if rt == nil {
		return out, errors.New("runtime unavailable")
	}
	declarationChanges, err := a.ensureSourceDeclarations(rt)
	if err != nil {
		return out, err
	}
	if len(declarationChanges.created) > 0 {
		out.CreatedEntries = append(out.CreatedEntries, declarationChanges.created...)
	}
	if declarationChanges.changed {
		rt = a.snapshot()
		if rt == nil {
			return out, errors.New("runtime unavailable after source adoption")
		}
	}
	var bySource map[string][]release.Release
	plan := candidate.Plan{}
	if !options.LocalOnly {
		var warnings []string
		bySource, warnings = a.ingestSources(ctx, rt)
		out.Issues = appendMessageIssues(out.Issues, "warning", "source", "source_observation_failed", warnings)
		out.SourceReleases = make(map[string]int, len(bySource))
		for sourceID, releases := range bySource {
			out.SourceReleases[sourceID] = len(releases)
		}
	}

	physical, observeErr := observation.Build(rt.bundle.Organizer.Source, rt.bundle.Organizer.Target, rt.bundle.Entries)
	if observeErr != nil {
		return out, fmt.Errorf("observe filesystem graph: %w", observeErr)
	}
	currentGraph := physical
	nameIndex := naming.BuildIndex(rt.bundle.Entries, physical.Sources, physical.Libraries, rt.bundle.Organizer.Extensions)
	if !options.LocalOnly {
		changed, warnings := a.discoverEntries(rt, bySource, nameIndex, &out)
		out.Issues = appendMessageIssues(out.Issues, "warning", "discovery", "discovery_warning", warnings)
		if changed {
			if err := refreshRuntimeEntries(rt); err != nil {
				return out, err
			}
		}
		var err error
		plan, err = a.planCandidates(ctx, rt.bundle, bySource, nameIndex, physical)
		if err != nil {
			return out, err
		}
		out.Issues = appendMessageIssues(out.Issues, "warning", "candidate", "candidate_warning", plan.Warnings)
	}
	fsInventory := inventory.ScanGraph(rt.bundle.Entries, physical, rt.bundle.Organizer.Extensions)
	states, stateErr := a.updateCompleteness(rt.bundle, fsInventory, bySource)
	out.Completeness = states
	if stateErr != nil {
		return out, fmt.Errorf("update completeness: %w", stateErr)
	}
	out.Issues = appendMessageIssues(out.Issues, "warning", "inventory", "availability_unknown", gateAvailability(&plan, fsInventory))

	classifyCandidates(plan, &out)

	runner := automation.Runner{Bundle: rt.bundle, Clients: rt.backends, Now: a.now}
	acquisitionGraphDirty := false
	if options.Acquire {
		values, runErr := runner.Acquire(ctx, plan, physical)
		out.Acquisitions = values
		if runErr != nil {
			out.Issues = appendIssue(out.Issues, OperationIssue{Severity: "error", Stage: "acquisition", Code: "acquisition_failed", Message: runErr.Error()})
		}
		accepted := map[string]bool{}
		for _, value := range values {
			if value.Decision == automation.AcquireSubmitted || value.Decision == automation.AcquireRecovered {
				accepted[value.AcquisitionID] = true
			}
		}
		acceptedPlan := candidate.Plan{}
		for _, value := range plan.Candidates {
			if accepted[value.Acquisition.ID] {
				acceptedPlan.Candidates = append(acceptedPlan.Candidates, value)
			}
		}
		if a.ensureCandidateSeasons(rt.bundle, acceptedPlan, &out) {
			if err := refreshRuntimeEntries(rt); err != nil {
				return out, err
			}
		}
		// A quiet acquisition pass cannot have changed the filesystem, so reuse the
		// coherent graph we already own. If Runner produced any acquisition result
		// we conservatively refresh once because topology repair or downloader
		// materialization may have changed the source namespace.
		if len(values) > 0 {
			var rebuildErr error
			currentGraph, rebuildErr = observation.Build(rt.bundle.Organizer.Source, rt.bundle.Organizer.Target, rt.bundle.Entries)
			if rebuildErr != nil {
				return out, fmt.Errorf("re-observe filesystem graph after current acquisition: %w", rebuildErr)
			}
		}
		postAcquireGraph := currentGraph
		postAcquireInventory := inventory.ScanGraph(rt.bundle.Entries, postAcquireGraph, rt.bundle.Organizer.Extensions)
		postAcquireStates, stateErr := a.updateCompleteness(rt.bundle, postAcquireInventory, nil)
		if stateErr != nil {
			return out, fmt.Errorf("update completeness after current acquisition: %w", stateErr)
		}
		out.Completeness = postAcquireStates

		if !options.LocalOnly || options.RepairGaps {
			historical, backfillValues, backfillWarnings, backfillErr := a.runBackfill(ctx, rt, postAcquireStates, postAcquireInventory, postAcquireGraph, runner, plan)
			out.SelectedReleases = append(out.SelectedReleases, historical...)
			out.Acquisitions = append(out.Acquisitions, backfillValues...)
			if len(backfillValues) > 0 {
				acquisitionGraphDirty = true
			}
			out.Issues = appendMessageIssues(out.Issues, "warning", "backfill", "historical_search_warning", backfillWarnings)
			if backfillErr != nil {
				out.Issues = appendIssue(out.Issues, OperationIssue{Severity: "error", Stage: "backfill", Code: "backfill_failed", Message: backfillErr.Error()})
			}
		}
	}
	if options.Reconcile {
		if acquisitionGraphDirty {
			var rebuildErr error
			currentGraph, rebuildErr = observation.Build(rt.bundle.Organizer.Source, rt.bundle.Organizer.Target, rt.bundle.Entries)
			if rebuildErr != nil {
				return out, fmt.Errorf("re-observe filesystem graph after backfill: %w", rebuildErr)
			}
		}
		reconcileGraph := currentGraph
		values, runErr := runner.ReconcilePublicationsWithGraph(ctx, reconcileGraph)
		out.CompletedTasks = values
		for _, value := range values {
			out.OrganizedFiles += value.Result.Linked
		}
		if runErr != nil {
			out.Issues = appendIssue(out.Issues, OperationIssue{Severity: "error", Stage: "publication", Code: "publication_failed", Message: runErr.Error()})
		}
		finalGraph := reconcileGraph
		if reconcileMutatedFilesystem(values) {
			var finalObserveErr error
			finalGraph, finalObserveErr = observation.Build(rt.bundle.Organizer.Source, rt.bundle.Organizer.Target, rt.bundle.Entries)
			if finalObserveErr != nil {
				return out, fmt.Errorf("re-observe filesystem graph after publication: %w", finalObserveErr)
			}
		}
		finalInventory := inventory.ScanGraph(rt.bundle.Entries, finalGraph, rt.bundle.Organizer.Extensions)
		finalStates, finalStateErr := a.updateCompleteness(rt.bundle, finalInventory, nil)
		if finalStateErr != nil {
			return out, fmt.Errorf("update completeness after publication: %w", finalStateErr)
		}
		out.Completeness = finalStates
	}
	a.runtimeMu.Lock()
	a.runtime = rt
	a.runtimeMu.Unlock()
	return out, nil
}

func reconcileMutatedFilesystem(values []automation.ReconcileResult) bool {
	for _, value := range values {
		if value.Result.Linked > 0 || value.Result.Removed > 0 {
			return true
		}
	}
	return false
}

func (a *App) ingestSources(ctx context.Context, rt *runtimeSnapshot) (map[string][]release.Release, []string) {
	a.rssMu.Lock()
	a.rssRefreshErr = nil
	a.rssMu.Unlock()
	bySource := make(map[string][]release.Release)
	var warnings []string
	ids := configstore.EnabledSourceIDs(rt.bundle)
	type sourceResult struct {
		values []release.Release
		err    error
	}
	results := make([]sourceResult, len(ids))
	var wg sync.WaitGroup
	for index, id := range ids {
		src, ok := rt.sources[id]
		if !ok {
			results[index].err = errors.New("runtime source unavailable")
			continue
		}
		if !src.Capabilities().RSS {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[index].values, results[index].err = src.Current(ctx)
			a.recordRSSObservation(src.Config, results[index].values, results[index].err)
		}()
	}
	wg.Wait()
	for index, id := range ids {
		result := results[index]
		if result.err != nil {
			warnings = append(warnings, fmt.Sprintf("feed %s: %v", id, result.err))
		}
		if len(result.values) > 0 {
			bySource[id] = append(bySource[id], result.values...)
		}
	}
	return bySource, warnings
}

func (a *App) discoverEntries(rt *runtimeSnapshot, bySource map[string][]release.Release, names naming.Index, out *CycleResult) (bool, []string) {
	for sourceID, releases := range bySource {
		for _, group := range discovery.Unmatched(releases, sourceID, rt.bundle.Entries, names) {
			out.DiscoveredEntries = appendUnique(out.DiscoveredEntries, group.Title)
		}
	}
	return false, nil
}

func (a *App) planCandidates(ctx context.Context, bundle configstore.Bundle, bySource map[string][]release.Release, names naming.Index, graph observation.Graph) (candidate.Plan, error) {
	// Current acquisition duplication is resolved by automation.Runner from
	// downloader observations. Candidate planning only decides release semantics.
	evidence := make(map[string][]catalog.FolderProjectionEvidence, len(bundle.Entries))
	for key, w := range bundle.Entries {
		evidence[key] = catalog.ObserveFolderProjectionEvidenceFromSnapshot(w, graph.Sources[key], bundle.Organizer.Extensions)
	}
	return candidate.BuildObserved(ctx, bundle, bySource, names, evidence)
}

func (a *App) ensureCandidateSeasons(bundle configstore.Bundle, plan candidate.Plan, out *CycleResult) bool {
	changed := false
	for _, value := range plan.Candidates {
		if value.Decision != candidate.DecisionSelected {
			continue
		}
		w := bundle.Entries[value.EntryKey]
		// When filesystem evidence selected an existing declared source folder,
		// that folder is the acquisition destination. Do not materialize a
		// synthetic Season 01 merely because a seasonless release uses 1 as its
		// parser/source-identity fallback.
		if value.SourceFolder != "" || hasSeason(w, value.SourceEpisode.Season) || !w.Enabled {
			continue
		}
		created, err := catalog.EnsureSeason(w, value.SourceEpisode.Season)
		if err != nil {
			out.Issues = appendIssue(out.Issues, OperationIssue{Severity: "warning", Stage: "catalog", Code: "season_creation_failed", Message: err.Error(), Entry: w.Key})
			continue
		}
		if created {
			out.CreatedSeasons = appendUnique(out.CreatedSeasons, fmt.Sprintf("%s/Season %02d", w.Key, value.SourceEpisode.Season))
			changed = true
		}
	}
	return changed
}

func refreshRuntimeEntries(rt *runtimeSnapshot) error {
	entries, err := catalog.DiscoverAt(rt.bundle.Organizer.SeriesSource(), rt.bundle.Organizer.SeriesTarget())
	if err != nil {
		return fmt.Errorf("refresh entries: %w", err)
	}
	movies, err := catalog.DiscoverMoviesAt(rt.bundle.Organizer.MovieSource(), rt.bundle.Organizer.MovieTarget())
	if err != nil {
		return fmt.Errorf("refresh movies: %w", err)
	}
	for id, value := range movies {
		entries[id] = value
	}
	rt.bundle.Entries = entries
	return nil
}

func classifyCandidates(plan candidate.Plan, out *CycleResult) {
	for _, value := range plan.Candidates {
		switch value.Decision {
		case candidate.DecisionSelected:
			out.SelectedReleases = append(out.SelectedReleases, value)
		case candidate.DecisionDuplicate:
			out.DuplicateReleases = append(out.DuplicateReleases, value)
		default:
			out.SkippedReleases = append(out.SkippedReleases, value)
		}
	}
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func hasSeason(value catalog.Entry, season int) bool {
	for _, existing := range value.Seasons {
		if existing == season {
			return true
		}
	}
	return false
}

type MigrationResult struct {
	Groups  []MigrationGroup   `json:"groups,omitempty"`
	Plans   []migration.Plan   `json:"-"`
	Applied []migration.Result `json:"applied,omitempty"`
}

type MigrationGroup struct {
	Key           string                `json:"key"`
	EntryKey      string                `json:"entry_key,omitempty"`
	Title         string                `json:"title"`
	Season        int                   `json:"season"`
	MediaType     string                `json:"media_type,omitempty"`
	Plans         []migration.Plan      `json:"plans"`
	Ready         int                   `json:"ready"`
	Blocked       int                   `json:"blocked"`
	Filters       releasefilter.Filters `json:"filters,omitempty"`
	FilterOptions releasefilter.Options `json:"filter_options,omitempty"`
}

func migrationSeriesTitle(taskName, savePath string) string {
	value, _ := migration.PresentationIdentity(taskName, savePath)
	return value
}

func groupMigrationPlans(bundle configstore.Bundle, plans []migration.Plan) []MigrationGroup {
	groups := map[string]*MigrationGroup{}
	for _, plan := range plans {
		key, entryKey, title, season := migrationPlanIdentity(bundle, plan)
		group := groups[key]
		if group == nil {
			group = &MigrationGroup{Key: key, EntryKey: entryKey, Title: title, Season: season, MediaType: plan.MediaType}
			if entry, ok := bundle.Entries[entryKey]; ok {
				group.Filters = entry.Filters.Normalize()
				group.FilterOptions = group.FilterOptions.WithFilters(group.Filters)
			}
			groups[key] = group
		}
		group.FilterOptions = group.FilterOptions.Merge(plan.FilterOptions).WithFilters(group.Filters)
		group.Plans = append(group.Plans, plan)
		if plan.Decision == migration.DecisionRelocate || plan.Decision == migration.DecisionAdopt {
			group.Ready++
		} else {
			group.Blocked++
		}
	}
	out := make([]MigrationGroup, 0, len(groups))
	for _, group := range groups {
		sort.Slice(group.Plans, func(i, j int) bool {
			if group.Plans[i].Client != group.Plans[j].Client {
				return group.Plans[i].Client < group.Plans[j].Client
			}
			if group.Plans[i].TaskName != group.Plans[j].TaskName {
				return group.Plans[i].TaskName < group.Plans[j].TaskName
			}
			return group.Plans[i].TaskID < group.Plans[j].TaskID
		})
		group.Key = migrationConfirmationKey(group.Key, group.Plans)
		out = append(out, *group)
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Title) < strings.ToLower(out[j].Title) })
	return out
}

// Migrate returns the current read-only migration view. Mutation requires an
// explicit, current confirmation key through MigrationApplyGroups.
func (a *App) Migrate(ctx context.Context, apply bool) (out MigrationResult, finalErr error) {
	if apply {
		return MigrationResult{}, errors.New("migration apply requires explicitly selected current groups")
	}
	return a.migrate(ctx, apply, nil)
}

func (a *App) migrate(ctx context.Context, apply bool, selectedGroups map[string]bool) (out MigrationResult, finalErr error) {
	started := a.clock()
	defer func() { a.recordMigration(started, out, apply, finalErr) }()
	a.operationMu.Lock()
	defer a.operationMu.Unlock()
	if apply {
		lock, err := filesystem.AcquireWriterLock(a.configRoot)
		if err != nil {
			return out, managementErr(ErrorConflict, fmt.Errorf("acquire writer ownership: %w", err))
		}
		defer lock.Close()
	}

	if err := a.reloadRuntime(); err != nil {
		return out, err
	}
	rt := a.snapshot()
	if rt == nil {
		return out, errors.New("runtime unavailable")
	}
	engine := migration.Engine{Bundle: rt.bundle, Clients: rt.backends}
	out = MigrationResult{}
	var errs []error
	plans, scanErr := engine.Scan(ctx)
	out.Plans = plans
	out.Groups = groupMigrationPlans(rt.bundle, plans)
	if scanErr != nil {
		errs = append(errs, scanErr)
	}
	if apply {
		approvedTasks := map[string]bool{}
		matchedSelections := map[string]bool{}
		selectionStale := false
		for _, group := range out.Groups {
			if !selectedGroups[group.Key] {
				continue
			}
			matchedSelections[group.Key] = true
			for _, plan := range group.Plans {
				approvedTasks[plan.Client+"\x00"+plan.TaskID] = true
			}
		}
		for key := range selectedGroups {
			if !matchedSelections[key] {
				selectionStale = true
				errs = append(errs, managementErr(ErrorConflict, fmt.Errorf("migration confirmation %q is stale; inspect the new plan and confirm again", key)))
			}
		}
		if selectionStale {
			return out, errors.Join(errs...)
		}
		for _, plan := range plans {
			if !approvedTasks[plan.Client+"\x00"+plan.TaskID] {
				continue
			}
			if plan.Decision != migration.DecisionRelocate && plan.Decision != migration.DecisionAdopt {
				continue
			}
			result, applyErr := engine.Apply(ctx, plan)
			out.Applied = append(out.Applied, result)
			if applyErr != nil {
				errs = append(errs, applyErr)
			}
		}
	}
	return out, errors.Join(errs...)
}

func migrationConfirmationKey(groupKey string, plans []migration.Plan) string {
	identity := groupKey
	for _, plan := range plans {
		identity += "\x00" + plan.Client + "\x00" + plan.TaskID + "\x00" + string(plan.Decision)
	}
	sum := sha256.Sum256([]byte(identity))
	return groupKey + "/confirm-" + hex.EncodeToString(sum[:8])
}

func migrationPlanIdentity(bundle configstore.Bundle, plan migration.Plan) (key, entryKey, title string, season int) {
	title, season = strings.TrimSpace(plan.Title), plan.Season
	seasonKnown := plan.SeasonKnown || season > 0
	if plan.EntryKey != "" {
		entryKey = plan.EntryKey
		if plan.SourceEpisode.Season > 0 || plan.SourceEpisode.EpisodeStart > 0 {
			season = plan.SourceEpisode.Season
			seasonKnown = true
		}
		if value, ok := bundle.Entries[entryKey]; ok {
			title = value.Title
			plan.MediaType = value.MediaType
		}
	}
	if title == "" {
		title = migrationSeriesTitle(plan.TaskName, plan.SavePath)
	}
	if title == "" {
		title = "未识别作品"
	}
	if plan.MediaType == catalog.MediaMovie {
		season = 0
	} else if season <= 0 && !seasonKnown {
		season = 1
	}
	base := entryKey
	if base == "" {
		base = strings.ToLower(title)
	}
	if plan.MediaType == catalog.MediaMovie {
		key = base + "/movie"
	} else {
		key = fmt.Sprintf("%s/season/%02d", base, season)
	}
	return key, entryKey, title, season
}
