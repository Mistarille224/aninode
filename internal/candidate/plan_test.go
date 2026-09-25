package candidate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aninode/internal/catalog"
	"aninode/internal/configstore"
	"aninode/internal/medianame"
	"aninode/internal/naming"
	"aninode/internal/release"
)

func TestUnboundGlobalRSSSourceStillRequiresTitleMatch(t *testing.T) {
	entry := catalog.Entry{Key: "series/abcabc146", MediaType: catalog.MediaSeries, Title: "abcabc146", Enabled: true}
	bundle := configstore.Bundle{
		Sources: map[string]configstore.ContentSource{"global": {ID: "global", Provider: "generic", RSS: []configstore.RSSSource{{URL: "https://example.invalid/rss"}}, Enabled: true}},
		Entries: map[string]catalog.Entry{entry.Key: entry},
	}
	r := release.Release{MediaType: "series", Title: "grpabc167 E03", DownloadURL: "magnet:?xt=urn:btih:1123456789abcdef0123456789abcdef01234567", Components: medianame.Components{Title: "grpabc167", Season: 1, EpisodeStart: 3, EpisodeEvidence: "E03"}}
	plan, err := Build(context.Background(), bundle, map[string][]release.Release{"global": {r}}, titleNames(bundle))
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Candidates) != 0 {
		t.Fatalf("global feed incorrectly routed unrelated release: %+v", plan.Candidates)
	}
}

