package rss

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestFetcherRedactsURLCredentialsFromRequestErrors(t *testing.T) {
	fetcher := Fetcher{Client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, context.DeadlineExceeded
	})}}
	_, err := fetcher.Fetch(context.Background(), "https://user:password@example.test/rss?token=secret&view=full")
	if err == nil {
		t.Fatal("Fetch succeeded")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error lost timeout identity: %v", err)
	}
	message := err.Error()
	for _, secret := range []string{"password", "secret", "token="} {
		if strings.Contains(message, secret) {
			t.Fatalf("error leaked %q: %s", secret, message)
		}
	}
}

func TestFetcherRetriesOneTransientGatewayFailure(t *testing.T) {
	calls := 0
	fetcher := Fetcher{Client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return &http.Response{StatusCode: http.StatusGatewayTimeout, Body: io.NopCloser(strings.NewReader("gateway timeout"))}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`<rss><channel></channel></rss>`))}, nil
	})}}
	entries, err := fetcher.Fetch(context.Background(), "https://example.test/rss")
	if err != nil || len(entries) != 0 || calls != 2 {
		t.Fatalf("entries=%+v calls=%d err=%v", entries, calls, err)
	}
}

func TestParseRSS(t *testing.T) {
	input := `<?xml version="1.0"?><rss version="2.0" xmlns:torrent="urn:test"><channel><item><title>[grpabc165] abcabc146 [01][1080p]</title><guid>item-1</guid><link>https://example.test/item/1</link><pubDate>Sun, 10 Aug 2026 12:00:00 +0800</pubDate><enclosure url="https://example.test/1.torrent"/><torrent:magnetURI>magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567</torrent:magnetURI></item></channel></rss>`
	entries, err := Parse(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || !strings.HasPrefix(entries[0].DownloadURL, "magnet:") || entries[0].PublishedAt.IsZero() {
		t.Fatalf("entries = %+v", entries)
	}
}

func TestParseMikanMyBangumiRSS(t *testing.T) {
	input := `<?xml version="1.0" encoding="utf-8"?><rss version="2.0"><channel><title>abcabc - 甲乙丙丁</title><item><guid isPermaLink="false">[grpabc166] abcabc - 44 [1080P]</guid><link>https://mikanani.me/Home/Episode/id</link><title>[grpabc166] abcabc - 44 [1080P]</title><description>release</description><torrent xmlns="https://mikanani.me/0.1/"><link>https://mikanani.me/Home/Episode/id</link><contentLength>123</contentLength><pubDate>2026-08-19T22:00:49.519445</pubDate></torrent><enclosure type="application/x-bittorrent" length="123" url="https://mikanani.me/Download/20260819/id.torrent" /></item></channel></rss>`
	entries, err := Parse(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Title != "[grpabc166] abcabc - 44 [1080P]" || entries[0].DownloadURL != "https://mikanani.me/Download/20260819/id.torrent" {
		t.Fatalf("entries=%+v", entries)
	}
}

func TestParseAtom(t *testing.T) {
	input := `<feed xmlns="http://www.w3.org/2005/Atom"><entry><title>abcabc146 01</title><id>tag:example,1</id><updated>2026-08-10T04:00:00Z</updated><link rel="alternate" href="https://example.test/1"/><link rel="enclosure" href="https://example.test/1.torrent"/></entry></feed>`
	entries, err := Parse(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Link != "https://example.test/1" || entries[0].DownloadURL != "https://example.test/1.torrent" {
		t.Fatalf("entries = %+v", entries)
	}
}

func TestTorrentRateLimitPreservesStatusWithoutRetry(t *testing.T) {
	calls := 0
	fetcher := Fetcher{Client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: http.StatusTooManyRequests, Body: io.NopCloser(strings.NewReader("limited")), Header: http.Header{"Retry-After": []string{"60"}}}, nil
	})}}
	_, err := fetcher.FetchTorrentMetadata(context.Background(), "https://example.test/file.torrent")
	var status *HTTPStatusError
	if !errors.As(err, &status) || status.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("lost HTTP status: %v", err)
	}
	if calls != 1 {
		t.Fatalf("retried rate-limited request: %d calls", calls)
	}
}

func TestFetchUsesHTTPValidatorsWithoutMakingCacheAuthoritative(t *testing.T) {
	var calls int
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Etag": {`"v1"`}}, Body: io.NopCloser(strings.NewReader(`<?xml version="1.0"?><rss version="2.0"><channel><item><title>abcabc146 E01</title><guid>one</guid></item></channel></rss>`))}, nil
		}
		if got := r.Header.Get("If-None-Match"); got != `"v1"` {
			t.Fatalf("If-None-Match=%q", got)
		}
		return &http.Response{StatusCode: http.StatusNotModified, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}, nil
	})}
	fetcher := Fetcher{Client: client, feeds: &feedCache{entries: make(map[string]cachedFeed)}}
	first, err := fetcher.Fetch(context.Background(), "https://example.invalid/feed")
	if err != nil || len(first) != 1 {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	second, err := fetcher.Fetch(context.Background(), "https://example.invalid/feed")
	if err != nil || len(second) != 1 || second[0].GUID != "one" {
		t.Fatalf("second=%+v err=%v", second, err)
	}
	second[0].GUID = "mutated"
	third, ok := fetcher.feeds.notModified("https://example.invalid/feed")
	if !ok || third[0].GUID != "one" {
		t.Fatalf("cached representation was mutated through caller: %+v", third)
	}
}
