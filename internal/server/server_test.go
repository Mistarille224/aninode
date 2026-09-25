package server

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"aninode/internal/application"
	"aninode/internal/candidate"
	"aninode/internal/catalog"
	"aninode/internal/configstore"
	"aninode/internal/download"
	"aninode/internal/rss"
)

func TestAutomationDetailLoggingKeepsRoutineDecisionsBelowInfo(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelInfo}))
	result := application.CycleResult{
		DiscoveredEntries: []string{"abcabc023"},
		SkippedReleases: []candidate.Candidate{
			{EntryKey: "series/abcabc146", Decision: candidate.DecisionSuperseded, Reason: "newer release exists"},
			{EntryKey: "series/Broken", Decision: candidate.DecisionInvalid, Reason: "invalid episode"},
		},
	}
	logAutomationDetails(context.Background(), logger, result, nil)
	logged := output.String()
	if strings.Contains(logged, "abcabc023") || strings.Contains(logged, "series/abcabc146") {
		t.Fatalf("routine detail reached info logs: %s", logged)
	}
	if !strings.Contains(logged, "series/Broken") {
		t.Fatalf("actionable rejection missing from logs: %s", logged)
	}
}

func TestAutomationDetailLoggingEmitsSubsystemErrors(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelInfo}))
	issue := application.OperationIssue{Severity: "error", Stage: "backfill", Code: "provider_search_failed", Message: "mikan search parser failed", Source: "mikan", Hint: "retry after checking provider availability"}
	logAutomationDetails(context.Background(), logger, application.CycleResult{Issues: []application.OperationIssue{issue}}, nil)
	logged := output.String()
	for _, want := range []string{"mikan search parser failed", "provider_search_failed", "mikan", "retry after checking provider availability"} {
		if !strings.Contains(logged, want) {
			t.Fatalf("structured issue field %q missing from logs: %s", want, logged)
		}
	}
}

type blockingFetcher struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (f *blockingFetcher) Fetch(ctx context.Context, _ string) ([]rss.Entry, error) {
	f.once.Do(func() { close(f.started) })
	select {
	case <-f.release:
		return nil, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func writeFixture(t *testing.T, p, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(s), 0644); err != nil {
		t.Fatal(err)
	}
}
func webApp(t *testing.T) (*application.App, catalog.Entry) {
	t.Helper()
	root := t.TempDir()
	cfg := filepath.Join(root, "config")
	lib := filepath.Join(root, "library", "TV")
	source := filepath.Join(root, "downloads", "TV")
	writeFixture(t, filepath.Join(cfg, "organizer.json"), `{"source":"`+filepath.Join(root, "downloads")+`","target":"`+filepath.Join(root, "library")+`","extensions":[".mkv"]}`)
	writeFixture(t, filepath.Join(cfg, "clients/c.json"), `{"id":"c","type":"aria2","url":"http://client","enabled":true}`)
	writeFixture(t, filepath.Join(cfg, "sources/f.json"), `{"id":"f","provider":"generic","rss":[{"url":"https://example.test/rss"}],"enabled":true}`)
	w, err := catalog.CreateManagedAt(source, lib, "abcabc055", 2026, 1, catalog.Declaration{Sources: []string{"f"}})
	if err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(w.DeclarationPath, "Season 01", "abcabc055 - 01.mkv"), "media")
	writeFixture(t, filepath.Join(w.DeclarationPath, "Season 01", "[grpabc165] defdef - 02.mkv"), "media2")
	app, err := application.Open(application.Options{ConfigRoot: cfg})
	if err != nil {
		t.Fatal(err)
	}
	return app, w
}

func entryAPI(key string) string {
	media, name, err := catalog.SplitKey(key)
	if err != nil {
		panic(err)
	}
	return "/ui/catalog/" + url.PathEscape(media) + "/" + url.PathEscape(name)
}

