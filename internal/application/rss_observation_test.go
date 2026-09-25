package application

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"aninode/internal/catalog"
	"aninode/internal/configstore"
	"aninode/internal/download"
	"aninode/internal/medianame"
	"aninode/internal/release"
	"aninode/internal/rss"
)

func TestRSSCachePreservesFailureClearsOnEmptySuccessAndInvalidatesConfig(t *testing.T) {
	f := newFixture(t, map[string]string{"f": "feed"})
	fetch := fakeFetcher{entries: map[string][]rss.Entry{"feed": {entry("[G] abcabc046 - 01 [1080p]", "one")}}, errs: map[string]error{}}
	app := service(t, f, fetch)
	now := time.Date(2026, 9, 19, 1, 0, 0, 0, time.UTC)
	app.now = func() time.Time { return now }
	read := func() RSSDiscoveryResult {
		t.Helper()
		v, err := app.RSSDiscoveries(context.Background(), "", 50)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	if err := app.RefreshRSS(context.Background()); err != nil {
		t.Fatal(err)
	}
	initial := read()
	if len(initial.Groups) != 1 || initial.Sources[0].Status != "ready" || !initial.Sources[0].UpdatedAt.Equal(now) {
		t.Fatalf("initial=%+v", initial)
	}
	if len(f.client.adds) != 0 {
		t.Fatal("refresh acquired media")
	}
	// An independent App starts cold; cached observations are not durable state.
	restarted := service(t, f, fetch)
	cold, err := restarted.RSSDiscoveries(context.Background(), "", 50)
	if err != nil || len(cold.Groups) != 0 || cold.Sources[0].Status != "pending" {
		t.Fatalf("restart=%+v %v", cold, err)
	}
	originalTime := now
	now = now.Add(time.Minute)
	fetch.errs["feed"] = context.DeadlineExceeded
	if err := app.RefreshRSS(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lost deadline identity: %v", err)
	}
	failed := read()
	if len(failed.Groups) != 1 || !failed.Sources[0].Stale || failed.Sources[0].Status != "error" || !failed.Sources[0].UpdatedAt.Equal(originalTime) || !failed.Sources[0].AttemptedAt.Equal(now) || len(failed.Issues) != 1 {
		t.Fatalf("failed=%+v", failed)
	}
	// Automation must not reuse retained display data as current RSS input.
	cycle, err := app.Cycle(context.Background(), CycleOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if cycle.SourceReleases["f"] != 0 {
		t.Fatalf("automation reused stale RSS: %+v", cycle.SourceReleases)
	}
	delete(fetch.errs, "feed")
	fetch.entries["feed"] = nil
	if err := app.RefreshRSS(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := read(); len(got.Groups) != 0 || got.Sources[0].Stale || got.Sources[0].Status != "ready" {
		t.Fatalf("empty success=%+v", got)
	}
	// A changed endpoint must not inherit the previous endpoint's timestamp.
	cfg := app.snapshot().bundle.Sources["f"]
	cfg.RSS = []configstore.RSSSource{{URL: "https://example.test/new"}}
	if _, err := app.PutSource(context.Background(), "f", cfg); err != nil {
		t.Fatal(err)
	}
	if got := read(); got.Sources[0].Status != "pending" || !got.Sources[0].UpdatedAt.IsZero() {
		t.Fatalf("changed feed=%+v", got)
	}
	cfg.Enabled = false
	if _, err := app.PutSource(context.Background(), "f", cfg); err != nil {
		t.Fatal(err)
	}
	if got := read(); len(got.Sources) != 0 || len(got.Groups) != 0 {
		t.Fatalf("disabled feed=%+v", got)
	}
	cfg.Enabled = true
	if _, err := app.PutSource(context.Background(), "f", cfg); err != nil {
		t.Fatal(err)
	}
	if got := read(); got.Sources[0].Status != "pending" {
		t.Fatalf("re-enabled feed=%+v", got)
	}
	if err := app.DeleteSource(context.Background(), "f"); err != nil {
		t.Fatal(err)
	}
	if got := read(); len(got.Sources) != 0 {
		t.Fatalf("removed feed=%+v", got)
	}
}

func TestFullCyclePublishesRSSCacheAndLocalCycleKeepsIt(t *testing.T) {
	f := newFixture(t, map[string]string{"f": "feed"})
	fetch := fakeFetcher{entries: map[string][]rss.Entry{"feed": {entry("[G] abcabc028 - 01 [1080p]", "one")}}}
	app := service(t, f, fetch)
	if _, err := app.Cycle(context.Background(), CycleOptions{}); err != nil {
		t.Fatal(err)
	}
	got, err := app.RSSDiscoveries(context.Background(), "", 50)
	if err != nil || len(got.Groups) != 1 {
		t.Fatalf("full cycle cache=%+v %v", got, err)
	}
	fetch.entries["feed"] = nil
	if _, err := app.Cycle(context.Background(), CycleOptions{LocalOnly: true}); err != nil {
		t.Fatal(err)
	}
	got, err = app.RSSDiscoveries(context.Background(), "", 50)
	if err != nil || len(got.Groups) != 1 {
		t.Fatalf("local cycle replaced RSS=%+v %v", got, err)
	}
	// A validated external source edit also invalidates cached data.
	mustWrite(t, filepath.Join(f.cfg, "sources", "f.json"), `{"id":"f","provider":"generic","rss":[{"url":"https://example.test/changed"}],"enabled":true}`)
	if err := app.reloadRuntime(); err != nil {
		t.Fatal(err)
	}
	got, err = app.RSSDiscoveries(context.Background(), "", 50)
	if err != nil || len(got.Groups) != 0 || got.Sources[0].Status != "pending" {
		t.Fatalf("external config=%+v %v", got, err)
	}
}

func TestRSSPartialObservationRetainsUsableGroupsAndWarning(t *testing.T) {
	f := newFixture(t, map[string]string{"f": "feed"})
	app := service(t, f, fakeFetcher{})
	cfg := app.snapshot().bundle.Sources["f"]
	values := mustReleases(t, "generic", []rss.Entry{entry("[G] abcabc039 - 01 [1080p]", "one")})
	app.recordRSSObservation(cfg, values, errors.New("one torrent unavailable"))
	got, err := app.RSSDiscoveries(context.Background(), "", 50)
	if err != nil || len(got.Groups) != 1 || len(got.Issues) != 1 || got.Sources[0].Status != "partial" {
		t.Fatalf("partial=%+v %v", got, err)
	}
}

func TestRSSCacheIsIsolatedFromProducerAndReaderMutation(t *testing.T) {
	f := newFixture(t, map[string]string{"f": "feed"})
	app := service(t, f, fakeFetcher{})
	values := mustReleases(t, "generic", []rss.Entry{entry("[G] abcabc027 - 01 [1080p]", "one")})
	values[0].Components.Metadata = []string{"original"}
	values[0].Differences = []medianame.Difference{{Values: []string{"original"}}}
	app.recordRSSObservation(app.snapshot().bundle.Sources["f"], values, nil)
	values[0].Components.Metadata[0] = "producer mutation"
	values[0].Differences[0].Values[0] = "producer mutation"
	first, err := app.RSSDiscoveries(context.Background(), "", 50)
	if err != nil {
		t.Fatal(err)
	}
	if first.Groups[0].Releases[0].Components.Metadata[0] != "original" || first.Groups[0].Releases[0].Differences[0].Values[0] != "original" {
		t.Fatal("producer mutated cache")
	}
	first.Groups[0].Releases[0].Components.Metadata[0] = "reader mutation"
	first.Groups[0].Releases[0].Differences[0].Values[0] = "reader mutation"
	second, err := app.RSSDiscoveries(context.Background(), "", 50)
	if err != nil {
		t.Fatal(err)
	}
	if second.Groups[0].Releases[0].Components.Metadata[0] != "original" || second.Groups[0].Releases[0].Differences[0].Values[0] != "original" {
		t.Fatal("reader mutated cache")
	}
}

type independentlyBlockedRSSFetcher struct {
	slow chan struct{}
	name string
}

func (f independentlyBlockedRSSFetcher) Fetch(ctx context.Context, u string) ([]rss.Entry, error) {
	if strings.HasSuffix(u, "slow") {
		select {
		case <-f.slow:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return []rss.Entry{{Title: "abcabc008", DownloadURL: "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567&dn=" + url.QueryEscape(f.name)}}, nil
}

func TestRSSPublishesFastSourceWhileSlowSourceIsStillFetching(t *testing.T) {
	for _, tc := range []struct {
		name, nativeName string
		groups           int
	}{
		{"explicit episode", "abcabc008 S01E01", 1},
		{"publication episode", "[G] abcabc008 - 01 [1080p]", 1},
		{"weak numeric title is filtered", "abcabc008 - 01", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newFixture(t, map[string]string{"fast": "fast", "slow": "slow"})
				fetcher := independentlyBlockedRSSFetcher{slow: make(chan struct{}), name: tc.nativeName}
				app, err := Open(Options{ConfigRoot: f.cfg, Fetcher: fetcher, Backends: map[string]download.Backend{"c": f.client}})
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				done := make(chan error, 1)
				go func() { done <- app.RefreshRSS(ctx) }()
				synctest.Wait()
				during, err := app.RSSDiscoveries(context.Background(), "", 50)
				if err != nil || len(during.Sources) != 2 || during.Sources[0].SourceID != "fast" || during.Sources[0].Status != "ready" || during.Sources[1].SourceID != "slow" || during.Sources[1].Status != "pending" {
					t.Fatalf("fast source was held by slow source: %+v %v", during, err)
				}
				if len(during.Groups) != tc.groups {
					t.Fatalf("native name %q produced %d discovery groups, want %d: %+v", tc.nativeName, len(during.Groups), tc.groups, during.Groups)
				}
				if len(during.Groups) > 0 && (during.Groups[0].MediaType != catalog.MediaSeries || during.Groups[0].Title != "abcabc008") {
					t.Fatalf("unexpected fast-source discovery: %+v", during.Groups)
				}
				select {
				case err := <-done:
					t.Fatalf("refresh returned before slow source was released: %v", err)
				default:
				}
				cancel()
				if err := <-done; !errors.Is(err, context.Canceled) {
					t.Fatalf("cancellation identity lost: %v", err)
				}
				after, err := app.RSSDiscoveries(context.Background(), "", 50)
				if err != nil || len(after.Groups) != tc.groups || len(after.Sources) != 2 || after.Sources[0].Status != "ready" || after.Sources[1].Status != "error" || len(after.Issues) != 1 {
					t.Fatalf("partial cancellation=%+v %v", after, err)
				}
			})
		})
	}
}

func TestRSSDiscoveriesFiltersMoviesBeforeLimit(t *testing.T) {
	f := newFixture(t, map[string]string{"f": "feed"})
	app := service(t, f, fakeFetcher{})

	// This test is deliberately parser-independent. It proves RSSDiscoveries
	// filters typed movie observations before applying its response limit.
	values := make([]release.Release, 0, 61)
	for i := 0; i < 60; i++ {
		title := fmt.Sprintf("abcabc048 %02d", i)
		values = append(values, release.Release{
			MediaType: catalog.MediaMovie,
			Title:     title,
			Components: medianame.Components{
				Title: title,
				Year:  2026,
			},
		})
	}
	values = append(values, release.Release{
		MediaType: catalog.MediaSeries,
		Title:     "abcabc007",
		Components: medianame.Components{
			Title:           "abcabc007",
			EpisodeStart:    1,
			EpisodeEnd:      1,
			EpisodeEvidence: medianame.EpisodeEvidenceExplicit,
		},
	})
	app.recordRSSObservation(app.snapshot().bundle.Sources["f"], values, nil)

	got, err := app.RSSDiscoveries(context.Background(), "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Groups) != 1 || got.Groups[0].MediaType != catalog.MediaSeries || got.Groups[0].Title != "abcabc007" {
		t.Fatalf("series was crowded out by movie noise: %+v", got.Groups)
	}
}

func TestRSSDiscoveriesIncludesEverySourceWithoutLimit(t *testing.T) {
	f := newFixture(t, map[string]string{"first": "feed-a", "last": "feed-z"})
	app := service(t, f, fakeFetcher{})
	makeRelease := func(title string) release.Release {
		return release.Release{MediaType: catalog.MediaSeries, Title: title, Components: medianame.Components{Title: title, EpisodeStart: 1, EpisodeEnd: 1, EpisodeEvidence: medianame.EpisodeEvidenceExplicit}}
	}
	values := make([]release.Release, 120)
	for i := range values {
		values[i] = makeRelease(fmt.Sprintf("abcabc051 %03d", i))
	}
	app.recordRSSObservation(app.snapshot().bundle.Sources["first"], values, nil)
	app.recordRSSObservation(app.snapshot().bundle.Sources["last"], []release.Release{values[0], makeRelease("abcabc010")}, nil)
	for _, tc := range []struct {
		name, source string
		limit, want  int
	}{
		{"all sources", "", 0, 121},
		{"selected source", "last", 0, 2},
		{"explicit limit", "", 50, 50},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := app.RSSDiscoveries(context.Background(), tc.source, tc.limit)
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Groups) != tc.want {
				t.Fatalf("groups=%d, want %d", len(got.Groups), tc.want)
			}
			if tc.limit != 0 {
				return
			}
			found := false
			for _, g := range got.Groups {
				if g.Title == "abcabc010" {
					found = true
				}
				if tc.source == "" && g.Title == values[0].Title && len(g.SourceIDs) != 2 {
					t.Fatalf("merged sources lost: %+v", g)
				}
			}
			if !found {
				t.Fatal("last source's unique work was truncated")
			}
		})
	}
}
