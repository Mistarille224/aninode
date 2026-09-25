package application

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"aninode/internal/automation"
	"aninode/internal/backfill"
	"aninode/internal/candidate"
	"aninode/internal/catalog"
	"aninode/internal/completeness"
	"aninode/internal/configstore"
	"aninode/internal/inventory"
	"aninode/internal/naming"
	"aninode/internal/observation"
	"aninode/internal/provider"
	"aninode/internal/release"
)

func (a *App) clock() time.Time {
	if a.now != nil {
		return a.now().UTC()
	}
	return time.Now().UTC()
}

func (a *App) updateCompleteness(bundle configstore.Bundle, inv inventory.Snapshot, _ map[string][]release.Release) ([]completeness.SeasonState, error) {
	var out []completeness.SeasonState
	for id, w := range bundle.Entries {
		if w.MediaType != catalog.MediaSeries {
			continue
		}
		seasons := map[int]bool{}
		for _, sn := range w.Seasons {
			seasons[sn] = true
		}
		if ei, ok := inv.Entries[id]; ok {
			for sn := range ei.Seasons {
				seasons[sn] = true
			}
		}
		nums := make([]int, 0, len(seasons))
		for sn := range seasons {
			if sn >= 0 {
				nums = append(nums, sn)
			}
		}
		sort.Ints(nums)
		for _, sn := range nums {
			si := inv.Season(id, sn)
			missing, status := completeness.InternalGaps(inv, id, sn)
			out = append(out, completeness.SeasonState{EntryKey: id, Season: sn, InventoryKnown: si.Known, Missing: missing, Status: status})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].EntryKey == out[j].EntryKey {
			return out[i].Season < out[j].Season
		}
		return out[i].EntryKey < out[j].EntryKey
	})
	return out, nil
}

func (a *App) searchBackfillSource(ctx context.Context, rt *runtimeSnapshot, sourceID string, req backfill.Request) ([]release.Release, error) {
	var evidence []catalog.FolderProjectionEvidence
	if rt != nil {
		if w, ok := rt.bundle.Entries[req.EntryKey]; ok {
			evidence = catalog.ObserveFolderProjectionEvidence(w, rt.bundle.Organizer.Extensions)
		}
	}
	return a.searchBackfillSourceObserved(ctx, rt, sourceID, req, evidence)
}

