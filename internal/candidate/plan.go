package candidate

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"aninode/internal/acquisition"
	"aninode/internal/catalog"
	"aninode/internal/configstore"
	"aninode/internal/episode"
	"aninode/internal/medianame"
	"aninode/internal/naming"
	"aninode/internal/release"
	"aninode/internal/releasefilter"
)

type Decision string

const (
	DecisionSelected   Decision = "selected"
	DecisionFiltered   Decision = "filtered"
	DecisionDuplicate  Decision = "duplicate"
	DecisionInvalid    Decision = "invalid"
	DecisionSuperseded Decision = "superseded"
)

type Candidate struct {
	MediaType     string               `json:"media_type"`
	EntryKey      string               `json:"entry_key"`
	SourceID      string               `json:"source_id"`
	Release       release.Release      `json:"release"`
	SourceEpisode episode.Key          `json:"source_episode"`
	Episode       episode.Key          `json:"episode"`
	Acquisition   acquisition.Identity `json:"acquisition,omitempty"`
	Decision      Decision             `json:"decision"`
	Reason        string               `json:"reason,omitempty"`

	// SourceFolder is the real declared first-level source container selected
	// by filesystem evidence. It is operation-local routing evidence, not
	// persisted semantic state and not part of the HTTP candidate contract.
	SourceFolder string `json:"-"`
}
type Plan struct {
	Candidates []Candidate `json:"candidates"`
	Warnings   []string    `json:"warnings,omitempty"`
}

func Build(ctx context.Context, b configstore.Bundle, bySource map[string][]release.Release, names naming.Index) (Plan, error) {
	evidence := make(map[string][]catalog.FolderProjectionEvidence, len(b.Entries))
	for _, w := range sortedEntries(b.Entries) {
		evidence[w.Key] = catalog.ObserveFolderProjectionEvidence(w, b.Organizer.Extensions)
	}
	return BuildObserved(ctx, b, bySource, names, evidence)
}

// BuildObserved evaluates RSS candidates using caller-supplied filesystem
// evidence. Reconciliation uses this form so one observation graph feeds naming,
// inventory and candidate projection without a second tree walk.
func BuildObserved(ctx context.Context, b configstore.Bundle, bySource map[string][]release.Release, names naming.Index, evidenceByEntry map[string][]catalog.FolderProjectionEvidence) (Plan, error) {
	p := Plan{}
	seenRelease := map[string]bool{}
	for _, w := range sortedEntries(b.Entries) {
		evidence := evidenceByEntry[w.Key]
		for _, sourceID := range sourcesForEntry(b, w) {
			source, ok := b.Sources[sourceID]
			if !ok || !source.Enabled {
				continue
			}
			for _, r := range bySource[sourceID] {
				if r.MediaType != "" && r.MediaType != w.MediaType {
					continue
				}
				// RSS is a broad observation stream. A release becomes actionable only
				// when it matches an Entry declaration.
				if !matchesWork(w, names[w.Key], r) {
					continue
				}
				c := evaluate(w, sourceID, r, evidence)
				if c.Decision == DecisionSelected {
					if seenRelease[c.Acquisition.ID] {
						c.Decision, c.Reason = DecisionDuplicate, "duplicate release in plan"
					} else {
						seenRelease[c.Acquisition.ID] = true
					}
				}
				p.Candidates = append(p.Candidates, c)
			}
		}
	}
	selectBestByScore(&p, b, func(c Candidate) int { return continuityScore(c, evidenceByEntry[c.EntryKey]) })
	return p, nil
}

// BuildBoundPlan evaluates historical/search releases against an Entry that is
// already known by the caller. Unlike Build, it deliberately does not route by
// release title: targeted backfill lives in the tracker naming domain,
// where the native release title can differ from the canonical Entry title.
// Filters, source->target episode projection, acquisition identity, duplicate
// suppression, and best-candidate selection remain identical to the RSS path.
func BuildBoundPlan(ctx context.Context, b configstore.Bundle, entryKey string, bySource map[string][]release.Release) (Plan, error) {
	w, ok := b.Entries[entryKey]
	if !ok {
		return Plan{}, fmt.Errorf("entry %q not found", entryKey)
	}
	return BuildBoundPlanObserved(ctx, b, entryKey, bySource, catalog.ObserveFolderProjectionEvidence(w, b.Organizer.Extensions))
}