func TestWebUIRootAndSPAFallbackAreServed(t *testing.T) {
	s := &Service{}
	for _, path := range []string{"/", "/catalog/series/abcabc%20(2026)"} {
		r := httptest.NewRequest("GET", path, nil)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("%s status=%d", path, w.Code)
		}
		if !strings.Contains(w.Body.String(), "<title>aninode</title>") {
			t.Fatalf("%s missing app shell", path)
		}
	}
	r := httptest.NewRequest("GET", "/not-a-route", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 404 {
		t.Fatalf("unknown route=%d", w.Code)
	}
}
func TestWebUIAssetsExposeOnlyManagementAPIWorkflow(t *testing.T) {
	r := httptest.NewRequest("GET", "/assets/app.js", nil)
	w := httptest.NewRecorder()
	(&Service{}).Handler().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("asset=%d", w.Code)
	}
	js := w.Body.String()
	for _, want := range []string{"/ui/catalog", "/declaration", "/seasons/", "/backfill", "/repairs/", "/ui/migrations", "/ui/discovery/search", "/preview", "Content source"} {
		if !strings.Contains(js, want) {
			t.Fatalf("app.js missing %q", want)
		}
	}
	if strings.Contains(js, "内容来源") {
		t.Fatal("app.js must keep user-facing source strings in English; Chinese belongs in locale files")
	}

	r = httptest.NewRequest("GET", "/assets/locales/zh-CN.json", nil)
	w = httptest.NewRecorder()
	(&Service{}).Handler().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("zh-CN locale asset=%d", w.Code)
	}
	locale := w.Body.String()
	for _, want := range []string{`"Content source"`, "内容来源"} {
		if !strings.Contains(locale, want) {
			t.Fatalf("zh-CN locale missing %q", want)
		}
	}
	for _, forbidden := range []string{"/pause", "/resume", "/remove", "/tasks", "/receipts"} {
		if strings.Contains(js, forbidden) {
			t.Fatalf("app.js contains forbidden downloader/receipt API %q", forbidden)
		}
	}
}
func TestEntriesListAndSearch(t *testing.T) {
	app, _ := webApp(t)
	h := authenticatedHandler(t, &Service{App: app})

	r := httptest.NewRequest("GET", "/ui/catalog", nil)
	out := httptest.NewRecorder()
	h.ServeHTTP(out, r)
	var all []application.EntryView
	if out.Code != 200 || json.Unmarshal(out.Body.Bytes(), &all) != nil || len(all) != 1 {
		t.Fatalf("empty query should list entries: status=%d body=%s", out.Code, out.Body.String())
	}
	if all[0].Summary == nil || all[0].Summary.Status == "" {
		t.Fatalf("catalog list must include filesystem-derived summary: %+v", all[0])
	}

	r = httptest.NewRequest("GET", "/ui/catalog?q=defdef", nil)
	out = httptest.NewRecorder()
	h.ServeHTTP(out, r)
	if out.Code != 200 {
		t.Fatalf("filesystem-name search=%d %s", out.Code, out.Body.String())
	}
	var entries []application.EntryView
	if err := json.Unmarshal(out.Body.Bytes(), &entries); err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Entry.Title != "abcabc055" {
		t.Fatalf("filesystem-name search entries=%+v", entries)
	}
}

