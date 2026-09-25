package application

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aninode/internal/catalog"
	"aninode/internal/configstore"
	"aninode/internal/download"
	"aninode/internal/medianame"
	"aninode/internal/release"
	"aninode/internal/releasefilter"
	"aninode/internal/rss"
	"aninode/internal/secretstore"
	"aninode/internal/torrentmeta"
)

func managedEntryApp(t *testing.T) (*App, fixture, catalog.Entry) {
	t.Helper()
	f := newFixture(t, map[string]string{"f": "feed"})
	w, err := catalog.CreateManagedAt(f.src, f.lib, "abcabc146", 2026, 1, catalog.Declaration{Sources: []string{"f"}})
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(w.DeclarationPath, "Season 01", "abcabc146 - 01.mkv"), "media")
	return service(t, f, fakeFetcher{entries: map[string][]rss.Entry{}}), f, w
}

func TestSearchDiscoveryReturnsParsedDMHYCandidates(t *testing.T) {
	f := newFixture(t, nil)
	mustWrite(t, filepath.Join(f.cfg, "sources", "dmhy.json"), `{"id":"dmhy","provider":"dmhy","enabled":true}`)
	app := service(t, f, fakeFetcher{entries: map[string][]rss.Entry{"https://share.dmhy.org/topics/rss/rss.xml?keyword=abcabc065": {entry("[grpabc165] abcabc065 S01E03 1080p", "old-3")}}})
	got, err := app.SearchDiscovery(context.Background(), "dmhy", "abcabc065", 10)
	if err != nil {
		t.Fatal(err)
	}
	if got.SourceID != "dmhy" || got.Query != "abcabc065" || len(got.Groups) != 1 || len(got.Groups[0].Releases) != 1 || got.Groups[0].Releases[0].Episode != "S1E3" || got.Groups[0].Releases[0].Resolution != "1080p" {
		t.Fatalf("discovery=%+v", got)
	}
}

