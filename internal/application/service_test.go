package application

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"aninode/internal/acquisition"
	"aninode/internal/backfill"
	"aninode/internal/candidate"
	"aninode/internal/catalog"
	"aninode/internal/claim"
	"aninode/internal/completeness"
	"aninode/internal/configstore"
	"aninode/internal/download"
	"aninode/internal/episode"
	"aninode/internal/filesystem"
	"aninode/internal/medianame"
	"aninode/internal/migration"
	"aninode/internal/observation"
	"aninode/internal/provider"
	"aninode/internal/release"
	"aninode/internal/releasefilter"
	"aninode/internal/rss"
	"aninode/internal/torrentmeta"
)

type fakeFetcher struct {
	entries map[string][]rss.Entry
	errs    map[string]error
}

func (f fakeFetcher) Fetch(_ context.Context, u string) ([]rss.Entry, error) {
	key := u
	if strings.HasPrefix(u, "http://feed.test/") {
		key = strings.TrimPrefix(u, "http://feed.test/")
	}
	if e := f.errs[key]; e != nil {
		return nil, e
	}
	return f.entries[key], nil
}

// Test feeds historically used RSS titles as shorthand for torrent names. The
// production path no longer does that, so the fake explicitly models native
// torrent metadata instead of relying on feed presentation text implicitly.
func (f fakeFetcher) FetchTorrentMetadata(_ context.Context, u string) (torrentmeta.Metadata, error) {
	for _, entries := range f.entries {
		for _, entry := range entries {
			if entry.DownloadURL == u {
				return torrentmeta.Metadata{Name: entry.Title}, nil
			}
		}
	}
	return torrentmeta.Metadata{}, errors.New("torrent metadata not found")
}

type fakeClient struct {
	name      string
	adds      []download.AddRequest
	tasks     []download.Task
	files     map[string][]download.File
	listCalls int
	getCalls  int
	fileIDs   []string
}

func (c *fakeClient) Name() string { return c.name }
func (c *fakeClient) Add(_ context.Context, r download.AddRequest) (download.Task, error) {
	c.adds = append(c.adds, r)
	task := download.Task{ID: "task-" + string(rune('0'+len(c.adds))), Name: r.URL, State: download.StateDownloading, Labels: append([]string(nil), r.Labels...), Sources: []string{r.URL}}
	c.tasks = append(c.tasks, task)
	return task, nil
}
func (c *fakeClient) SetLabels(_ context.Context, id string, labels []string) error {
	for i := range c.tasks {
		if c.tasks[i].ID == id {
			c.tasks[i].Labels = append(c.tasks[i].Labels, labels...)
			return nil
		}
	}
	return nil
}
func (c *fakeClient) Get(_ context.Context, id string) (download.Task, error) {
	c.getCalls++
	for _, t := range c.tasks {
		if t.ID == id {
			return t, nil
		}
	}
	return download.Task{}, errors.New("not found")
}
func (c *fakeClient) List(context.Context) ([]download.Task, error) {
	c.listCalls++
	return append([]download.Task(nil), c.tasks...), nil
}
func (c *fakeClient) Files(_ context.Context, id string) ([]download.File, error) {
	c.fileIDs = append(c.fileIDs, id)
	return append([]download.File(nil), c.files[id]...), nil
}
func (c *fakeClient) Pause(_ context.Context, id string) error {
	for i := range c.tasks {
		if c.tasks[i].ID == id {
			c.tasks[i].State = download.StatePaused
		}
	}
	return nil
}
func (c *fakeClient) Resume(_ context.Context, id string) error {
	for i := range c.tasks {
		if c.tasks[i].ID == id {
			c.tasks[i].State = download.StateSeeding
		}
	}
	return nil
}
func (c *fakeClient) Relocate(_ context.Context, id, destination string) error {
	for i := range c.tasks {
		if c.tasks[i].ID != id {
			continue
		}
		oldSave := c.tasks[i].SavePath
		if err := os.MkdirAll(destination, 0o755); err != nil {
			return err
		}
		for j := range c.files[id] {
			old := c.files[id][j].Path
			rel, err := filepath.Rel(oldSave, old)
			if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return fmt.Errorf("fake relocation file outside save path: %s", old)
			}
			updated := filepath.Join(destination, rel)
			if err := os.MkdirAll(filepath.Dir(updated), 0o755); err != nil {
				return err
			}
			if filepath.Clean(old) != filepath.Clean(updated) {
				if err := os.Rename(old, updated); err != nil {
					return err
				}
			}
			c.files[id][j].Path = updated
		}
		if c.tasks[i].ContentPath != "" {
			rel, err := filepath.Rel(oldSave, c.tasks[i].ContentPath)
			if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return fmt.Errorf("fake relocation content path outside save path: %s", c.tasks[i].ContentPath)
			}
			c.tasks[i].ContentPath = filepath.Join(destination, rel)
		}
		c.tasks[i].SavePath = destination
	}
	return nil
}
func (c *fakeClient) Verify(context.Context, string) error       { return nil }
func (c *fakeClient) Remove(context.Context, string, bool) error { return nil }

type fixture struct {
	root, cfg, state, secrets, src, lib string
	client                              *fakeClient
}

func newFixture(t *testing.T, sources map[string]string) fixture {
	t.Helper()
	root := t.TempDir()
	f := fixture{root: root, cfg: filepath.Join(root, "config"), state: filepath.Join(root, "state"), secrets: filepath.Join(root, "secrets"), src: filepath.Join(root, "downloads", "TV"), lib: filepath.Join(root, "library", "TV"), client: &fakeClient{name: "c", files: map[string][]download.File{}}}
	mustWrite(t, filepath.Join(f.cfg, "organizer.json"), `{"source":"`+filepath.Join(f.root, "downloads")+`","target":"`+filepath.Join(f.root, "library")+`","extensions":[".mkv"]}`)
	mustWrite(t, filepath.Join(f.cfg, "clients/c.json"), `{"id":"c","type":"aria2","url":"http://client","enabled":true}`)
	for id, u := range sources {
		mustWrite(t, filepath.Join(f.cfg, "sources", id+".json"), `{"id":"`+id+`","provider":"generic","rss":[{"url":"http://feed.test/`+u+`"}],"enabled":true}`)
	}
	return f
}
func mustWrite(t *testing.T, p, s string) {
	t.Helper()
	if e := os.MkdirAll(filepath.Dir(p), 0755); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(p, []byte(s), 0644); e != nil {
		t.Fatal(e)
	}
}

func mustReleases(t *testing.T, provider string, entries []rss.Entry) []release.Release {
	t.Helper()
	observed := make([]release.NameObservation, len(entries))
	for i := range entries {
		// Test fixtures model provider output after native torrent-name resolution.
		observed[i] = release.NameObservation{Value: entries[i].Title, Source: "test-native-name"}
	}
	values, err := release.EnrichManyObserved(provider, entries, observed)
	if err != nil {
		t.Fatal(err)
	}
	return values
}
func entry(title, id string) rss.Entry {
	return rss.Entry{Title: title, GUID: id, DownloadURL: "https://example.test/" + id + ".torrent", PublishedAt: time.Unix(int64(len(id)), 0)}
}
func service(t *testing.T, f fixture, fetch fakeFetcher) *App {
	t.Helper()
	app, err := Open(Options{ConfigRoot: f.cfg, Fetcher: fetch, Backends: map[string]download.Backend{"c": f.client}})
	if err != nil {
		t.Fatal(err)
	}
	return app
}

func TestOpenBootstrapsEmptyConfigDirectory(t *testing.T) {
	root := t.TempDir()
	configRoot := filepath.Join(root, "config")
	app, err := Open(Options{ConfigRoot: configRoot, Backends: map[string]download.Backend{}})
	if err != nil {
		t.Fatal(err)
	}
	if app == nil {
		t.Fatal("application is nil")
	}
	for _, name := range []string{"organizer.json"} {
		if _, err := os.Stat(filepath.Join(configRoot, name)); err != nil {
			t.Fatalf("%s was not bootstrapped: %v", name, err)
		}
	}
}

func TestReloadDoesNotRetainWorkMissingFromFilesystem(t *testing.T) {
	f := newFixture(t, nil)
	w, err := catalog.CreateManagedAt(f.src, f.lib, "abcabc136", 2026, 1, catalog.Declaration{})
	if err != nil {
		t.Fatal(err)
	}
	app := service(t, f, fakeFetcher{})
	if _, ok := app.snapshot().bundle.Entries[w.Key]; !ok {
		t.Fatal("entry was not loaded")
	}
	loaded := app.snapshot().bundle.Entries[w.Key]
	if err := os.RemoveAll(loaded.DeclarationPath); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(loaded.Path); err != nil {
		t.Fatal(err)
	}
	if err := app.reloadRuntime(); err != nil {
		t.Fatal(err)
	}
	if _, ok := app.snapshot().bundle.Entries[w.Key]; ok {
		t.Fatal("removed entry survived runtime reload")
	}
}

func TestSourceObservationDoesNotCreateEntryOrAcquireUnknownRelease(t *testing.T) {
	f := newFixture(t, map[string]string{"f": "feed"})
	s := service(t, f, fakeFetcher{entries: map[string][]rss.Entry{"feed": {entry("[grpabc166] abcabc defdef ghighi E01 1080p", "r1")}}})
	r, e := s.Cycle(context.Background(), CycleOptions{Acquire: true})
	if e != nil {
		t.Fatal(e)
	}
	if len(r.CreatedEntries) != 0 || len(f.client.adds) != 0 || len(r.DiscoveredEntries) != 1 {
		t.Fatalf("result=%+v adds=%d", r, len(f.client.adds))
	}
	if _, e := os.Stat(filepath.Join(f.src, "abcabc defdef ghighi")); !errors.Is(e, os.ErrNotExist) {
		t.Fatalf("feed observation materialized source: %v", e)
	}
}
func TestManyUnknownSourceTitlesCreateNoDirectories(t *testing.T) {
	f := newFixture(t, map[string]string{"f": "feed"})
	entries := make([]rss.Entry, 100)
	for i := range entries {
		entries[i] = entry(fmt.Sprintf("[G] abcabc044 %03d E01 1080p", i), fmt.Sprintf("x%d", i))
	}
	s := service(t, f, fakeFetcher{entries: map[string][]rss.Entry{"feed": entries}})
	r, e := s.Cycle(context.Background(), CycleOptions{Acquire: true})
	if e != nil {
		t.Fatal(e)
	}
	_, statErr := os.Stat(f.src)
	if len(r.CreatedEntries) != 0 || len(f.client.adds) != 0 || !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("%+v", r)
	}
}
func TestFilesystemDerivedNameMatchesCanonicalChineseWork(t *testing.T) {
	f := newFixture(t, map[string]string{"f": "feed"})
	w, e := catalog.CreateManagedAt(f.src, f.lib, "甲乙丙丁", 2023, 1, catalog.Declaration{})
	if e != nil {
		t.Fatal(e)
	}
	mustWrite(t, filepath.Join(w.DeclarationPath, "Season 01", "[G] abcabc defdef ghighi E02 1080p.mkv"), "two")
	s := service(t, f, fakeFetcher{entries: map[string][]rss.Entry{"feed": {entry("[G] abcabc defdef ghighi E03 1080p", "a3")}}})
	r, e := s.Cycle(context.Background(), CycleOptions{Acquire: true})
	if e != nil {
		t.Fatal(e)
	}
	if len(r.CreatedEntries) != 0 || len(f.client.adds) != 1 {
		t.Fatalf("%+v", r)
	}
}

