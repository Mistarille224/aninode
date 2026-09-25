package server

import (
	"aninode/internal/rss"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"aninode/internal/application"
	"aninode/internal/download"
)

func TestRSSReadAndRefreshDoNotWaitForRemoteSource(t *testing.T) {
	root := t.TempDir()
	cfg := filepath.Join(root, "config")
	writeFixture(t, filepath.Join(cfg, "organizer.json"), `{"source":"`+filepath.Join(root, "downloads")+`","target":"`+filepath.Join(root, "library")+`","extensions":[".mkv"]}`)
	writeFixture(t, filepath.Join(cfg, "sources/f.json"), `{"id":"f","provider":"generic","rss":[{"url":"https://example.test/rss"}],"enabled":true}`)
	fetcher := &blockingFetcher{started: make(chan struct{}), release: make(chan struct{})}
	app, err := application.Open(application.Options{ConfigRoot: cfg, Fetcher: fetcher, Backends: map[string]download.Backend{}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	auth, cookie := testWebAuth(t)
	s := &Service{App: app, reconcileCtx: ctx, Auth: auth}
	defer func() { cancel(); s.reconcileWG.Wait() }()
	h := s.Handler()
	call := func(method, path, body, token string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
		r.Header.Set("X-Aninode-Request", "1")
		result := make(chan *httptest.ResponseRecorder, 1)
		go func() { w := httptest.NewRecorder(); h.ServeHTTP(w, r); result <- w }()
		select {
		case w := <-result:
			return w
		case <-time.After(2 * time.Second):
			t.Fatal("RSS HTTP request waited on remote source")
			return nil
		}
	}
	read := func() application.RSSDiscoveryResult {
		t.Helper()
		w := call("GET", "/ui/discovery/rss", "", cookie.Value)
		if w.Code != http.StatusOK {
			t.Fatalf("read: %d %s", w.Code, w.Body.String())
		}
		var out application.RSSDiscoveryResult
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	if w := call("POST", "/ui/discovery/rss/refresh", "{}", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated refresh: %d", w.Code)
	}
	for _, body := range []string{`{"unexpected":true}`, `{} {}`} {
		if w := call("POST", "/ui/discovery/rss/refresh", body, cookie.Value); w.Code != http.StatusBadRequest {
			t.Fatalf("invalid body: %d", w.Code)
		}
	}
	initial := read()
	if initial.Refreshing || len(initial.Groups) != 0 || len(initial.Sources) != 1 || initial.Sources[0].Status != "pending" {
		t.Fatalf("initial=%+v", initial)
	}
	select {
	case <-fetcher.started:
		t.Fatal("GET initiated remote observation")
	default:
	}
	if w := call("POST", "/ui/discovery/rss/refresh", "{}", cookie.Value); w.Code != http.StatusAccepted {
		t.Fatalf("refresh: %d %s", w.Code, w.Body.String())
	}
	select {
	case <-fetcher.started:
	case <-time.After(2 * time.Second):
		t.Fatal("background fetch did not start")
	}
	if during := read(); !during.Refreshing {
		t.Fatalf("refresh status=%+v", during)
	}
	if w := call("POST", "/ui/discovery/rss/refresh", "{}", cookie.Value); w.Code != http.StatusAccepted || !strings.Contains(w.Body.String(), "coalesced") {
		t.Fatalf("duplicate: %d %s", w.Code, w.Body.String())
	}
	close(fetcher.release)
	s.reconcileWG.Wait()
	complete := read()
	if complete.Refreshing || complete.Sources[0].Status != "ready" || complete.Sources[0].UpdatedAt.IsZero() {
		t.Fatalf("complete=%+v", complete)
	}
	if app.Diagnostics(context.Background()).LastCycle != nil {
		t.Fatal("manual RSS refresh ran acquisition/reconciliation")
	}
}

func TestRSSRefreshSharesRunnerWithoutLosingLocalOrFullIntents(t *testing.T) {
	for _, tc := range []struct {
		name                            string
		running, pending, request, want reconcileMode
	}{
		{"duplicate RSS", reconcileRSS, reconcileNone, reconcileRSS, reconcileNone},
		{"full subsumes RSS", reconcileFull, reconcileNone, reconcileRSS, reconcileNone},
		{"local queues RSS", reconcileLocal, reconcileNone, reconcileRSS, reconcileRSS},
		{"RSS queues full", reconcileRSS, reconcileNone, reconcileFull, reconcileFull},
		{"retain local after RSS", reconcileRSS, reconcileNone, reconcileLocal, reconcileLocal},
		{"combine pending local and RSS", reconcileLocal, reconcileLocal, reconcileRSS, reconcileLocal | reconcileRSS},
		{"full supersedes pending", reconcileLocal, reconcileLocal | reconcileRSS, reconcileFull, reconcileFull},
		{"pending full subsumes RSS", reconcileLocal, reconcileFull, reconcileRSS, reconcileFull},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Service{reconcileRunning: tc.running, reconcilePending: tc.pending}
			if got := s.requestReconcile(context.Background(), tc.request); got != "coalesced" {
				t.Fatal(got)
			}
			if s.reconcilePending != tc.want {
				t.Fatalf("pending=%v want=%v", s.reconcilePending, tc.want)
			}
		})
	}
}

// Fake time proves that a manual RSS refresh resets the existing remote deadline
// without adding another timer or waiting through a wall-clock interval.
func TestRSSManualRefreshResetsScheduledRemoteDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		root := t.TempDir()
		cfg := filepath.Join(root, "config")
		writeFixture(t, filepath.Join(cfg, "organizer.json"), `{"source":"`+filepath.Join(root, "downloads")+`","target":"`+filepath.Join(root, "library")+`","extensions":[".mkv"]}`)
		writeFixture(t, filepath.Join(cfg, "sources/f.json"), `{"id":"f","provider":"generic","rss":[{"url":"https://example.test/rss"}],"enabled":true}`)
		fetcher := &countingRSSFetcher{}
		app, err := application.Open(application.Options{ConfigRoot: cfg, Fetcher: fetcher, Backends: map[string]download.Backend{}})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		s := &Service{App: app, reconcileCtx: ctx, LocalInterval: time.Hour}
		done := make(chan struct{})
		go func() { defer close(done); s.scheduler(ctx, 10*time.Minute) }()
		synctest.Wait()
		if got := fetcher.calls.Load(); got != 1 {
			t.Fatalf("startup fetch count=%d", got)
		}
		time.Sleep(5 * time.Minute)
		s.requestReconcile(ctx, reconcileRSS)
		synctest.Wait()
		if got := fetcher.calls.Load(); got != 2 {
			t.Fatalf("manual fetch count=%d", got)
		}
		time.Sleep(5 * time.Minute)
		synctest.Wait()
		if got := fetcher.calls.Load(); got != 2 {
			t.Fatalf("old remote deadline still armed: %d", got)
		}
		time.Sleep(5 * time.Minute)
		synctest.Wait()
		if got := fetcher.calls.Load(); got != 3 {
			t.Fatalf("new remote deadline did not run: %d", got)
		}
		cancel()
		<-done
		s.reconcileWG.Wait()
	})
}

