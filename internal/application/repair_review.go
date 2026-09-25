package application

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"aninode/internal/candidate"
	"aninode/internal/catalog"
	"aninode/internal/inventory"
	"aninode/internal/medianame"
)

// RepairDifference describes one observable way a low-confidence historical
// candidate differs from the stable neighboring files. It is presentation
// evidence only and is reconstructed from current filesystem-derived Entry
// evidence on every repair cycle.
type RepairDifference struct {
	Field     string `json:"field"`
	Expected  string `json:"expected,omitempty"`
	Candidate string `json:"candidate,omitempty"`
}

// RepairReview is a pending manual decision produced by historical repair.
// Reviews remain available across later repair scans until their episode range
// is actually filled or the user confirms the candidate. Candidate is retained
// in memory so confirmation can revalidate the exact release against the current
// configuration before acquisition. A daemon restart may reconstruct the review
// from the next historical search; filesystem truth remains authoritative.
type RepairReview struct {
	ID            string             `json:"id"`
	EntryKey      string             `json:"entry_key"`
	Season        int                `json:"season"`
	EpisodeStart  int                `json:"episode_start"`
	EpisodeEnd    int                `json:"episode_end,omitempty"`
	SourceID      string             `json:"source_id"`
	Release       RepairRelease      `json:"release"`
	ExpectedGroup string             `json:"expected_group,omitempty"`
	Differences   []RepairDifference `json:"differences,omitempty"`
	Reason        string             `json:"reason"`
	candidate     candidate.Candidate
}

// RepairRelease keeps the review API compact while retaining the fields a
// user needs to judge the fallback. The full transient candidate remains
// server-side and is revalidated at confirmation time.
type RepairRelease struct {
	Title       string `json:"title"`
	MediaName   string `json:"media_name,omitempty"`
	Provider    string `json:"provider,omitempty"`
	Group       string `json:"group,omitempty"`
	Resolution  string `json:"resolution,omitempty"`
	Subtitle    string `json:"subtitle,omitempty"`
	PublishedAt string `json:"published_at,omitempty"`
}

func newRepairReview(w catalog.Entry, c candidate.Candidate) RepairReview {
	return newRepairReviewObserved(w, c, catalog.ObserveFolderProjectionEvidence(w))
}

func newRepairReviewObserved(w catalog.Entry, c candidate.Candidate, evidence []catalog.FolderProjectionEvidence) RepairReview {
	group, stable := catalog.StableObservedGroup(evidence, c.Episode.Season)
	if !stable {
		group, stable = catalog.StableObservedGroupAny(evidence)
	}
	if !stable {
		group = ""
	}
	traits := catalog.StableObservedReleaseTraits(evidence, c.Episode.Season)
	technical := medianame.TechnicalTraitsOf(c.Release.MediaName)

	differences := make([]RepairDifference, 0, 7)
	addDifference := func(field, expected, got string) {
		expected, got = strings.TrimSpace(expected), strings.TrimSpace(got)
		if expected == "" || strings.EqualFold(expected, got) {
			return
		}
		differences = append(differences, RepairDifference{Field: field, Expected: expected, Candidate: got})
	}
	addDifference("字幕组", group, c.Release.Group)
	addDifference("分辨率", traits.Resolution, c.Release.Resolution)
	addDifference("字幕", traits.Subtitle, c.Release.Subtitle)
	addDifference("来源", traits.Source, technical.Source)
	addDifference("视频编码", traits.VideoCodec, technical.VideoCodec)
	addDifference("位深", traits.BitDepth, technical.BitDepth)
	addDifference("音频编码", traits.AudioCodec, technical.AudioCodec)

	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d\x00%d\x00%d\x00%s", w.Key, c.Episode.Season, c.Episode.EpisodeStart, c.Episode.End(), c.Acquisition.ID)))
	published := ""
	if !c.Release.PublishedAt.IsZero() {
		published = c.Release.PublishedAt.UTC().Format("2006-01-02T15:04:05Z")
	}
	return RepairReview{
		ID:            hex.EncodeToString(sum[:12]),
		EntryKey:      w.Key,
		Season:        c.Episode.Season,
		EpisodeStart:  c.Episode.EpisodeStart,
		EpisodeEnd:    c.Episode.End(),
		SourceID:      c.SourceID,
		ExpectedGroup: group,
		Differences:   differences,
		Reason:        "未找到可自动接受的同字幕组版本；这个候选需要手动确认",
		Release: RepairRelease{
			Title: c.Release.Title, MediaName: c.Release.MediaName, Provider: c.Release.Provider,
			Group: c.Release.Group, Resolution: c.Release.Resolution, Subtitle: c.Release.Subtitle,
			PublishedAt: published,
		},
		candidate: c,
	}
}