func TestSourceFailureDoesNotBlockHealthySource(t *testing.T) {
	f := newFixture(t, map[string]string{"a": "A", "b": "B"})
	_, _ = catalog.CreateManagedAt(f.src, f.lib, "abcabc146", 2026, 1, catalog.Declaration{})
	s := service(t, f, fakeFetcher{entries: map[string][]rss.Entry{"B": {entry("[G] abcabc146 E01 1080p", "b1")}}, errs: map[string]error{"A": errors.New("timeout")}})
	r, e := s.Cycle(context.Background(), CycleOptions{Acquire: true})
	if e != nil {
		t.Fatal(e)
	}
	if len(r.Issues) == 0 || len(f.client.adds) != 1 {
		t.Fatalf("%+v adds=%d", r, len(f.client.adds))
	}
}
func TestCompetingGroupsOnlyOneEpisodeDownloaded(t *testing.T) {
	f := newFixture(t, map[string]string{"f": "feed"})
	_, _ = catalog.CreateManagedAt(f.src, f.lib, "abcabc146", 2026, 1, catalog.Declaration{})
	s := service(t, f, fakeFetcher{entries: map[string][]rss.Entry{"feed": {entry("[grpabc166] abcabc146 E03 1080p", "g1"), entry("[grpabc156] abcabc146 E03 1080p", "g2")}}})
	r, e := s.Cycle(context.Background(), CycleOptions{Acquire: true})
	if e != nil {
		t.Fatal(e)
	}
	if len(f.client.adds) != 1 || len(r.SelectedReleases) != 1 {
		t.Fatalf("selected=%d adds=%d %+v", len(r.SelectedReleases), len(f.client.adds), r)
	}
}
func TestV2BeatsV1(t *testing.T) {
	f := newFixture(t, map[string]string{"f": "feed"})
	_, _ = catalog.CreateManagedAt(f.src, f.lib, "abcabc146", 2026, 1, catalog.Declaration{})
	s := service(t, f, fakeFetcher{entries: map[string][]rss.Entry{"feed": {entry("[G] abcabc146 E03 1080p", "v1"), entry("[G] abcabc146 E03v2 1080p", "v2")}}})
	r, _ := s.Cycle(context.Background(), CycleOptions{Acquire: true})
	if len(r.SelectedReleases) != 1 || r.SelectedReleases[0].Episode.Revision != 2 {
		t.Fatalf("%+v", r.SelectedReleases)
	}
}
func TestSeasonTwoAutoCreated(t *testing.T) {
	f := newFixture(t, map[string]string{"f": "feed"})
	w, _ := catalog.CreateManagedAt(f.src, f.lib, "abcabc146", 2026, 1, catalog.Declaration{})
	s := service(t, f, fakeFetcher{entries: map[string][]rss.Entry{"feed": {entry("[G] abcabc146 S02E01 1080p", "s2")}}})
	r, _ := s.Cycle(context.Background(), CycleOptions{Acquire: true})
	if _, e := os.Stat(filepath.Join(w.Path, "Season 02")); e != nil {
		t.Fatal(e)
	}
	if len(f.client.adds) != 1 || f.client.adds[0].SavePath != filepath.Join(f.src, filepath.Base(w.Path), "Season 02") {
		t.Fatalf("%+v %+v", r, f.client.adds)
	}
}
func TestRestartDownloaderMetadataCompletedTaskOrganizes(t *testing.T) {
	f := newFixture(t, map[string]string{})
	w, _ := catalog.CreateManagedAt(f.src, f.lib, "abcabc146", 2026, 1, catalog.Declaration{})
	source := filepath.Join(f.src, filepath.Base(w.Path), "Season 01", "abcabc146 E01.mkv")
	mustWrite(t, source, "video")
	f.client.tasks = []download.Task{{ID: "done", State: download.StateCompleted, Progress: 1, SavePath: filepath.Dir(source)}}
	f.client.files["done"] = []download.File{{Path: source, Wanted: true, Progress: 1}}
	s := service(t, f, fakeFetcher{})
	r, e := s.Cycle(context.Background(), CycleOptions{Reconcile: true})
	if e != nil {
		t.Fatal(e)
	}
	if r.OrganizedFiles != 1 {
		t.Fatalf("%+v", r)
	}
	target := filepath.Join(w.Path, "Season 01", "abcabc146 S01E01.mkv")
	si, _ := os.Stat(source)
	ti, e := os.Stat(target)
	if e != nil || !os.SameFile(si, ti) {
		t.Fatalf("target=%v same=%v", e, e == nil && os.SameFile(si, ti))
	}
}

type serialFetcher struct {
	mu      sync.Mutex
	active  int
	maximum int
}

func (f *serialFetcher) Fetch(context.Context, string) ([]rss.Entry, error) {
	f.mu.Lock()
	f.active++
	if f.active > f.maximum {
		f.maximum = f.active
	}
	f.mu.Unlock()
	time.Sleep(20 * time.Millisecond)
	f.mu.Lock()
	f.active--
	f.mu.Unlock()
	return nil, nil
}

func TestCycleSerializesMutations(t *testing.T) {
	f := newFixture(t, map[string]string{"f": "feed"})
	fetcher := &serialFetcher{}
	app, err := Open(Options{ConfigRoot: f.cfg, Fetcher: fetcher, Backends: map[string]download.Backend{"c": f.client}})
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			<-start
			_, err := app.Cycle(context.Background(), CycleOptions{})
			errs <- err
		}()
	}
	close(start)
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	fetcher.mu.Lock()
	defer fetcher.mu.Unlock()
	if fetcher.maximum != 1 {
		t.Fatalf("maximum concurrent mutation cycles=%d, want 1", fetcher.maximum)
	}
}