func TestWebUIAPIIntegrationCatalogLookupAndDeclarationRoundTrip(t *testing.T) {
	app, wk := webApp(t)
	h := authenticatedHandler(t, &Service{App: app})
	r := httptest.NewRequest("GET", "/ui/catalog?q=abcabc055", nil)
	out := httptest.NewRecorder()
	h.ServeHTTP(out, r)
	if out.Code != 200 {
		t.Fatalf("entries=%d %s", out.Code, out.Body.String())
	}
	var entries []application.EntryView
	if err := json.Unmarshal(out.Body.Bytes(), &entries); err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Entry.Title != "abcabc055" {
		t.Fatalf("entries=%+v", entries)
	}
	body := `{"title":"abcabc055","year":2026,"enabled":true,"sources":["f"],"filters":{"groups":["A"]}}`
	r = httptest.NewRequest("PUT", entryAPI(wk.Key)+"/declaration", strings.NewReader(body))
	out = httptest.NewRecorder()
	h.ServeHTTP(out, r)
	if out.Code != 200 {
		t.Fatalf("put=%d %s", out.Code, out.Body.String())
	}
	side, err := catalog.ReadDeclaration(wk.DeclarationPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(side.Filters.Groups) != 1 || side.Filters.Groups[0] != "A" {
		t.Fatalf("groups=%v", side.Filters.Groups)
	}
	folderPath := entryAPI(wk.Key) + "/folders/" + url.PathEscape("Season 01") + "/projection"
	r = httptest.NewRequest("PUT", folderPath, strings.NewReader(`{"season":2,"episode_offset":-12}`))
	out = httptest.NewRecorder()
	h.ServeHTTP(out, r)
	if out.Code != 200 {
		t.Fatalf("folder put=%d %s", out.Code, out.Body.String())
	}
	r = httptest.NewRequest("GET", entryAPI(wk.Key), nil)
	out = httptest.NewRecorder()
	h.ServeHTTP(out, r)
	if out.Code != 200 {
		t.Fatalf("entry get=%d %s", out.Code, out.Body.String())
	}
	var entry application.EntryView
	if err := json.Unmarshal(out.Body.Bytes(), &entry); err != nil {
		t.Fatal(err)
	}
	if len(entry.Folders) != 1 || entry.Folders[0].Name != "Season 01" || entry.Folders[0].Projection.Season != 2 || entry.Folders[0].Projection.EpisodeOffset != -12 {
		t.Fatalf("folders=%+v", entry.Folders)
	}
}
func TestWebUIAPIInvalidMutationDoesNotPretendToSave(t *testing.T) {
	app, wk := webApp(t)
	h := authenticatedHandler(t, &Service{App: app})
	before, _ := os.ReadFile(filepath.Join(wk.DeclarationPath, ".aninode.json"))
	body := `{"title":"abcabc055","year":2026,"enabled":true,"obsolete_names":["obsolete"],"sources":["f"],"filters":{}}`
	r := httptest.NewRequest("PUT", entryAPI(wk.Key)+"/declaration", strings.NewReader(body))
	out := httptest.NewRecorder()
	h.ServeHTTP(out, r)
	if out.Code == 200 {
		t.Fatalf("invalid mutation succeeded: %s", out.Body.String())
	}
	if !strings.Contains(out.Body.String(), `"error"`) {
		t.Fatalf("missing error envelope: %s", out.Body.String())
	}
	after, _ := os.ReadFile(filepath.Join(wk.DeclarationPath, ".aninode.json"))
	if string(after) != string(before) {
		t.Fatalf("declaration changed: before=%s after=%s", before, after)
	}
}

func TestHealthUnauthenticated(t *testing.T) {
	s := &Service{}
	r := httptest.NewRequest("GET", "/healthz", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("health=%d", w.Code)
	}
}
func TestStrictJSONRejectsUnknownFields(t *testing.T) {
	r := httptest.NewRequest("POST", "/", strings.NewReader(`{"action":"reset","surprise":true}`))
	var req BackfillRequest
	if err := decodeStrict(r, &req); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("error=%v", err)
	}
}
func TestStrictJSONRejectsMultipleValues(t *testing.T) {
	r := httptest.NewRequest("POST", "/", strings.NewReader(`{} {}`))
	if err := decodeStrict(r, &EmptyRequest{}); err == nil {
		t.Fatal("expected multiple JSON values rejection")
	}
}