// BuildBoundPlanObserved is the historical/search counterpart of BuildObserved.
// The caller supplies evidence from its current operation graph.
func BuildBoundPlanObserved(ctx context.Context, b configstore.Bundle, entryKey string, bySource map[string][]release.Release, evidence []catalog.FolderProjectionEvidence) (Plan, error) {
	return buildBoundPlan(ctx, b, entryKey, bySource, evidence)
}

func buildBoundPlan(ctx context.Context, b configstore.Bundle, entryKey string, bySource map[string][]release.Release, evidence []catalog.FolderProjectionEvidence) (Plan, error) {
	if err := ctx.Err(); err != nil {
		return Plan{}, err
	}
	w, ok := b.Entries[entryKey]
	if !ok {
		return Plan{}, fmt.Errorf("entry %q not found", entryKey)
	}
	p := Plan{}
	seenRelease := map[string]bool{}
	sourceIDs := make([]string, 0, len(bySource))
	for sourceID := range bySource {
		sourceIDs = append(sourceIDs, sourceID)
	}
	sort.Strings(sourceIDs)
	for _, sourceID := range sourceIDs {
		source, ok := b.Sources[sourceID]
		if !ok {
			return Plan{}, fmt.Errorf("content source %q not found", sourceID)
		}
		if !source.Enabled {
			return Plan{}, fmt.Errorf("content source %q is disabled", sourceID)
		}
		values := bySource[sourceID]
		skippedMediaType := 0
		for _, r := range values {
			if err := ctx.Err(); err != nil {
				return Plan{}, err
			}
			// Historical search is deliberately high-recall. Per-release noise is
			// retained as diagnostics instead of failing the whole search batch.
			if r.MediaType != "" && r.MediaType != w.MediaType {
				skippedMediaType++
				p.Candidates = append(p.Candidates, Candidate{MediaType: w.MediaType, EntryKey: entryKey, SourceID: sourceID, Release: r, Decision: DecisionInvalid, Reason: fmt.Sprintf("release media type %q does not match bound entry media type %q", r.MediaType, w.MediaType)})
				continue
			}
			c := evaluate(w, sourceID, r, evidence)
			if c.Decision == DecisionSelected {
				if seenRelease[c.Acquisition.ID] {
					c.Decision, c.Reason = DecisionDuplicate, "duplicate release in plan"
				} else {
					seenRelease[c.Acquisition.ID] = true
				}
			}
			p.Candidates = append(p.Candidates, c)
		}
		if skippedMediaType > 0 {
			p.Warnings = append(p.Warnings, fmt.Sprintf("historical source %s: skipped %d release(s) whose native filename did not identify the bound entry media type", sourceID, skippedMediaType))
		}
	}
	selectBestByScore(&p, b, func(c Candidate) int { return continuityScore(c, evidence) })
	return p, nil
}

// BuildBound is the explicit search-to-acquisition boundary. The caller has
// selected the destination Entry, so routing/title matching is intentionally
// skipped. All actual candidate evaluation is shared with RSS planning.
func BuildBound(ctx context.Context, b configstore.Bundle, entryKey, sourceID string, r release.Release) (Candidate, error) {
	select {
	case <-ctx.Done():
		return Candidate{}, ctx.Err()
	default:
	}
	w, ok := b.Entries[entryKey]
	if !ok {
		return Candidate{}, fmt.Errorf("entry %q not found", entryKey)
	}
	source, ok := b.Sources[sourceID]
	if !ok {
		return Candidate{}, fmt.Errorf("content source %q not found", sourceID)
	}
	if !source.Enabled {
		return Candidate{}, fmt.Errorf("content source %q is disabled", sourceID)
	}
	if r.MediaType != "" && r.MediaType != w.MediaType {
		return Candidate{}, fmt.Errorf("release media type %q does not match entry media type %q", r.MediaType, w.MediaType)
	}
	return evaluate(w, sourceID, r, catalog.ObserveFolderProjectionEvidence(w, b.Organizer.Extensions)), nil
}