func TestUnknownEntryInventoryIsIsolatedFromHealthyWork(t *testing.T) {
	f := newFixture(t, map[string]string{"f": "feed"})
	bad, err := catalog.CreateManagedAt(f.src, f.lib, "abcabc063", 2026, 1, catalog.Declaration{Sources: []string{"f"}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = catalog.CreateManagedAt(f.src, f.lib, "abcabc059", 2026, 1, catalog.Declaration{Sources: []string{"f"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(bad.Path); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(bad.DeclarationPath); err != nil {
		t.Fatal(err)
	}
	app := service(t, f, fakeFetcher{entries: map[string][]rss.Entry{"feed": {entry("[G] abcabc059 E01 1080p", "good1")}}})
	r, err := app.Cycle(context.Background(), CycleOptions{Acquire: true})
	if err != nil {
		t.Fatalf("healthy Entry was blocked by unrelated unknown inventory: %v", err)
	}
	if len(f.client.adds) != 1 || f.client.adds[0].URL != "https://example.test/good1.torrent" {
		t.Fatalf("healthy acquisition did not continue: adds=%+v result=%+v", f.client.adds, r)
	}
}

func TestCycleSerializationRejectsConcurrentWriterAcrossAppInstances(t *testing.T) {
	f := newFixture(t, map[string]string{"f": "feed"})
	fetcher := &serialFetcher{}
	options := Options{ConfigRoot: f.cfg, Fetcher: fetcher, Backends: map[string]download.Backend{"c": f.client}}
	a, err := Open(options)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Open(options)
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	errs := make(chan error, 2)
	for _, app := range []*App{a, b} {
		go func(app *App) {
			<-start
			_, err := app.Cycle(context.Background(), CycleOptions{})
			errs <- err
		}(app)
	}
	close(start)
	var failures int
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			if !errors.Is(err, filesystem.ErrWriterLocked) {
				t.Fatal(err)
			}
			failures++
		}
	}
	if failures != 1 {
		t.Fatalf("expected exactly one concurrent writer rejection, got %d", failures)
	}
	fetcher.mu.Lock()
	defer fetcher.mu.Unlock()
	if fetcher.maximum < 1 {
		t.Fatalf("cycles did not run: maximum=%d", fetcher.maximum)
	}
}

func TestFolderProjectionDoesNotMutateAcquisitionSourceIdentity(t *testing.T) {
	f := newFixture(t, map[string]string{"f": "feed"})
	w, err := catalog.CreateManagedAt(f.src, f.lib, "abcabc146", 2026, 1, catalog.Declaration{})
	if err != nil {
		t.Fatal(err)
	}
	d, err := catalog.ReadDeclaration(w.DeclarationPath)
	if err != nil {
		t.Fatal(err)
	}
	d.Folders = map[string]catalog.FolderProjection{"Season 01": {Season: 2, EpisodeOffset: -12}}
	if err := catalog.WriteDeclaration(w.DeclarationPath, *d); err != nil {
		t.Fatal(err)
	}
	s := service(t, f, fakeFetcher{entries: map[string][]rss.Entry{"feed": {entry("[G] abcabc146 E13 1080p", "mapped13")}}})
	r, err := s.Cycle(context.Background(), CycleOptions{Acquire: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.SelectedReleases) != 1 || len(f.client.adds) != 1 {
		t.Fatalf("result=%+v adds=%+v", r, f.client.adds)
	}
	c := r.SelectedReleases[0]
	if c.SourceEpisode.Season != 1 || c.SourceEpisode.EpisodeStart != 13 {
		t.Fatalf("source=%+v", c.SourceEpisode)
	}
	if filepath.Base(f.client.adds[0].SavePath) != "Season 01" {
		t.Fatalf("save path=%q", f.client.adds[0].SavePath)
	}
}

func TestMigrationOmitsTaskAlreadyInsideManagedEntry(t *testing.T) {
	f := newFixture(t, map[string]string{})
	w, err := catalog.CreateManagedAt(f.src, f.lib, "abcabc146", 2026, 1, catalog.Declaration{})
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(f.src, filepath.Base(w.Path), "Season 01", "abcabc146 - 01.mkv")
	mustWrite(t, source, "video")
	f.client.tasks = []download.Task{{ID: "existing", InfoHash: "0123456789abcdef0123456789abcdef01234567", Name: "abcabc146 - 01", State: download.StateSeeding, Progress: 1, SavePath: filepath.Dir(source)}}
	f.client.files["existing"] = []download.File{{Path: source, Size: 5, Completed: 5, Wanted: true, Progress: 1}}
	app := service(t, f, fakeFetcher{})
	preview, err := app.MigrationPlan(context.Background())
	if err != nil || len(preview.Groups) != 0 {
		t.Fatalf("already managed task leaked into migration preview: %+v err=%v", preview, err)
	}
}

func TestMigrationBulkApplyIsRejectedWithoutExplicitCurrentSelection(t *testing.T) {
	f := newFixture(t, map[string]string{})
	app := service(t, f, fakeFetcher{})
	if _, err := app.Migrate(context.Background(), true); err == nil {
		t.Fatal("bulk migration apply unexpectedly succeeded")
	}
}

func TestMigrationGroupsMergeObservedFilterOptionsWithExistingExplicitFilters(t *testing.T) {
	entry := catalog.Entry{
		Key:       "series/abcabc146",
		Title:     "abcabc146",
		MediaType: catalog.MediaSeries,
		Filters:   releasefilter.Filters{Groups: []string{"grpabc166"}},
	}
	bundle := configstore.Bundle{Entries: map[string]catalog.Entry{entry.Key: entry}}
	plans := []migration.Plan{{
		Client: "c", TaskID: "task", EntryKey: entry.Key, Title: "abcabc146", MediaType: catalog.MediaSeries,
		Season: 1, SeasonKnown: true, Decision: migration.DecisionRelocate,
		FilterOptions: releasefilter.NewOptions([]string{"grpabc154"}, []string{"1080p"}, []string{"zh-Hans"}),
	}}
	groups := groupMigrationPlans(bundle, plans)
	if len(groups) != 1 {
		t.Fatalf("groups=%+v", groups)
	}
	group := groups[0]
	if !group.Filters.Equal(entry.Filters) {
		t.Fatalf("filters=%+v want=%+v", group.Filters, entry.Filters)
	}
	if !containsString(group.FilterOptions.Groups, "grpabc166") || !containsString(group.FilterOptions.Groups, "grpabc154") {
		t.Fatalf("filter options=%+v", group.FilterOptions)
	}
}

func TestMigrationUnitCreatesCorrectedWorkAndMovesTask(t *testing.T) {
	f := newFixture(t, map[string]string{})
	sourceDir := filepath.Join(f.src, "incoming")
	source := filepath.Join(sourceDir, "[grpabc166] Release - 01 [1080p][CHS].mkv")
	mustWrite(t, source, "video")
	f.client.tasks = []download.Task{{ID: "existing", InfoHash: "1123456789abcdef0123456789abcdef01234567", Name: "[grpabc166] Release - 01 [1080p][CHS]", State: download.StateSeeding, Progress: 1, SavePath: sourceDir}}
	f.client.files["existing"] = []download.File{{Path: source, Size: 5, Completed: 5, Wanted: true, Progress: 1}}
	app := service(t, f, fakeFetcher{})
	preview, err := app.MigrationPlan(context.Background())
	if err != nil || len(preview.Groups) != 1 || preview.Groups[0].Blocked != 1 {
		t.Fatalf("preview=%+v err=%v", preview, err)
	}
	if !containsString(preview.Groups[0].FilterOptions.Groups, "grpabc166") || !containsString(preview.Groups[0].FilterOptions.Resolutions, "1080p") {
		t.Fatalf("migration filter options did not reuse parsed task/file traits: %+v", preview.Groups[0].FilterOptions)
	}
	if !preview.Groups[0].Filters.Empty() {
		t.Fatalf("new migration unexpectedly inferred durable filters: %+v", preview.Groups[0].Filters)
	}
	f.client.listCalls, f.client.getCalls, f.client.fileIDs = 0, 0, nil
	result, err := app.MigrationApplyUnit(context.Background(), MigrationUnitInput{Key: preview.Groups[0].Key, Title: "abcabc032", Season: 2, MediaType: catalog.MediaSeries, Filters: releasefilter.Filters{Groups: []string{"grpabc166"}, Resolutions: []string{"1080p"}}, Tasks: []MigrationTaskIdentity{{Client: preview.Groups[0].Plans[0].Client, TaskID: preview.Groups[0].Plans[0].TaskID, InfoHash: preview.Groups[0].Plans[0].InfoHash}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Applied) != 1 {
		t.Fatalf("result=%+v", result)
	}
	if f.client.listCalls != 0 {
		t.Fatalf("apply performed %d global List calls", f.client.listCalls)
	}
	if f.client.getCalls == 0 {
		t.Fatal("apply did not perform targeted Get")
	}
	for _, id := range f.client.fileIDs {
		if id != "existing" {
			t.Fatalf("observed unrelated task %q", id)
		}
	}
	w, err := catalog.DiscoverAt(f.src, f.lib)
	if err != nil || len(w) != 1 {
		t.Fatalf("entries=%+v err=%v", w, err)
	}
	for _, value := range w {
		if value.Title != "abcabc032" || len(value.Seasons) != 1 || value.Seasons[0] != 2 {
			t.Fatalf("entry=%+v", value)
		}
		if !value.Filters.Equal(releasefilter.Filters{Groups: []string{"grpabc166"}, Resolutions: []string{"1080p"}}) {
			t.Fatalf("migration filters were not persisted: %+v", value.Filters)
		}
		if _, err := os.Stat(filepath.Join(value.Path, ".aninode.json")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("library declaration must not exist: %v", err)
		}
		if _, err := os.Stat(filepath.Join(value.DeclarationPath, ".aninode.json")); err != nil {
			t.Fatalf("source declaration missing: %v", err)
		}
	}
}

func TestMigrationUnitNormalizesMovieSeasonAndCreatesNoSeasonDirectory(t *testing.T) {
	f := newFixture(t, map[string]string{})
	sourceDir := filepath.Join(f.root, "downloads", "incoming")
	source := filepath.Join(sourceDir, "abcabc148 2024.mkv")
	mustWrite(t, source, "movie")
	f.client.tasks = []download.Task{{ID: "movie", InfoHash: "2123456789abcdef0123456789abcdef01234567", Name: "abcabc148 2024", State: download.StateSeeding, Progress: 1, SavePath: sourceDir}}
	f.client.files["movie"] = []download.File{{Path: source, Size: 5, Completed: 5, Wanted: true, Progress: 1}}
	app := service(t, f, fakeFetcher{})
	preview, err := app.MigrationPlan(context.Background())
	if err != nil || len(preview.Groups) != 1 || preview.Groups[0].MediaType != catalog.MediaMovie {
		t.Fatalf("preview=%+v err=%v", preview, err)
	}
	group := preview.Groups[0]
	_, err = app.MigrationApplyUnit(context.Background(), MigrationUnitInput{Key: group.Key, Title: "abcabc148", Year: 2024, Season: 99, MediaType: catalog.MediaMovie, Tasks: []MigrationTaskIdentity{{Client: group.Plans[0].Client, TaskID: group.Plans[0].TaskID, InfoHash: group.Plans[0].InfoHash}}})
	if err != nil {
		t.Fatal(err)
	}
	movies, err := catalog.DiscoverMoviesAt(filepath.Join(f.root, "downloads", "Movies"), filepath.Join(f.root, "library", "Movies"))
	if err != nil || len(movies) != 1 {
		t.Fatalf("movies=%+v err=%v", movies, err)
	}
	for _, movie := range movies {
		if movie.Title != "abcabc148" || movie.Year != 2024 || len(movie.Seasons) != 0 {
			t.Fatalf("movie=%+v", movie)
		}
		if _, err := os.Stat(filepath.Join(movie.DeclarationPath, "Season 01")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("movie unexpectedly has Season 01: %v", err)
		}
	}
}

func TestMigrationCorrectedMovieIdentityAdoptsStructuredBundleAndSurvivesRestart(t *testing.T) {
	f := newFixture(t, map[string]string{})
	release := "[grpabc202] abcabc113 [tagabc_816p]"
	container := filepath.Join(f.root, "downloads", "Movies", release)
	assets := map[string]string{
		"abcabc116.mkv": "video", "abcabc116.mka": "audio",
		"abcabc116.ass": "subtitle", "CDs/disc.flac": "disc",
		"Scans/001.jpg": "scan", "SPs/trailer.mkv": "trailer", "Fonts.zip": "fonts",
	}
	var files []download.File
	for relative, content := range assets {
		path := filepath.Join(container, filepath.FromSlash(relative))
		mustWrite(t, path, content)
		files = append(files, download.File{Path: path, Size: int64(len(content)), Completed: int64(len(content)), Wanted: true, Progress: 1})
	}
	f.client.tasks = []download.Task{{ID: "taskdef", InfoHash: "3123456789abcdef0123456789abcdef01234567", Name: release, State: download.StateSeeding, Progress: 1, SavePath: filepath.Dir(container), ContentPath: container}}
	f.client.files["taskdef"] = files
	app := service(t, f, fakeFetcher{})
	preview, err := app.MigrationPlan(context.Background())
	if err != nil || len(preview.Groups) != 1 {
		t.Fatalf("preview=%+v err=%v", preview, err)
	}
	g := preview.Groups[0]
	result, err := app.MigrationApplyUnit(context.Background(), MigrationUnitInput{Key: g.Key, Title: "甲乙〈III丙丁〉", Year: 2017, MediaType: catalog.MediaMovie, Tasks: []MigrationTaskIdentity{{Client: g.Plans[0].Client, TaskID: "taskdef", InfoHash: g.Plans[0].InfoHash}}})
	if err != nil || len(result.Applied) != 1 {
		t.Fatalf("apply=%+v err=%v", result, err)
	}
	if f.client.tasks[0].ContentPath != container || f.client.tasks[0].SavePath != filepath.Dir(container) {
		t.Fatalf("adopt renamed or relocated source: %+v", f.client.tasks[0])
	}
	movies, err := catalog.DiscoverMoviesAt(filepath.Join(f.root, "downloads", "Movies"), filepath.Join(f.root, "library", "Movies"))
	if err != nil {
		t.Fatalf("movies=%+v err=%v", movies, err)
	}
	var movie catalog.Entry
	active := 0
	for _, candidate := range movies {
		if candidate.Enabled {
			movie = candidate
			active++
		}
	}
	if active != 1 {
		t.Fatalf("active movies=%d all=%+v", active, movies)
	}
	if movie.Title != "甲乙〈III丙丁〉" || movie.Year != 2017 {
		t.Fatalf("movie identity=%+v", movie)
	}
	publication := filepath.Join(f.root, "library", "Movies", "甲乙〈III丙丁〉 (2017)", "甲乙〈III丙丁〉 (2017).mkv")
	if sourceInfo, statErr := os.Stat(filepath.Join(container, "abcabc116.mkv")); statErr != nil {
		t.Fatal(statErr)
	} else if publishedInfo, statErr := os.Stat(publication); statErr != nil || !os.SameFile(sourceInfo, publishedInfo) {
		t.Fatalf("publication is not a hardlink: %s err=%v", publication, statErr)
	}
	restarted := service(t, f, fakeFetcher{})
	b := restarted.snapshot().bundle
	c := claim.Resolve(context.Background(), b, "c", f.client.tasks[0], f.client.files["taskdef"])
	if c.Status != claim.Resolved || c.EntryKey != movie.Key {
		t.Fatalf("restart claim=%+v entry=%+v", c, movie)
	}
	if again, planErr := restarted.MigrationPlan(context.Background()); planErr != nil || len(again.Groups) != 0 {
		t.Fatalf("published task requested migration again: %+v err=%v", again, planErr)
	}
}

func TestMigrationConfirmedFolderMovieRelocatesParentPreservesTorrentRootAndPublishesCanonicalName(t *testing.T) {
	f := newFixture(t, map[string]string{})
	release := "[grpabc202] abcabc111 [tagabc_816p]"
	existingSave := filepath.Join(f.root, "downloads", "video", "movie")
	container := filepath.Join(existingSave, release)
	assets := map[string]string{
		"abcabc114.mkv": "video", "abcabc114.mka": "audio",
		"abcabc114.ass": "subtitle", "CDs/disc.flac": "disc",
		"Scans/001.jpg": "scan", "SPs/trailer.mkv": "trailer", "Fonts.zip": "fonts",
	}
	var files []download.File
	for relative, content := range assets {
		path := filepath.Join(container, filepath.FromSlash(relative))
		mustWrite(t, path, content)
		files = append(files, download.File{Path: path, Size: int64(len(content)), Completed: int64(len(content)), Wanted: true, Progress: 1})
	}
	f.client.tasks = []download.Task{{ID: "tekketsu", InfoHash: "5123456789abcdef0123456789abcdef01234567", Name: release, State: download.StateSeeding, Progress: 1, SavePath: existingSave, ContentPath: container}}
	f.client.files["tekketsu"] = files
	app := service(t, f, fakeFetcher{})
	preview, err := app.MigrationPlan(context.Background())
	if err != nil || len(preview.Groups) != 1 {
		t.Fatalf("preview=%+v err=%v", preview, err)
	}
	g := preview.Groups[0]
	result, err := app.MigrationApplyUnit(context.Background(), MigrationUnitInput{
		Key: g.Key, Title: "甲乙〈Ⅰ丙丁〉", Year: 2016, MediaType: catalog.MediaMovie,
		Tasks: []MigrationTaskIdentity{{Client: g.Plans[0].Client, TaskID: "tekketsu", InfoHash: g.Plans[0].InfoHash}},
	})
	if err != nil || len(result.Applied) != 1 {
		t.Fatalf("apply=%+v err=%v", result, err)
	}
	wantSave := filepath.Join(f.root, "downloads", "Movies")
	wantContainer := filepath.Join(wantSave, release)
	if got := f.client.tasks[0]; got.SavePath != wantSave || got.ContentPath != wantContainer {
		t.Fatalf("torrent root was canonicalized instead of preserving release folder: %+v", got)
	}
	if _, err := os.Stat(filepath.Join(wantContainer, ".aninode.json")); err != nil {
		t.Fatalf("durable Entry declaration was not moved into confirmed movie source: %v", err)
	}
	movies, err := catalog.DiscoverMoviesAt(wantSave, filepath.Join(f.root, "library", "Movies"))
	if err != nil || len(movies) != 1 {
		t.Fatalf("movies=%+v err=%v", movies, err)
	}
	var movie catalog.Entry
	for _, candidate := range movies {
		movie = candidate
	}
	if movie.Title != "甲乙〈Ⅰ丙丁〉" || movie.Year != 2016 || movie.Key == "" || movie.DeclarationPath != wantContainer {
		t.Fatalf("movie identity/source=%+v", movie)
	}
	publication := filepath.Join(f.root, "library", "Movies", "甲乙〈Ⅰ丙丁〉 (2016)", "甲乙〈Ⅰ丙丁〉 (2016).mkv")
	sourceInfo, err := os.Stat(filepath.Join(wantContainer, "abcabc114.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	publishedInfo, err := os.Stat(publication)
	if err != nil || !os.SameFile(sourceInfo, publishedInfo) {
		t.Fatalf("canonical publication is not a hardlink: %s err=%v", publication, err)
	}
	restarted := service(t, f, fakeFetcher{})
	reloaded, ok := restarted.snapshot().bundle.Entries[movie.Key]
	if !ok || reloaded.Key != movie.Key || reloaded.Title != movie.Title || reloaded.DeclarationPath != wantContainer {
		t.Fatalf("restart changed durable identity: before=%+v after=%+v", movie, reloaded)
	}
	c := claim.Resolve(context.Background(), restarted.snapshot().bundle, "c", f.client.tasks[0], f.client.files["tekketsu"])
	if c.Status != claim.Resolved || c.EntryKey != movie.Key {
		t.Fatalf("restart claim=%+v", c)
	}
	if again, planErr := restarted.MigrationPlan(context.Background()); planErr != nil || len(again.Groups) != 0 {
		t.Fatalf("relocated published task requested migration again: %+v err=%v", again, planErr)
	}
}

func TestMigrationMovieClassificationCanCompleteCorrection(t *testing.T) {
	f := newFixture(t, map[string]string{})
	container := filepath.Join(f.root, "downloads", "Movies", "abcabc127")
	main, bonus := filepath.Join(container, "abcabc148.mkv"), filepath.Join(container, "Bonus.mkv")
	mustWrite(t, main, "main")
	mustWrite(t, bonus, "bonus")
	f.client.tasks = []download.Task{{ID: "classified", InfoHash: "4123456789abcdef0123456789abcdef01234567", Name: "abcabc127", State: download.StateSeeding, Progress: 1, SavePath: filepath.Dir(container), ContentPath: container}}
	f.client.files["classified"] = []download.File{{Path: main, Size: 4, Completed: 4, Wanted: true, Progress: 1}, {Path: bonus, Size: 5, Completed: 5, Wanted: true, Progress: 1}}
	app := service(t, f, fakeFetcher{})
	preview, err := app.MigrationPlan(context.Background())
	if err != nil || len(preview.Groups) != 1 {
		t.Fatalf("preview=%+v err=%v", preview, err)
	}
	identity := []MigrationTaskIdentity{{Client: "c", TaskID: "classified", InfoHash: preview.Groups[0].Plans[0].InfoHash}}
	blockedResult, err := app.MigrationApplyUnit(context.Background(), MigrationUnitInput{Key: preview.Groups[0].Key, Title: "abcabc018", Year: 2020, MediaType: catalog.MediaMovie, Tasks: identity})
	if err == nil {
		t.Fatal("unclassified secondary video unexpectedly migrated")
	}
	if !strings.Contains(err.Error(), "movie bundle requires classification") || len(blockedResult.Plans) != 1 || !strings.Contains(blockedResult.Plans[0].Detail, "movie bundle requires classification") {
		t.Fatalf("classification reason missing from confirmed planning result: result=%+v err=%v", blockedResult, err)
	}
	canonicalSource := filepath.Join(f.root, "downloads", "Movies", "abcabc018 (2020)")
	if _, statErr := os.Stat(canonicalSource); !os.IsNotExist(statErr) {
		t.Fatalf("failed confirmed planning created canonical source placeholder: %s err=%v", canonicalSource, statErr)
	}
	result, err := app.MigrationApplyUnit(context.Background(), MigrationUnitInput{Key: preview.Groups[0].Key, Title: "abcabc018", Year: 2020, MediaType: catalog.MediaMovie, Movie: catalog.MovieDeclaration{Classify: map[string]string{"Bonus.mkv": "extra"}}, Tasks: identity})
	if err != nil || len(result.Applied) != 1 {
		t.Fatalf("classified apply=%+v err=%v", result, err)
	}
	movies, err := catalog.DiscoverMoviesAt(filepath.Join(f.root, "downloads", "Movies"), filepath.Join(f.root, "library", "Movies"))
	if err != nil {
		t.Fatal(err)
	}
	for _, movie := range movies {
		if movie.Enabled && movie.Title == "abcabc018" && movie.Movie.Classify["Bonus.mkv"] == "extra" {
			return
		}
	}
	t.Fatalf("classification was not persisted: %+v", movies)
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

type fakeHistory struct {
	calls   []backfill.Request
	entries []release.Release
	err     error
}

func (h *fakeHistory) SearchHistory(_ context.Context, _ string, r backfill.Request) ([]release.Release, error) {
	h.calls = append(h.calls, r)
	return append([]release.Release(nil), h.entries...), h.err
}

func TestCompletenessBackfillSearchesOnlyCurrentExactMissingSet(t *testing.T) {
	f := newFixture(t, map[string]string{"f": "feed"})
	w, err := catalog.CreateManagedAt(f.src, f.lib, "abcabc146", 2026, 1, catalog.Declaration{})
	if err != nil {
		t.Fatal(err)
	}
	season := filepath.Join(w.Path, "Season 01")
	mustWrite(t, filepath.Join(season, "abcabc146 - S01E01.mkv"), "one")
	mustWrite(t, filepath.Join(season, "abcabc146 - S01E03.mkv"), "three")
	history := &fakeHistory{entries: mustReleases(t, "generic", []rss.Entry{entry("[G] abcabc146 S01E02 1080p", "hist2"), entry("[G] abcabc146 S01E01 1080p", "hist1")})}
	now := time.Unix(10000, 0).UTC()
	fetch := fakeFetcher{entries: map[string][]rss.Entry{"feed": {entry("[G] abcabc146 S01E03 1080p", "current3")}}}
	app, err := Open(Options{ConfigRoot: f.cfg, Fetcher: fetch, History: history, Backends: map[string]download.Backend{"c": f.client}, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	r, err := app.Cycle(context.Background(), CycleOptions{Acquire: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(history.calls) != 1 {
		t.Fatalf("history calls=%d", len(history.calls))
	}
	req := history.calls[0]
	if len(req.SourceMissing) != 1 || req.SourceMissing[0].EpisodeStart != 2 {
		t.Fatalf("request=%+v", req)
	}
	if len(f.client.adds) != 1 || len(r.Acquisitions) == 0 {
		t.Fatalf("adds=%d acquisitions=%+v", len(f.client.adds), r.Acquisitions)
	}
	if got := f.client.adds[0].URL; got != "https://example.test/hist2.torrent" {
		t.Fatalf("unexpected backfill URL %s", got)
	}
	// Restart recomputes from current evidence; no cadence is persisted.
	app2, err := Open(Options{ConfigRoot: f.cfg, Fetcher: fetch, History: history, Backends: map[string]download.Backend{"c": f.client}, Now: func() time.Time { return now.Add(time.Minute) }})
	if err != nil {
		t.Fatal(err)
	}
	_, err = app2.Cycle(context.Background(), CycleOptions{Acquire: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(history.calls) != 2 {
		t.Fatalf("current evidence was not recomputed, calls=%d", len(history.calls))
	}
}

func TestLocalOnlyCycleNeverSearchesHistoricalSources(t *testing.T) {
	f := newFixture(t, map[string]string{"f": "feed"})
	w, err := catalog.CreateManagedAt(f.src, f.lib, "abcabc146", 2026, 1, catalog.Declaration{})
	if err != nil {
		t.Fatal(err)
	}
	season := filepath.Join(w.Path, "Season 01")
	mustWrite(t, filepath.Join(season, "abcabc146 - S01E01.mkv"), "one")
	mustWrite(t, filepath.Join(season, "abcabc146 - S01E03.mkv"), "three")
	history := &fakeHistory{entries: mustReleases(t, "generic", []rss.Entry{entry("[G] abcabc146 S01E02 1080p", "hist2")})}
	app, err := Open(Options{ConfigRoot: f.cfg, Fetcher: fakeFetcher{}, History: history, Backends: map[string]download.Backend{"c": f.client}})
	if err != nil {
		t.Fatal(err)
	}

	result, err := app.Cycle(context.Background(), CycleOptions{LocalOnly: true, Acquire: true, Reconcile: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(history.calls) != 0 {
		t.Fatalf("local-only cycle made %d historical search calls", len(history.calls))
	}
	if len(result.Acquisitions) != 0 || len(f.client.adds) != 0 {
		t.Fatalf("local-only cycle acquired remote releases: result=%+v adds=%+v", result.Acquisitions, f.client.adds)
	}
}

func TestProductionHistoricalProviderWiringRepairsThroughLatestObserved(t *testing.T) {
	f := newFixture(t, map[string]string{"f": "feed"})
	w, err := catalog.CreateManagedAt(f.src, f.lib, "abcabc146", 2026, 1, catalog.Declaration{Sources: []string{"f"}})
	if err != nil {
		t.Fatal(err)
	}
	season := filepath.Join(w.Path, "Season 01")
	mustWrite(t, filepath.Join(season, "abcabc146 S01E01.mkv"), "1")
	mustWrite(t, filepath.Join(season, "abcabc146 S01E02.mkv"), "2")
	mustWrite(t, filepath.Join(season, "abcabc146 S01E04.mkv"), "4")
	mustWrite(t, filepath.Join(f.cfg, "sources/f.json"), `{"id":"f","provider":"generic","rss":[{"url":"http://feed.test/feed"}],"search":{"url_template":"http://history.test/search?q={query}"},"enabled":true}`)
	searchURL := "http://history.test/search?q=abcabc146+S01E03"
	fetch := fakeFetcher{entries: map[string][]rss.Entry{
		"feed":    {entry("[G] abcabc146 S01E05 1080p", "current5")},
		searchURL: {entry("[G] abcabc146 S01E03 1080p", "history3")},
	}}
	app, err := Open(Options{ConfigRoot: f.cfg, Fetcher: fetch, Backends: map[string]download.Backend{"c": f.client}})
	if err != nil {
		t.Fatal(err)
	}
	r, err := app.Cycle(context.Background(), CycleOptions{Acquire: true})
	if err != nil {
		t.Fatal(err)
	}
	var st completeness.SeasonState
	for _, s := range r.Completeness {
		if s.EntryKey == w.Key && s.Season == 1 {
			st = s
		}
	}
	if len(st.Missing) != 1 || st.Missing[0] != 3 || st.Status != "incomplete" {
		t.Fatalf("state=%+v", st)
	}
	foundHistory := false
	for _, add := range f.client.adds {
		if add.URL == "https://example.test/history3.torrent" {
			foundHistory = true
		}
	}
	if !foundHistory {
		t.Fatalf("historical E03 not acquired, adds=%+v result=%+v", f.client.adds, r)
	}
}

func TestUnknownInventoryNeverRunsHistoricalSearch(t *testing.T) {
	f := newFixture(t, map[string]string{"f": "feed"})
	w, err := catalog.CreateManagedAt(f.src, f.lib, "abcabc146", 2026, 1, catalog.Declaration{Sources: []string{"f"}})
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(w.Path, "Season 01", "unparseable episode.mkv"), "video")
	h := &fakeHistory{entries: mustReleases(t, "generic", []rss.Entry{entry("[G] abcabc146 S01E03 1080p", "h3")})}
	app, err := Open(Options{ConfigRoot: f.cfg, Fetcher: fakeFetcher{}, History: h, Backends: map[string]download.Backend{"c": f.client}})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = app.Cycle(context.Background(), CycleOptions{Acquire: true})
	if len(h.calls) != 0 || len(f.client.adds) != 0 {
		t.Fatalf("history=%d adds=%d", len(h.calls), len(f.client.adds))
	}
}

func TestMikanAndGenericRSSSourcesShareCandidateFilterPipeline(t *testing.T) {
	f := newFixture(t, map[string]string{"generic": "generic", "mikan": "mikan"})
	mustWrite(t, filepath.Join(f.cfg, "sources/mikan.json"), `{"id":"mikan","provider":"mikan","enabled":true}`)
	groups := []string{"grpabc166"}
	_, err := catalog.CreateManagedAt(f.src, f.lib, "abcabc146", 2026, 1, catalog.Declaration{Sources: []string{"generic", "mikan"}, Filters: releasefilter.Filters{Groups: groups}})
	if err != nil {
		t.Fatal(err)
	}
	app := service(t, f, fakeFetcher{entries: map[string][]rss.Entry{
		"generic":                         {entry("[Bad] abcabc146 E01 1080p", "generic-bad")},
		"https://mikanani.me/RSS/Classic": {entry("[grpabc166] abcabc146 E02 1080p", "mikan-good")},
	}})
	r, err := app.Cycle(context.Background(), CycleOptions{Acquire: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.client.adds) != 1 || f.client.adds[0].URL != "https://example.test/mikan-good.torrent" {
		t.Fatalf("adds=%+v result=%+v", f.client.adds, r)
	}
}

func TestMigrationPlansGroupByEntryInsteadOfTorrent(t *testing.T) {
	entryKey := "series/Test"
	bundle := configstore.Bundle{Entries: map[string]catalog.Entry{entryKey: {Key: entryKey, Title: "abcabc034"}}}
	plans := []migration.Plan{
		{Client: "c", TaskID: "one", TaskName: "abcabc034 - 01", Decision: migration.DecisionRelocate, EntryKey: entryKey, SourceEpisode: migration.Target{Season: 2}},
		{Client: "c", TaskID: "two", TaskName: "abcabc034 - 02", Decision: migration.DecisionRelocate, EntryKey: entryKey, SourceEpisode: migration.Target{Season: 2}},
	}
	groups := groupMigrationPlans(bundle, plans)
	if len(groups) != 1 || groups[0].Title != "abcabc034" || groups[0].Season != 2 || len(groups[0].Plans) != 2 || groups[0].Ready != 2 {
		t.Fatalf("groups=%+v", groups)
	}
}

func TestMigrationConfirmationChangesWithExactTaskSet(t *testing.T) {
	plans := []migration.Plan{{Client: "qbit", TaskID: "one", Title: "abcabc146", Season: 1, Decision: migration.DecisionRelocate}}
	first := groupMigrationPlans(configstore.Bundle{}, plans)
	plans = append(plans, migration.Plan{Client: "qbit", TaskID: "two", Title: "abcabc146", Season: 1, Decision: migration.DecisionRelocate})
	second := groupMigrationPlans(configstore.Bundle{}, plans)
	if len(first) != 1 || len(second) != 1 || first[0].Key == second[0].Key {
		t.Fatalf("confirmation did not bind task set: first=%+v second=%+v", first, second)
	}
}

func TestMigrationPlansNeverMergeDifferentSeasonsOfOneSeries(t *testing.T) {
	plans := []migration.Plan{
		{Client: "qbit", TaskID: "s1", Title: "abcabc146", Season: 1, Decision: migration.DecisionUnknown},
		{Client: "qbit", TaskID: "s2", Title: "abcabc146", Season: 2, Decision: migration.DecisionUnknown},
	}
	groups := groupMigrationPlans(configstore.Bundle{}, plans)
	if len(groups) != 2 || groups[0].Season == groups[1].Season {
		t.Fatalf("groups=%+v", groups)
	}
}

func TestMigrationPlansStripReleaseTagsAndEpisodeSuffixBeforeGrouping(t *testing.T) {
	plans := []migration.Plan{
		{Client: "qbit", TaskID: "09", TaskName: "甲乙丙丁戊己 - 09 [1080P][metaabc][WEB-DL][AAC AVC][ASS]", SavePath: "/media/downloads/anime", Decision: migration.DecisionUnknown},
		{Client: "qbit", TaskID: "10", TaskName: "甲乙丙丁戊己 - 10v2 [1080P][metaabc][WEB-DL][AAC AVC][SRT]", SavePath: "/media/downloads/anime", Decision: migration.DecisionUnknown},
		{Client: "qbit", TaskID: "11", TaskName: "甲乙丙丁戊己 - 11 [1080P][metaabc][WEB-DL][AAC AVC][CHT]", SavePath: "/media/downloads/anime", Decision: migration.DecisionUnknown},
	}
	groups := groupMigrationPlans(configstore.Bundle{}, plans)
	if len(groups) != 1 || groups[0].Title != "甲乙丙丁戊己" || len(groups[0].Plans) != 3 {
		t.Fatalf("groups=%+v", groups)
	}
}

func TestCycleAutoAdoptsStructuredLocalSeriesAndPublishesWithoutDownloader(t *testing.T) {
	f := newFixture(t, nil)
	container := filepath.Join(f.src, "abcabc011 (2026)", "whatever-season-container")
	if err := os.MkdirAll(container, 0o755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(container, "abcabc011 S01E04.mkv")
	if err := os.WriteFile(source, []byte("media"), 0o644); err != nil {
		t.Fatal(err)
	}
	app := service(t, f, fakeFetcher{})
	result, err := app.Cycle(context.Background(), CycleOptions{Reconcile: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.CreatedEntries) != 1 || result.CreatedEntries[0] != "series/abcabc011 (2026)" {
		t.Fatalf("created=%v", result.CreatedEntries)
	}
	target := filepath.Join(f.lib, "abcabc011 (2026)", "Season 01", "abcabc011 S01E04.mkv")
	srcInfo, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	dstInfo, err := os.Stat(target)
	if err != nil {
		t.Fatalf("publication missing: %v; result=%+v", err, result)
	}
	if !os.SameFile(srcInfo, dstInfo) {
		t.Fatal("auto-adopted publication is not a hardlink")
	}
	if len(f.client.tasks) != 0 {
		t.Fatal("local adoption unexpectedly created downloader work")
	}
}

func TestCycleAutoAdoptsWeakEpisodeNamesFromCanonicalSeasonDirectory(t *testing.T) {
	f := newFixture(t, nil)
	container := filepath.Join(f.src, "abcabc022 (2026)", "Season 01")
	if err := os.MkdirAll(container, 0o755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(container, "[grpabc165] abcabc022 - 01 [1080P].mkv")
	if err := os.WriteFile(source, []byte("media"), 0o644); err != nil {
		t.Fatal(err)
	}
	app := service(t, f, fakeFetcher{})
	result, err := app.Cycle(context.Background(), CycleOptions{Reconcile: true})
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(f.lib, "abcabc022 (2026)", "Season 01", "abcabc022 S01E01.mkv")
	a, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.Stat(target)
	if err != nil {
		t.Fatalf("publication missing: %v; result=%+v", err, result)
	}
	if !os.SameFile(a, b) {
		t.Fatal("publication is not a hardlink")
	}
}

func TestCycleBestEffortAdoptsUnlabeledSingleSeasonContainer(t *testing.T) {
	f := newFixture(t, nil)
	container := filepath.Join(f.src, "abcabc053", "random-release-folder")
	if err := os.MkdirAll(container, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, ep := range []string{"01", "02"} {
		if err := os.WriteFile(filepath.Join(container, "abcabc053 - "+ep+".mkv"), []byte("media"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	app := service(t, f, fakeFetcher{})
	result, err := app.Cycle(context.Background(), CycleOptions{Reconcile: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.src, "abcabc053", ".aninode.json")); err != nil {
		t.Fatalf("declaration missing: %v", err)
	}
	for _, ep := range []string{"01", "02"} {
		if _, err := os.Stat(filepath.Join(f.lib, "abcabc053", "Season 01", "abcabc053 S01E"+ep+".mkv")); err != nil {
			t.Fatalf("episode %s publication missing: %v result=%+v", ep, err, result)
		}
	}
}

func TestCycleAutoAdoptsStructuredLocalMovieAndPublishesWithoutDownloader(t *testing.T) {
	f := newFixture(t, nil)
	movieRoot := filepath.Join(f.root, "downloads", "Movies", "abcabc006 (2026)")
	if err := os.MkdirAll(movieRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(movieRoot, "abcabc006.mkv")
	if err := os.WriteFile(source, []byte("movie-media"), 0o644); err != nil {
		t.Fatal(err)
	}
	app := service(t, f, fakeFetcher{})
	result, err := app.Cycle(context.Background(), CycleOptions{Reconcile: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.CreatedEntries) != 1 || result.CreatedEntries[0] != "movie/abcabc006 (2026)" {
		t.Fatalf("created=%v", result.CreatedEntries)
	}
	if _, err := os.Stat(filepath.Join(movieRoot, ".aninode.json")); err != nil {
		t.Fatalf("movie declaration missing: %v", err)
	}
	target := filepath.Join(f.root, "library", "Movies", "abcabc006 (2026)", "abcabc006 (2026).mkv")
	srcInfo, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	dstInfo, err := os.Stat(target)
	if err != nil {
		t.Fatalf("movie publication missing: %v; result=%+v", err, result)
	}
	if !os.SameFile(srcInfo, dstInfo) {
		t.Fatal("auto-adopted movie publication is not a hardlink")
	}
	if len(f.client.tasks) != 0 {
		t.Fatal("local movie adoption unexpectedly created downloader work")
	}
}

func TestMoviePublicationConvergesAfterDeclarationTitleCorrection(t *testing.T) {
	f := newFixture(t, nil)
	movieRoot := filepath.Join(f.root, "downloads", "Movies", "abcabc057 (2026)")
	if err := os.MkdirAll(movieRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(movieRoot, "abcabc057.mkv")
	if err := os.WriteFile(source, []byte("movie-media"), 0o644); err != nil {
		t.Fatal(err)
	}
	app := service(t, f, fakeFetcher{})
	if _, err := app.Cycle(context.Background(), CycleOptions{Reconcile: true}); err != nil {
		t.Fatal(err)
	}
	oldDir := filepath.Join(f.root, "library", "Movies", "abcabc057 (2026)")
	oldTarget := filepath.Join(oldDir, "abcabc057 (2026).mkv")
	if _, err := os.Stat(oldTarget); err != nil {
		t.Fatalf("initial publication missing: %v", err)
	}
	declaration, err := catalog.ReadDeclaration(movieRoot)
	if err != nil || declaration == nil {
		t.Fatalf("read declaration=%+v err=%v", declaration, err)
	}
	declaration.Title = "abcabc025"
	if err := catalog.WriteDeclaration(movieRoot, *declaration); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Cycle(context.Background(), CycleOptions{Reconcile: true}); err != nil {
		t.Fatal(err)
	}
	newTarget := filepath.Join(f.root, "library", "Movies", "abcabc025 (2026)", "abcabc025 (2026).mkv")
	srcInfo, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	newInfo, err := os.Stat(newTarget)
	if err != nil {
		t.Fatalf("corrected publication missing: %v", err)
	}
	if !os.SameFile(srcInfo, newInfo) {
		t.Fatal("corrected publication is not the source hardlink")
	}
	if _, err := os.Lstat(oldTarget); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale publication still exists: %v", err)
	}
	if _, err := os.Lstat(oldDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("empty stale movie directory still exists: %v", err)
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatalf("source was modified during publication convergence: %v", err)
	}
}

func TestSeriesPublicationConvergesAfterDeclarationTitleCorrection(t *testing.T) {
	f := newFixture(t, nil)
	container := filepath.Join(f.src, "abcabc062 (2026)", "season-container")
	if err := os.MkdirAll(container, 0o755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(container, "abcabc062 S01E01.mkv")
	if err := os.WriteFile(source, []byte("episode"), 0o644); err != nil {
		t.Fatal(err)
	}
	app := service(t, f, fakeFetcher{})
	if _, err := app.Cycle(context.Background(), CycleOptions{Reconcile: true}); err != nil {
		t.Fatal(err)
	}
	entryRoot := filepath.Dir(container)
	oldDir := filepath.Join(f.root, "library", "TV", "abcabc062 (2026)")
	oldTarget := filepath.Join(oldDir, "Season 01", "abcabc062 S01E01.mkv")
	if _, err := os.Stat(oldTarget); err != nil {
		t.Fatalf("initial series publication missing: %v", err)
	}
	declaration, err := catalog.ReadDeclaration(entryRoot)
	if err != nil || declaration == nil {
		t.Fatalf("read declaration=%+v err=%v", declaration, err)
	}
	declaration.Title = "abcabc032"
	if err := catalog.WriteDeclaration(entryRoot, *declaration); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Cycle(context.Background(), CycleOptions{Reconcile: true}); err != nil {
		t.Fatal(err)
	}
	newTarget := filepath.Join(f.root, "library", "TV", "abcabc032 (2026)", "Season 01", "abcabc032 S01E01.mkv")
	srcInfo, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	newInfo, err := os.Stat(newTarget)
	if err != nil {
		t.Fatalf("corrected series publication missing: %v", err)
	}
	if !os.SameFile(srcInfo, newInfo) {
		t.Fatal("corrected series publication is not the source hardlink")
	}
	if _, err := os.Lstat(oldTarget); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale series publication still exists: %v", err)
	}
	if _, err := os.Lstat(oldDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("empty stale series directory still exists: %v", err)
	}
}

func TestBestEffortFolderProjectionPersistsAndConvergesLibrary(t *testing.T) {
	f := newFixture(t, map[string]string{})
	container := filepath.Join(f.src, "abcabc003 (2026)", "weird cour folder")
	source := filepath.Join(container, "abcabc003 - 13.mkv")
	mustWrite(t, source, "media")
	app := service(t, f, fakeFetcher{})
	if _, err := app.Cycle(context.Background(), CycleOptions{Reconcile: true}); err != nil {
		t.Fatal(err)
	}
	entries, err := catalog.DiscoverAt(f.src, f.lib)
	if err != nil {
		t.Fatal(err)
	}
	var w catalog.Entry
	for _, e := range entries {
		if e.Title == "abcabc003" {
			w = e
			break
		}
	}
	if w.Key == "" {
		t.Fatalf("entry not adopted: %+v", entries)
	}
	d, err := catalog.ReadDeclaration(w.DeclarationPath)
	if err != nil || d == nil {
		t.Fatalf("declaration=%+v err=%v", d, err)
	}
	if got := d.Folders["weird cour folder"]; got.Season != 1 {
		t.Fatalf("guessed folder projection=%+v declaration=%+v", got, d)
	}
	oldTarget := filepath.Join(f.lib, "abcabc003 (2026)", "Season 01", "abcabc003 S01E13.mkv")
	if _, err := os.Stat(oldTarget); err != nil {
		t.Fatalf("initial projection missing: %v", err)
	}
	if _, err := app.PutFolderProjection(context.Background(), w.Key, "weird cour folder", catalog.FolderProjection{Season: 2, EpisodeOffset: -12}); err != nil {
		t.Fatal(err)
	}
	newTarget := filepath.Join(f.lib, "abcabc003 (2026)", "Season 02", "abcabc003 S02E01.mkv")
	a, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.Stat(newTarget)
	if err != nil {
		t.Fatalf("projected target missing: %v", err)
	}
	if !os.SameFile(a, b) {
		t.Fatal("projected target is not source hardlink")
	}
	if _, err := os.Stat(oldTarget); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old publication path still exists: %v", err)
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatalf("source was mutated: %v", err)
	}
}

func TestSeriesWeakDirectChildRepublishesAfterDeclarationTitleCorrection(t *testing.T) {
	f := newFixture(t, nil)
	container := filepath.Join(f.src, "abcabc024 (2026)", "Season 01")
	if err := os.MkdirAll(container, 0o755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(container, "abcabc024 - 01.mkv")
	if err := os.WriteFile(source, []byte("episode"), 0o644); err != nil {
		t.Fatal(err)
	}
	app := service(t, f, fakeFetcher{})
	if _, err := app.Cycle(context.Background(), CycleOptions{Reconcile: true}); err != nil {
		t.Fatal(err)
	}
	entryRoot := filepath.Dir(container)
	oldTarget := filepath.Join(f.lib, "abcabc024 (2026)", "Season 01", "abcabc024 S01E01.mkv")
	if _, err := os.Stat(oldTarget); err != nil {
		t.Fatalf("initial weak-name publication missing: %v", err)
	}
	d, err := catalog.ReadDeclaration(entryRoot)
	if err != nil || d == nil {
		t.Fatalf("declaration=%+v err=%v", d, err)
	}
	d.Title = "abcabc005"
	if err := catalog.WriteDeclaration(entryRoot, *d); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Cycle(context.Background(), CycleOptions{Reconcile: true}); err != nil {
		t.Fatal(err)
	}
	newTarget := filepath.Join(f.lib, "abcabc005 (2026)", "Season 01", "abcabc005 S01E01.mkv")
	a, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.Stat(newTarget)
	if err != nil {
		t.Fatalf("corrected weak-name publication missing: %v", err)
	}
	if !os.SameFile(a, b) {
		t.Fatal("corrected weak-name publication is not source hardlink")
	}
	if _, err := os.Lstat(oldTarget); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale weak-name publication still exists: %v", err)
	}
}

func TestSeriesFolderProjectionPublishesToSpecialsSeason(t *testing.T) {
	f := newFixture(t, nil)
	container := filepath.Join(f.src, "abcabc042 (2026)", "Extras")
	if err := os.MkdirAll(container, 0o755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(container, "abcabc042 - 01.mkv")
	if err := os.WriteFile(source, []byte("special"), 0o644); err != nil {
		t.Fatal(err)
	}
	entryRoot := filepath.Dir(container)
	if err := catalog.WriteDeclaration(entryRoot, catalog.Declaration{
		Title: "abcabc042",
		Year:  2026,
		Folders: map[string]catalog.FolderProjection{
			"Extras": {Season: 0},
		},
	}); err != nil {
		t.Fatal(err)
	}
	app := service(t, f, fakeFetcher{})
	if _, err := app.Cycle(context.Background(), CycleOptions{Reconcile: true}); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(f.lib, "abcabc042 (2026)", "Season 00", "abcabc042 S00E01.mkv")
	a, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.Stat(target)
	if err != nil {
		t.Fatalf("specials publication missing: %v", err)
	}
	if !os.SameFile(a, b) {
		t.Fatal("specials publication is not source hardlink")
	}
}

func TestSeriesBlacklistRemovesExistingPublication(t *testing.T) {
	f := newFixture(t, nil)
	container := filepath.Join(f.src, "abcabc017 (2026)", "Season 01")
	if err := os.MkdirAll(container, 0o755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(container, "abcabc017 S01E01.mkv")
	if err := os.WriteFile(source, []byte("episode"), 0o644); err != nil {
		t.Fatal(err)
	}
	app := service(t, f, fakeFetcher{})
	if _, err := app.Cycle(context.Background(), CycleOptions{Reconcile: true}); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(f.lib, "abcabc017 (2026)", "Season 01", "abcabc017 S01E01.mkv")
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("initial publication missing: %v", err)
	}
	entryRoot := filepath.Dir(container)
	d, err := catalog.ReadDeclaration(entryRoot)
	if err != nil || d == nil {
		t.Fatalf("declaration=%+v err=%v", d, err)
	}
	d.Blacklist = []string{"*S01E01*"}
	if err := catalog.WriteDeclaration(entryRoot, *d); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Cycle(context.Background(), CycleOptions{Reconcile: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("blacklisted publication still exists: %v", err)
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatalf("blacklist convergence modified source: %v", err)
	}
}

func TestEnsureSourceDeclarationsIsImmediateAndRestorative(t *testing.T) {
	f := newFixture(t, nil)
	entryRoot := filepath.Join(f.src, "abcabc020 (2026)")
	if err := os.MkdirAll(filepath.Join(entryRoot, "Season 01"), 0o755); err != nil {
		t.Fatal(err)
	}
	app := service(t, f, fakeFetcher{})
	for pass := 0; pass < 2; pass++ {
		created, err := app.EnsureSourceDeclarations(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(created) != 1 || created[0] != "series/abcabc020 (2026)" {
			t.Fatalf("pass %d created=%v", pass, created)
		}
		if _, err := os.Stat(filepath.Join(entryRoot, ".aninode.json")); err != nil {
			t.Fatalf("pass %d declaration missing: %v", pass, err)
		}
		if err := os.Remove(filepath.Join(entryRoot, ".aninode.json")); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRSSDiscoveryUsesCurrentFilesystemNamesWithoutPersistingThem(t *testing.T) {
	f := newFixture(t, map[string]string{"f": "bridge-existing"})
	w, err := catalog.CreateManagedAt(f.src, f.lib, "Canonical", 0, 1, catalog.Declaration{})
	if err != nil {
		t.Fatal(err)
	}
	fetch := fakeFetcher{entries: map[string][]rss.Entry{"bridge-existing": {{Title: "[grpabc165] Tracker Facing Name - 02", GUID: "rss-2", DownloadURL: "magnet:?xt=urn:btih:9123456789abcdef0123456789abcdef01234567&dn=%5BGroup%5D+Tracker+Facing+Name+-+02"}}}}
	app := service(t, f, fetch)
	if err := app.RefreshRSS(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := app.RSSDiscoveries(context.Background(), "", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Groups) != 1 || got.Groups[0].MediaType != catalog.MediaSeries || got.Groups[0].Title != "Tracker Facing Name" {
		t.Fatalf("unmatched series should be discoverable before filesystem evidence: %+v", got.Groups)
	}
	// Keep the RSS snapshot unchanged: only new filesystem naming evidence
	// should make the release resolve to the existing Entry on the next read.
	mustWrite(t, filepath.Join(w.DeclarationPath, "Season 01", "[grpabc165] Tracker Facing Name - 01.mkv"), "one")
	got, err = app.RSSDiscoveries(context.Background(), "", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Groups) != 0 {
		t.Fatalf("filesystem-derived name should resolve existing Entry: %+v", got.Groups)
	}
	data, err := os.ReadFile(filepath.Join(w.DeclarationPath, ".aninode.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "Tracker Facing Name") {
		t.Fatalf("observed name was persisted: %s", data)
	}
}

func TestAutomaticBackfillRepairsFilesystemInternalGaps(t *testing.T) {
	f := newFixture(t, map[string]string{"f": "feed"})
	w, err := catalog.CreateManagedAt(f.src, f.lib, "abcabc064", 2026, 1, catalog.Declaration{Sources: []string{"f"}})
	if err != nil {
		t.Fatal(err)
	}
	season := filepath.Join(w.Path, "Season 01")
	for _, ep := range []int{1, 2, 3, 6} {
		mustWrite(t, filepath.Join(season, fmt.Sprintf("abcabc064 S01E%02d.mkv", ep)), fmt.Sprint(ep))
	}
	history := &fakeHistory{entries: mustReleases(t, "generic", []rss.Entry{
		entry("[G] abcabc064 S01E04 1080p", "hist4"), entry("[G] abcabc064 S01E05 1080p", "hist5"),
	})}
	app, err := Open(Options{ConfigRoot: f.cfg, Fetcher: fakeFetcher{}, History: history, Backends: map[string]download.Backend{"c": f.client}})
	if err != nil {
		t.Fatal(err)
	}
	r, err := app.Cycle(context.Background(), CycleOptions{Acquire: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(history.calls) != 1 {
		t.Fatalf("history calls=%d", len(history.calls))
	}
	got := history.calls[0].SourceMissing
	if len(got) != 2 || got[0].EpisodeStart != 4 || got[1].EpisodeStart != 5 {
		t.Fatalf("filesystem gaps=%+v", got)
	}
	found4, found5 := false, false
	for _, add := range f.client.adds {
		if add.URL == "https://example.test/hist4.torrent" {
			found4 = true
		}
		if add.URL == "https://example.test/hist5.torrent" {
			found5 = true
		}
	}
	if !found4 || !found5 || len(r.Acquisitions) < 2 {
		t.Fatalf("adds=%+v acquisitions=%+v", f.client.adds, r.Acquisitions)
	}
}

func TestLocalGapRepairBackfillsMiddleHoleWithoutRSSCycle(t *testing.T) {
	f := newFixture(t, map[string]string{"f": "feed"})
	w, err := catalog.CreateManagedAt(f.src, f.lib, "abcabc021", 2026, 1, catalog.Declaration{Sources: []string{"f"}})
	if err != nil {
		t.Fatal(err)
	}
	season := filepath.Join(w.Path, "Season 01")
	for _, ep := range []int{3, 4, 5, 7} {
		mustWrite(t, filepath.Join(season, fmt.Sprintf("abcabc021 S01E%02d.mkv", ep)), fmt.Sprint(ep))
	}
	history := &fakeHistory{entries: mustReleases(t, "generic", []rss.Entry{
		entry("[G] abcabc021 S01E06 1080p", "hist6"),
		entry("[G] abcabc021 S01E01 1080p", "hist1"),
		entry("[G] abcabc021 S01E02 1080p", "hist2"),
	})}
	app, err := Open(Options{ConfigRoot: f.cfg, Fetcher: fakeFetcher{}, History: history, Backends: map[string]download.Backend{"c": f.client}})
	if err != nil {
		t.Fatal(err)
	}
	r, err := app.Cycle(context.Background(), CycleOptions{LocalOnly: true, Acquire: true, RepairGaps: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(history.calls) != 1 {
		t.Fatalf("history calls=%d", len(history.calls))
	}
	got := history.calls[0].SourceMissing
	if len(got) != 1 || got[0].EpisodeStart != 6 || got[0].EpisodeEnd != 6 {
		t.Fatalf("filesystem gaps=%+v", got)
	}
	if len(f.client.adds) != 1 || f.client.adds[0].URL != "https://example.test/hist6.torrent" {
		t.Fatalf("adds=%+v", f.client.adds)
	}
	if len(r.Acquisitions) != 1 {
		t.Fatalf("acquisitions=%+v", r.Acquisitions)
	}
}

func TestManualSeasonCompletionDiscoversAndRepairsMissingTail(t *testing.T) {
	f := newFixture(t, map[string]string{"f": "feed"})
	w, err := catalog.CreateManagedAt(f.src, f.lib, "abcabc061", 2026, 1, catalog.Declaration{Sources: []string{"f"}})
	if err != nil {
		t.Fatal(err)
	}
	season := filepath.Join(w.Path, "Season 01")
	for _, ep := range []int{1, 2, 3} {
		mustWrite(t, filepath.Join(season, fmt.Sprintf("abcabc061 S01E%02d.mkv", ep)), fmt.Sprint(ep))
	}
	history := &fakeHistory{entries: mustReleases(t, "generic", []rss.Entry{
		entry("[G] abcabc061 S01E01 1080p", "hist1"),
		entry("[G] abcabc061 S01E02 1080p", "hist2"),
		entry("[G] abcabc061 S01E03 1080p", "hist3"),
		entry("[G] abcabc061 S01E04 1080p", "hist4"),
		entry("[G] abcabc061 S01E05 1080p", "hist5"),
	})}
	app, err := Open(Options{ConfigRoot: f.cfg, Fetcher: fakeFetcher{}, History: history, Backends: map[string]download.Backend{"c": f.client}})
	if err != nil {
		t.Fatal(err)
	}
	out, err := app.Backfill(context.Background(), w.Key, 1, 1, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(history.calls) != 1 || len(history.calls[0].SourceMissing) != 2 {
		t.Fatalf("manual history request=%+v", history.calls)
	}
	if len(out.State.Missing) != 2 || out.State.Missing[0] != 4 || out.State.Missing[1] != 5 {
		t.Fatalf("manual completion state=%+v", out.State)
	}
	if len(f.client.adds) != 2 || len(out.Acquisitions) != 2 {
		t.Fatalf("manual tail repair adds=%+v acquisitions=%+v", f.client.adds, out.Acquisitions)
	}
}

func TestManualSeasonCompletionReportsSupportedProviderFailure(t *testing.T) {
	f := newFixture(t, map[string]string{"f": "feed"})
	w, err := catalog.CreateManagedAt(f.src, f.lib, "abcabc043", 2026, 1, catalog.Declaration{Sources: []string{"f"}})
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(w.Path, "Season 01", "abcabc043 S01E01.mkv"), "one")
	history := &fakeHistory{err: context.DeadlineExceeded}
	app, err := Open(Options{ConfigRoot: f.cfg, Fetcher: fakeFetcher{}, History: history, Backends: map[string]download.Backend{"c": f.client}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = app.Backfill(context.Background(), w.Key, 1, 1, 5)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Backfill error=%v", err)
	}
	if errors.Is(err, backfill.ErrUnsupported) || strings.Contains(err.Error(), backfill.ErrUnsupported.Error()) {
		t.Fatalf("supported provider failure mislabeled unsupported: %v", err)
	}
}

func TestSingleEnabledClientIsImplicitDefaultForAcquisition(t *testing.T) {
	f := newFixture(t, map[string]string{"f": "feed"})
	w, err := catalog.CreateManagedAt(f.src, f.lib, "Implicit Client", 2026, 1, catalog.Declaration{Sources: []string{"f"}})
	if err != nil {
		t.Fatal(err)
	}
	_ = w
	fetch := fakeFetcher{entries: map[string][]rss.Entry{"feed": {entry("[G] Implicit Client S01E01 1080p", "current1")}}}
	app, err := Open(Options{ConfigRoot: f.cfg, Fetcher: fetch, Backends: map[string]download.Backend{"c": f.client}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := app.Cycle(context.Background(), CycleOptions{Acquire: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.client.adds) != 1 || len(result.Acquisitions) != 1 || result.Acquisitions[0].Client != "c" || result.Acquisitions[0].Decision != "submitted" {
		t.Fatalf("adds=%+v acquisitions=%+v", f.client.adds, result.Acquisitions)
	}
}

type nameSearchProvider struct{ queries []string }

func (p *nameSearchProvider) Name() string { return "mikan" }
func (p *nameSearchProvider) Capabilities() provider.Capability {
	return provider.Capability{Search: true, Historical: true}
}
func (p *nameSearchProvider) Poll(context.Context, string) ([]release.Release, error) {
	return nil, nil
}
func (p *nameSearchProvider) Search(_ context.Context, req provider.SearchRequest) ([]release.Release, error) {
	p.queries = append(p.queries, req.Query)
	switch req.Query {
	case "Canonical Title":
		return []release.Release{{InfoHash: "2123456789abcdef0123456789abcdef01234567", MediaName: "[A] Canonical Title S01E03", Components: medianame.Components{Title: "Canonical Title", Season: 1, EpisodeStart: 3, EpisodeEvidence: "explicit"}}}, nil
	case "abcabc124":
		return []release.Release{{InfoHash: "1123456789abcdef0123456789abcdef01234567", MediaName: "[B] abcabc124 S01E03", Components: medianame.Components{Title: "abcabc124", Season: 1, EpisodeStart: 3, EpisodeEvidence: "explicit"}}}, nil
	default:
		return nil, nil
	}
}

func TestProductionBackfillSearchUnionsPlannedNames(t *testing.T) {
	p := &nameSearchProvider{}
	a := &App{}
	rt := &runtimeSnapshot{sources: map[string]provider.Source{"mikan": {Config: configstore.ContentSource{ID: "mikan", Provider: "mikan", Enabled: true}, Provider: p}}}
	values, err := a.searchBackfillSource(context.Background(), rt, "mikan", backfill.Request{Queries: []backfill.QueryEvidence{{Title: "Canonical Title", Origin: "entry_title", Tier: backfill.QueryFallback}, {Title: "abcabc124", Origin: "entry_title", Tier: backfill.QueryFallback}}, SourceMissing: []episode.Key{{EntryKey: "series/x", Season: 1, EpisodeStart: 3, EpisodeEnd: 3}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 2 || len(p.queries) != 2 || p.queries[0] != "Canonical Title" || p.queries[1] != "abcabc124" {
		t.Fatalf("all recall queries must be unioned; queries=%q values=%+v", p.queries, values)
	}
}

func TestManualSeasonCompletionTreatsPartialHistoryFailureAsWarning(t *testing.T) {
	f := newFixture(t, map[string]string{"f": "feed"})
	w, err := catalog.CreateManagedAt(f.src, f.lib, "abcabc122", 2026, 1, catalog.Declaration{Sources: []string{"f"}})
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(w.Path, "Season 01", "abcabc122 S01E01.mkv"), "one")
	history := &fakeHistory{entries: mustReleases(t, "generic", []rss.Entry{
		entry("[G] abcabc122 S01E01 1080p", "hist1"),
		entry("[G] abcabc122 S01E02 1080p", "hist2"),
	}), err: context.DeadlineExceeded}
	app, err := Open(Options{ConfigRoot: f.cfg, Fetcher: fakeFetcher{}, History: history, Backends: map[string]download.Backend{"c": f.client}})
	if err != nil {
		t.Fatal(err)
	}
	out, err := app.Backfill(context.Background(), w.Key, 1, 1, 5)
	if err != nil {
		t.Fatalf("partial historical results should remain usable: %v", err)
	}
	if len(out.Issues) == 0 || !strings.Contains(out.Issues[0].Message, "deadline exceeded") {
		t.Fatalf("partial failure was not surfaced as issue: %+v", out.Issues)
	}
	if len(out.Acquisitions) != 1 || len(f.client.adds) != 1 {
		t.Fatalf("partial historical results were not acquired: acquisitions=%+v adds=%+v", out.Acquisitions, f.client.adds)
	}
}

type targetedFallbackProvider struct{ requests []provider.SearchRequest }

func (p *targetedFallbackProvider) Name() string { return "dmhy" }
func (p *targetedFallbackProvider) Capabilities() provider.Capability {
	return provider.Capability{Search: true, Historical: true}
}
func (p *targetedFallbackProvider) Poll(context.Context, string) ([]release.Release, error) {
	return nil, nil
}
func (p *targetedFallbackProvider) Search(_ context.Context, req provider.SearchRequest) ([]release.Release, error) {
	p.requests = append(p.requests, req)
	if req.EpisodeStart == 3 {
		return []release.Release{{InfoHash: "3123456789abcdef0123456789abcdef01234567", MediaName: "abcabc146 S01E03", Components: medianame.Components{Title: "abcabc146", Season: 1, EpisodeStart: 3, EpisodeEvidence: "E03"}}}, nil
	}
	return []release.Release{{InfoHash: "a123456789abcdef0123456789abcdef01234567", MediaName: "abcabc146 S01E10", Components: medianame.Components{Title: "abcabc146", Season: 1, EpisodeStart: 10, EpisodeEvidence: "E10"}}}, nil
}

func TestProductionBackfillSearchesExactMissingEpisodeOnly(t *testing.T) {
	p := &targetedFallbackProvider{}
	a := &App{}
	rt := &runtimeSnapshot{sources: map[string]provider.Source{"dmhy": {Config: configstore.ContentSource{ID: "dmhy", Provider: "dmhy", Enabled: true}, Provider: p}}}
	values, err := a.searchBackfillSource(context.Background(), rt, "dmhy", backfill.Request{Queries: []backfill.QueryEvidence{{Title: "abcabc146", Origin: "entry_title", Tier: backfill.QueryFallback}}, SourceMissing: []episode.Key{{EntryKey: "series/show", Season: 1, EpisodeStart: 3, EpisodeEnd: 3}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.requests) != 1 || p.requests[0].EpisodeStart != 3 || p.requests[0].EpisodeEnd != 3 {
		t.Fatalf("requests=%+v", p.requests)
	}
	if len(values) != 1 || values[0].Components.EpisodeStart != 3 {
		t.Fatalf("exact missing episode not recovered: %+v", values)
	}
}

type recallExpansionProvider struct {
	requests []provider.SearchRequest
	mode     string
}

func (p *recallExpansionProvider) Name() string { return "mikan" }
func (p *recallExpansionProvider) Capabilities() provider.Capability {
	return provider.Capability{Search: true, Historical: true}
}
func (p *recallExpansionProvider) Poll(context.Context, string) ([]release.Release, error) {
	return nil, nil
}
func (p *recallExpansionProvider) Search(_ context.Context, req provider.SearchRequest) ([]release.Release, error) {
	p.requests = append(p.requests, req)
	makeRelease := func(group, hash string) release.Release {
		return release.Release{
			MediaType:       "series",
			Provider:        "mikan",
			MediaName:       fmt.Sprintf("[%s] abcabc defdef ghighi - 06 [1080p].mkv", group),
			NormalizedTitle: "abcabc defdef ghighi",
			Group:           group,
			Resolution:      "1080p",
			InfoHash:        hash,
			Components: medianame.Components{
				Title:           "abcabc defdef ghighi",
				Season:          1,
				EpisodeStart:    6,
				EpisodeEnd:      6,
				EpisodeEvidence: "weak",
				ReleaseGroup:    group,
			},
		}
	}
	wrong := makeRelease("grpabc155", "6123456789abcdef0123456789abcdef01234567")
	wanted := makeRelease("grpabc154", "7123456789abcdef0123456789abcdef01234567")
	switch p.mode {
	case "broad-title":
		if req.EpisodeStart > 0 {
			return []release.Release{wrong}, nil
		}
		return []release.Release{wanted}, nil
	case "canonical-fallback":
		if req.Query == "abcabc defdef ghighi" {
			return []release.Release{wrong}, nil
		}
		if req.Query == "あいうえおかきく" {
			return []release.Release{wanted}, nil
		}
	}
	return nil, nil
}

func backfillRecallFixture(t *testing.T, p provider.Provider) (*App, *runtimeSnapshot, catalog.Entry) {
	t.Helper()
	root := t.TempDir()
	w, err := catalog.CreateManagedAt(filepath.Join(root, "source"), filepath.Join(root, "library"), "あいうえおかきく", 2026, 1, catalog.Declaration{Sources: []string{"mikan"}, Folders: map[string]catalog.FolderProjection{"Season 01": {Season: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	season := filepath.Join(w.DeclarationPath, "Season 01")
	for _, ep := range []int{3, 4, 5, 7} {
		mustWrite(t, filepath.Join(season, fmt.Sprintf("[grpabc154] abcabc defdef ghighi - %02d [WebRip 1080p HEVC-10bit AAC SRTx2].mkv", ep)), fmt.Sprint(ep))
	}
	cfg := configstore.ContentSource{ID: "mikan", Provider: "mikan", Enabled: true}
	rt := &runtimeSnapshot{
		bundle: configstore.Bundle{
			Sources: map[string]configstore.ContentSource{"mikan": cfg},
			Entries: map[string]catalog.Entry{w.Key: w},
		},
		sources: map[string]provider.Source{"mikan": {Config: cfg, Provider: p}},
	}
	return &App{}, rt, w
}

func TestBackfillExpandsToTitleOnlySearchWhenExactHitIsWrongStableGroup(t *testing.T) {
	p := &recallExpansionProvider{mode: "broad-title"}
	a, rt, w := backfillRecallFixture(t, p)
	values, err := a.searchBackfillSource(context.Background(), rt, "mikan", backfill.Request{
		EntryKey:      w.Key,
		Queries:       []backfill.QueryEvidence{{Title: "abcabc defdef ghighi", Origin: "source_media", Tier: backfill.QueryPrimary}},
		SourceMissing: []episode.Key{{EntryKey: w.Key, Season: 1, EpisodeStart: 6, EpisodeEnd: 6}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.requests) != 2 || p.requests[0].EpisodeStart != 6 || p.requests[1].EpisodeStart != 0 || len(p.requests[1].Targets) != 1 || p.requests[1].Targets[0].Episode != 6 {
		t.Fatalf("requests=%+v", p.requests)
	}
	found := false
	for _, value := range values {
		if value.Group == "grpabc154" && value.Components.EpisodeStart == 6 {
			found = true
		}
	}
	if !found {
		t.Fatalf("title-only recall did not recover same-group E06: %+v", values)
	}
}

func TestBackfillWrongGroupHitDoesNotSuppressCanonicalAliasFallback(t *testing.T) {
	p := &recallExpansionProvider{mode: "canonical-fallback"}
	a, rt, w := backfillRecallFixture(t, p)
	values, err := a.searchBackfillSource(context.Background(), rt, "mikan", backfill.Request{
		EntryKey: w.Key,
		Queries: []backfill.QueryEvidence{
			{Title: "abcabc defdef ghighi", Origin: "source_media", Tier: backfill.QueryPrimary},
			{Title: "あいうえおかきく", Origin: "entry_title", Tier: backfill.QueryFallback},
		},
		SourceMissing: []episode.Key{{EntryKey: w.Key, Season: 1, EpisodeStart: 6, EpisodeEnd: 6}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.requests) != 3 {
		t.Fatalf("requests=%+v", p.requests)
	}
	if p.requests[0].Query != "abcabc defdef ghighi" || p.requests[0].EpisodeStart != 6 || p.requests[1].Query != "abcabc defdef ghighi" || p.requests[1].EpisodeStart != 0 || p.requests[2].Query != "あいうえおかきく" || p.requests[2].EpisodeStart != 6 {
		t.Fatalf("unexpected staged recall: %+v", p.requests)
	}
	found := false
	for _, value := range values {
		if value.Group == "grpabc154" {
			found = true
		}
	}
	if !found {
		t.Fatalf("canonical alias fallback did not recover same-group E06: %+v", values)
	}
}

func TestBackfillSearchTitlesPreferObservedSourceReleaseTitle(t *testing.T) {
	w := catalog.Entry{
		Key:             "series/abcabc (2004)",
		MediaType:       catalog.MediaSeries,
		Title:           "abcabc",
		DeclarationPath: "/media/TV/abcabc (2004)",
	}
	graph := observation.Graph{Sources: map[string]filesystem.Snapshot{
		w.Key: {
			Paths: map[string]filesystem.ObjectID{
				"/media/TV/abcabc (2004)/Season 02/[ExampleGroup] abcabc defdef ghighi - 43 [1080p][JPSC].mp4": {},
				"/media/TV/abcabc (2004)/Season 02/[ExampleGroup] abcabc defdef ghighi - 46 [1080p][JPSC].mp4": {},
			},
		},
	}}
	queries := backfillSearchEvidence(w, graph, []string{".mkv", ".mp4"})
	if len(queries) < 3 || queries[0].Title != "abcabc defdef ghighi" || queries[0].Group != "ExampleGroup" || queries[1].Title != "abcabc defdef ghighi" || queries[1].Group != "" || queries[len(queries)-1].Title != "abcabc" {
		t.Fatalf("queries=%+v", queries)
	}
}

type tieredRecallProvider struct{ queries []string }

func (p *tieredRecallProvider) Name() string { return "mikan" }
func (p *tieredRecallProvider) Capabilities() provider.Capability {
	return provider.Capability{Search: true, Historical: true}
}
func (p *tieredRecallProvider) Poll(context.Context, string) ([]release.Release, error) {
	return nil, nil
}
func (p *tieredRecallProvider) Search(_ context.Context, req provider.SearchRequest) ([]release.Release, error) {
	p.queries = append(p.queries, req.Query)
	if strings.Contains(req.Query, "abcabc123") {
		return []release.Release{{InfoHash: fmt.Sprintf("%040d", len(p.queries)), MediaName: "[G] abcabc123 S01E07.mkv", Components: medianame.Components{Title: "abcabc123", Season: 1, EpisodeStart: 7, EpisodeEvidence: "explicit"}}}, nil
	}
	return nil, nil
}

func TestBackfillRunsAllFilesystemEvidenceBeforeSkippingCanonicalFallback(t *testing.T) {
	p := &tieredRecallProvider{}
	a := &App{}
	rt := &runtimeSnapshot{sources: map[string]provider.Source{"mikan": {Config: configstore.ContentSource{ID: "mikan", Provider: "mikan", Enabled: true}, Provider: p}}}
	values, err := a.searchBackfillSource(context.Background(), rt, "mikan", backfill.Request{
		Queries: []backfill.QueryEvidence{
			{Title: "abcabc123", Group: "grpabc163", Origin: "source_media", Tier: backfill.QueryPrimary},
			{Title: "abcabc123", Origin: "source_media", Tier: backfill.QueryPrimary},
			{Title: "abcabc126", Origin: "entry_title", Tier: backfill.QueryFallback},
		},
		SourceMissing: []episode.Key{{EntryKey: "series/x", Season: 1, EpisodeStart: 7, EpisodeEnd: 7}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.queries) != 2 || p.queries[0] != "[grpabc163] abcabc123" || p.queries[1] != "abcabc123" {
		t.Fatalf("queries=%q", p.queries)
	}
	if len(values) != 2 {
		t.Fatalf("source-evidence results were not unioned: %+v", values)
	}
}

func TestBackfillUsesCanonicalFallbackOnlyAfterFilesystemEvidenceMisses(t *testing.T) {
	p := &nameSearchProvider{}
	a := &App{}
	rt := &runtimeSnapshot{sources: map[string]provider.Source{"mikan": {Config: configstore.ContentSource{ID: "mikan", Provider: "mikan", Enabled: true}, Provider: p}}}
	values, err := a.searchBackfillSource(context.Background(), rt, "mikan", backfill.Request{
		Queries: []backfill.QueryEvidence{
			{Title: "abcabc191", Origin: "source_media", Tier: backfill.QueryPrimary},
			{Title: "abcabc124", Origin: "entry_title", Tier: backfill.QueryFallback},
		},
		SourceMissing: []episode.Key{{EntryKey: "series/x", Season: 1, EpisodeStart: 3, EpisodeEnd: 3}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.queries) != 3 || p.queries[0] != "abcabc191" || p.queries[1] != "abcabc191" || p.queries[2] != "abcabc124" {
		t.Fatalf("queries=%q", p.queries)
	}
	if len(values) != 1 || values[0].Components.EpisodeStart != 3 {
		t.Fatalf("values=%+v", values)
	}
}

func TestBackfillSelectionUsesProjectedTargetForSeasonlessAbsoluteEpisode(t *testing.T) {
	w := catalog.Entry{Key: "series/abcabc (2004)", MediaType: catalog.MediaSeries, Title: "abcabc", Seasons: []int{1, 2}}
	plan := candidate.Plan{Candidates: []candidate.Candidate{{
		EntryKey: w.Key,
		Decision: candidate.DecisionSelected,
		// A seasonless tracker filename such as "abcabc - 44" retains the
		// historical source-season default while live folder evidence projects the
		// publication target to Season 02.
		SourceEpisode: episode.Key{EntryKey: w.Key, Season: 1, EpisodeStart: 44, EpisodeEnd: 44},
		Episode:       episode.Key{EntryKey: w.Key, Season: 2, EpisodeStart: 44, EpisodeEnd: 44},
	}}}
	got, err := backfillSelection(plan, w, 2, []int{44, 45})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Candidates) != 1 || got.Candidates[0].Episode.Season != 2 || got.Candidates[0].Episode.EpisodeStart != 44 {
		t.Fatalf("projected target was rejected by redundant source identity check: %+v", got.Candidates)
	}
}

func TestBackfillSelectionAcceptsRangeCoveringMissingEpisode(t *testing.T) {
	w := catalog.Entry{Key: "series/abcabc146", MediaType: catalog.MediaSeries, Title: "abcabc146", Seasons: []int{1}}
	plan := candidate.Plan{Candidates: []candidate.Candidate{{
		EntryKey:      w.Key,
		Decision:      candidate.DecisionSelected,
		SourceEpisode: episode.Key{EntryKey: w.Key, Season: 1, EpisodeStart: 5, EpisodeEnd: 6},
		Episode:       episode.Key{EntryKey: w.Key, Season: 1, EpisodeStart: 5, EpisodeEnd: 6},
	}}}
	got, err := backfillSelection(plan, w, 1, []int{6})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Candidates) != 1 || !got.Candidates[0].Episode.Contains(6) {
		t.Fatalf("range candidate covering E06 was rejected: %+v", got.Candidates)
	}
}

func TestLocalGapRepairRequiresReviewForLowInformationCrossGroupFallback(t *testing.T) {
	f := newFixture(t, map[string]string{"f": "feed"})
	w, err := catalog.CreateManagedAt(f.src, f.lib, "abcabc014", 2026, 1, catalog.Declaration{Sources: []string{"f"}})
	if err != nil {
		t.Fatal(err)
	}
	sourceSeason := filepath.Join(w.DeclarationPath, "Season 01")
	librarySeason := filepath.Join(w.Path, "Season 01")
	for _, ep := range []int{3, 4, 5, 7} {
		sourcePath := filepath.Join(sourceSeason, fmt.Sprintf("[grpabc157] abcabc014 - %02d [WebRip 1080p HEVC-10bit AAC][CHT].mkv", ep))
		targetPath := filepath.Join(librarySeason, fmt.Sprintf("abcabc014 S01E%02d.mkv", ep))
		mustWrite(t, sourcePath, fmt.Sprint(ep))
		if err := os.Link(sourcePath, targetPath); err != nil {
			t.Fatal(err)
		}
	}
	history := &fakeHistory{entries: []release.Release{{
		MediaType:       "series",
		Provider:        "generic",
		MediaName:       "[grpabc158] abcabc014 - 06 [720p][CHS].mkv",
		NormalizedTitle: "abcabc014",
		Group:           "grpabc158",
		Resolution:      "720p",
		Subtitle:        "CHS",
		DownloadURL:     "https://example.test/continuity-e06.torrent",
		Components: medianame.Components{
			Title: "abcabc014", Season: 1, EpisodeStart: 6, EpisodeEnd: 6, EpisodeEvidence: "weak", ReleaseGroup: "grpabc158",
		},
	}}}
	app, err := Open(Options{ConfigRoot: f.cfg, Fetcher: fakeFetcher{}, History: history, Backends: map[string]download.Backend{"c": f.client}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := app.Cycle(context.Background(), CycleOptions{LocalOnly: true, Acquire: true, RepairGaps: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(history.calls) != 1 || len(history.calls[0].SourceMissing) != 1 || history.calls[0].SourceMissing[0].EpisodeStart != 6 {
		t.Fatalf("history calls=%+v", history.calls)
	}
	if len(f.client.adds) != 0 || len(result.Acquisitions) != 0 {
		t.Fatalf("automatic gap repair accepted a cross-group release with unknown source family: adds=%+v result=%+v", f.client.adds, result)
	}
	found := false
	for _, issue := range result.Issues {
		if issue.Stage == "backfill" && strings.Contains(issue.Message, "source family is unknown") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("missing manual-review continuity diagnostic: %+v", result.Issues)
	}
}

func TestLocalGapRepairRequiresConfirmationForBestCompatibleCrossGroupCandidate(t *testing.T) {
	f := newFixture(t, map[string]string{"f": "feed"})
	w, err := catalog.CreateManagedAt(f.src, f.lib, "abcabc014", 2026, 1, catalog.Declaration{Sources: []string{"f"}})
	if err != nil {
		t.Fatal(err)
	}
	sourceSeason := filepath.Join(w.DeclarationPath, "Season 01")
	librarySeason := filepath.Join(w.Path, "Season 01")
	for _, ep := range []int{3, 4, 5, 7} {
		sourcePath := filepath.Join(sourceSeason, fmt.Sprintf("[grpabc154] abcabc014 - %02d [WebRip 1080p HEVC-10bit AAC SRTx2].mkv", ep))
		targetPath := filepath.Join(librarySeason, fmt.Sprintf("abcabc014 S01E%02d.mkv", ep))
		mustWrite(t, sourcePath, fmt.Sprint(ep))
		if err := os.Link(sourcePath, targetPath); err != nil {
			t.Fatal(err)
		}
	}
	history := &fakeHistory{entries: []release.Release{
		{
			MediaType: "series", Provider: "generic", MediaName: "[grpabc155] abcabc014[1080P][BIG5][06][MP4]", NormalizedTitle: "abcabc014", Group: "grpabc155", Resolution: "1080p",
			DownloadURL: "https://example.test/kisssub-e06.torrent",
			Components:  medianame.Components{Title: "abcabc014", Season: 1, EpisodeStart: 6, EpisodeEnd: 6, EpisodeEvidence: medianame.EpisodeEvidencePublication, ReleaseGroup: "grpabc155"},
		},
		{
			MediaType: "series", Provider: "generic", MediaName: "[grpabc153] abcabc014 - 06 [CR WEB-DL 1080p AVC AAC][CHT].mkv", NormalizedTitle: "abcabc014", Group: "grpabc153", Resolution: "1080p", Subtitle: "zh-Hant",
			DownloadURL: "https://example.test/nix-e06.torrent",
			Components:  medianame.Components{Title: "abcabc014", Season: 1, EpisodeStart: 6, EpisodeEnd: 6, EpisodeEvidence: medianame.EpisodeEvidencePublication, ReleaseGroup: "grpabc153"},
		},
		{
			MediaType: "series", Provider: "generic", MediaName: "[grpabc166] abcabc014 - 06 [WEB-DL 720p HEVC-10bit AAC][CHT].mkv", NormalizedTitle: "abcabc014", Group: "grpabc166", Resolution: "720p", Subtitle: "zh-Hant",
			DownloadURL: "https://example.test/ani-e06.torrent",
			Components:  medianame.Components{Title: "abcabc014", Season: 1, EpisodeStart: 6, EpisodeEnd: 6, EpisodeEvidence: medianame.EpisodeEvidencePublication, ReleaseGroup: "grpabc166"},
		},
	}}
	app, err := Open(Options{ConfigRoot: f.cfg, Fetcher: fakeFetcher{}, History: history, Backends: map[string]download.Backend{"c": f.client}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := app.Cycle(context.Background(), CycleOptions{LocalOnly: true, Acquire: true, RepairGaps: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.client.adds) != 0 || len(result.Acquisitions) != 0 {
		t.Fatalf("cross-group fallback must wait for confirmation: adds=%+v result=%+v", f.client.adds, result)
	}
	view, err := app.GetEntry(context.Background(), w.Key)
	if err != nil {
		t.Fatal(err)
	}
	seasons := view.Seasons
	var review RepairReview
	foundReview := false
	for _, season := range seasons {
		if season.Season == 1 && len(season.RepairReviews) == 1 {
			review = season.RepairReviews[0]
			foundReview = true
		}
	}
	if !foundReview {
		t.Fatalf("missing repair review: seasons=%+v issues=%+v", seasons, result.Issues)
	}
	if review.Release.Group != "grpabc153" || review.Release.Resolution != "1080p" {
		t.Fatalf("wrong review candidate: %+v", review)
	}
	if len(review.Differences) == 0 || review.ExpectedGroup != "grpabc154" {
		t.Fatalf("review did not explain continuity break: %+v", review)
	}
	confirmed, err := app.ConfirmRepair(context.Background(), w.Key, 1, review.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.client.adds) != 1 || f.client.adds[0].URL != "https://example.test/nix-e06.torrent" {
		t.Fatalf("confirmed review was not acquired: adds=%+v result=%+v", f.client.adds, confirmed)
	}
	if len(confirmed.Acquisitions) != 1 {
		t.Fatalf("confirm acquisitions=%+v", confirmed.Acquisitions)
	}
	if _, err := app.ConfirmRepair(context.Background(), w.Key, 1, review.ID); err == nil {
		t.Fatal("consumed repair review was accepted a second time")
	}
	if len(f.client.adds) != 1 {
		t.Fatalf("stale confirmation submitted a second task: %+v", f.client.adds)
	}
}

func TestLocalGapRepairRelaxesTraitsWithinStableReleaseGroup(t *testing.T) {
	f := newFixture(t, map[string]string{"f": "feed"})
	w, err := catalog.CreateManagedAt(f.src, f.lib, "abcabc014", 2026, 1, catalog.Declaration{Sources: []string{"f"}})
	if err != nil {
		t.Fatal(err)
	}
	sourceSeason := filepath.Join(w.DeclarationPath, "Season 01")
	librarySeason := filepath.Join(w.Path, "Season 01")
	for _, ep := range []int{3, 4, 5, 7} {
		sourcePath := filepath.Join(sourceSeason, fmt.Sprintf("[grpabc157] abcabc014 - %02d [1080p][CHT].mkv", ep))
		targetPath := filepath.Join(librarySeason, fmt.Sprintf("abcabc014 S01E%02d.mkv", ep))
		mustWrite(t, sourcePath, fmt.Sprint(ep))
		if err := os.Link(sourcePath, targetPath); err != nil {
			t.Fatal(err)
		}
	}
	history := &fakeHistory{entries: []release.Release{{
		MediaType:       "series",
		Provider:        "generic",
		MediaName:       "[grpabc157] abcabc014 - 06 [720p][CHS].mkv",
		NormalizedTitle: "abcabc014",
		Group:           "grpabc157",
		Resolution:      "720p",
		Subtitle:        "CHS",
		DownloadURL:     "https://example.test/continuity-e06.torrent",
		Components: medianame.Components{
			Title: "abcabc014", Season: 1, EpisodeStart: 6, EpisodeEnd: 6, EpisodeEvidence: "weak", ReleaseGroup: "grpabc157",
		},
	}}}
	app, err := Open(Options{ConfigRoot: f.cfg, Fetcher: fakeFetcher{}, History: history, Backends: map[string]download.Backend{"c": f.client}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := app.Cycle(context.Background(), CycleOptions{LocalOnly: true, Acquire: true, RepairGaps: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.client.adds) != 1 || f.client.adds[0].URL != "https://example.test/continuity-e06.torrent" {
		t.Fatalf("same-group repair was not acquired: adds=%+v result=%+v", f.client.adds, result)
	}
	if len(result.Acquisitions) != 1 {
		t.Fatalf("acquisitions=%+v", result.Acquisitions)
	}
}

func TestBackfillFallbackKeepsPreferredReleaseAndRepairsOtherHoles(t *testing.T) {
	root := t.TempDir()
	folder := filepath.Join(root, "Season 01")
	for _, ep := range []int{3, 5, 7} {
		mustWrite(t, filepath.Join(folder, fmt.Sprintf("[grpabc157] abcabc146 - %02d [1080p][CHT].mkv", ep)), "media")
	}
	w := catalog.Entry{Key: "series/abcabc146", MediaType: catalog.MediaSeries, Title: "abcabc146", Enabled: true, DeclarationPath: root,
		FolderProjections: map[string]catalog.FolderProjection{"Season 01": {Season: 1}}}
	b := configstore.Bundle{Entries: map[string]catalog.Entry{w.Key: w}, Sources: map[string]configstore.ContentSource{"f": {ID: "f", Enabled: true}}}
	makeRelease := func(ep int, group, resolution, subtitle string) release.Release {
		return release.Release{MediaType: "series", Group: group, Resolution: resolution, Subtitle: subtitle,
			DownloadURL: fmt.Sprintf("https://example.test/%s-%d.torrent", group, ep),
			Components:  medianame.Components{Title: "abcabc146", Season: 1, EpisodeStart: ep, EpisodeEnd: ep, ReleaseGroup: group}}
	}
	plans, err := buildBackfillSelection(context.Background(), b, w, 1, []int{4, 6},
		map[string][]release.Release{"f": {
			makeRelease(4, "grpabc157", "1080p", "CHT"),
			makeRelease(4, "grpabc158", "1080p", "CHT"),
			makeRelease(6, "grpabc157", "720p", "CHS"),
			makeRelease(6, "grpabc158", "1080p", "CHT"),
		}}, catalog.ObserveFolderProjectionEvidence(w))
	if err != nil {
		t.Fatal(err)
	}
	got := plans.Automatic
	if len(got.Candidates) != 2 || got.Candidates[0].Release.Group != "grpabc157" || got.Candidates[0].Episode.EpisodeStart != 4 || got.Candidates[1].Release.Group != "grpabc157" || got.Candidates[1].Episode.EpisodeStart != 6 {
		t.Fatalf("selection=%+v", got.Candidates)
	}
	if len(plans.Review.Candidates) != 0 {
		t.Fatalf("same-group candidates should not require review: %+v", plans.Review.Candidates)
	}
}

func TestMigrationPlanIdentityPreservesKnownSeasonZero(t *testing.T) {
	key, _, title, season := migrationPlanIdentity(configstore.Bundle{}, migration.Plan{
		Title:       "abcabc042",
		Season:      0,
		SeasonKnown: true,
		MediaType:   catalog.MediaSeries,
	})
	if title != "abcabc042" || season != 0 || !strings.HasSuffix(key, "/season/00") {
		t.Fatalf("known S00 migration identity was defaulted: key=%q title=%q season=%d", key, title, season)
	}
}

func TestRepairReviewRetainsCandidateEpisodeRange(t *testing.T) {
	candidate := candidate.Candidate{
		EntryKey:    "series/abcabc146",
		SourceID:    "f",
		Episode:     episode.Key{EntryKey: "series/abcabc146", Season: 1, EpisodeStart: 5, EpisodeEnd: 6},
		Acquisition: acquisition.Identity{ID: "test-acquisition"},
		Release:     release.Release{MediaName: "abcabc146 S01E05-E06.mkv"},
	}
	review := newRepairReview(catalog.Entry{Key: "series/abcabc146"}, candidate)
	start, end := review.episodeRange()
	if review.EpisodeStart != 5 || review.EpisodeEnd != 6 || start != 5 || end != 6 {
		t.Fatalf("review range=%+v normalized=%d-%d", review, start, end)
	}
}
