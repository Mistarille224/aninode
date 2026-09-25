package release

import (
	"testing"

	"aninode/internal/rss"
)

func observed(provider string, entries []rss.Entry) ([]Release, error) {
	names := make([]NameObservation, len(entries))
	for i := range entries {
		names[i] = NameObservation{Value: entries[i].Title, Source: "test-native-name"}
	}
	return EnrichManyObserved(provider, entries, names)
}

func TestObservedAnimeRelease(t *testing.T) {
	entries := []rss.Entry{{Title: "display label"}}
	values, err := EnrichManyObserved("dmhy", entries, []NameObservation{{Value: "[grpabc165][abcabc142] [12v2][1080p][CHS].mkv", Source: "torrent-info"}})
	if err != nil {
		t.Fatal(err)
	}
	value := values[0]
	if value.Group != "grpabc165" || value.Episode != "E12v2" || value.Resolution != "1080p" || value.Subtitle != "zh-Hans" {
		t.Fatalf("release = %+v", value)
	}
	if value.Title != "display label" || value.MediaName == value.Title {
		t.Fatalf("display/native names were not kept separate: %+v", value)
	}
}

func TestObservedClassifiesEachReleaseIndependently(t *testing.T) {
	entries := []rss.Entry{{Title: "display one"}, {Title: "display two"}}
	values, err := EnrichManyObserved("mikan", entries, []NameObservation{
		{Value: "[grpabc166] abcabc145 - 03 [1080P]", Source: "torrent-info"},
		{Value: "[grpabc166] abcabc026 (2026) [1080P]", Source: "torrent-info"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if values[0].MediaType != "series" || values[1].MediaType != "movie" {
		t.Fatalf("classification: %+v", values)
	}
}

func TestObservedManyUsesSiblingEpisodeDifferences(t *testing.T) {
	entries := []rss.Entry{{Title: "display one"}, {Title: "display two"}}
	values, err := EnrichManyObserved("generic", entries, []NameObservation{
		{Value: "abcabc033 01 WEB 1080p", Source: "torrent-info"},
		{Value: "abcabc033 02 WEB 1080p", Source: "torrent-info"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for i, value := range values {
		if value.MediaType != "series" || value.Parsed.Title != "abcabc033" || value.Parsed.EpisodeStart != i+1 {
			t.Fatalf("release[%d]=%+v", i, value)
		}
	}
}

func TestObservedFullyBracketedSeasonOnlyReleaseIsSeries(t *testing.T) {
	entries := []rss.Entry{{Title: "display label"}}
	values, err := EnrichManyObserved("generic", entries, []NameObservation{{
		Value:  "[grpabc150][abcabc067 S4][1080p][CHS]",
		Source: "torrent-info",
	}})
	if err != nil {
		t.Fatal(err)
	}
	value := values[0]
	if value.MediaType != "series" || value.Components.Title != "abcabc067" || value.Components.Season != 4 {
		t.Fatalf("release=%+v", value)
	}
}

func TestObservedWeakNumericTailNeedsReliableSeriesEvidence(t *testing.T) {
	entries := []rss.Entry{{Title: "display label"}}
	values, err := EnrichManyObserved("generic", entries, []NameObservation{{Value: "abcabc145 - 03.mkv", Source: "torrent-info"}})
	if err != nil {
		t.Fatal(err)
	}
	if values[0].MediaType != "movie" || values[0].Components.EpisodeEvidence != "weak" {
		t.Fatalf("release=%+v", values[0])
	}
}

func TestObservedParenthesizedYearReachesReleaseComponents(t *testing.T) {
	entries := []rss.Entry{{Title: "display label"}}
	values, err := EnrichManyObserved("mikan", entries, []NameObservation{{Value: "[grpabc166] abcabc068 (2026) - 01 [1080P]", Source: "torrent-info"}})
	if err != nil {
		t.Fatal(err)
	}
	value := values[0]
	if value.MediaType != "series" || value.Components.Title != "abcabc068" || value.Components.Year != 2026 || value.Components.EpisodeStart != 1 {
		t.Fatalf("release=%+v", value)
	}
}