// evaluate contains the single candidate semantics used by both automatic RSS
// observations and explicitly bound search results. It does not perform routing.
func evaluate(w catalog.Entry, sourceID string, r release.Release, evidence []catalog.FolderProjectionEvidence) Candidate {
	c := Candidate{MediaType: w.MediaType, EntryKey: w.Key, SourceID: sourceID, Release: r}
	if reason := w.Filters.RejectReason(r); reason != "" {
		c.Decision, c.Reason = DecisionFiltered, reason
		return c
	}
	if w.MediaType == catalog.MediaMovie {
		c.SourceEpisode = episode.Key{EntryKey: w.Key}
		c.Episode = c.SourceEpisode
	} else {
		if r.Components.EpisodeStart <= 0 {
			c.Decision, c.Reason = DecisionInvalid, "release has no episode identity"
			return c
		}
		end := r.Components.EpisodeEnd
		if end < r.Components.EpisodeStart {
			end = r.Components.EpisodeStart
		}
		// Preserve the parser's seasonless state while projecting the release.
		// ProjectObservedEpisode has a dedicated filesystem-evidence path for
		// absolute-numbered releases (for example 41,42,43 -> Season 02).
		// Converting Season=0 to Season=1 before projection makes that path
		// unreachable and incorrectly routes the release to S01.
		parsedSeason := r.Components.Season
		seasonExplicit := r.Components.SeasonExplicit
		ts, te, sourceFolder, ok := catalog.ProjectObservedEpisode(w, evidence, parsedSeason, seasonExplicit, r.Components.EpisodeStart)
		if !ok || te <= 0 {
			c.Decision, c.Reason = DecisionInvalid, "seasonless release cannot be mapped unambiguously to a declared source folder"
			return c
		}
		es, ee, endFolder, endOK := catalog.ProjectObservedEpisode(w, evidence, parsedSeason, seasonExplicit, end)
		if !endOK || ee <= 0 || es != ts || (sourceFolder != "" && endFolder != "" && sourceFolder != endFolder) {
			c.Decision, c.Reason = DecisionInvalid, "episode mapping crosses or cannot resolve a target season"
			return c
		}
		if sourceFolder == "" {
			sourceFolder = endFolder
		}
		// Keep source identity independent from publication projection. The
		// acquisition identity keeps the historical default season hint for a
		// seasonless filename, while the target identity above is derived from
		// current filesystem evidence.
		sourceSeason := parsedSeason
		if sourceSeason <= 0 && !seasonExplicit {
			sourceSeason = 1
		}
		c.SourceEpisode = episode.Key{EntryKey: w.Key, Season: sourceSeason, EpisodeStart: r.Components.EpisodeStart, EpisodeEnd: end, Revision: r.Components.Version, Special: sourceSeason == 0}
		c.Episode = episode.Key{EntryKey: w.Key, Season: ts, EpisodeStart: te, EpisodeEnd: ee, Revision: r.Components.Version, Special: ts == 0}
		c.SourceFolder = sourceFolder

	}
	ident, err := identity(r)
	if err != nil {
		c.Decision, c.Reason = DecisionInvalid, err.Error()
		return c
	}
	c.Acquisition = ident
	c.Decision = DecisionSelected
	return c
}

func matchesWork(w catalog.Entry, names naming.EntryEvidence, r release.Release) bool {
	if r.MediaType != "" && w.MediaType != r.MediaType {
		return false
	}
	return naming.Match(w, names, r.Components.Title, r.Components.Year)
}

func identity(r release.Release) (acquisition.Identity, error) { return release.AcquisitionIdentity(r) }

// ReviewReason separates acquisition eligibility from automation confidence.
// Explicit filters decide whether a release is legal; stable neighboring media
// only decides whether a legal historical candidate should be auto-submitted or
// surfaced for confirmation.
func ReviewReason(c Candidate, evidence []catalog.FolderProjectionEvidence) string {
	if c.MediaType != catalog.MediaSeries || c.Decision != DecisionSelected {
		return ""
	}
	group, stable := catalog.StableObservedGroup(evidence, c.Episode.Season)
	if !stable {
		group, stable = catalog.StableObservedGroupAny(evidence)
	}
	if !stable || releasefilter.Same(group, c.Release.Group) {
		return ""
	}
	technical := medianame.TechnicalTraitsOf(c.Release.MediaName)
	traits := catalog.StableObservedReleaseTraits(evidence, c.Episode.Season)
	if technical.SourceKind == "" {
		return fmt.Sprintf("release group %q differs from stable managed group %q and source family is unknown", c.Release.Group, group)
	}
	if traits.SourceKind != "" && !releasefilter.Same(traits.SourceKind, technical.SourceKind) {
		return fmt.Sprintf("release group %q differs from stable managed group %q and source kind %q differs from %q", c.Release.Group, group, technical.SourceKind, traits.SourceKind)
	}
	return fmt.Sprintf("release group %q differs from stable managed group %q", c.Release.Group, group)
}

