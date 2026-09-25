package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path"
	"strings"
	"sync/atomic"
	"testing"

	"aninode/internal/configstore"
	"aninode/internal/rss"
	"aninode/internal/torrentmeta"
)

type errorFetcher struct{ err error }

func (f errorFetcher) Fetch(context.Context, string) ([]rss.Entry, error) { return nil, f.err }

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func rewriteClient(target string) *http.Client {
	u, err := url.Parse(target)
	if err != nil {
		panic(err)
	}
	base := http.DefaultTransport
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		clone := r.Clone(r.Context())
		clone.URL.Scheme = u.Scheme
		clone.URL.Host = u.Host
		clone.Host = u.Host
		return base.RoundTrip(clone)
	})}
}

func TestSearchUsesConfiguredTargetedQuery(t *testing.T) {
	var queries []string
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.Query().Get("q"))
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = w.Write([]byte(`<rss><channel><item><title>[G] abcabc146 S01E03 1080p</title><guid>h3</guid><enclosure url="magnet:?xt=urn:btih:1123456789abcdef0123456789abcdef01234567&amp;dn=%5BG%5D+abcabc146+S01E03+1080p"/></item></channel></rss>`))
	}))
	defer srv.Close()
	src, _ := NewSource(configstore.ContentSource{ID: "f", Provider: "generic", RSS: []configstore.RSSSource{{URL: srv.URL + "/current"}}, Search: &configstore.SearchSource{URLTemplate: srv.URL + "/search?q={query}"}, Enabled: true}, rss.Fetcher{})
	got, err := src.Search(context.Background(), SearchRequest{Query: "abcabc146", Season: 1, EpisodeStart: 3, EpisodeEnd: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || len(queries) != 1 || queries[0] != "abcabc146 S01E03" {
		t.Fatalf("entries=%+v queries=%q", got, queries)
	}
}

func TestMikanSupportsDiscoveryAndHistoricalSearch(t *testing.T) {
	src, _ := NewSource(configstore.ContentSource{Provider: "mikan", RSS: []configstore.RSSSource{{URL: "https://example.test/rss"}}}, nil)
	caps := src.Capabilities()
	if !caps.Search || !caps.Historical {
		t.Fatalf("caps=%+v", caps)
	}
}

func TestGenericSourceSupportsDiscoverySearch(t *testing.T) {
	src, _ := NewSource(configstore.ContentSource{Provider: "generic", Search: &configstore.SearchSource{URLTemplate: "https://example.test/?q={query}"}}, nil)
	if !src.DiscoveryCapable() {
		t.Fatal("generic search source unexpectedly unavailable")
	}
}

func TestDMHYBuiltInSearchNeedsNoTemplate(t *testing.T) {
	src, err := NewSource(configstore.ContentSource{Provider: "dmhy"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	caps := src.Capabilities()
	if !caps.Search || !caps.Historical {
		t.Fatalf("caps=%+v", caps)
	}
}

func TestNyaaBuiltInSearchNeedsNoTemplate(t *testing.T) {
	cfg := configstore.ContentSource{Provider: "nyaa"}
	src, err := NewSource(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	caps := src.Capabilities()
	if !caps.Search || !caps.Historical {
		t.Fatalf("caps=%+v", caps)
	}
}

func TestMikanSearchWalksAllPagesAndUsesTorrentNativeName(t *testing.T) {
	nativeName := "[grpabc166] abcabc047 - 03 [1080P].mkv"
	torrent := []byte(fmt.Sprintf("d4:infod6:lengthi1e4:name%d:%see", len(nativeName), nativeName))
	var pages []string
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/Home/Search":
			pages = append(pages, r.URL.Query().Get("page"))
			if r.URL.Query().Get("searchstr") != "abcabc047 03" {
				t.Fatalf("query=%q", r.URL.Query().Get("searchstr"))
			}
			if r.URL.Query().Get("page") == "1" {
				_, _ = fmt.Fprintf(w, `<table><tr><td><a class="changed-layout" href="/Home/Episode/abc123">abcabc125</a></td><td><a href="%s/Download/20260902/abc123.torrent">torrent</a></td></tr></table>`, srv.URL)
				return
			}
			_, _ = w.Write([]byte(`<table></table>`))
		case "/Download/20260902/abc123.torrent":
			w.Header().Set("Content-Type", "application/x-bittorrent")
			_, _ = w.Write(torrent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	p := mikan{base: base{fetcher: rss.Fetcher{Client: rewriteClient(srv.URL)}}}
	values, err := p.Search(context.Background(), SearchRequest{Query: "abcabc047", Season: 1, EpisodeStart: 3, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) != 2 || pages[0] != "1" || pages[1] != "2" {
		t.Fatalf("pages=%v", pages)
	}
	if len(values) != 1 {
		t.Fatalf("values=%+v", values)
	}
	got := values[0]
	if got.Title != "abcabc125" || got.MediaName != nativeName || got.Components.EpisodeStart != 3 {
		t.Fatalf("release=%+v", got)
	}
	if got.NameSource != "torrent-info" || got.InfoHash == "" {
		t.Fatalf("native evidence missing: %+v", got)
	}
}

func TestMikanSearchDoesNotApplyProviderLimitBeforeFiltering(t *testing.T) {
	var torrentHits atomic.Int32
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/Home/Search":
			page := r.URL.Query().Get("page")
			switch page {
			case "1":
				_, _ = fmt.Fprintf(w, `<table>
<tr><td><a href="/Home/Episode/e0">first</a></td><td><a href="%s/Download/e0.torrent">torrent</a></td></tr>
<tr><td><a href="/Home/Episode/e1">second</a></td><td><a href="%s/Download/e1.torrent">torrent</a></td></tr>
</table>`, srv.URL, srv.URL)
			case "2":
				_, _ = fmt.Fprintf(w, `<table><tr><td><a href="/Home/Episode/e2">third</a></td><td><a href="%s/Download/e2.torrent">torrent</a></td></tr></table>`, srv.URL)
			default:
				_, _ = w.Write([]byte(`<table></table>`))
			}
		case strings.HasPrefix(r.URL.Path, "/Download/"):
			torrentHits.Add(1)
			id := strings.TrimSuffix(path.Base(r.URL.Path), ".torrent")
			name := fmt.Sprintf("[G] abcabc047 - 0%s [1080P].mkv", strings.TrimPrefix(id, "e"))
			_, _ = fmt.Fprintf(w, "d4:infod6:lengthi1e4:name%d:%see", len(name), name)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	p := mikan{base: base{fetcher: rss.Fetcher{Client: rewriteClient(srv.URL)}}}
	values, err := p.Search(context.Background(), SearchRequest{Query: "abcabc047", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 3 || torrentHits.Load() != 3 {
		t.Fatalf("values=%d torrentHits=%d", len(values), torrentHits.Load())
	}
}

func TestProviderParsesTorrentNativeNameNotRSSTitle(t *testing.T) {
	nativeName := "[grpabc166] abcabc047 - 03 [1080P].mkv"
	torrent := []byte(fmt.Sprintf("d4:infod6:lengthi1e4:name%d:%see", len(nativeName), nativeName))
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/release.torrent" {
			w.Header().Set("Content-Type", "application/x-bittorrent")
			_, _ = w.Write(torrent)
			return
		}
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = fmt.Fprintf(w, `<rss><channel><item><title>abcabc189</title><guid>x</guid><enclosure url="%s/release.torrent"/></item></channel></rss>`, srv.URL)
	}))
	defer srv.Close()

	src, err := NewSource(configstore.ContentSource{ID: "f", Provider: "generic", RSS: []configstore.RSSSource{{URL: srv.URL}}, Enabled: true}, rss.Fetcher{})
	if err != nil {
		t.Fatal(err)
	}
	values, err := src.Current(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 1 {
		t.Fatalf("values=%+v", values)
	}
	got := values[0]
	if got.Title != "abcabc189" {
		t.Fatalf("display title=%q", got.Title)
	}
	if got.MediaName != nativeName || got.Components.Title != "abcabc047" || got.Components.EpisodeStart != 3 {
		t.Fatalf("release=%+v", got)
	}
	if got.NameSource != "torrent-info" || got.InfoHash == "" {
		t.Fatalf("native evidence missing: %+v", got)
	}
}

func TestMikanTargetedSearchKeepsLaterPageFilterVariants(t *testing.T) {
	var torrentHits atomic.Int32
	var pages atomic.Int32
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/Home/Search":
			pages.Add(1)
			page := r.URL.Query().Get("page")
			switch page {
			case "1":
				// The first page already contains episode 07, but only from an
				// unacceptable group. Search must continue instead of concluding
				// that the target episode has been found.
				_, _ = fmt.Fprintf(w, `<table><tr><td><a href="/Home/Episode/other7">grpabc167 07</a></td><td><a href="%s/Download/other7.torrent">torrent</a></td></tr></table>`, srv.URL)
			case "2":
				_, _ = fmt.Fprintf(w, `<table><tr><td><a href="/Home/Episode/wanted7">Wanted 07</a></td><td><a href="%s/Download/wanted7.torrent">torrent</a></td></tr></table>`, srv.URL)
			default:
				_, _ = w.Write([]byte(`<table></table>`))
			}
		case strings.HasPrefix(r.URL.Path, "/Download/"):
			torrentHits.Add(1)
			id := strings.TrimSuffix(path.Base(r.URL.Path), ".torrent")
			group := "grpabc167"
			if id == "wanted7" {
				group = "Wanted"
			}
			name := fmt.Sprintf("[%s] abcabc047 - 07 [1080P].mkv", group)
			_, _ = fmt.Fprintf(w, "d4:infod6:lengthi1e4:name%d:%see", len(name), name)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	p := mikan{base: base{fetcher: rss.Fetcher{Client: rewriteClient(srv.URL)}}}
	values, err := p.Search(context.Background(), SearchRequest{Query: "abcabc047", EpisodeStart: 7, EpisodeEnd: 7, Targets: []SearchEpisode{{Season: 1, Episode: 7}}, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	foundWanted := false
	for _, value := range values {
		if value.Group == "Wanted" && value.Components.EpisodeStart == 7 {
			foundWanted = true
		}
	}
	if !foundWanted || torrentHits.Load() != 2 || pages.Load() != 3 {
		t.Fatalf("foundWanted=%v values=%d torrentHits=%d pages=%d", foundWanted, len(values), torrentHits.Load(), pages.Load())
	}
}

func TestBuiltinSourceOwnsCurrentRSSEndpoint(t *testing.T) {
	fetcher := &recordingFetcher{}
	src, err := NewSource(configstore.ContentSource{ID: "dmhy", Provider: "dmhy", Enabled: true}, fetcher)
	if err != nil {
		t.Fatal(err)
	}
	if !src.Capabilities().RSS {
		t.Fatal("built-in source should expose RSS without configured endpoint")
	}
	if _, err := src.Current(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fetcher.url != "https://share.dmhy.org/topics/rss/rss.xml" {
		t.Fatalf("url=%q", fetcher.url)
	}
}

type recordingFetcher struct{ url string }

func (f *recordingFetcher) Fetch(_ context.Context, u string) ([]rss.Entry, error) {
	f.url = u
	return nil, nil
}

func TestSourceCurrentPollsMultipleRSSAndDeduplicates(t *testing.T) {
	var hits atomic.Int32
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = fmt.Fprintf(w, `<rss><channel><item><title>display</title><guid>same</guid><enclosure url="magnet:?xt=urn:btih:1123456789abcdef0123456789abcdef01234567&amp;dn=abcabc146+S01E03"/></item></channel></rss>`)
	}))
	defer srv.Close()
	src, err := NewSource(configstore.ContentSource{ID: "f", Provider: "generic", RSS: []configstore.RSSSource{{URL: srv.URL + "/a"}, {URL: srv.URL + "/b"}}, Enabled: true}, rss.Fetcher{})
	if err != nil {
		t.Fatal(err)
	}
	values, err := src.Current(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 2 {
		t.Fatalf("hits=%d", hits.Load())
	}
	if len(values) != 1 {
		t.Fatalf("values=%+v", values)
	}
}

func TestSourceCurrentPreservesUnderlyingCancellation(t *testing.T) {
	fetcher := errorFetcher{err: context.Canceled}
	src, err := NewSource(configstore.ContentSource{ID: "generic", Provider: "generic", RSS: []configstore.RSSSource{{URL: "https://example.test/rss"}}, Enabled: true}, fetcher)
	if err != nil {
		t.Fatal(err)
	}
	_, err = src.Current(context.Background())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Current error=%v, want context.Canceled", err)
	}
}

func TestDMHYRSSResolvesNativeNameThroughItemPageWhenMagnetHasNoDN(t *testing.T) {
	var pageHits atomic.Int32
	var torrentHits atomic.Int32
	const nativeName = "[grpabc154] abcabc009 - 07 [WebRip 1080p].mkv"
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/topics/rss/rss.xml":
			w.Header().Set("Content-Type", "application/rss+xml")
			_, _ = w.Write([]byte(`<rss><channel><item><title>abcabc190</title><guid>dmhy-7</guid><link>https://share.dmhy.org/topics/view/123_native_show.html</link><enclosure url="magnet:?xt=urn:btih:ABCDEFGHIJKLMNOPQRSTUVWXYZ234567"/></item></channel></rss>`))
		case "/topics/view/123_native_show.html":
			pageHits.Add(1)
			_, _ = w.Write([]byte(`<html><body><a href="//dl.dmhy.org/2026/09/0123456789abcdef0123456789abcdef01234567.torrent">download</a></body></html>`))
		case "/2026/09/0123456789abcdef0123456789abcdef01234567.torrent":
			torrentHits.Add(1)
			w.Header().Set("Content-Type", "application/x-bittorrent")
			_, _ = fmt.Fprintf(w, "d4:infod6:lengthi1e4:name%d:%see", len(nativeName), nativeName)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	p := dmhy{base: base{fetcher: rss.Fetcher{Client: rewriteClient(srv.URL)}}}
	values, err := p.Poll(context.Background(), "https://share.dmhy.org/topics/rss/rss.xml")
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 1 {
		t.Fatalf("values=%+v", values)
	}
	if values[0].MediaName != nativeName || values[0].NameSource != "torrent-info" {
		t.Fatalf("media_name=%q source=%q", values[0].MediaName, values[0].NameSource)
	}
	if values[0].Group != "grpabc154" || values[0].Components.EpisodeStart != 7 {
		t.Fatalf("release=%+v", values[0])
	}
	if pageHits.Load() != 1 || torrentHits.Load() != 1 {
		t.Fatalf("pageHits=%d torrentHits=%d", pageHits.Load(), torrentHits.Load())
	}
}

func TestDMHYDoesNotFetchItemPageWhenMagnetAlreadyHasDN(t *testing.T) {
	var pageHits atomic.Int32
	fetcher := &dmhyDNFetcher{pageHits: &pageHits}
	p := dmhy{base: base{fetcher: fetcher}}
	values, err := p.Poll(context.Background(), "https://share.dmhy.org/topics/rss/rss.xml")
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 1 || values[0].MediaName != "[G] abcabc146 - 07 [1080p].mkv" || values[0].NameSource != "magnet-dn" {
		t.Fatalf("values=%+v", values)
	}
	if pageHits.Load() != 0 {
		t.Fatalf("unexpected page fetches=%d", pageHits.Load())
	}
}

type dmhyDNFetcher struct{ pageHits *atomic.Int32 }

func (f *dmhyDNFetcher) Fetch(context.Context, string) ([]rss.Entry, error) {
	return []rss.Entry{{
		Title:       "display",
		Link:        "https://share.dmhy.org/topics/view/not-needed.html",
		DownloadURL: "magnet:?xt=urn:btih:1123456789abcdef0123456789abcdef01234567&dn=%5BG%5D+abcabc146+-+07+%5B1080p%5D.mkv",
	}}, nil
}
func (f *dmhyDNFetcher) FetchPage(context.Context, string) ([]byte, error) {
	f.pageHits.Add(1)
	return nil, errors.New("should not fetch page")
}
func (f *dmhyDNFetcher) FetchTorrentMetadata(context.Context, string) (torrentmeta.Metadata, error) {
	return torrentmeta.Metadata{}, errors.New("should not fetch torrent")
}

func TestSearchCheapFilterRejectsOnlyExplicitWrongEpisode(t *testing.T) {
	entries := []rss.Entry{
		{Title: "[G] abcabc146 S01E02 [1080p]", DownloadURL: "https://example.test/2.torrent"},
		{Title: "[G] abcabc146 S01E07 [1080p]", DownloadURL: "https://example.test/7.torrent"},
		// Weak tracker syntax must remain: presentation titles are not identity.
		{Title: "[G] abcabc146 - 99 [1080p]", DownloadURL: "https://example.test/weak.torrent"},
	}
	got := filterSearchEntries(entries, SearchRequest{Season: 1, EpisodeStart: 7, EpisodeEnd: 7, Targets: []SearchEpisode{{Season: 1, Episode: 7}}})
	if len(got) != 2 || got[0].Title != entries[1].Title || got[1].Title != entries[2].Title {
		t.Fatalf("filtered=%+v", got)
	}
}

func TestTargetedTorrentMetadataUsesMatchingFileFromMultiFileTorrent(t *testing.T) {
	meta := torrentmeta.Metadata{
		Name:  "abcabc144",
		Files: []string{"abcabc146/abcabc146 S01E06.mkv", "abcabc146/abcabc146 S01E07.mkv", "Subs/readme.txt"},
	}
	got := nativeSearchName(meta, SearchRequest{Season: 1, EpisodeStart: 7, EpisodeEnd: 7, Targets: []SearchEpisode{{Season: 1, Episode: 7}}})
	if got != "abcabc146 S01E07.mkv" {
		t.Fatalf("name=%q", got)
	}
}

type pruneMetadataFetcher struct{ hits atomic.Int32 }

func (f *pruneMetadataFetcher) Fetch(context.Context, string) ([]rss.Entry, error) {
	return []rss.Entry{
		{Title: "[G] abcabc146 S01E02 [1080p]", DownloadURL: "https://example.test/2.torrent"},
		{Title: "[G] abcabc146 S01E07 [1080p]", DownloadURL: "https://example.test/7.torrent"},
	}, nil
}
func (f *pruneMetadataFetcher) FetchTorrentMetadata(_ context.Context, raw string) (torrentmeta.Metadata, error) {
	f.hits.Add(1)
	if strings.Contains(raw, "/7.torrent") {
		return torrentmeta.Metadata{Name: "[G] abcabc146 S01E07 [1080p].mkv", InfoHash: "7123456789abcdef0123456789abcdef01234567"}, nil
	}
	return torrentmeta.Metadata{Name: "[G] abcabc146 S01E02 [1080p].mkv", InfoHash: "2123456789abcdef0123456789abcdef01234567"}, nil
}

func TestNyaaSearchPrunesExplicitWrongEpisodeBeforeTorrentMetadata(t *testing.T) {
	fetcher := &pruneMetadataFetcher{}
	p := nyaa{base: base{fetcher: fetcher}}
	values, err := p.Search(context.Background(), SearchRequest{Query: "abcabc146", Season: 1, EpisodeStart: 7, EpisodeEnd: 7, Targets: []SearchEpisode{{Season: 1, Episode: 7}}})
	if err != nil {
		t.Fatal(err)
	}
	if fetcher.hits.Load() != 1 {
		t.Fatalf("metadata hits=%d, want 1", fetcher.hits.Load())
	}
	if len(values) != 1 || values[0].Components.EpisodeStart != 7 {
		t.Fatalf("values=%+v", values)
	}
}

func TestTargetedTorrentMetadataRejectsMultiFileTorrentWithoutRequestedEpisode(t *testing.T) {
	meta := torrentmeta.Metadata{
		Name:  "abcabc144",
		Files: []string{"abcabc146/abcabc146 S01E01.mkv", "abcabc146/abcabc146 S01E02.mkv"},
	}
	got := nativeSearchName(meta, SearchRequest{Season: 1, EpisodeStart: 7, EpisodeEnd: 7, Targets: []SearchEpisode{{Season: 1, Episode: 7}}})
	if got != "" {
		t.Fatalf("name=%q, want empty because no native media path covers E07", got)
	}
}

func TestTargetedTorrentMetadataRejectsExplicitWrongSingleFileEpisode(t *testing.T) {
	meta := torrentmeta.Metadata{Name: "[G] abcabc146 S01E02 [1080p].mkv"}
	got := nativeSearchName(meta, SearchRequest{Season: 1, EpisodeStart: 7, EpisodeEnd: 7, Targets: []SearchEpisode{{Season: 1, Episode: 7}}})
	if got != "" {
		t.Fatalf("name=%q, want empty for explicit wrong episode", got)
	}
}

type weakTargetMetadataFetcher struct{}

func (weakTargetMetadataFetcher) Fetch(context.Context, string) ([]rss.Entry, error) {
	return []rss.Entry{{
		Title:       "abcabc125",
		GUID:        "weak-6",
		DownloadURL: "https://example.test/weak-6.torrent",
	}}, nil
}

func (weakTargetMetadataFetcher) FetchTorrentMetadata(context.Context, string) (torrentmeta.Metadata, error) {
	return torrentmeta.Metadata{
		Name:     "abcabc defdef ghighi - 06.mkv",
		InfoHash: "6123456789abcdef0123456789abcdef01234567",
	}, nil
}

func TestTargetedSearchPromotesMatchingWeakNativeEpisodeOnlyWithinSearchContext(t *testing.T) {
	fetcher := weakTargetMetadataFetcher{}
	p := genericRSS{base: base{fetcher: fetcher}, searchURLTemplate: "https://example.test/search?q={query}"}

	polled, err := p.Poll(context.Background(), "https://example.test/feed")
	if err != nil {
		t.Fatal(err)
	}
	if len(polled) != 1 || polled[0].MediaType != "movie" || polled[0].Components.EpisodeEvidence != "weak" {
		t.Fatalf("untargeted weak release should remain conservative: %+v", polled)
	}

	searched, err := p.Search(context.Background(), SearchRequest{
		Query:        "abcabc defdef ghighi",
		Season:       1,
		EpisodeStart: 6,
		EpisodeEnd:   6,
		Targets:      []SearchEpisode{{Season: 1, Episode: 6}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(searched) != 1 {
		t.Fatalf("searched=%+v", searched)
	}
	got := searched[0]
	if got.MediaType != "series" || got.Components.EpisodeStart != 6 || got.Components.EpisodeEvidence != "weak" {
		t.Fatalf("targeted native E06 was not promoted safely: %+v", got)
	}
	if got.NameSource != "torrent-info" {
		t.Fatalf("name source=%q, want torrent-info", got.NameSource)
	}
}

func TestQueryWithEpisodePreservesExplicitSeasonZero(t *testing.T) {
	got := queryWithEpisode(SearchRequest{Query: "abcabc146", Season: 0, SeasonExplicit: true, EpisodeStart: 3}, true)
	if got != "abcabc146 S00E03" {
		t.Fatalf("query=%q want explicit S00", got)
	}
	seasonless := queryWithEpisode(SearchRequest{Query: "abcabc146", EpisodeStart: 3}, true)
	if seasonless != "abcabc146 S01E03" {
		t.Fatalf("seasonless query=%q want default S01", seasonless)
	}
}
