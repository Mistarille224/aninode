package episode

import (
	"aninode/internal/medianame"
	"aninode/internal/release"
	"testing"
)

func TestEpisodeIdentityRevisionAndRange(t *testing.T) {
	cases := []struct {
		ep      string
		a, b, v int
	}{{"03", 3, 3, 1}, {"E03", 3, 3, 1}, {"S01E03", 3, 3, 1}, {"03v2", 3, 3, 2}, {"03-04", 3, 4, 1}}
	for _, c := range cases {
		k, e := FromRelease("w", 1, release.Release{Components: medianame.Components{EpisodeStart: c.a, EpisodeEnd: c.b, Version: c.v}, Episode: c.ep, Title: c.ep})
		if e != nil || k.EpisodeStart != c.a || k.EpisodeEnd != c.b || k.Revision != c.v {
			t.Fatalf("%s => %+v %v", c.ep, k, e)
		}
	}
}

func TestFromReleasePreservesExplicitSeasonZero(t *testing.T) {
	k, err := FromRelease("w", 1, release.Release{Title: "abcabc146 S00E03", Components: medianame.Components{Season: 0, SeasonExplicit: true, EpisodeStart: 3, EpisodeEnd: 3}})
	if err != nil {
		t.Fatal(err)
	}
	if k.Season != 0 || !k.Special {
		t.Fatalf("explicit S00 became default season: %+v", k)
	}
}

func TestEpisodeRangeOverlapAndCoverage(t *testing.T) {
	pack := Key{EntryKey: "w", Season: 1, EpisodeStart: 5, EpisodeEnd: 6}
	six := Key{EntryKey: "w", Season: 1, EpisodeStart: 6, EpisodeEnd: 6}
	seven := Key{EntryKey: "w", Season: 1, EpisodeStart: 7, EpisodeEnd: 7}
	if !pack.Contains(6) || !pack.Overlaps(six) || !pack.Covers(six) {
		t.Fatalf("range semantics failed: pack=%+v six=%+v", pack, six)
	}
	if pack.Overlaps(seven) || pack.Covers(seven) {
		t.Fatalf("non-overlapping episode was treated as covered: %+v", seven)
	}
}