func continuityScore(c Candidate, evidence []catalog.FolderProjectionEvidence) int {
	traits := catalog.StableObservedReleaseTraits(evidence, c.Episode.Season)
	technical := medianame.TechnicalTraitsOf(c.Release.MediaName)
	score := 0

	group, stableGroup := catalog.StableObservedGroup(evidence, c.Episode.Season)
	if !stableGroup {
		group, stableGroup = catalog.StableObservedGroupAny(evidence)
	}
	if stableGroup && releasefilter.Same(group, c.Release.Group) {
		score += 1000
	}

	if traits.Resolution != "" {
		switch {
		case releasefilter.Same(traits.Resolution, c.Release.Resolution):
			score += 160
		case strings.TrimSpace(c.Release.Resolution) == "":
			score -= 25
		default:
			score -= 40
		}
	}
	switch strings.ToLower(strings.TrimSpace(c.Release.Resolution)) {
	case "2160p":
		score += 8
	case "1080p":
		score += 6
	case "720p":
		score += 4
	case "480p":
		score += 2
	}

	if traits.Subtitle != "" {
		switch {
		case releasefilter.Same(traits.Subtitle, c.Release.Subtitle):
			score += 70
		case strings.TrimSpace(c.Release.Subtitle) == "":
			score -= 10
		default:
			score -= 20
		}
	}
	if traits.SourceKind != "" && releasefilter.Same(traits.SourceKind, technical.SourceKind) {
		score += 90
	}
	if traits.Source != "" && releasefilter.Same(traits.Source, technical.Source) {
		score += 35
	}
	if technical.Source != "" {
		score += 10
	}
	switch technical.SourceKind {
	case "web":
		score += 12
	case "disc":
		score += 8
	case "broadcast":
		score += 4
	}
	if traits.VideoCodec != "" {
		if releasefilter.Same(traits.VideoCodec, technical.VideoCodec) {
			score += 45
		} else if technical.VideoCodec != "" {
			score -= 10
		}
	}
	if technical.VideoCodec != "" {
		score += 5
	}
	if traits.BitDepth != "" {
		if releasefilter.Same(traits.BitDepth, technical.BitDepth) {
			score += 25
		} else if technical.BitDepth != "" {
			score -= 5
		}
	}
	if traits.AudioCodec != "" {
		if releasefilter.Same(traits.AudioCodec, technical.AudioCodec) {
			score += 15
		} else if technical.AudioCodec != "" {
			score -= 3
		}
	}
	if medianame.ReliableEpisodeEvidence(c.Release.Components) {
		score += 10
	}
	return score
}