func (a *App) searchBackfillSourceObserved(ctx context.Context, rt *runtimeSnapshot, sourceID string, req backfill.Request, observedEvidence []catalog.FolderProjectionEvidence) ([]release.Release, error) {
	if a.history != nil {
		return a.history.SearchHistory(ctx, sourceID, req)
	}
	src, ok := rt.sources[sourceID]
	if !ok || !src.HistoricalCapable() {
		return nil, backfill.ErrUnsupported
	}

	targets := make([]provider.SearchEpisode, 0, len(req.SourceMissing))
	seenTarget := map[[2]int]bool{}
	for _, key := range req.SourceMissing {
		target := provider.SearchEpisode{Season: key.Season, Episode: key.EpisodeStart}
		identity := [2]int{target.Season, target.Episode}
		if target.Episode <= 0 || seenTarget[identity] {
			continue
		}
		seenTarget[identity] = true
		targets = append(targets, target)
	}
	if len(targets) == 0 {
		return nil, errors.New("historical search requires at least one exact missing episode")
	}

	// Query planning belongs to the application layer, not provider behavior. Keep a
	// deliberately small set of high-information tracker queries. Every primary
	// filesystem-derived query is executed and unioned so one subtitle group cannot
	// hide another; the declared Entry title is a second tier used only when the
	// observed naming evidence cannot recover the exact missing episode.
	type plannedQuery struct {
		text     string
		origin   string
		fallback bool
	}
	queries := make([]plannedQuery, 0, len(req.Queries))
	seenQuery := map[string]bool{}
	for _, evidence := range req.Queries {
		if evidence.Tier != backfill.QueryPrimary && evidence.Tier != backfill.QueryFallback {
			return nil, fmt.Errorf("historical query %q has invalid tier %q", evidence.Title, evidence.Tier)
		}
		title := strings.TrimSpace(evidence.Title)
		if title == "" {
			continue
		}
		text := title
		if group := strings.TrimSpace(evidence.Group); group != "" {
			text = fmt.Sprintf("[%s] %s", group, title)
		}
		key := catalog.Comparable(text)
		if key == "" || seenQuery[key] {
			continue
		}
		seenQuery[key] = true
		queries = append(queries, plannedQuery{text: text, origin: evidence.Origin, fallback: evidence.Tier == backfill.QueryFallback})
	}
	if len(queries) == 0 {
		return nil, errors.New("historical search requires at least one query evidence item")
	}

	var out []release.Release
	var errs []error
	seenRelease := map[string]bool{}
	appendValues := func(values []release.Release) {
		for _, value := range values {
			key := strings.TrimSpace(value.InfoHash)
			if key == "" {
				key = strings.TrimSpace(value.DownloadURL)
			}
			if key == "" {
				key = value.MediaName + "\x00" + value.Title
			}
			if seenRelease[key] {
				continue
			}
			seenRelease[key] = true
			out = append(out, value)
		}
	}

	runQuery := func(target provider.SearchEpisode, q plannedQuery, titleOnly bool) {
		search := provider.SearchRequest{
			Query:          q.text,
			Season:         target.Season,
			SeasonExplicit: true,
			Targets:        []provider.SearchEpisode{target},
			Limit:          50,
		}
		// Built-in tracker searches normally add the episode number to the query
		// for efficiency. Some trackers do not tokenize punctuation/group prefixes
		// the same way their browser search does, so an exact "Title 06" query can
		// miss a release that a plain "Title" search returns. Keep Targets populated
		// in the broad pass: native torrent metadata still has to prove the exact
		// episode before the result can be acquired.
		if !titleOnly {
			search.EpisodeStart = target.Episode
			search.EpisodeEnd = target.Episode
		}
		values, err := src.Search(ctx, search)
		appendValues(values)
		if err != nil {
			mode := "episode"
			if titleOnly {
				mode = "title"
			}
			errs = append(errs, fmt.Errorf("query %q episode %d mode=%s evidence=%s: %w", q.text, target.Episode, mode, q.origin, err))
		}
	}

	runQueries := func(target provider.SearchEpisode, values []plannedQuery, titleOnly bool) error {
		for _, q := range values {
			runQuery(target, q, titleOnly)
			if ctx.Err() != nil {
				return ctx.Err()
			}
		}
		return nil
	}

	for _, target := range targets {
		primary := make([]plannedQuery, 0, len(queries))
		fallback := make([]plannedQuery, 0, len(queries))
		for _, q := range queries {
			if !q.fallback {
				primary = append(primary, q)
			} else {
				fallback = append(fallback, q)
			}
		}
		if len(primary) == 0 {
			primary, fallback = fallback, nil
		}

		// Phase 1: cheap exact-episode queries from the strongest filesystem names.
		if err := runQueries(target, primary, false); err != nil {
			return out, errors.Join(append(errs, err)...)
		}
		if backfillSearchHasAcceptableTarget(ctx, rt, sourceID, req.EntryKey, out, target, observedEvidence) {
			continue
		}

		// Phase 2: use the same strong names without forcing the episode token into
		// the tracker query. This mirrors the high-recall manual search users perform
		// on DMHY/Mikan while retaining exact native-metadata episode verification.
		if err := runQueries(target, primary, true); err != nil {
			return out, errors.Join(append(errs, err)...)
		}
		if backfillSearchHasAcceptableTarget(ctx, rt, sourceID, req.EntryKey, out, target, observedEvidence) {
			continue
		}

		// Phase 3/4: canonical/declared-title fallback, first targeted and then
		// title-only. Crucially, finding *some* E06 with low continuity confidence no
		// longer suppresses these fallbacks; only a candidate that can actually pass
		// the repair boundary stops further recall work.
		if err := runQueries(target, fallback, false); err != nil {
			return out, errors.Join(append(errs, err)...)
		}
		if backfillSearchHasAcceptableTarget(ctx, rt, sourceID, req.EntryKey, out, target, observedEvidence) {
			continue
		}
		if err := runQueries(target, fallback, true); err != nil {
			return out, errors.Join(append(errs, err)...)
		}
	}
	if len(out) > 0 {
		return out, errors.Join(errs...)
	}
	return nil, errors.Join(errs...)
}