func TestEntryFilterOptionsCanGrowBeyondCurrentFilters(t *testing.T) {
	f := newFixture(t, map[string]string{"f": "feed"})
	w, err := catalog.CreateManagedAt(f.src, f.lib, "abcabc146", 2026, 1, catalog.Declaration{
		Sources: []string{"f"},
		Filters: releasefilter.Filters{Groups: []string{"grpabc166"}},
		Folders: map[string]catalog.FolderProjection{"Season 01": {Season: 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(w.DeclarationPath, "Season 01", "[grpabc159] abcabc146 - 01 [1080P][CHT].mkv"), "media")
	app := service(t, f, fakeFetcher{})
	rt := app.snapshot()
	cfg := rt.bundle.Sources["f"]
	app.recordRSSObservation(cfg, []release.Release{{
		MediaType: "series",
		MediaName: "[grpabc158] abcabc146 - 02 [2160P][CHS].mkv",
		Group:     "grpabc158", Resolution: "2160p", Subtitle: "CHS",
		DownloadURL: "https://example.test/other.torrent",
		Components:  medianame.Components{Title: "abcabc146", Season: 1, EpisodeStart: 2, EpisodeEnd: 2, ReleaseGroup: "grpabc158", Resolution: "2160p", Subtitle: "CHS"},
	}}, nil)

	view, err := app.GetEntry(context.Background(), w.Key)
	if err != nil {
		t.Fatal(err)
	}
	wantGroups := map[string]bool{"grpabc166": true, "grpabc159": true, "grpabc158": true}
	for _, group := range view.FilterOptions.Groups {
		delete(wantGroups, group)
	}
	if len(wantGroups) != 0 {
		t.Fatalf("filter options stopped growing behind current allow-list: got=%+v missing=%+v", view.FilterOptions, wantGroups)
	}
	if !containsFilterOption(view.FilterOptions.Resolutions, "1080P") || !containsFilterOption(view.FilterOptions.Resolutions, "2160p") {
		t.Fatalf("resolution observations were not merged: %+v", view.FilterOptions)
	}
}

func containsFilterOption(values []string, want string) bool {
	for _, value := range values {
		if releasefilter.Same(value, want) {
			return true
		}
	}
	return false
}

func TestPutEntryEmptyFiltersRemainOpen(t *testing.T) {
	app, _, w := managedEntryApp(t)
	view, err := app.GetEntry(context.Background(), w.Key)
	if err != nil {
		t.Fatal(err)
	}
	view.Declaration.Sources = []string{"f"}
	view.Declaration.Filters = releasefilter.Filters{}
	got, err := app.PutEntry(context.Background(), w.Key, view.Declaration)
	if err != nil {
		t.Fatal(err)
	}
	if got.Entry.Key != w.Key {
		t.Fatalf("id=%s want %s", got.Entry.Key, w.Key)
	}
	side, err := catalog.ReadDeclaration(w.DeclarationPath)
	if err != nil {
		t.Fatal(err)
	}
	if !side.Filters.Empty() {
		t.Fatalf("filters=%+v, want open filters", side.Filters)
	}
}

func TestPutEntryPreservesRootFolderProjection(t *testing.T) {
	app, _, w := managedEntryApp(t)
	if _, err := app.PutFolderProjection(context.Background(), w.Key, "Season 01", catalog.FolderProjection{Season: 2, EpisodeOffset: -10}); err != nil {
		t.Fatal(err)
	}
	view, err := app.GetEntry(context.Background(), w.Key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.PutEntry(context.Background(), w.Key, view.Declaration); err != nil {
		t.Fatal(err)
	}
	d, err := catalog.ReadDeclaration(w.DeclarationPath)
	if err != nil {
		t.Fatal(err)
	}
	got := d.Folders["Season 01"]
	if got.Season != 2 || got.EpisodeOffset != -10 {
		t.Fatalf("folder projection lost after root declaration save: %+v", d.Folders)
	}
}

func TestPutEntryDoesNotRestoreOmittedFolderProjections(t *testing.T) {
	app, _, w := managedEntryApp(t)
	if _, err := app.PutFolderProjection(context.Background(), w.Key, "Season 01", catalog.FolderProjection{Season: 2, EpisodeOffset: -10}); err != nil {
		t.Fatal(err)
	}
	view, err := app.GetEntry(context.Background(), w.Key)
	if err != nil {
		t.Fatal(err)
	}
	view.Declaration.Folders = nil
	if _, err := app.PutEntry(context.Background(), w.Key, view.Declaration); err != nil {
		t.Fatal(err)
	}
	d, err := catalog.ReadDeclaration(w.DeclarationPath)
	if err != nil {
		t.Fatal(err)
	}
	if d.Folders != nil {
		t.Fatalf("omitted folders were restored from the previous declaration: %+v", d.Folders)
	}
}

func TestPutEntryRoundTripsOutputAndBlacklistAndSearchesOutputTitle(t *testing.T) {
	app, _, w := managedEntryApp(t)
	view, err := app.GetEntry(context.Background(), w.Key)
	if err != nil {
		t.Fatal(err)
	}
	d := view.Declaration
	d.Sources = []string{"f"}
	d.Output = catalog.OutputLayout{Title: "abcabc036"}
	d.Blacklist = []string{"*NCOP*"}
	got, err := app.PutEntry(context.Background(), w.Key, d)
	if err != nil {
		t.Fatal(err)
	}
	if got.Entry.Title != "abcabc146" || got.Entry.Key != w.Key || got.Declaration.Output.Title != "abcabc036" || got.Declaration.Blacklist[0] != "*NCOP*" {
		t.Fatalf("view=%+v", got)
	}
	found, err := app.SearchEntries(context.Background(), "abcabc036", 30)
	if err != nil || len(found) != 1 || found[0].Entry.Key != w.Key {
		t.Fatalf("found=%+v err=%v", found, err)
	}
	all, err := app.SearchEntries(context.Background(), "", 30)
	if err != nil || len(all) != 1 {
		t.Fatalf("all=%+v err=%v", all, err)
	}
}

func TestPutEntryImmediatelyConvergesPublication(t *testing.T) {
	app, f, w := managedEntryApp(t)
	view, err := app.GetEntry(context.Background(), w.Key)
	if err != nil {
		t.Fatal(err)
	}
	view.Declaration.Output.Title = "abcabc004"
	if _, err := app.PutEntry(context.Background(), w.Key, view.Declaration); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(f.lib, "abcabc004 (2026)", "Season 01", "abcabc004 S01E01.mkv")
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("updated publication was not converged before PutEntry returned: %v", err)
	}
}

func TestPreviewEntryReportsBlacklistExclusion(t *testing.T) {
	app, _, w := managedEntryApp(t)
	view, err := app.GetEntry(context.Background(), w.Key)
	if err != nil {
		t.Fatal(err)
	}
	view.Declaration.Sources = []string{"f"}
	view.Declaration.Blacklist = []string{"*NCOP*"}
	if _, err := app.PutEntry(context.Background(), w.Key, view.Declaration); err != nil {
		t.Fatal(err)
	}
	got, err := app.PreviewEntry(context.Background(), w.Key, PreviewRequest{Name: "[grpabc165] abcabc146 NCOP.mkv"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Decision != "excluded" || got.ExpectedPath != "" {
		t.Fatalf("preview=%+v", got)
	}
}

func TestFolderProjectionUsesAbsoluteTargetSeason(t *testing.T) {
	app, _, w := managedEntryApp(t)
	got, err := app.PutFolderProjection(context.Background(), w.Key, "Season 01", catalog.FolderProjection{Season: 2, EpisodeOffset: -12})
	if err != nil {
		t.Fatal(err)
	}
	if got.Season != 2 || got.EpisodeOffset != -12 {
		t.Fatalf("projection=%+v", got)
	}
	view, err := app.GetEntry(context.Background(), w.Key)
	if err != nil {
		t.Fatal(err)
	}
	folders := view.Folders
	if len(folders) != 1 || folders[0].Name != "Season 01" || folders[0].Projection.Season != 2 || folders[0].Projection.EpisodeOffset != -12 {
		t.Fatalf("folders=%+v", folders)
	}
}

func TestFolderProjectionIsStoredInRootDeclaration(t *testing.T) {
	app, _, w := managedEntryApp(t)
	if _, err := app.PutFolderProjection(context.Background(), w.Key, "Season 01", catalog.FolderProjection{Season: 2, EpisodeOffset: -10}); err != nil {
		t.Fatal(err)
	}
	d, err := catalog.ReadDeclaration(w.DeclarationPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := d.Folders["Season 01"]; got.Season != 2 || got.EpisodeOffset != -10 {
		t.Fatalf("projection=%+v", got)
	}
	if _, err := os.Stat(filepath.Join(w.DeclarationPath, "Season 01", ".aninode.json")); !os.IsNotExist(err) {
		t.Fatalf("season sidecar should not exist: %v", err)
	}
}

func containsInt(v []int, n int) bool {
	for _, x := range v {
		if x == n {
			return true
		}
	}
	return false
}

func TestRejectedWorkMutationRollsBackDeclaration(t *testing.T) {
	app, _, w := managedEntryApp(t)
	view, err := app.GetEntry(context.Background(), w.Key)
	if err != nil {
		t.Fatal(err)
	}
	before, err := catalog.ReadDeclaration(w.DeclarationPath)
	if err != nil {
		t.Fatal(err)
	}
	view.Declaration.Sources = []string{"missing-feed"}
	_, err = app.PutEntry(context.Background(), w.Key, view.Declaration)
	if err == nil {
		t.Fatal("expected reference conflict")
	}
	after, err := catalog.ReadDeclaration(w.DeclarationPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Sources) != len(before.Sources) || after.Sources[0] != before.Sources[0] {
		t.Fatalf("rejected mutation changed declaration: before=%+v after=%+v", before, after)
	}
}

func TestConfigMutationRollbackAndRuntimeBackendRebuild(t *testing.T) {
	f := newFixture(t, map[string]string{})
	var calls1, calls2 int
	rpc := func(counter *int) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			*counter++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"gid"}`))
		}))
	}
	s1 := rpc(&calls1)
	defer s1.Close()
	s2 := rpc(&calls2)
	defer s2.Close()
	mustWrite(t, filepath.Join(f.cfg, "clients/c.json"), `{"id":"c","type":"aria2","url":"`+s1.URL+`","path_mappings":[{"remote":"/remote","local":"`+f.src+`"}],"enabled":true}`)
	app, err := Open(Options{ConfigRoot: f.cfg})
	if err != nil {
		t.Fatal(err)
	}
	before := app.snapshot().backends["c"]
	cfg, err := app.PutClient(context.Background(), "c", configstore.Client{ID: "c", Type: "aria2", URL: s2.URL, PathMappings: []configstore.PathMapping{{Remote: "/new", Local: f.src}}, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.URL != s2.URL || cfg.PathMappings[0].Remote != "/new" {
		t.Fatalf("client=%+v", cfg)
	}
	after := app.snapshot().backends["c"]
	if before == after {
		t.Fatal("backend instance was not rebuilt")
	}
	submitter, ok := after.(interface {
		Add(context.Context, download.AddRequest) (download.Task, error)
	})
	if !ok {
		t.Fatalf("rebuilt backend %T cannot submit", after)
	}
	if _, err := submitter.Add(context.Background(), download.AddRequest{URL: "https://example.test/x.torrent"}); err != nil {
		t.Fatal(err)
	}
	if calls1 != 0 || calls2 != 1 {
		t.Fatalf("old=%d new=%d", calls1, calls2)
	}
	_, err = app.PutClient(context.Background(), "c", configstore.Client{ID: "c", Type: "aria2", URL: "invalid-url", Enabled: true})
	if err == nil {
		t.Fatal("expected invalid client rollback")
	}
	view, err := app.Config(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if view.Clients["c"].URL != s2.URL {
		t.Fatalf("rollback failed: %+v", view.Clients["c"])
	}
	rolled := app.snapshot().backends["c"]
	sub2, ok := rolled.(interface {
		Add(context.Context, download.AddRequest) (download.Task, error)
	})
	if !ok {
		t.Fatalf("rolled backend %T cannot submit", rolled)
	}
	if _, err := sub2.Add(context.Background(), download.AddRequest{URL: "https://example.test/y.torrent"}); err != nil {
		t.Fatal(err)
	}
	if calls1 != 0 || calls2 != 2 {
		t.Fatalf("rollback runtime not last-known-good: old=%d new=%d", calls1, calls2)
	}
}

func TestExternalInvalidConfigPreservesSnapshotAndRecovers(t *testing.T) {
	f := newFixture(t, map[string]string{"f": "feed"})
	app := service(t, f, fakeFetcher{})
	before, _ := app.Config(context.Background())
	mustWrite(t, filepath.Join(f.cfg, "sources/f.json"), `{"id":"f","provider":"generic","rss":[{"url":"not-a-url"}],"enabled":true}`)
	if _, err := app.Cycle(context.Background(), CycleOptions{}); err == nil {
		t.Fatal("cycle accepted invalid external config")
	}
	if r := app.Readiness(context.Background()); r.Ready || r.Reason != "invalid_configuration" {
		t.Fatalf("readiness=%+v", r)
	}
	still, _ := app.Config(context.Background())
	if still.Sources["f"].RSS[0].URL != before.Sources["f"].RSS[0].URL {
		t.Fatalf("split snapshot: before=%q after=%q", before.Sources["f"].RSS[0].URL, still.Sources["f"].RSS[0].URL)
	}
	mustWrite(t, filepath.Join(f.cfg, "sources/f.json"), `{"id":"f","provider":"generic","rss":[{"url":"http://feed.test/repaired"}],"enabled":true}`)
	if _, err := app.Cycle(context.Background(), CycleOptions{}); err != nil {
		t.Fatal(err)
	}
	if r := app.Readiness(context.Background()); !r.Ready {
		t.Fatalf("did not recover: %+v", r)
	}
	got, _ := app.Config(context.Background())
	if got.Sources["f"].RSS[0].URL != "http://feed.test/repaired" {
		t.Fatalf("runtime did not reload repaired feed: %+v", got.Sources["f"])
	}
}

func TestReadOnlyManagementDoesNotTakeExclusiveOperationLock(t *testing.T) {
	app, _, _ := managedEntryApp(t)
	app.operationMu.Lock()
	defer app.operationMu.Unlock()
	done := make(chan error, 1)
	go func() { _, err := app.SearchEntries(context.Background(), "show", 30); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("read-only management blocked on exclusive operation lock")
	}
}

func TestPreviewUsesBackendDomainSeam(t *testing.T) {
	app, _, w := managedEntryApp(t)
	got, err := app.PreviewEntry(context.Background(), w.Key, PreviewRequest{Name: "[G] abcabc146 S01E02 1080p.mkv"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Decision == "unknown" || got.SourceEpisode == nil || got.TargetEpisode == nil || got.ExpectedPath == "" {
		t.Fatalf("preview=%+v", got)
	}
}

func TestClientStatusesProbeEnabledBackends(t *testing.T) {
	f := newFixture(t, nil)
	app := service(t, f, fakeFetcher{})
	statuses := app.ClientStatuses(context.Background())
	if len(statuses) != 1 || statuses[0].ID != "c" || !statuses[0].Enabled || !statuses[0].Reachable || statuses[0].Error != "" {
		t.Fatalf("statuses=%+v", statuses)
	}
}

func TestSearchResultAcquiresIntoSelectedEntrySourceDirectory(t *testing.T) {
	f := newFixture(t, nil)
	w, err := catalog.CreateManagedAt(f.src, f.lib, "abcabc146", 2026, 1, catalog.Declaration{})
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(f.cfg, "sources", "search.json"), `{"id":"search","provider":"generic","rss":[{"url":"http://feed.test/current"}],"search":{"url_template":"http://feed.test/search?q={query}"},"enabled":true}`)
	app := service(t, f, fakeFetcher{entries: map[string][]rss.Entry{"search?q=abcabc146": {entry("[G] abcabc146 S01E03 1080p", "search-3")}}})
	found, err := app.SearchDiscovery(context.Background(), "search", "abcabc146", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(found.Groups) != 1 || len(found.Groups[0].Releases) != 1 {
		t.Fatalf("search groups=%d", len(found.Groups))
	}
	result, err := app.AcquireDiscovery(context.Background(), DiscoveryAcquireRequest{EntryKey: w.Key, SourceID: "search", Query: "abcabc146", Release: found.Groups[0].Releases[0]})
	if err != nil {
		t.Fatal(err)
	}
	if result.Acquisition.Decision != "submitted" {
		t.Fatalf("acquisition=%+v", result.Acquisition)
	}
	if len(f.client.adds) != 1 {
		t.Fatalf("adds=%d", len(f.client.adds))
	}
	if got, want := filepath.Clean(f.client.adds[0].SavePath), filepath.Clean(filepath.Join(w.DeclarationPath, "Season 01")); got != want {
		t.Fatalf("search SavePath=%q want source season container %q", got, want)
	}
}

func TestRSSDiscoveryIsDerivedAndDisappearsAfterEntryCreation(t *testing.T) {
	f := newFixture(t, map[string]string{"f": "feed"})
	fetch := fakeFetcher{entries: map[string][]rss.Entry{"feed": {
		entry("[grpabc166] abcabc052 - 01 [1080P][CHS]", "fresh-1"),
		entry("[grpabc166] abcabc052 - 02 [1080P][CHS]", "fresh-2"),
	}}}
	app := service(t, f, fetch)

	if err := app.RefreshRSS(context.Background()); err != nil {
		t.Fatal(err)
	}

	before, err := app.RSSDiscoveries(context.Background(), "f", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(before.Groups) != 1 {
		t.Fatalf("discoveries=%+v", before.Groups)
	}
	g := before.Groups[0]
	if g.Title != "abcabc052" || len(g.Releases) != 2 || len(g.Groups) == 0 {
		t.Fatalf("group=%+v", g)
	}

	groups := append([]string(nil), g.Groups...)
	subs := append([]string(nil), g.Subtitles...)
	resolutions := append([]string(nil), g.Resolutions...)
	created, err := app.CreateEntry(context.Background(), catalog.MediaSeries, g.Season, EntryDeclaration{
		Title:     g.Title,
		Year:      g.Year,
		Sources:   []string{"f"},
		Blacklist: []string{},
		Filters: releasefilter.Filters{
			Groups:      groups,
			Subtitles:   subs,
			Resolutions: resolutions,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.Entry.Title != "abcabc052" {
		t.Fatalf("created=%+v", created.Entry)
	}

	after, err := app.RSSDiscoveries(context.Background(), "f", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Groups) != 0 {
		t.Fatalf("accepted work remained in derived discovery: %+v", after.Groups)
	}

	cycle, err := app.Cycle(context.Background(), CycleOptions{Acquire: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(cycle.Acquisitions) == 0 || len(f.client.adds) == 0 {
		t.Fatalf("accepted RSS work did not enter normal acquisition: cycle=%+v adds=%d", cycle, len(f.client.adds))
	}
}

func TestSearchDiscoveryMarksExistingEntryBeforeAcquisition(t *testing.T) {
	f := newFixture(t, nil)
	w, err := catalog.CreateManagedAt(f.src, f.lib, "abcabc054", 2026, 1, catalog.Declaration{})
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(f.cfg, "sources", "search.json"), `{"id":"search","provider":"generic","search":{"url_template":"http://feed.test/search?q={query}"},"enabled":true}`)
	app := service(t, f, fakeFetcher{entries: map[string][]rss.Entry{"search?q=abcabc054": {entry("[G] abcabc054 S01E03 1080p", "known-3")}}})
	got, err := app.SearchDiscovery(context.Background(), "search", "abcabc054", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Groups) != 1 || got.Groups[0].EntryKey != w.Key {
		t.Fatalf("search grouping did not resolve existing Entry: %+v", got.Groups)
	}
}

func TestCreateEntryRollsBackFilesystemWhenDeclarationIsRejected(t *testing.T) {
	f := newFixture(t, nil)
	app := service(t, f, fakeFetcher{})
	_, err := app.CreateEntry(context.Background(), catalog.MediaSeries, 2, EntryDeclaration{Title: "abcabc029", Sources: []string{"missing"}})
	if err == nil {
		t.Fatal("expected dangling source reference to reject new Entry")
	}
	for _, p := range []string{
		filepath.Join(f.root, "downloads", "TV", "abcabc029"),
		filepath.Join(f.root, "library", "TV", "abcabc029"),
	} {
		if _, statErr := os.Stat(p); !os.IsNotExist(statErr) {
			t.Fatalf("rejected Entry left filesystem residue at %s: %v", p, statErr)
		}
	}
}

func TestRSSDiscoveryMergesSameWorkAcrossSources(t *testing.T) {
	f := newFixture(t, map[string]string{"a": "feed-a", "b": "feed-b"})
	app := service(t, f, fakeFetcher{entries: map[string][]rss.Entry{
		"feed-a": {entry("[A] abcabc049 - 01 [1080P][CHS]", "a1")},
		"feed-b": {entry("[B] abcabc049 - 02 [1080P][CHS]", "b2")},
	}})
	if err := app.RefreshRSS(context.Background()); err != nil {
		t.Fatal(err)
	}

	got, err := app.RSSDiscoveries(context.Background(), "", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Groups) != 1 {
		t.Fatalf("groups=%+v", got.Groups)
	}
	g := got.Groups[0]
	if len(g.SourceIDs) != 2 || g.SourceIDs[0] != "a" || g.SourceIDs[1] != "b" {
		t.Fatalf("source ids=%v", g.SourceIDs)
	}
	if len(g.Releases) != 2 {
		t.Fatalf("releases=%d", len(g.Releases))
	}
}

func TestRSSDiscoveryIsolatesBrokenSources(t *testing.T) {
	f := newFixture(t, map[string]string{"good": "good", "bad": "bad"})
	app := service(t, f, fakeFetcher{
		entries: map[string][]rss.Entry{"good": {entry("[A] abcabc035 - 01 [1080P]", "g1")}},
		errs:    map[string]error{"bad": errors.New("temporary feed failure")},
	})
	if err := app.RefreshRSS(context.Background()); err == nil {
		t.Fatal("refresh should report broken source")
	}

	got, err := app.RSSDiscoveries(context.Background(), "", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Groups) != 1 || got.Groups[0].Title != "abcabc035" {
		t.Fatalf("groups=%+v", got.Groups)
	}
	if len(got.Issues) != 1 || got.Issues[0].Source != "bad" || !strings.Contains(got.Issues[0].Message, "temporary feed failure") {
		t.Fatalf("issues=%+v", got.Issues)
	}
	if got, err := app.RSSDiscoveries(context.Background(), "bad", 20); err != nil || len(got.Issues) != 1 || got.Sources[0].Status != "error" {
		t.Fatalf("cached source failure must remain visible: %+v %v", got, err)
	}
}

func TestDiscoveryEntryDoesNotFabricateLibraryEntryDirectories(t *testing.T) {
	f := newFixture(t, map[string]string{"f": "feed"})
	app := service(t, f, fakeFetcher{})
	created, err := app.CreateEntry(context.Background(), catalog.MediaSeries, 2, EntryDeclaration{Title: "abcabc188", Sources: []string{"f"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(created.Entry.Path, "Season 02")); !os.IsNotExist(err) {
		t.Fatalf("discovery declaration fabricated target season: %v", err)
	}
	if _, err := os.Stat(created.Entry.Path); !os.IsNotExist(err) {
		t.Fatalf("discovery declaration fabricated target entry: %v", err)
	}
	if _, err := os.Stat(f.lib); err != nil {
		t.Fatalf("library category namespace should exist for observation: %v", err)
	}
}

func TestSearchAcquisitionReverifiesProviderResult(t *testing.T) {
	f := newFixture(t, nil)
	w, err := catalog.CreateManagedAt(f.src, f.lib, "abcabc030", 2026, 1, catalog.Declaration{})
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(f.cfg, "sources", "search.json"), `{"id":"search","provider":"generic","search":{"url_template":"http://feed.test/search?q={query}"},"enabled":true}`)
	fetch := fakeFetcher{entries: map[string][]rss.Entry{"search?q=abcabc030": {entry("[G] abcabc030 S01E03 1080p", "verified-3")}}}
	app := service(t, f, fetch)
	found, err := app.SearchDiscovery(context.Background(), "search", "abcabc030", 10)
	if err != nil {
		t.Fatal(err)
	}
	selected := found.Groups[0].Releases[0]
	selected.Group = "FORGED"
	selected.Components.EpisodeStart = 99
	selected.Components.EpisodeEnd = 99
	result, err := app.AcquireDiscovery(context.Background(), DiscoveryAcquireRequest{EntryKey: w.Key, SourceID: "search", Query: "abcabc030", Release: selected})
	if err != nil {
		t.Fatal(err)
	}
	if result.Candidate.Episode.EpisodeStart != 3 || result.Candidate.Release.Group == "FORGED" {
		t.Fatalf("client-supplied release fields were trusted: %+v", result.Candidate)
	}
	forged := selected
	forged.DownloadURL = "https://example.invalid/arbitrary.torrent"
	if _, err := app.AcquireDiscovery(context.Background(), DiscoveryAcquireRequest{EntryKey: w.Key, SourceID: "search", Query: "abcabc030", Release: forged}); err == nil {
		t.Fatal("arbitrary release outside provider search result was accepted")
	}
}

func TestSearchAcquisitionRespectsFilesystemAvailability(t *testing.T) {
	f := newFixture(t, nil)
	w, err := catalog.CreateManagedAt(f.src, f.lib, "abcabc040", 2026, 1, catalog.Declaration{})
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(w.Path, "Season 01", "abcabc040 S01E03.mkv"), "already here")
	mustWrite(t, filepath.Join(f.cfg, "sources", "search.json"), `{"id":"search","provider":"generic","search":{"url_template":"http://feed.test/search?q={query}"},"enabled":true}`)
	fetch := fakeFetcher{entries: map[string][]rss.Entry{"search?q=abcabc040": {entry("[G] abcabc040 S01E03 1080p", "present-3")}}}
	app := service(t, f, fetch)
	found, err := app.SearchDiscovery(context.Background(), "search", "abcabc040", 10)
	if err != nil {
		t.Fatal(err)
	}
	_, err = app.AcquireDiscovery(context.Background(), DiscoveryAcquireRequest{EntryKey: w.Key, SourceID: "search", Query: "abcabc040", Release: found.Groups[0].Releases[0]})
	if err == nil || !strings.Contains(err.Error(), "present in library") {
		t.Fatalf("present episode should be rejected by filesystem availability gate: %v", err)
	}
	if len(f.client.adds) != 0 {
		t.Fatalf("present episode reached downloader: adds=%d", len(f.client.adds))
	}
}

func TestPutEntryAllowsClearingYearAndRejectsBlankTitle(t *testing.T) {
	app, _, w := managedEntryApp(t)
	view, err := app.GetEntry(context.Background(), w.Key)
	if err != nil {
		t.Fatal(err)
	}
	view.Declaration.Year = 0
	got, err := app.PutEntry(context.Background(), w.Key, view.Declaration)
	if err != nil {
		t.Fatal(err)
	}
	if got.Entry.Year != 0 || got.Declaration.Year != 0 {
		t.Fatalf("year was not cleared: entry=%d declaration=%d", got.Entry.Year, got.Declaration.Year)
	}
	view.Declaration.Title = "   "
	if _, err := app.PutEntry(context.Background(), w.Key, view.Declaration); err == nil {
		t.Fatal("blank title unexpectedly accepted")
	}
}

type partialSearchFetcher struct{}

func (partialSearchFetcher) Fetch(_ context.Context, u string) ([]rss.Entry, error) {
	if strings.Contains(u, "nyaa.si") {
		return []rss.Entry{
			{Title: "display one", DownloadURL: "https://nyaa.si/download/good.torrent"},
			{Title: "display two", DownloadURL: "https://nyaa.si/download/slow.torrent"},
		}, nil
	}
	return nil, nil
}
func (partialSearchFetcher) FetchTorrentMetadata(_ context.Context, u string) (torrentmeta.Metadata, error) {
	if strings.Contains(u, "good.torrent") {
		return torrentmeta.Metadata{Name: "[G] abcabc039 - 01 [1080p].mkv", InfoHash: "0123456789abcdef0123456789abcdef01234567"}, nil
	}
	return torrentmeta.Metadata{}, context.DeadlineExceeded
}

func TestSearchDiscoveryKeepsUsableReleasesWhenSomeTorrentMetadataFails(t *testing.T) {
	f := newFixture(t, nil)
	mustWrite(t, filepath.Join(f.cfg, "sources", "nyaa.json"), `{"id":"nyaa","provider":"nyaa","enabled":true}`)
	app, err := Open(Options{ConfigRoot: f.cfg, Fetcher: partialSearchFetcher{}, Backends: map[string]download.Backend{"c": f.client}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := app.SearchDiscovery(context.Background(), "nyaa", "abcabc039 01", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Groups) != 1 || len(got.Groups[0].Releases) != 1 {
		t.Fatalf("groups=%+v", got.Groups)
	}
	if len(got.Issues) != 1 || got.Issues[0].Code != "search_observation_partial" {
		t.Fatalf("issues=%+v", got.Issues)
	}
}

type delayedRSSFetcher struct {
	started chan string
	release chan struct{}
}

func (f delayedRSSFetcher) Fetch(ctx context.Context, u string) ([]rss.Entry, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case f.started <- u:
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-f.release:
	}
	name := "A"
	if strings.Contains(u, "feed-b") {
		name = "B"
	}
	return []rss.Entry{{Title: "[" + name + "] abcabc012 - 01 [1080p]", DownloadURL: "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567&dn=%5B" + name + "%5D+Concurrent+abcabc146+-+01+%5B1080p%5D"}}, nil
}

func TestRSSRefreshesSourcesConcurrentlyAndReadsNeverFetch(t *testing.T) {
	f := newFixture(t, map[string]string{"a": "feed-a", "b": "feed-b"})
	fetcher := delayedRSSFetcher{started: make(chan string, 2), release: make(chan struct{})}
	app, err := Open(Options{ConfigRoot: f.cfg, Fetcher: fetcher, Backends: map[string]download.Backend{"c": f.client}})
	if err != nil {
		t.Fatal(err)
	}
	cold, err := app.RSSDiscoveries(context.Background(), "", 20)
	if err != nil || len(cold.Groups) != 0 || len(cold.Sources) != 2 || cold.Sources[0].Status != "pending" {
		t.Fatalf("cold=%+v err=%v", cold, err)
	}
	select {
	case <-fetcher.started:
		t.Fatal("GET fetched remote RSS")
	default:
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- app.RefreshRSS(ctx) }()
	for range 2 {
		select {
		case <-fetcher.started:
		case <-time.After(2 * time.Second):
			t.Fatal("sources did not start concurrently")
		}
	}
	// The read must finish while both remote requests are still blocked.
	if _, err := app.RSSDiscoveries(context.Background(), "", 20); err != nil {
		t.Fatal(err)
	}
	close(fetcher.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	got, err := app.RSSDiscoveries(context.Background(), "", 20)
	if err != nil || len(got.Groups) == 0 {
		t.Fatalf("groups=%+v err=%v", got, err)
	}
	select {
	case <-fetcher.started:
		t.Fatal("cached GET fetched RSS again")
	default:
	}
}

func TestCreateEntryPreservesExplicitSeasonZero(t *testing.T) {
	f := newFixture(t, nil)
	app := service(t, f, fakeFetcher{})
	created, err := app.CreateEntry(context.Background(), catalog.MediaSeries, 0, EntryDeclaration{Title: "abcabc042"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(created.Entry.DeclarationPath, "Season 00")); err != nil {
		t.Fatalf("Season 00 source directory was not created: %v", err)
	}
	if _, err := os.Stat(filepath.Join(created.Entry.DeclarationPath, "Season 01")); !os.IsNotExist(err) {
		t.Fatalf("explicit S00 was silently rewritten to Season 01: %v", err)
	}
}

func TestDownloaderSecretLifecycle(t *testing.T) {
	f := newFixture(t, nil)
	app, err := Open(Options{ConfigRoot: f.cfg})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	client := configstore.Client{ID: "c", Type: "aria2", URL: "http://localhost:6800/jsonrpc", Enabled: true}
	password := " first secret \n"
	saved, err := app.SaveClient(ctx, "c", client, &password)
	if err != nil {
		t.Fatal(err)
	}
	if !saved.CredentialSet {
		t.Fatalf("secret presence missing from public response: %+v", saved)
	}
	secrets := secretstore.Store{Root: f.cfg}
	assertPassword := func(want string, wantSet bool) {
		t.Helper()
		got, set, err := secrets.Lookup(secretstore.DownloaderPassword("c"))
		if err != nil || set != wantSet || got != want {
			t.Fatalf("password=%q set=%v err=%v", got, set, err)
		}
	}
	assertPassword(password, true)
	data, err := os.ReadFile(filepath.Join(f.cfg, "clients", "c.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"first secret", "encrypted_credential", "credential_set", "credential"} {
		if strings.Contains(string(data), forbidden) {
			t.Fatalf("client config contains secret storage field %q: %s", forbidden, data)
		}
	}
	secretData, err := os.ReadFile(filepath.Join(f.cfg, "secrets", "integrations", "downloader", "c", "password.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(secretData), "first secret") || !strings.Contains(string(secretData), "ciphertext") {
		t.Fatal("recoverable secret was not encrypted in the secret store")
	}
	client.Username = "changed"
	if _, err := app.SaveClient(ctx, "c", client, nil); err != nil {
		t.Fatal(err)
	}
	assertPassword(password, true)
	replacement := "second secret"
	invalid := client
	invalid.URL = "invalid-url"
	if _, err := app.SaveClient(ctx, "c", invalid, &replacement); err == nil {
		t.Fatal("accepted invalid configuration")
	}
	assertPassword(password, true)
	if _, err := app.SaveClient(ctx, "c", client, &replacement); err != nil {
		t.Fatal(err)
	}
	assertPassword(replacement, true)
	reopened, err := Open(Options{ConfigRoot: f.cfg})
	if err != nil {
		t.Fatal(err)
	}
	view, err := reopened.Config(ctx)
	if err != nil || !view.Clients["c"].CredentialSet {
		t.Fatal("restart did not preserve safe secret status")
	}
	empty := ""
	if _, err := reopened.SaveClient(ctx, "c", client, &empty); err != nil {
		t.Fatal(err)
	}
	assertPassword("", false)
	third := "third secret"
	if _, err := reopened.SaveClient(ctx, "c", client, &third); err != nil {
		t.Fatal(err)
	}
	if err := reopened.DeleteClient(ctx, "c"); err != nil {
		t.Fatal(err)
	}
	assertPassword("", false)
}
