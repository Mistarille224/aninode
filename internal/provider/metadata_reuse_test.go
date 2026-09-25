package provider

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"aninode/internal/rss"
	"aninode/internal/torrentmeta"
)

type metadataRoundTrip func(*http.Request) (*http.Response, error)

func (f metadataRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestDMHYRefreshAndSearchReuseNativeMetadata(t *testing.T) {
	const name = "[grpabc165] abcabc146 S01E07 1080p.mkv"
	body := fmt.Sprintf("d4:infod6:lengthi1e4:name%d:%see", len(name), name)
	meta, err := torrentmeta.Parse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	hits := map[string]int{}
	f := rss.NewFetcher()
	f.Client = &http.Client{Transport: metadataRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.Scheme != "https" {
			t.Errorf("unexpected HTTP request: %s", r.URL)
		}
		mu.Lock()
		hits[r.URL.Path]++
		mu.Unlock()
		var data string
		switch r.URL.Path {
		case "/topics/rss/rss.xml":
			data = fmt.Sprintf(`<rss><channel><item><title>abcabc146 S01E07</title><link>http://share.dmhy.org/topics/view/123.html</link><enclosure url="magnet:?xt=urn:btih:%s"/></item></channel></rss>`, meta.InfoHash)
		case "/topics/view/123.html":
			data = `<a href="http://dl.dmhy.org/a.torrent">download</a>`
		case "/a.torrent":
			data = body
		default:
			return nil, fmt.Errorf("unexpected URL: %s", r.URL)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(data))}, nil
	})}
	p := dmhy{base: base{fetcher: f}}
	first, err := p.Poll(context.Background(), "http://share.dmhy.org/topics/rss/rss.xml")
	if err != nil {
		t.Fatal(err)
	}
	second, err := p.Poll(context.Background(), "http://share.dmhy.org/topics/rss/rss.xml")
	if err != nil {
		t.Fatal(err)
	}
	searched, err := p.Search(context.Background(), SearchRequest{Query: "abcabc146", EpisodeStart: 7})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) || !reflect.DeepEqual(first, searched) || len(first) != 1 || first[0].MediaName != name {
		t.Fatalf("results changed: %+v %+v %+v", first, second, searched)
	}
	if hits["/topics/rss/rss.xml"] != 3 || hits["/topics/view/123.html"] != 1 || hits["/a.torrent"] != 1 {
		t.Fatalf("hits=%v", hits)
	}
}

func TestNyaaAndMikanReuseMetadataWithoutPacingCacheHits(t *testing.T) {
	for _, kind := range []string{"nyaa", "mikan"} {
		t.Run(kind, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := rss.NewFetcher()
				var mu sync.Mutex
				feedHits, torrentHits := 0, 0
				f.Client = &http.Client{Transport: metadataRoundTrip(func(r *http.Request) (*http.Response, error) {
					mu.Lock()
					defer mu.Unlock()
					var body string
					if r.URL.Path == "/feed" {
						feedHits++
						body = `<rss><channel><item><title>abcabc146 01</title><enclosure url="https://example.test/1.torrent"/></item><item><title>abcabc146 02</title><enclosure url="https://example.test/2.torrent"/></item></channel></rss>`
					} else {
						torrentHits++
						name := "[G] abcabc146 S01E0" + strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/"), ".torrent") + ".mkv"
						body = fmt.Sprintf("d4:infod6:lengthi1e4:name%d:%see", len(name), name)
					}
					return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
				})}
				p, err := (Registry{Fetcher: f}).New(kind, Options{})
				if err != nil {
					t.Fatal(err)
				}
				first, err := p.Poll(context.Background(), "https://example.test/feed")
				if err != nil {
					t.Fatal(err)
				}
				start := time.Now()
				second, err := p.Poll(context.Background(), "https://example.test/feed")
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(first, second) || torrentHits != 2 || feedHits != 2 || time.Since(start) != 0 {
					t.Fatalf("cold/warm differ, torrents=%d feeds=%d elapsed=%s", torrentHits, feedHits, time.Since(start))
				}
			})
		})
	}
}

func TestDMHYHTTPSURLPreservesCustomOrigins(t *testing.T) {
	for _, tt := range []struct{ in, want string }{
		{"http://share.dmhy.org/topics/view/1?a=b", "https://share.dmhy.org/topics/view/1?a=b"},
		{"http://dl.dmhy.org/a.torrent", "https://dl.dmhy.org/a.torrent"},
		{"http://mirror.test/a", "http://mirror.test/a"},
		{"http://share.dmhy.org:8080/a", "http://share.dmhy.org:8080/a"},
		{"http://user@share.dmhy.org/a", "http://user@share.dmhy.org/a"},
		{"https://share.dmhy.org/a", "https://share.dmhy.org/a"},
		{"http://share.dmhy.org.evil.test/a", "http://share.dmhy.org.evil.test/a"},
	} {
		t.Run(tt.in, func(t *testing.T) {
			if got := dmhyHTTPSURL(tt.in); got != tt.want {
				t.Fatalf("got %q want %q", got, tt.want)
			}
		})
	}
}

func TestCachedMetadataStillSelectsRequestedNativeEpisode(t *testing.T) {
	const first = "[G] abcabc146 S01E01.mkv"
	const second = "[G] abcabc146 S01E02.mkv"
	body := fmt.Sprintf("d4:infod4:name4:Show5:filesld6:lengthi1e4:pathl%d:%seed6:lengthi1e4:pathl%d:%seeeee", len(first), first, len(second), second)
	f := rss.NewFetcher()
	hits := 0
	f.Client = &http.Client{Transport: metadataRoundTrip(func(*http.Request) (*http.Response, error) {
		hits++
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	entries := []rss.Entry{{DownloadURL: "https://example.test/pack.torrent"}}
	for _, tt := range []struct {
		episode int
		name    string
	}{{1, first}, {2, second}, {3, ""}} {
		observed, err := resolveMediaNames(context.Background(), f, entries, SearchRequest{EpisodeStart: tt.episode})
		if err != nil || len(observed) != 1 || observed[0].Value != tt.name {
			t.Fatalf("episode=%d observed=%+v err=%v", tt.episode, observed, err)
		}
	}
	if hits != 1 {
		t.Fatalf("torrent reads=%d", hits)
	}
}