func releasesCoverSearchTarget(values []release.Release, target provider.SearchEpisode) bool {
	for _, value := range values {
		start := value.Components.EpisodeStart
		if start <= 0 {
			continue
		}
		end := value.Components.EpisodeEnd
		if end < start {
			end = start
		}
		if target.Episode < start || target.Episode > end {
			continue
		}
		season := value.Components.Season
		if (value.Components.SeasonExplicit || season > 0) && season != target.Season {
			continue
		}
		return true
	}
	return false
}

// backfillSearchHasAcceptableTarget decides whether recall may stop. Merely
// finding the requested episode is insufficient: a legal but low-confidence
// continuity fallback must not prevent later aliases or broader tracker queries
// from finding a candidate safe for automatic repair. Reuse the same candidate
// eligibility and ReviewReason boundaries used by acquisition so search and
// selection cannot drift apart. Isolated provider tests may omit the Entry from
// the runtime bundle; in that case retain the older episode-coverage behavior.
func backfillSearchHasAcceptableTarget(ctx context.Context, rt *runtimeSnapshot, sourceID, entryKey string, values []release.Release, target provider.SearchEpisode, evidence []catalog.FolderProjectionEvidence) bool {
	if !releasesCoverSearchTarget(values, target) {
		return false
	}
	if rt == nil {
		return true
	}
	_, ok := rt.bundle.Entries[entryKey]
	if !ok {
		return true
	}
	plan, err := candidate.BuildBoundPlanObserved(ctx, rt.bundle, entryKey, map[string][]release.Release{sourceID: values}, evidence)
	if err != nil {
		// An intermediate diagnostic failure is not proof that recall succeeded.
		// Continue to broader aliases; the final bound plan remains authoritative.
		return false
	}
	for _, c := range plan.Candidates {
		if c.Decision != candidate.DecisionSelected {
			continue
		}
		start := c.SourceEpisode.EpisodeStart
		if start <= 0 {
			continue
		}
		end := c.SourceEpisode.EpisodeEnd
		if end < start {
			end = start
		}
		if target.Episode < start || target.Episode > end {
			continue
		}
		if (c.Release.Components.SeasonExplicit || c.Release.Components.Season > 0) && c.SourceEpisode.Season != target.Season {
			continue
		}
		if candidate.ReviewReason(c, evidence) != "" {
			continue
		}
		return true
	}
	return false
}

// backfillSearchEvidence derives exact-episode tracker queries entirely from
// current filesystem naming evidence. Source/library directory names and every
// configured media filename are re-observed on each operation; no alternate
// title state is persisted. Filesystem-derived names are the primary tier and
// the declared canonical title is only a cold-start fallback.
func backfillSearchEvidence(w catalog.Entry, graph observation.Graph, extensions []string) []backfill.QueryEvidence {
	evidence := naming.BuildEntry(w, graph.Sources[w.Key], graph.Libraries[w.Key], extensions)
	out := make([]backfill.QueryEvidence, 0, len(evidence.Search))
	seen := map[string]bool{}
	for _, value := range evidence.Search {
		key := catalog.Comparable(value.Group + "\x00" + value.Title)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		tier := backfill.QueryPrimary
		if value.Origin == "entry_title" {
			tier = backfill.QueryFallback
		}
		out = append(out, backfill.QueryEvidence{Title: value.Title, Group: value.Group, Origin: value.Origin, Tier: tier})
	}
	return out
}

func backfillSelection(plan candidate.Plan, w catalog.Entry, season int, missing []int) (candidate.Plan, error) {
	filtered := candidate.Plan{}
	for _, c := range plan.Candidates {
		if c.Decision != candidate.DecisionSelected || c.EntryKey != w.Key || c.Episode.Season != season || c.Episode.Special != (season == 0) {
			continue
		}
		for _, ep := range missing {
			if c.Episode.Contains(ep) {
				filtered.Candidates = append(filtered.Candidates, c)
				break
			}
		}
	}
	return filtered, nil
}

type backfillSelectionPlans struct {
	Automatic  candidate.Plan
	Review     candidate.Plan
	Diagnostic candidate.Plan
	Warnings   []string
}