func TestSeasonlessRSSUsesStableGroupAsPreference(t *testing.T) {
	root := t.TempDir()
	entryRoot := filepath.Join(root, "abcabc")
	for _, folder := range []string{"Season 01", "Season 02"} {
		if err := os.MkdirAll(filepath.Join(entryRoot, folder), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{
		"[grpabc166] abcabc - 41 [1080P][CHT].mp4",
		"[grpabc166] abcabc - 42 [1080P][CHT].mp4",
		"[grpabc166] abcabc - 43 [1080P][CHT].mp4",
	} {
		if err := os.WriteFile(filepath.Join(entryRoot, "Season 02", name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{
		"[grpabc166] abcabc - 01 [1080P][CHT].mp4",
		"[grpabc166] abcabc - 02 [1080P][CHT].mp4",
	} {
		if err := os.WriteFile(filepath.Join(entryRoot, "Season 01", name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	entry := catalog.Entry{
		Key: "series/abcabc", MediaType: catalog.MediaSeries, Title: "abcabc", Enabled: true,
		DeclarationPath: entryRoot, Seasons: []int{1, 2},
		FolderProjections: map[string]catalog.FolderProjection{
			"Season 01": {Season: 1}, "Season 02": {Season: 2},
		},
	}
	bundle := configstore.Bundle{
		Sources: map[string]configstore.ContentSource{"mikan": {ID: "mikan", Provider: "mikan", Enabled: true}},
		Entries: map[string]catalog.Entry{entry.Key: entry},
	}
	bad := release.Release{MediaType: "series", Provider: "mikan", MediaName: "[grpabc152] abcabc - 45 [1080P][CHT].mp4", NormalizedTitle: "abcabc", Group: "grpabc152", DownloadURL: "magnet:?xt=urn:btih:1123456789abcdef0123456789abcdef01234567", Components: medianame.Components{Title: "abcabc", EpisodeStart: 45, EpisodeEnd: 45, ReleaseGroup: "grpabc152", EpisodeEvidence: "explicit"}}
	good := release.Release{MediaType: "series", Provider: "mikan", MediaName: "[grpabc166] abcabc - 45 [1080P][CHT].mp4", NormalizedTitle: "abcabc", Group: "grpabc166", DownloadURL: "magnet:?xt=urn:btih:2123456789abcdef0123456789abcdef01234567", Components: medianame.Components{Title: "abcabc", EpisodeStart: 45, EpisodeEnd: 45, ReleaseGroup: "grpabc166", EpisodeEvidence: "explicit"}}
	plan, err := Build(context.Background(), bundle, map[string][]release.Release{"mikan": {bad, good}}, titleNames(bundle))
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Candidates) != 2 {
		t.Fatalf("candidates=%+v", plan.Candidates)
	}
	if plan.Candidates[0].Decision != DecisionSuperseded {
		t.Fatalf("different group became illegal instead of remaining a legal alternative: %+v", plan.Candidates[0])
	}
	if got := plan.Candidates[1]; got.Decision != DecisionSelected || got.Episode.Season != 2 || got.Episode.EpisodeStart != 45 || got.Release.Group != "grpabc166" {
		t.Fatalf("stable group was not preferred for the same projected episode: %+v", got)
	}
}

func TestObservedReleaseTraitsRankButDoNotFilterByDefault(t *testing.T) {
	root := t.TempDir()
	entryRoot := filepath.Join(root, "abcabc146")
	season := filepath.Join(entryRoot, "Season 01")
	if err := os.MkdirAll(season, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, ep := range []string{"01", "02"} {
		if err := os.WriteFile(filepath.Join(season, "[grpabc166] abcabc146 - "+ep+" [1080P][CHT].mkv"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	entry := catalog.Entry{Key: "series/abcabc146", MediaType: catalog.MediaSeries, Title: "abcabc146", Enabled: true, DeclarationPath: entryRoot, Seasons: []int{1}, FolderProjections: map[string]catalog.FolderProjection{"Season 01": {Season: 1}}}
	bundle := configstore.Bundle{Sources: map[string]configstore.ContentSource{"feed": {ID: "feed", Provider: "generic", Enabled: true}}, Entries: map[string]catalog.Entry{entry.Key: entry}}
	releases := []release.Release{
		{MediaType: "series", MediaName: "[grpabc166] abcabc146 - 03 [720P][CHT].mkv", NormalizedTitle: "abcabc146", Group: "grpabc166", Resolution: "720p", Subtitle: "CHT", DownloadURL: "https://example.test/720", Components: medianame.Components{Title: "abcabc146", Season: 1, EpisodeStart: 3, EpisodeEnd: 3, EpisodeEvidence: "explicit"}},
		{MediaType: "series", MediaName: "[grpabc166] abcabc146 - 03 [1080P][CHS].mkv", NormalizedTitle: "abcabc146", Group: "grpabc166", Resolution: "1080p", Subtitle: "CHS", DownloadURL: "https://example.test/chs", Components: medianame.Components{Title: "abcabc146", Season: 1, EpisodeStart: 3, EpisodeEnd: 3, EpisodeEvidence: "explicit"}},
		{MediaType: "series", MediaName: "[grpabc166] abcabc146 - 03 [1080P][CHT].mkv", NormalizedTitle: "abcabc146", Group: "grpabc166", Resolution: "1080p", Subtitle: "CHT", DownloadURL: "https://example.test/match", Components: medianame.Components{Title: "abcabc146", Season: 1, EpisodeStart: 3, EpisodeEnd: 3, EpisodeEvidence: "explicit"}},
	}
	plan, err := Build(context.Background(), bundle, map[string][]release.Release{"feed": releases}, titleNames(bundle))
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Candidates) != 3 {
		t.Fatalf("candidates=%+v", plan.Candidates)
	}
	selected := 0
	for _, c := range plan.Candidates {
		if c.Decision == DecisionFiltered {
			t.Fatalf("observed traits became an implicit filter: %+v", c)
		}
		if c.Decision == DecisionSelected {
			selected++
			if c.Release.Resolution != "1080p" || c.Release.Subtitle != "CHT" {
				t.Fatalf("continuity ranking selected unexpected release: %+v", c)
			}
		}
	}
	if selected != 1 {
		t.Fatalf("selected=%d candidates=%+v", selected, plan.Candidates)
	}
}
func TestBoundHistoricalPlanDoesNotReRouteByCanonicalTitle(t *testing.T) {
	root := t.TempDir()
	entryRoot := filepath.Join(root, "abcabc (2004)")
	season := filepath.Join(entryRoot, "Season 02")
	if err := os.MkdirAll(season, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, ep := range []string{"41", "42", "43"} {
		name := "[grpabc166] abcabc 甲乙甲乙 丙丁丙丁 - " + ep + " [1080P][CHT].mp4"
		if err := os.WriteFile(filepath.Join(season, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	entry := catalog.Entry{
		Key: "series/abcabc (2004)", MediaType: catalog.MediaSeries, Title: "abcabc", Enabled: true,
		DeclarationPath: entryRoot, Seasons: []int{2}, FolderProjections: map[string]catalog.FolderProjection{"Season 02": {Season: 2}},
	}
	bundle := configstore.Bundle{
		Sources: map[string]configstore.ContentSource{"mikan": {ID: "mikan", Provider: "mikan", Enabled: true}},
		Entries: map[string]catalog.Entry{entry.Key: entry},
	}
	r := release.Release{
		MediaType: "series", Provider: "mikan",
		MediaName:       "[grpabc166] abcabc 甲乙甲乙 丙丁丙丁 - 44 [1080P][CHT].mp4",
		NormalizedTitle: "abcabc 甲乙甲乙 丙丁丙丁", Group: "grpabc166",
		DownloadURL: "magnet:?xt=urn:btih:4123456789abcdef0123456789abcdef01234567",
		Components:  medianame.Components{Title: "abcabc 甲乙甲乙 丙丁丙丁", EpisodeStart: 44, EpisodeEnd: 44, ReleaseGroup: "grpabc166", EpisodeEvidence: "explicit"},
	}
	names := titleNames(bundle)
	names[entry.Key] = naming.EntryEvidence{Names: []string{"abcabc", "abcabc 甲乙甲乙 丙丁丙丁"}}
	unbound, err := Build(context.Background(), bundle, map[string][]release.Release{"mikan": {r}}, names)
	if err != nil {
		t.Fatal(err)
	}
	if len(unbound.Candidates) != 1 || unbound.Candidates[0].EntryKey != entry.Key {
		t.Fatalf("filesystem-derived title did not route RSS release: %+v", unbound.Candidates)
	}
	bound, err := BuildBoundPlan(context.Background(), bundle, entry.Key, map[string][]release.Release{"mikan": {r}})
	if err != nil {
		t.Fatal(err)
	}
	if len(bound.Candidates) != 1 {
		t.Fatalf("bound historical result was lost: %+v", bound.Candidates)
	}
	got := bound.Candidates[0]
	if got.Decision != DecisionSelected || got.EntryKey != entry.Key || got.Episode.Season != 2 || got.Episode.EpisodeStart != 44 {
		t.Fatalf("bound historical result did not preserve target identity: %+v", got)
	}
}

func titleNames(bundle configstore.Bundle) naming.Index {
	out := naming.Index{}
	for key, entry := range bundle.Entries {
		out[key] = naming.EntryEvidence{Names: []string{entry.Title}}
	}
	return out
}

func TestBuildBoundPlanSkipsMismatchedMediaTypeWithoutFailingBatch(t *testing.T) {
	root := t.TempDir()
	season := filepath.Join(root, "Season 02")
	if err := os.MkdirAll(season, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(season, "[grpabc166] abcabc - 43 [1080p].mkv"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	entry := catalog.Entry{
		Key: "series/abcabc (2004)", MediaType: catalog.MediaSeries, Title: "abcabc", Enabled: true,
		DeclarationPath: root, Seasons: []int{2}, FolderProjections: map[string]catalog.FolderProjection{"Season 02": {Season: 2}},
	}
	bundle := configstore.Bundle{
		Sources: map[string]configstore.ContentSource{"mikan": {ID: "mikan", Provider: "mikan", Enabled: true}},
		Entries: map[string]catalog.Entry{entry.Key: entry},
	}
	movieNoise := release.Release{
		MediaType: "movie", Provider: "mikan", MediaName: "abcabc031.mkv",
		DownloadURL: "magnet:?xt=urn:btih:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Components:  medianame.Components{Title: "abcabc031"},
	}
	valid := release.Release{
		MediaType: "series", Provider: "mikan", MediaName: "[grpabc166] abcabc - 44 [1080p].mkv", Group: "grpabc166",
		DownloadURL: "magnet:?xt=urn:btih:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Components:  medianame.Components{Title: "abcabc", EpisodeStart: 44, EpisodeEnd: 44, EpisodeEvidence: "explicit", ReleaseGroup: "grpabc166"},
	}
	plan, err := BuildBoundPlan(context.Background(), bundle, entry.Key, map[string][]release.Release{"mikan": {movieNoise, valid}})
	if err != nil {
		t.Fatalf("high-recall batch must not fail on one movie-shaped result: %v", err)
	}
	if len(plan.Candidates) != 2 {
		t.Fatalf("candidates=%+v", plan.Candidates)
	}
	if plan.Candidates[0].Decision != DecisionInvalid || !strings.Contains(plan.Candidates[0].Reason, "media type") {
		t.Fatalf("movie noise not retained as invalid evidence: %+v", plan.Candidates[0])
	}
	if plan.Candidates[1].Decision != DecisionSelected || plan.Candidates[1].Episode.Season != 2 || plan.Candidates[1].Episode.EpisodeStart != 44 {
		t.Fatalf("valid series result was lost: %+v", plan.Candidates[1])
	}
	if len(plan.Warnings) != 1 || !strings.Contains(plan.Warnings[0], "skipped 1 release") {
		t.Fatalf("warnings=%v", plan.Warnings)
	}
}

func TestBuildBoundPlanProjectsSeasonlessAbsoluteEpisodeFromFilesystemEvidence(t *testing.T) {
	root := t.TempDir()
	entryRoot := filepath.Join(root, "abcabc (2004)")
	if err := os.MkdirAll(filepath.Join(entryRoot, "Season 02"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, ep := range []string{"41", "42", "43"} {
		name := "[grpabc166] abcabc 甲乙甲乙 丙丁丙丁 - " + ep + " [1080P][CHT].mp4"
		if err := os.WriteFile(filepath.Join(entryRoot, "Season 02", name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	entry := catalog.Entry{
		Key: "series/abcabc (2004)", MediaType: catalog.MediaSeries, Title: "abcabc", Enabled: true,
		DeclarationPath: entryRoot, Seasons: []int{2},
		FolderProjections: map[string]catalog.FolderProjection{"Season 02": {Season: 2}},
	}
	bundle := configstore.Bundle{
		Sources: map[string]configstore.ContentSource{"mikan": {ID: "mikan", Provider: "mikan", Enabled: true}},
		Entries: map[string]catalog.Entry{entry.Key: entry},
	}
	r := release.Release{
		MediaType: "series", Provider: "mikan",
		MediaName:       "[grpabc166] abcabc 甲乙甲乙 丙丁丙丁 - 44 [1080P][CHT].mp4",
		NormalizedTitle: "abcabc 甲乙甲乙 丙丁丙丁", Group: "grpabc166",
		DownloadURL: "magnet:?xt=urn:btih:3123456789abcdef0123456789abcdef01234567",
		Components:  medianame.Components{Title: "abcabc 甲乙甲乙 丙丁丙丁", EpisodeStart: 44, EpisodeEnd: 44, ReleaseGroup: "grpabc166", EpisodeEvidence: "explicit"},
	}
	plan, err := BuildBoundPlan(context.Background(), bundle, entry.Key, map[string][]release.Release{"mikan": {r}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Candidates) != 1 {
		t.Fatalf("candidates=%+v", plan.Candidates)
	}
	got := plan.Candidates[0]
	if got.Decision != DecisionSelected || got.Episode.Season != 2 || got.Episode.EpisodeStart != 44 {
		t.Fatalf("seasonless bound historical release was not projected to S02E44: %+v", got)
	}
	if got.SourceEpisode.Season != 1 || got.SourceEpisode.EpisodeStart != 44 {
		t.Fatalf("source identity contract changed unexpectedly: %+v", got.SourceEpisode)
	}
	if got.SourceFolder != "Season 02" {
		t.Fatalf("seasonless continuation lost its observed source folder: %q", got.SourceFolder)
	}
}

func TestBuildBoundPlanKeepsContinuityAsPreferenceAndFiltersAuthoritative(t *testing.T) {
	root := t.TempDir()
	entryRoot := filepath.Join(root, "abcabc146 (2026)")
	seasonRoot := filepath.Join(entryRoot, "Season 01")
	if err := os.MkdirAll(seasonRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, ep := range []string{"03", "04", "05", "07"} {
		name := "[grpabc157] abcabc146 - " + ep + " [1080p][CHT].mkv"
		if err := os.WriteFile(filepath.Join(seasonRoot, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	entry := catalog.Entry{
		Key:             "series/abcabc146 (2026)",
		MediaType:       catalog.MediaSeries,
		Title:           "abcabc146",
		Enabled:         true,
		DeclarationPath: entryRoot,
		Seasons:         []int{1},
		FolderProjections: map[string]catalog.FolderProjection{
			"Season 01": {Season: 1},
		},
	}
	bundle := configstore.Bundle{
		Sources: map[string]configstore.ContentSource{"mikan": {ID: "mikan", Provider: "mikan", Enabled: true}},
		Entries: map[string]catalog.Entry{entry.Key: entry},
	}
	makeRelease := func(group, resolution, subtitle, hash string) release.Release {
		return release.Release{
			MediaType:       "series",
			Provider:        "mikan",
			MediaName:       "[" + group + "] abcabc146 - 06 [" + resolution + "][" + subtitle + "].mkv",
			NormalizedTitle: "abcabc146",
			Group:           group,
			Resolution:      resolution,
			Subtitle:        subtitle,
			DownloadURL:     "magnet:?xt=urn:btih:" + hash,
			Components: medianame.Components{
				Title: "abcabc146", EpisodeStart: 6, EpisodeEnd: 6, EpisodeEvidence: "weak", ReleaseGroup: group,
			},
		}
	}
	other := makeRelease("grpabc158", "1080p", "CHT", "6123456789abcdef0123456789abcdef01234567")
	continuous := makeRelease("grpabc157", "720p", "CHS", "7123456789abcdef0123456789abcdef01234567")

	plan, err := BuildBoundPlan(context.Background(), bundle, entry.Key, map[string][]release.Release{"mikan": {other, continuous}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Candidates) != 2 {
		t.Fatalf("candidates=%+v", plan.Candidates)
	}
	if plan.Candidates[0].Decision != DecisionSuperseded {
		t.Fatalf("different group became illegal instead of remaining a legal alternative: %+v", plan.Candidates[0])
	}
	if plan.Candidates[1].Decision != DecisionSelected || plan.Candidates[1].Release.Group != "grpabc157" {
		t.Fatalf("stable group was not used as a ranking preference: %+v", plan.Candidates)
	}

	entry.Filters.Resolutions = []string{"1080p"}
	bundle.Entries[entry.Key] = entry
	explicit, err := BuildBoundPlan(context.Background(), bundle, entry.Key, map[string][]release.Release{"mikan": {continuous}})
	if err != nil {
		t.Fatal(err)
	}
	if len(explicit.Candidates) != 1 || explicit.Candidates[0].Decision != DecisionFiltered || explicit.Candidates[0].Reason != "resolution is not allowed by entry filters" {
		t.Fatalf("explicit filter did not own legality: %+v", explicit.Candidates)
	}
}
func TestBuildBoundPlanValidatesEmptyAndMismatchedInputs(t *testing.T) {
	entry := catalog.Entry{Key: "series/abcabc146", MediaType: catalog.MediaSeries}
	b := configstore.Bundle{Entries: map[string]catalog.Entry{entry.Key: entry},
		Sources: map[string]configstore.ContentSource{"disabled": {ID: "disabled"}}}
	for _, tc := range []struct {
		name     string
		key      string
		releases map[string][]release.Release
	}{
		{name: "missing entry with empty results", key: "series/Missing"},
		{name: "missing source with mismatched media", key: entry.Key, releases: map[string][]release.Release{"missing": {{MediaType: "movie"}}}},
		{name: "disabled source with empty results", key: entry.Key, releases: map[string][]release.Release{"disabled": {}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := BuildBoundPlan(context.Background(), b, tc.key, tc.releases); err == nil {
				t.Fatal("expected validation failure")
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := BuildBoundPlan(ctx, b, entry.Key, nil); err != context.Canceled {
		t.Fatalf("error=%v, want cancellation", err)
	}
}

func TestBuildBoundPlanRanksLegalFallbacksAndReviewReasonOwnsConfidence(t *testing.T) {
	root := t.TempDir()
	season := filepath.Join(root, "Season 01")
	if err := os.MkdirAll(season, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, ep := range []string{"03", "04", "05", "07"} {
		name := "[grpabc154] abcabc146 - " + ep + " [WebRip 1080p HEVC-10bit AAC SRTx2].mkv"
		if err := os.WriteFile(filepath.Join(season, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	entry := catalog.Entry{
		Key: "series/abcabc146", MediaType: catalog.MediaSeries, Title: "abcabc146", Enabled: true,
		DeclarationPath: root, Seasons: []int{1}, FolderProjections: map[string]catalog.FolderProjection{"Season 01": {Season: 1}},
	}
	bundle := configstore.Bundle{
		Sources: map[string]configstore.ContentSource{"mikan": {ID: "mikan", Provider: "mikan", Enabled: true}},
		Entries: map[string]catalog.Entry{entry.Key: entry},
	}
	lowInformation := release.Release{
		MediaType: "series", Provider: "mikan", MediaName: "[grpabc155] abcabc146[1080P][BIG5][06][MP4]", Group: "grpabc155", Resolution: "1080p",
		DownloadURL: "magnet:?xt=urn:btih:8123456789abcdef0123456789abcdef01234567",
		Components:  medianame.Components{Title: "abcabc146", EpisodeStart: 6, EpisodeEnd: 6, EpisodeEvidence: medianame.EpisodeEvidencePublication, ReleaseGroup: "grpabc155"},
	}
	webDL := release.Release{
		MediaType: "series", Provider: "mikan", MediaName: "[grpabc153] abcabc146 - 06 [CR WEB-DL 1080p AVC AAC][CHT].mkv", Group: "grpabc153", Resolution: "1080p", Subtitle: "zh-Hant",
		DownloadURL: "magnet:?xt=urn:btih:9123456789abcdef0123456789abcdef01234567",
		Components:  medianame.Components{Title: "abcabc146", EpisodeStart: 6, EpisodeEnd: 6, EpisodeEvidence: medianame.EpisodeEvidencePublication, ReleaseGroup: "grpabc153"},
	}
	lowerResolutionSameCodec := release.Release{
		MediaType: "series", Provider: "mikan", MediaName: "[grpabc166] abcabc146 - 06 [WEB-DL 720p HEVC-10bit AAC][CHT].mkv", Group: "grpabc166", Resolution: "720p", Subtitle: "zh-Hant",
		DownloadURL: "magnet:?xt=urn:btih:a123456789abcdef0123456789abcdef01234567",
		Components:  medianame.Components{Title: "abcabc146", EpisodeStart: 6, EpisodeEnd: 6, EpisodeEvidence: medianame.EpisodeEvidencePublication, ReleaseGroup: "grpabc166"},
	}

	plan, err := BuildBoundPlan(context.Background(), bundle, entry.Key, map[string][]release.Release{"mikan": {lowInformation, webDL, lowerResolutionSameCodec}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Candidates) != 3 {
		t.Fatalf("candidates=%+v", plan.Candidates)
	}
	selected := -1
	for i, c := range plan.Candidates {
		if c.Decision == DecisionFiltered {
			t.Fatalf("continuity incorrectly became a hard filter: %+v", c)
		}
		if c.Decision == DecisionSelected {
			selected = i
		}
	}
	if selected < 0 || plan.Candidates[selected].Release.Group != "grpabc153" {
		t.Fatalf("closest compatible fallback was not ranked first: %+v", plan.Candidates)
	}
	reason := ReviewReason(plan.Candidates[selected], catalog.ObserveFolderProjectionEvidence(entry))
	if !strings.Contains(reason, "differs from stable managed group") {
		t.Fatalf("cross-group confidence did not move to review reasoning: %q", reason)
	}
}

func TestReviewReasonFlagsUnknownSourceWithoutMakingReleaseIllegal(t *testing.T) {
	root := t.TempDir()
	season := filepath.Join(root, "Season 01")
	if err := os.MkdirAll(season, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, ep := range []string{"03", "04", "05", "07"} {
		if err := os.WriteFile(filepath.Join(season, "[grpabc154] abcabc146 - "+ep+" [WebRip 1080p HEVC-10bit AAC].mkv"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	entry := catalog.Entry{Key: "series/abcabc146", MediaType: catalog.MediaSeries, Title: "abcabc146", Enabled: true, DeclarationPath: root, Seasons: []int{1}, FolderProjections: map[string]catalog.FolderProjection{"Season 01": {Season: 1}}}
	bundle := configstore.Bundle{Sources: map[string]configstore.ContentSource{"mikan": {ID: "mikan", Enabled: true}}, Entries: map[string]catalog.Entry{entry.Key: entry}}
	r := release.Release{
		MediaType: "series", MediaName: "[grpabc155] abcabc146[1080P][BIG5][06][MP4]", Group: "grpabc155", Resolution: "1080p",
		DownloadURL: "magnet:?xt=urn:btih:b123456789abcdef0123456789abcdef01234567",
		Components:  medianame.Components{Title: "abcabc146", EpisodeStart: 6, EpisodeEnd: 6, EpisodeEvidence: medianame.EpisodeEvidencePublication, ReleaseGroup: "grpabc155"},
	}
	plan, err := BuildBoundPlan(context.Background(), bundle, entry.Key, map[string][]release.Release{"mikan": {r}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Candidates) != 1 || plan.Candidates[0].Decision != DecisionSelected {
		t.Fatalf("legal cross-group fallback was rejected: %+v", plan.Candidates)
	}
	reason := ReviewReason(plan.Candidates[0], catalog.ObserveFolderProjectionEvidence(entry))
	if !strings.Contains(reason, "source family is unknown") {
		t.Fatalf("review reason=%q", reason)
	}
}
func TestBuildBoundPlanPreservesExplicitSeasonZero(t *testing.T) {
	root := t.TempDir()
	entryRoot := filepath.Join(root, "abcabc146")
	if err := os.MkdirAll(filepath.Join(entryRoot, "Season 01"), 0o755); err != nil {
		t.Fatal(err)
	}
	entry := catalog.Entry{
		Key: "series/abcabc146", MediaType: catalog.MediaSeries, Title: "abcabc146", Enabled: true,
		DeclarationPath: entryRoot, Seasons: []int{1},
		FolderProjections: map[string]catalog.FolderProjection{"Season 01": {Season: 1}},
	}
	bundle := configstore.Bundle{
		Sources: map[string]configstore.ContentSource{"mikan": {ID: "mikan", Provider: "mikan", Enabled: true}},
		Entries: map[string]catalog.Entry{entry.Key: entry},
	}
	r := release.Release{
		MediaType: "series", Provider: "mikan", MediaName: "abcabc146 S00E03.mkv", NormalizedTitle: "abcabc146",
		DownloadURL: "magnet:?xt=urn:btih:8123456789abcdef0123456789abcdef01234567",
		Components:  medianame.Components{Title: "abcabc146", Season: 0, SeasonExplicit: true, EpisodeStart: 3, EpisodeEnd: 3, EpisodeEvidence: medianame.EpisodeEvidenceExplicit},
	}
	plan, err := BuildBoundPlan(context.Background(), bundle, entry.Key, map[string][]release.Release{"mikan": {r}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Candidates) != 1 {
		t.Fatalf("candidates=%+v", plan.Candidates)
	}
	got := plan.Candidates[0]
	if got.Decision != DecisionSelected || got.SourceEpisode.Season != 0 || !got.SourceEpisode.Special || got.Episode.Season != 0 || !got.Episode.Special {
		t.Fatalf("explicit S00 was projected through Season 01 evidence: %+v", got)
	}
}

func TestSelectBestSuppressesOverlappingEpisodeRanges(t *testing.T) {
	entry := catalog.Entry{Key: "series/abcabc146", MediaType: catalog.MediaSeries, Title: "abcabc146", Enabled: true, Seasons: []int{1}}
	bundle := configstore.Bundle{
		Sources: map[string]configstore.ContentSource{"mikan": {ID: "mikan", Provider: "mikan", Enabled: true}},
		Entries: map[string]catalog.Entry{entry.Key: entry},
	}
	pack := release.Release{
		MediaType: "series", Provider: "mikan", MediaName: "abcabc146 S01E05-E06.mkv", NormalizedTitle: "abcabc146",
		DownloadURL: "magnet:?xt=urn:btih:9123456789abcdef0123456789abcdef01234567",
		Components:  medianame.Components{Title: "abcabc146", Season: 1, SeasonExplicit: true, EpisodeStart: 5, EpisodeEnd: 6, EpisodeEvidence: medianame.EpisodeEvidenceExplicit},
	}
	single := release.Release{
		MediaType: "series", Provider: "mikan", MediaName: "abcabc146 S01E06.mkv", NormalizedTitle: "abcabc146",
		DownloadURL: "magnet:?xt=urn:btih:a123456789abcdef0123456789abcdef01234567",
		Components:  medianame.Components{Title: "abcabc146", Season: 1, SeasonExplicit: true, EpisodeStart: 6, EpisodeEnd: 6, EpisodeEvidence: medianame.EpisodeEvidenceExplicit},
	}
	plan, err := BuildBoundPlan(context.Background(), bundle, entry.Key, map[string][]release.Release{"mikan": {pack, single}})
	if err != nil {
		t.Fatal(err)
	}
	selected := make([]Candidate, 0, 2)
	for _, c := range plan.Candidates {
		if c.Decision == DecisionSelected {
			selected = append(selected, c)
		}
	}
	if len(selected) != 1 {
		t.Fatalf("overlapping releases both remained selected: %+v", plan.Candidates)
	}
}

func TestSelectBestPreservesMaximumEpisodeCoverage(t *testing.T) {
	entry := catalog.Entry{Key: "series/abcabc146", MediaType: catalog.MediaSeries, Title: "abcabc146", Enabled: true, Seasons: []int{1}}
	bundle := configstore.Bundle{
		Sources: map[string]configstore.ContentSource{"mikan": {ID: "mikan", Provider: "mikan", Enabled: true}},
		Entries: map[string]catalog.Entry{entry.Key: entry},
	}
	pack := release.Release{
		MediaType: "series", Provider: "mikan", MediaName: "abcabc146 S01E05-E06.mkv", NormalizedTitle: "abcabc146",
		DownloadURL: "magnet:?xt=urn:btih:b123456789abcdef0123456789abcdef01234567",
		Components:  medianame.Components{Title: "abcabc146", Season: 1, SeasonExplicit: true, EpisodeStart: 5, EpisodeEnd: 6, Version: 1, EpisodeEvidence: medianame.EpisodeEvidenceExplicit},
	}
	single := release.Release{
		MediaType: "series", Provider: "mikan", MediaName: "abcabc146 S01E05v2.mkv", NormalizedTitle: "abcabc146",
		DownloadURL: "magnet:?xt=urn:btih:c123456789abcdef0123456789abcdef01234567",
		Components:  medianame.Components{Title: "abcabc146", Season: 1, SeasonExplicit: true, EpisodeStart: 5, EpisodeEnd: 5, Version: 2, EpisodeEvidence: medianame.EpisodeEvidenceExplicit},
	}
	plan, err := BuildBoundPlan(context.Background(), bundle, entry.Key, map[string][]release.Release{"mikan": {pack, single}})
	if err != nil {
		t.Fatal(err)
	}
	var selected []Candidate
	for _, c := range plan.Candidates {
		if c.Decision == DecisionSelected {
			selected = append(selected, c)
		}
	}
	if len(selected) != 1 || selected[0].Episode.EpisodeStart != 5 || selected[0].Episode.End() != 6 {
		t.Fatalf("selection sacrificed E06 coverage for higher-revision E05: %+v", plan.Candidates)
	}
}