func selectBestByScore(p *Plan, b configstore.Bundle, score func(Candidate) int) {
	groups := map[string][]int{}
	for i, c := range p.Candidates {
		if c.Decision != DecisionSelected {
			continue
		}
		key := c.EntryKey + "/movie"
		if c.MediaType != catalog.MediaMovie {
			key = fmt.Sprintf("%s/S%02d/special=%t", c.EntryKey, c.Episode.Season, c.Episode.Special)
		}
		groups[key] = append(groups[key], i)
	}

	better := func(a, bx Candidate) bool {
		if score != nil {
			sa, sb := score(a), score(bx)
			if sa != sb {
				return sa > sb
			}
		}
		if a.Episode.Revision != bx.Episode.Revision {
			return a.Episode.Revision > bx.Episode.Revision
		}
		pa, pb := b.Sources[a.SourceID].Priority, b.Sources[bx.SourceID].Priority
		if pa != pb {
			return pa < pb
		}
		if a.Release.PublishedAt != bx.Release.PublishedAt {
			return a.Release.PublishedAt.After(bx.Release.PublishedAt)
		}
		return a.Acquisition.ID < bx.Acquisition.ID
	}

	type selection struct {
		indexes  []int
		coverage int
	}
	cloneSelection := func(in selection) selection {
		return selection{indexes: append([]int(nil), in.indexes...), coverage: in.coverage}
	}
	betterSelection := func(a, bx selection) bool {
		if a.coverage != bx.coverage {
			return a.coverage > bx.coverage
		}
		// For equal coverage, preserve the existing release-quality ordering. Sort
		// each chosen set by quality and compare its first differing candidate.
		// This keeps a high-quality pair of single-episode releases competitive
		// with a pack covering the same episodes, while coverage remains the first
		// priority so choosing a single episode cannot accidentally discard the
		// only candidate that also covers the next episode.
		aRanked := append([]int(nil), a.indexes...)
		bRanked := append([]int(nil), bx.indexes...)
		sort.SliceStable(aRanked, func(i, j int) bool {
			ai, aj := aRanked[i], aRanked[j]
			if better(p.Candidates[ai], p.Candidates[aj]) {
				return true
			}
			if better(p.Candidates[aj], p.Candidates[ai]) {
				return false
			}
			return ai < aj
		})
		sort.SliceStable(bRanked, func(i, j int) bool {
			bi, bj := bRanked[i], bRanked[j]
			if better(p.Candidates[bi], p.Candidates[bj]) {
				return true
			}
			if better(p.Candidates[bj], p.Candidates[bi]) {
				return false
			}
			return bi < bj
		})
		for i := 0; i < len(aRanked) && i < len(bRanked); i++ {
			ai, bi := aRanked[i], bRanked[i]
			if ai == bi {
				continue
			}
			if better(p.Candidates[ai], p.Candidates[bi]) {
				return true
			}
			if better(p.Candidates[bi], p.Candidates[ai]) {
				return false
			}
			return ai < bi
		}
		if len(a.indexes) != len(bx.indexes) {
			// Fewer downloads for identical coverage and quality is preferable.
			return len(a.indexes) < len(bx.indexes)
		}
		return false
	}

	for _, idx := range groups {
		if len(idx) < 2 {
			continue
		}

		if p.Candidates[idx[0]].MediaType == catalog.MediaMovie {
			sort.SliceStable(idx, func(i, j int) bool {
				return better(p.Candidates[idx[i]], p.Candidates[idx[j]])
			})
			winner := idx[0]
			p.Candidates[winner].Reason = "best release for movie"
			for _, i := range idx[1:] {
				p.Candidates[i].Decision = DecisionSuperseded
				p.Candidates[i].Reason = "superseded by " + p.Candidates[winner].Acquisition.ID
			}
			continue
		}

		// A multi-episode release is atomic: selecting E05-E06 and E06 together
		// would submit duplicate work for E06. Pick a non-overlapping set with
		// maximum episode coverage, then use the established release-quality order
		// to break ties. This is weighted interval scheduling over the inclusive
		// episode ranges, so a high-ranked E05 cannot suppress the only E05-E06 pack
		// and accidentally leave E06 uncovered.
		sort.SliceStable(idx, func(i, j int) bool {
			a, bx := p.Candidates[idx[i]].Episode, p.Candidates[idx[j]].Episode
			if a.End() != bx.End() {
				return a.End() < bx.End()
			}
			if a.EpisodeStart != bx.EpisodeStart {
				return a.EpisodeStart < bx.EpisodeStart
			}
			return better(p.Candidates[idx[i]], p.Candidates[idx[j]])
		})

		dp := make([]selection, len(idx)+1)
		for pos, candidateIndex := range idx {
			previous := -1
			start := p.Candidates[candidateIndex].Episode.EpisodeStart
			for j := pos - 1; j >= 0; j-- {
				if p.Candidates[idx[j]].Episode.End() < start {
					previous = j
					break
				}
			}
			include := cloneSelection(dp[previous+1])
			include.indexes = append(include.indexes, candidateIndex)
			include.coverage += p.Candidates[candidateIndex].Episode.Width()
			exclude := cloneSelection(dp[pos])
			if betterSelection(include, exclude) {
				dp[pos+1] = include
			} else {
				dp[pos+1] = exclude
			}
		}

		chosen := map[int]bool{}
		for _, i := range dp[len(idx)].indexes {
			chosen[i] = true
			p.Candidates[i].Reason = "best non-overlapping release set for season"
		}
		for _, i := range idx {
			if chosen[i] {
				continue
			}
			conflicts := make([]int, 0, len(chosen))
			for winner := range chosen {
				if p.Candidates[i].Episode.Overlaps(p.Candidates[winner].Episode) {
					conflicts = append(conflicts, winner)
				}
			}
			sort.SliceStable(conflicts, func(a, bx int) bool {
				return better(p.Candidates[conflicts[a]], p.Candidates[conflicts[bx]])
			})
			p.Candidates[i].Decision = DecisionSuperseded
			if len(conflicts) > 0 {
				p.Candidates[i].Reason = "overlapping episode range superseded by " + p.Candidates[conflicts[0]].Acquisition.ID
			} else {
				p.Candidates[i].Reason = "superseded by better non-overlapping release set"
			}
		}
	}
}

func sortedEntries(m map[string]catalog.Entry) []catalog.Entry {
	r := make([]catalog.Entry, 0, len(m))
	for _, w := range m {
		r = append(r, w)
	}
	sort.Slice(r, func(i, j int) bool { return r[i].Key < r[j].Key })
	return r
}

func sourcesForEntry(b configstore.Bundle, w catalog.Entry) []string {
	values := catalog.EffectiveSources(w, configstore.EnabledSourceIDs(b))
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, id := range values {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}