func (r RepairReview) episodeRange() (int, int) {
	start := r.EpisodeStart
	end := r.EpisodeEnd
	if end < start {
		end = start
	}
	return start, end
}

func cloneRepairReview(in RepairReview) RepairReview {
	out := in
	out.Differences = append([]RepairDifference(nil), in.Differences...)
	return out
}

func reviewRangesOverlap(a, b RepairReview) bool {
	as, ae := a.episodeRange()
	bs, be := b.episodeRange()
	return as <= be && bs <= ae
}

func reviewStillNeeded(value RepairReview, entries map[string]catalog.Entry, inv inventory.Snapshot) bool {
	if _, ok := entries[value.EntryKey]; !ok {
		return false
	}
	si := inv.Season(value.EntryKey, value.Season)
	if !si.Known {
		return true
	}
	start, end := value.episodeRange()
	if start <= 0 || end < start {
		return false
	}
	for ep := start; ep <= end; ep++ {
		if _, ok := si.Episodes[ep]; !ok {
			return true
		}
	}
	return false
}

// reconcileRepairReviews updates the pending-review queue without treating a
// transient empty search result as a user decision. Existing reviews survive
// later scans while their target range is still missing. A newly observed
// review replaces older overlapping advice for the same Entry/season.
func (a *App) reconcileRepairReviews(values []RepairReview, entries map[string]catalog.Entry, inv inventory.Snapshot) {
	a.repairMu.Lock()
	defer a.repairMu.Unlock()

	next := make(map[string]RepairReview, len(a.repairReviews)+len(values))
	for id, value := range a.repairReviews {
		if reviewStillNeeded(value, entries, inv) {
			next[id] = cloneRepairReview(value)
		}
	}
	for _, value := range values {
		for id, previous := range next {
			if previous.EntryKey == value.EntryKey && previous.Season == value.Season && reviewRangesOverlap(previous, value) {
				delete(next, id)
			}
		}
		next[value.ID] = cloneRepairReview(value)
	}
	a.repairReviews = next
}

// replaceRepairReviewsFor refreshes advice for one explicitly searched season
// while preserving pending reviews for other missing ranges in that season.
// Empty search results therefore do not make a pending user decision disappear.
func (a *App) replaceRepairReviewsFor(entryKey string, season int, values []RepairReview) {
	a.repairMu.Lock()
	defer a.repairMu.Unlock()
	if a.repairReviews == nil {
		a.repairReviews = map[string]RepairReview{}
	}
	for _, value := range values {
		for id, previous := range a.repairReviews {
			if previous.EntryKey == entryKey && previous.Season == season && reviewRangesOverlap(previous, value) {
				delete(a.repairReviews, id)
			}
		}
		a.repairReviews[value.ID] = cloneRepairReview(value)
	}
}

func (a *App) repairReviewsFor(entryKey string, season int) []RepairReview {
	a.repairMu.RLock()
	out := make([]RepairReview, 0)
	for _, value := range a.repairReviews {
		if value.EntryKey == entryKey && value.Season == season {
			out = append(out, cloneRepairReview(value))
		}
	}
	a.repairMu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		is, ie := out[i].episodeRange()
		js, je := out[j].episodeRange()
		if is != js {
			return is < js
		}
		if ie != je {
			return ie < je
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func (a *App) repairReview(id string) (RepairReview, bool) {
	a.repairMu.RLock()
	value, ok := a.repairReviews[id]
	a.repairMu.RUnlock()
	return cloneRepairReview(value), ok
}

func (a *App) removeRepairReview(id string) {
	a.repairMu.Lock()
	delete(a.repairReviews, id)
	a.repairMu.Unlock()
}
