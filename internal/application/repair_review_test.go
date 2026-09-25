package application

import (
	"testing"

	"aninode/internal/catalog"
	"aninode/internal/inventory"
)

func TestRepairReviewSurvivesTransientEmptyScanUntilEpisodeIsFilled(t *testing.T) {
	a := &App{repairReviews: map[string]RepairReview{}}
	entries := map[string]catalog.Entry{
		"series/abcabc146": {Key: "series/abcabc146", MediaType: catalog.MediaSeries},
	}
	pending := RepairReview{ID: "old", EntryKey: "series/abcabc146", Season: 1, EpisodeStart: 6, EpisodeEnd: 6}

	missing := inventory.Snapshot{Entries: map[string]inventory.EntryInventory{
		"series/abcabc146": {Known: true, Seasons: map[int]inventory.SeasonInventory{
			1: {Known: true, Episodes: map[int]struct{}{1: {}, 2: {}, 3: {}, 4: {}, 5: {}}},
		}},
	}}
	a.reconcileRepairReviews([]RepairReview{pending}, entries, missing)
	if got := a.repairReviewsFor("series/abcabc146", 1); len(got) != 1 || got[0].ID != "old" {
		t.Fatalf("initial pending review = %#v, want old review", got)
	}

	// A later historical search can temporarily return no candidate. That is not
	// a user decision, so the pending review must remain visible.
	a.reconcileRepairReviews(nil, entries, missing)
	if got := a.repairReviewsFor("series/abcabc146", 1); len(got) != 1 || got[0].ID != "old" {
		t.Fatalf("review after empty scan = %#v, want old review retained", got)
	}

	// Fresh advice for the same missing episode supersedes the older candidate.
	fresh := RepairReview{ID: "fresh", EntryKey: "series/abcabc146", Season: 1, EpisodeStart: 6, EpisodeEnd: 6}
	a.reconcileRepairReviews([]RepairReview{fresh}, entries, missing)
	if got := a.repairReviewsFor("series/abcabc146", 1); len(got) != 1 || got[0].ID != "fresh" {
		t.Fatalf("review after fresh advice = %#v, want only fresh review", got)
	}

	complete := inventory.Snapshot{Entries: map[string]inventory.EntryInventory{
		"series/abcabc146": {Known: true, Seasons: map[int]inventory.SeasonInventory{
			1: {Known: true, Episodes: map[int]struct{}{1: {}, 2: {}, 3: {}, 4: {}, 5: {}, 6: {}}},
		}},
	}}
	a.reconcileRepairReviews(nil, entries, complete)
	if got := a.repairReviewsFor("series/abcabc146", 1); len(got) != 0 {
		t.Fatalf("review after episode is filled = %#v, want none", got)
	}
}

func TestManualBackfillDoesNotErasePendingReviewOnEmptySearch(t *testing.T) {
	a := &App{repairReviews: map[string]RepairReview{
		"keep": {ID: "keep", EntryKey: "series/abcabc146", Season: 1, EpisodeStart: 6, EpisodeEnd: 6},
	}}
	a.replaceRepairReviewsFor("series/abcabc146", 1, nil)
	if got := a.repairReviewsFor("series/abcabc146", 1); len(got) != 1 || got[0].ID != "keep" {
		t.Fatalf("pending review after empty manual search = %#v, want keep", got)
	}
}