func buildBackfillSelection(ctx context.Context, b configstore.Bundle, w catalog.Entry, season int, missing []int, bySource map[string][]release.Release, evidence []catalog.FolderProjectionEvidence) (backfillSelectionPlans, error) {
	plan, err := candidate.BuildBoundPlanObserved(ctx, b, w.Key, bySource, evidence)
	warnings := append([]string(nil), plan.Warnings...)
	if err != nil {
		return backfillSelectionPlans{Diagnostic: plan, Warnings: warnings}, err
	}
	selected, err := backfillSelection(plan, w, season, missing)
	if err != nil {
		return backfillSelectionPlans{Diagnostic: plan, Warnings: warnings}, err
	}

	automatic := candidate.Plan{}
	review := candidate.Plan{}
	for _, value := range selected.Candidates {
		if reason := candidate.ReviewReason(value, evidence); reason != "" {
			value.Reason = "manual review: " + reason
			review.Candidates = append(review.Candidates, value)
			warnings = appendUnique(warnings, fmt.Sprintf("backfill %s S%02dE%02d: %s", w.Key, value.Episode.Season, value.Episode.EpisodeStart, reason))
			continue
		}
		automatic.Candidates = append(automatic.Candidates, value)
	}
	return backfillSelectionPlans{Automatic: automatic, Review: review, Diagnostic: plan, Warnings: warnings}, nil
}

func backfillRejectionSummary(plan candidate.Plan, entryKey string, season int, missing []int) string {
	counts := map[string]int{}
	for _, c := range plan.Candidates {
		if c.EntryKey != entryKey {
			continue
		}
		reason := strings.ToLower(c.Reason)
		switch {
		case c.Decision == candidate.DecisionInvalid && strings.Contains(reason, "media type"):
			counts["media_type"]++
		case c.Decision == candidate.DecisionInvalid && strings.Contains(reason, "no episode identity"):
			counts["no_episode_identity"]++
		case c.Decision == candidate.DecisionInvalid && (strings.Contains(reason, "mapped unambiguously") || strings.Contains(reason, "episode mapping")):
			counts["projection"]++
		case c.Decision == candidate.DecisionFiltered:
			counts["filters"]++
		case c.Decision == candidate.DecisionSelected || c.Decision == candidate.DecisionSuperseded || c.Decision == candidate.DecisionDuplicate:
			coversTarget := c.Episode.Season == season && c.Episode.Special == (season == 0)
			if coversTarget {
				coversTarget = false
				for _, ep := range missing {
					if c.Episode.Contains(ep) {
						coversTarget = true
						break
					}
				}
			}
			if !coversTarget {
				counts["not_exact_target"]++
			} else {
				counts["selection"]++
			}
		default:
			counts["other"]++
		}
	}
	if len(counts) == 0 {
		return "no candidate diagnostics"
	}
	order := []string{"media_type", "no_episode_identity", "projection", "filters", "review", "not_exact_target", "selection", "other"}
	parts := make([]string, 0, len(counts))
	for _, key := range order {
		if counts[key] > 0 {
			parts = append(parts, fmt.Sprintf("%s=%d", key, counts[key]))
		}
	}
	return strings.Join(parts, ", ")
}

func acquireAvailable(ctx context.Context, runner automation.Runner, plan candidate.Plan, inv inventory.Snapshot, graph observation.Graph) ([]candidate.Candidate, []automation.AcquireResult, []string, error) {
	warnings := gateAvailability(&plan, inv)
	eligible := candidate.Plan{}
	for _, c := range plan.Candidates {
		if c.Decision == candidate.DecisionSelected {
			eligible.Candidates = append(eligible.Candidates, c)
		}
	}
	if len(eligible.Candidates) == 0 {
		return nil, nil, warnings, nil
	}
	values, err := runner.Acquire(ctx, eligible, graph)
	return eligible.Candidates, values, warnings, err
}

func reviewAvailable(plan candidate.Plan, inv inventory.Snapshot) ([]candidate.Candidate, []string) {
	warnings := gateAvailability(&plan, inv)
	values := make([]candidate.Candidate, 0, len(plan.Candidates))
	for _, c := range plan.Candidates {
		if c.Decision == candidate.DecisionSelected {
			values = append(values, c)
		}
	}
	return values, warnings
}

func uncoveredMissing(plan candidate.Plan, entryKey string, season int, missing []int) []int {
	covered := map[int]bool{}
	for _, c := range plan.Candidates {
		if c.Decision != candidate.DecisionSelected || c.EntryKey != entryKey || c.Episode.Season != season {
			continue
		}
		for ep := c.Episode.EpisodeStart; ep <= c.Episode.End(); ep++ {
			covered[ep] = true
		}
	}
	out := make([]int, 0, len(missing))
	for _, ep := range missing {
		if !covered[ep] {
			out = append(out, ep)
		}
	}
	return out
}

