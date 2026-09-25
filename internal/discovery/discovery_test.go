package discovery

import (
	"testing"

	"aninode/internal/release"
)

func TestMergeGroupsCombinesUnknownAndKnownYearButNotConflictingYears(t *testing.T) {
	groups := []Group{
		{MediaType: "series", Title: "abcabc146", Season: 1, Year: 0, SourceIDs: []string{"a"}, Releases: []release.Release{{Title: "unknown"}}},
		{MediaType: "series", Title: "abcabc146", Season: 1, Year: 2026, SourceIDs: []string{"b"}, Releases: []release.Release{{Title: "known"}}},
		{MediaType: "series", Title: "abcabc146", Season: 1, Year: 2027, SourceIDs: []string{"c"}, Releases: []release.Release{{Title: "remake"}}},
	}
	got := MergeGroups(groups)
	if len(got) != 2 {
		t.Fatalf("groups=%+v", got)
	}
	if got[0].Year != 2026 || len(got[0].SourceIDs) != 2 {
		t.Fatalf("compatible year evidence did not merge: %+v", got[0])
	}
	if got[1].Year != 2027 {
		t.Fatalf("conflicting year was merged: %+v", got[1])
	}
}