type countingRSSFetcher struct{ calls atomic.Int32 }

func (f *countingRSSFetcher) Fetch(context.Context, string) ([]rss.Entry, error) {
	f.calls.Add(1)
	return nil, nil
}

func TestRSSCompletionIsNotLostBehindLocalCompletion(t *testing.T) {
	s := &Service{}
	s.notifyReconcileCompleted(reconcileLocal)
	s.notifyReconcileCompleted(reconcileRSS)
	mode := <-s.reconcileCompleted
	if mode&reconcileRSS == 0 || mode&reconcileLocal == 0 {
		t.Fatalf("lost completion kind: %v", mode)
	}
}

// Use native magnet names so this exercises the real RSS-to-discovery pipeline.
type manyRSSFetcher struct{}

func (manyRSSFetcher) Fetch(_ context.Context, endpoint string) ([]rss.Entry, error) {
	count, prefix := 120, "abcabc051"
	if strings.HasSuffix(endpoint, "/last") {
		count, prefix = 1, "abcabc010"
	}
	values := make([]rss.Entry, count)
	for i := range values {
		name := fmt.Sprintf("[G] %s %03d S01E01 [1080p]", prefix, i)
		values[i] = rss.Entry{Title: name, GUID: name, DownloadURL: fmt.Sprintf("magnet:?xt=urn:btih:%040x&dn=%s", i+1, url.QueryEscape(name))}
	}
	return values, nil
}

func TestRSSHTTPReturnsAllSourcesWithoutTruncation(t *testing.T) {
	root := t.TempDir()
	cfg := filepath.Join(root, "config")
	writeFixture(t, filepath.Join(cfg, "organizer.json"), `{"source":"`+filepath.Join(root, "downloads")+`","target":"`+filepath.Join(root, "library")+`","extensions":[".mkv"]}`)
	for _, id := range []string{"first", "last"} {
		writeFixture(t, filepath.Join(cfg, "sources", id+".json"), `{"id":"`+id+`","provider":"generic","rss":[{"url":"https://example.test/`+id+`"}],"enabled":true}`)
	}
	app, err := application.Open(application.Options{ConfigRoot: cfg, Fetcher: manyRSSFetcher{}, Backends: map[string]download.Backend{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.RefreshRSS(context.Background()); err != nil {
		t.Fatal(err)
	}
	auth, cookie := testWebAuth(t)
	s := &Service{App: app, Auth: auth}
	r := httptest.NewRequest(http.MethodGet, "/ui/discovery/rss", nil)
	r.AddCookie(cookie)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d: %s", w.Code, w.Body.String())
	}
	var got application.RSSDiscoveryResult
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Groups) != 121 {
		t.Fatalf("groups=%d, want 121", len(got.Groups))
	}
	for _, group := range got.Groups {
		if strings.HasPrefix(group.Title, "abcabc010") {
			return
		}
	}
	t.Fatal("last source's unique work was truncated")
}