func (a *App) runBackfill(ctx context.Context, rt *runtimeSnapshot, states []completeness.SeasonState, inv inventory.Snapshot, graph observation.Graph, runner automation.Runner, current candidate.Plan) ([]candidate.Candidate, []automation.AcquireResult, []string, error) {
	var selected []candidate.Candidate
	var acquisitions []automation.AcquireResult
	var reviews []RepairReview
	var warnings []string
	var errs []error
	type cachedSearch struct {
		values []release.Release
		err    error
	}
	searchCache := map[string]cachedSearch{}
	for _, st := range states {
		if st.Status != "incomplete" || len(st.Missing) == 0 {
			continue
		}
		w, ok := rt.bundle.Entries[st.EntryKey]
		if !ok {
			continue
		}
		missing := uncoveredMissing(current, w.Key, st.Season, st.Missing)
		if len(missing) == 0 {
			continue
		}
		sourceKeys := completeness.EpisodeKeys(w.Key, st.Season, missing)
		queries := backfillSearchEvidence(w, graph, rt.bundle.Organizer.Extensions)
		evidence := catalog.ObserveFolderProjectionEvidenceFromSnapshot(w, graph.Sources[w.Key], rt.bundle.Organizer.Extensions)
		bySource := map[string][]release.Release{}
		searched := false
		searchResults := 0
		for _, fid := range catalog.EffectiveSources(w, configstore.EnabledSourceIDs(rt.bundle)) {
			cfg, ok := rt.bundle.Sources[fid]
			if !ok || !cfg.Enabled {
				continue
			}
			req := backfill.Request{EntryKey: w.Key, Queries: queries, SourceMissing: sourceKeys}
			cacheKey := fmt.Sprintf("%s\x00%s\x00%d", w.Key, fid, st.Season)
			cached, found := searchCache[cacheKey]
			if !found {
				cached.values, cached.err = a.searchBackfillSourceObserved(ctx, rt, fid, req, evidence)
				searchCache[cacheKey] = cached
			}
			if cached.err != nil {
				if !errors.Is(cached.err, backfill.ErrUnsupported) {
					warnings = append(warnings, fmt.Sprintf("targeted history %s: %v", fid, cached.err))
				}
				if len(cached.values) == 0 {
					continue
				}
			}
			searched = true
			searchResults += len(cached.values)
			bySource[fid] = append(bySource[fid], cached.values...)
			if len(cached.values) == 0 {
				warnings = append(warnings, fmt.Sprintf("backfill %s season %d source %s: historical search returned no releases for missing episodes %v", w.Key, st.Season, fid, missing))
			}
		}
		if !searched {
			warnings = append(warnings, fmt.Sprintf("backfill %s season %d: no enabled historical-search source is available for missing episodes %v", w.Key, st.Season, missing))
			continue
		}
		plans, e := buildBackfillSelection(ctx, rt.bundle, w, st.Season, missing, bySource, evidence)
		for _, warning := range plans.Warnings {
			warnings = appendUnique(warnings, warning)
		}
		if e != nil {
			warnings = append(warnings, fmt.Sprintf("backfill %s season %d: %v", w.Key, st.Season, e))
			errs = append(errs, e)
			continue
		}
		if len(plans.Automatic.Candidates) == 0 && len(plans.Review.Candidates) == 0 && searchResults > 0 {
			warnings = append(warnings, fmt.Sprintf("backfill %s season %d: %d historical releases were found but none matched the exact missing episodes %v after parsing, filters, and source-to-target projection; final rejection summary: %s", w.Key, st.Season, searchResults, missing, backfillRejectionSummary(plans.Diagnostic, w.Key, st.Season, missing)))
		}
		chosen, vals, ws, e := acquireAvailable(ctx, runner, plans.Automatic, inv, graph)
		warnings = append(warnings, ws...)
		selected = append(selected, chosen...)
		acquisitions = append(acquisitions, vals...)
		if e != nil {
			errs = append(errs, e)
		}
		reviewCandidates, reviewWarnings := reviewAvailable(plans.Review, inv)
		warnings = append(warnings, reviewWarnings...)
		for _, value := range reviewCandidates {
			review := newRepairReviewObserved(w, value, evidence)
			reviews = append(reviews, review)
			warnings = appendUnique(warnings, fmt.Sprintf("backfill %s season %d episode %d: low-confidence candidate %q requires manual confirmation", w.Key, st.Season, value.Episode.EpisodeStart, value.Release.MediaName))
		}
	}
	a.reconcileRepairReviews(reviews, rt.bundle.Entries, inv)
	return selected, acquisitions, warnings, errors.Join(errs...)
}