func TestReadinessReflectsApplicationConfigurationAndHealthStaysLive(t *testing.T) {
	root := t.TempDir()
	cfg := filepath.Join(root, "config")
	lib := filepath.Join(root, "library")
	writeFixture(t, filepath.Join(cfg, "organizer.json"), `{"source":"`+filepath.Join(root, "downloads")+`","target":"`+lib+`","extensions":[".mkv"]}`)
	app, err := application.Open(application.Options{ConfigRoot: cfg, Backends: map[string]download.Backend{}})
	if err != nil {
		t.Fatal(err)
	}
	h := (&Service{App: app}).Handler()
	for _, tc := range []struct {
		path string
		code int
	}{{"/healthz", 200}, {"/readyz", 200}} {
		r := httptest.NewRequest("GET", tc.path, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.code {
			t.Fatalf("%s=%d %s", tc.path, w.Code, w.Body.String())
		}
	}
	writeFixture(t, filepath.Join(cfg, "organizer.json"), `{bad json`)
	r := httptest.NewRequest("GET", "/readyz", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 503 || !strings.Contains(w.Body.String(), "invalid_configuration") {
		t.Fatalf("ready=%d %s", w.Code, w.Body.String())
	}
	r = httptest.NewRequest("GET", "/healthz", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("health=%d", w.Code)
	}
}

func TestManagementAuthAndConfigSecretReferencesDoNotLeakValues(t *testing.T) {
	root := t.TempDir()
	cfg := filepath.Join(root, "config")
	lib := filepath.Join(root, "library")
	writeFixture(t, filepath.Join(cfg, "organizer.json"), `{"source":"`+filepath.Join(root, "downloads")+`","target":"`+lib+`","extensions":[".mkv"]}`)
	app, err := application.Open(application.Options{ConfigRoot: cfg})
	if err != nil {
		t.Fatal(err)
	}
	password := "actual-super-secret-value"
	if _, err := app.SaveClient(context.Background(), "c", configstore.Client{ID: "c", Type: "aria2", URL: "http://client", Enabled: true}, &password); err != nil {
		t.Fatal(err)
	}
	auth, cookie := testWebAuth(t)
	h := (&Service{App: app, Auth: auth}).Handler()
	r := httptest.NewRequest("GET", "/ui/config", nil)
	r.Header.Set("Authorization", "Bearer old-token")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 401 || strings.Contains(w.Body.String(), cookie.Value) {
		t.Fatalf("auth response leaked token: %d %s", w.Code, w.Body.String())
	}
	r = httptest.NewRequest("GET", "/ui/config", nil)
	r.AddCookie(cookie)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("config=%d %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if strings.Contains(body, "actual-super-secret-value") || strings.Contains(body, "ciphertext") || strings.Contains(body, "encrypted_credential") {
		t.Fatalf("config leaked resolved secret: %s", body)
	}
	if !strings.Contains(body, "http://client") {
		t.Fatalf("authenticated config omitted editable client URL: %s", body)
	}
	if !strings.Contains(body, `"credential_set":true`) {
		t.Fatalf("credential presence missing: %s", body)
	}
}

func TestDiagnosticsAreOperationalOnlyAndAuthenticated(t *testing.T) {
	app, _ := webApp(t)
	auth, cookie := testWebAuth(t)
	h := (&Service{App: app, Auth: auth}).Handler()
	_, _ = app.Cycle(context.Background(), application.CycleOptions{})
	r := httptest.NewRequest("GET", "/ui/diagnostics", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatalf("unauth diagnostics=%d", w.Code)
	}
	r = httptest.NewRequest("GET", "/ui/diagnostics", nil)
	r.AddCookie(cookie)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"last_cycle"`) {
		t.Fatalf("diagnostics=%d %s", w.Code, w.Body.String())
	}
	for _, forbidden := range []string{"present_episodes", "missing", "task_id", "receipt", "torrent"} {
		if strings.Contains(w.Body.String(), forbidden) {
			t.Fatalf("diagnostics leaked business/task state %q: %s", forbidden, w.Body.String())
		}
	}
}

func TestLocalReconcileDoesNotPollSources(t *testing.T) {
	root := t.TempDir()
	cfg := filepath.Join(root, "config")
	writeFixture(t, filepath.Join(cfg, "organizer.json"), `{"source":"`+filepath.Join(root, "downloads")+`","target":"`+filepath.Join(root, "library")+`","extensions":[".mkv"]}`)
	writeFixture(t, filepath.Join(cfg, "sources/f.json"), `{"id":"f","provider":"generic","rss":[{"url":"https://example.test/rss"}],"enabled":true}`)
	fetcher := &blockingFetcher{started: make(chan struct{}), release: make(chan struct{})}
	app, err := application.Open(application.Options{ConfigRoot: cfg, Fetcher: fetcher, Backends: map[string]download.Backend{}})
	if err != nil {
		t.Fatal(err)
	}
	s := &Service{App: app}
	if got := s.requestLocalReconcile(context.Background()); got != "started" {
		t.Fatalf("local reconcile status=%q", got)
	}
	done := make(chan struct{})
	go func() {
		s.reconcileWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("local reconciliation did not finish")
	}
	select {
	case <-fetcher.started:
		t.Fatal("local reconciliation unexpectedly polled RSS")
	default:
	}
}

func TestAutomationRunReturnsAcceptedAndCoalescesSlowCycle(t *testing.T) {
	root := t.TempDir()
	cfg := filepath.Join(root, "config")
	writeFixture(t, filepath.Join(cfg, "organizer.json"), `{"source":"`+filepath.Join(root, "downloads")+`","target":"`+filepath.Join(root, "library")+`","extensions":[".mkv"]}`)
	writeFixture(t, filepath.Join(cfg, "sources/f.json"), `{"id":"f","provider":"generic","rss":[{"url":"https://example.test/rss"}],"enabled":true}`)
	fetcher := &blockingFetcher{started: make(chan struct{}), release: make(chan struct{})}
	app, err := application.Open(application.Options{ConfigRoot: cfg, Fetcher: fetcher, Backends: map[string]download.Backend{}})
	if err != nil {
		t.Fatal(err)
	}
	s := &Service{App: app}
	h := authenticatedHandler(t, s)

	first := httptest.NewRecorder()
	h.ServeHTTP(first, httptest.NewRequest("POST", "/ui/automation/run", strings.NewReader("{}")))
	if first.Code != http.StatusAccepted || !strings.Contains(first.Body.String(), `"status":"started"`) {
		t.Fatalf("first trigger status=%d body=%s", first.Code, first.Body.String())
	}
	select {
	case <-fetcher.started:
	case <-time.After(time.Second):
		t.Fatal("automation did not enter slow RSS fetch")
	}

	started := time.Now()
	second := httptest.NewRecorder()
	h.ServeHTTP(second, httptest.NewRequest("POST", "/ui/automation/run", strings.NewReader("{}")))
	if second.Code != http.StatusAccepted || !strings.Contains(second.Body.String(), `"status":"coalesced"`) {
		t.Fatalf("second trigger status=%d body=%s", second.Code, second.Body.String())
	}
	if time.Since(started) > 100*time.Millisecond {
		t.Fatalf("coalesced trigger blocked for %s", time.Since(started))
	}

	close(fetcher.release)
	done := make(chan struct{})
	go func() {
		s.reconcileWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("coalesced automation did not drain")
	}
}

func TestSchedulerRunsWithoutStateDirectory(t *testing.T) {
	root := t.TempDir()
	cfg := filepath.Join(root, "config")
	lib := filepath.Join(root, "library")
	writeFixture(t, filepath.Join(cfg, "organizer.json"), `{"source":"`+filepath.Join(root, "downloads")+`","target":"`+lib+`","extensions":[".mkv"]}`)
	if _, err := catalog.CreateManaged(filepath.Join(lib, "TV"), "Broken Cycle", 2026, 1, catalog.Declaration{}); err != nil {
		t.Fatal(err)
	}
	app, err := application.Open(application.Options{ConfigRoot: cfg, Backends: map[string]download.Backend{}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Service{App: app}
	schedulerDone := make(chan struct{})
	go func() {
		defer close(schedulerDone)
		s.scheduler(ctx, 2*time.Millisecond)
	}()
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if d := app.Diagnostics(context.Background()); d.LastCycle != nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-schedulerDone:
	case <-time.After(time.Second):
		t.Fatal("scheduler did not stop after cancellation")
	}
	d := app.Diagnostics(context.Background())
	if d.LastCycle == nil {
		t.Fatalf("diagnostics=%+v", d)
	}
	r := httptest.NewRequest("GET", "/healthz", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("health=%d", w.Code)
	}
}

func TestDeclarationMonitorRunsWhenAutomationSchedulerIsDisabled(t *testing.T) {
	root := t.TempDir()
	cfg := filepath.Join(root, "config")
	downloads := filepath.Join(root, "downloads")
	writeFixture(t, filepath.Join(cfg, "organizer.json"), `{"source":"`+downloads+`","target":"`+filepath.Join(root, "library")+`","extensions":[".mkv"]}`)
	app, err := application.Open(application.Options{ConfigRoot: cfg, Backends: map[string]download.Backend{}})
	if err != nil {
		t.Fatal(err)
	}
	entryRoot := filepath.Join(downloads, "TV", "abcabc037 (2026)")
	ctx, cancel := context.WithCancel(context.Background())
	s := &Service{App: app, LocalInterval: 10 * time.Millisecond}
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.scheduler(ctx, 0)
	}()
	if err := os.MkdirAll(filepath.Join(entryRoot, "Season 01"), 0o755); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(entryRoot, ".aninode.json")); err == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	s.reconcileWG.Wait()
	if _, err := os.Stat(filepath.Join(entryRoot, ".aninode.json")); err != nil {
		t.Fatalf("declaration monitor did not adopt source topology: %v", err)
	}
	if d := app.Diagnostics(context.Background()); d.LastCycle == nil {
		t.Fatal("periodic local reconciliation did not run")
	}
}

func TestServeGracefulShutdownOnContextCancellation(t *testing.T) {
	app, _ := webApp(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := (&Service{App: app}).Serve(ctx, "127.0.0.1:0", 0)
	if err != nil {
		t.Fatalf("shutdown error=%v", err)
	}
}

func TestConfigManagementAPIRoundTripsCompleteEditableFields(t *testing.T) {
	app, _ := webApp(t)
	h := authenticatedHandler(t, &Service{App: app})
	client := `{"id":"c","type":"transmission","url":"http://transmission.test:9091","username":"alice","password":"resolved-must-not-leak","path_mappings":[{"remote":"/downloads","local":"/media/downloads"}],"enabled":true}`
	r := httptest.NewRequest("PUT", "/ui/clients/c", strings.NewReader(client))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("client put=%d %s", w.Code, w.Body.String())
	}
	source := `{"id":"history","provider":"generic","rss":[{"url":"https://example.test/current.xml"}],"search":{"url_template":"https://example.test/search?q={query}"},"enabled":true,"priority":7}`
	r = httptest.NewRequest("PUT", "/ui/sources/history", strings.NewReader(source))
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("feed put=%d %s", w.Code, w.Body.String())
	}
	r = httptest.NewRequest("GET", "/ui/config", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("config=%d %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{"http://transmission.test:9091", `"credential_set":true`, "/downloads", "/media/downloads", "https://example.test/current.xml", "https://example.test/search?q={query}", `"priority":7`} {
		if !strings.Contains(body, want) {
			t.Fatalf("config lost %q: %s", want, body)
		}
	}
	if strings.Contains(body, "resolved-must-not-leak") {
		t.Fatalf("resolved secret leaked: %s", body)
	}
	// A second downloader is rejected; the configuration has exactly one slot.
	second := `{"id":"extra","type":"aria2","url":"http://extra.test","enabled":true}`
	r = httptest.NewRequest("PUT", "/ui/clients/extra", strings.NewReader(second))
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code == 200 {
		t.Fatalf("second downloader unexpectedly accepted: %s", w.Body.String())
	}
	// Content-source references remain strict.
	r = httptest.NewRequest("DELETE", "/ui/sources/f", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code == 200 {
		t.Fatalf("referenced entry feed delete succeeded: %s", w.Body.String())
	}
}

func TestManagementRejectsTraversalUnknownFieldsAndForbiddenProductRoutes(t *testing.T) {
	app, _ := webApp(t)
	h := authenticatedHandler(t, &Service{App: app})
	for _, tc := range []struct{ method, path, body string }{
		{"PUT", "/ui/clients/%2e%2e", `{"id":"..","type":"aria2","url":"http://client","enabled":true}`},
		{"PUT", "/ui/sources/f", `{"id":"f","provider":"generic","rss":[{"url":"https://example.test/rss"}],"enabled":true,"unknown":1}`},
	} {
		r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code < 400 {
			t.Fatalf("unsafe request %s %s succeeded: %d %s", tc.method, tc.path, w.Code, w.Body.String())
		}
	}
	for _, path := range []string{"/ui/tasks", "/ui/receipts", "/ui/enqueue", "/ui/problems", "/ui/fix"} {
		r := httptest.NewRequest("GET", path, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 404 {
			t.Fatalf("forbidden route %s=%d", path, w.Code)
		}
	}
}

func TestSchedulerWakesAutomationWhenLocalMiddleGapAppears(t *testing.T) {
	root := t.TempDir()
	cfg := filepath.Join(root, "config")
	downloads := filepath.Join(root, "downloads")
	library := filepath.Join(root, "library")
	writeFixture(t, filepath.Join(cfg, "organizer.json"), `{"source":"`+downloads+`","target":"`+library+`","extensions":[".mkv"]}`)
	w, err := catalog.CreateManagedAt(filepath.Join(downloads, "TV"), filepath.Join(library, "TV"), "abcabc121", 2026, 1, catalog.Declaration{})
	if err != nil {
		t.Fatal(err)
	}
	season := filepath.Join(w.Path, "Season 01")
	for _, ep := range []int{1, 2, 3} {
		writeFixture(t, filepath.Join(season, "abcabc121 S01E0"+string(rune('0'+ep))+".mkv"), "x")
	}
	app, err := application.Open(application.Options{ConfigRoot: cfg, Backends: map[string]download.Backend{}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &Service{App: app, LocalInterval: 10 * time.Millisecond}
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.scheduler(ctx, time.Hour)
	}()
	deadline := time.Now().Add(time.Second)
	var first time.Time
	for time.Now().Before(deadline) {
		if d := app.Diagnostics(context.Background()); d.LastCycle != nil {
			first = d.LastCycle.FinishedAt
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if first.IsZero() {
		t.Fatal("startup automation cycle did not run")
	}
	if err := os.Remove(filepath.Join(season, "abcabc121 S01E02.mkv")); err != nil {
		t.Fatal(err)
	}
	for time.Now().Before(deadline) {
		if d := app.Diagnostics(context.Background()); d.LastCycle != nil && d.LastCycle.FinishedAt.After(first) {
			cancel()
			<-done
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	t.Fatal("local middle gap was not rediscovered by periodic reconciliation")
}

func TestClientAPIRejectsCredentialStorageFields(t *testing.T) {
	app, _ := webApp(t)
	h := authenticatedHandler(t, &Service{App: app})
	for _, field := range []string{`"credential":"filename"`, `"encrypted_credential":"v1:fake"`} {
		req := httptest.NewRequest("PUT", "/ui/clients/c", strings.NewReader(`{"id":"c","type":"aria2","url":"http://client","enabled":true,`+field+`}`))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("storage field accepted: %d", rec.Code)
		}
	}
}