// runManualBackfill completes only the explicit range supplied by the user.
// The range is command input, not durable state; current filesystem inventory
// determines the exact holes and historical search is targeted only at them.
func (a *App) runManualBackfill(ctx context.Context, rt *runtimeSnapshot, entryKey string, season int, r completeness.EpisodeRange, inv inventory.Snapshot, graph observation.Graph, runner automation.Runner) (completeness.SeasonState, []candidate.Candidate, []automation.AcquireResult, []RepairReview, []string, error) {
	w, ok := rt.bundle.Entries[entryKey]
	if !ok {
		return completeness.SeasonState{}, nil, nil, nil, nil, managementErr(ErrorNotFound, fmt.Errorf("entry %q not found", entryKey))
	}
	if r.From <= 0 || r.Through < r.From {
		return completeness.SeasonState{}, nil, nil, nil, nil, managementErr(ErrorInvalid, fmt.Errorf("completion range requires positive from and through >= from"))
	}
	si := inv.Season(entryKey, season)
	if !si.Known {
		return completeness.SeasonState{EntryKey: entryKey, Season: season, InventoryKnown: false, Status: "unknown"}, nil, nil, nil, nil, managementErr(ErrorConflict, fmt.Errorf("manual completion requires known filesystem inventory"))
	}
	missing, status := completeness.MissingInRange(inv, entryKey, season, r)
	st := completeness.SeasonState{EntryKey: entryKey, Season: season, InventoryKnown: true, Missing: missing, Status: status}
	if status != "incomplete" || len(missing) == 0 {
		return st, nil, nil, nil, nil, nil
	}
	sourceKeys := completeness.EpisodeKeys(w.Key, season, missing)
	queries := backfillSearchEvidence(w, graph, rt.bundle.Organizer.Extensions)
	evidence := catalog.ObserveFolderProjectionEvidenceFromSnapshot(w, graph.Sources[w.Key], rt.bundle.Organizer.Extensions)
	bySource := map[string][]release.Release{}
	var warnings []string
	var searchErrs []error
	searched := false
	for _, fid := range catalog.EffectiveSources(w, configstore.EnabledSourceIDs(rt.bundle)) {
		cfg, exists := rt.bundle.Sources[fid]
		if !exists || !cfg.Enabled {
			continue
		}
		values, e := a.searchBackfillSourceObserved(ctx, rt, fid, backfill.Request{EntryKey: w.Key, Queries: queries, SourceMissing: sourceKeys}, evidence)
		if e != nil {
			if !errors.Is(e, backfill.ErrUnsupported) {
				warnings = append(warnings, fmt.Sprintf("targeted history %s: %v", fid, e))
				if len(values) == 0 {
					searchErrs = append(searchErrs, e)
				}
			}
			if len(values) == 0 {
				continue
			}
		}
		searched = true
		bySource[fid] = append(bySource[fid], values...)
	}
	if !searched {
		if len(searchErrs) > 0 {
			return st, nil, nil, nil, warnings, errors.Join(searchErrs...)
		}
		return st, nil, nil, nil, warnings, backfill.ErrUnsupported
	}
	plans, err := buildBackfillSelection(ctx, rt.bundle, w, season, missing, bySource, evidence)
	for _, warning := range plans.Warnings {
		warnings = appendUnique(warnings, warning)
	}
	if err != nil {
		return st, nil, nil, nil, warnings, err
	}
	chosen, acq, ws, runErr := acquireAvailable(ctx, runner, plans.Automatic, inv, graph)
	warnings = append(warnings, ws...)
	reviewCandidates, reviewWarnings := reviewAvailable(plans.Review, inv)
	warnings = append(warnings, reviewWarnings...)
	reviews := make([]RepairReview, 0, len(reviewCandidates))
	for _, value := range reviewCandidates {
		reviews = append(reviews, newRepairReviewObserved(w, value, evidence))
	}
	return st, chosen, acq, reviews, warnings, runErr
}
