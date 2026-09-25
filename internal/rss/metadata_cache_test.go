package rss

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"aninode/internal/torrentmeta"
)

const cacheTestTorrent = "d4:infod4:name4:Show5:filesld6:lengthi1e4:pathl8:file.mkveeeee"

func cacheTestFetcher(t *testing.T, body string) (Fetcher, *atomic.Int32) {
	t.Helper()
	calls := &atomic.Int32{}
	f := NewFetcher()
	f.Client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	return f, calls
}

func TestMetadataCacheExpiryAndIsolation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f, calls := cacheTestFetcher(t, cacheTestTorrent)
		first, err := f.FetchTorrentMetadata(context.Background(), "https://example.test/a.torrent")
		if err != nil {
			t.Fatal(err)
		}
		original := cloneMetadata(first)
		first.Files[0] = "mutated"
		byHash, ok := f.CachedTorrentMetadataByHash(original.InfoHash)
		if !ok || !reflect.DeepEqual(byHash, original) {
			t.Fatalf("hash lookup=%+v", byHash)
		}
		byHash.Files[0] = "also-mutated"
		second, err := f.FetchTorrentMetadata(context.Background(), "https://example.test/a.torrent")
		if err != nil || !reflect.DeepEqual(second, original) || calls.Load() != 1 {
			t.Fatalf("second=%+v err=%v calls=%d", second, err, calls.Load())
		}
		if _, ok := f.CachedTorrentMetadataByHash(strings.Repeat("0", 40)); ok {
			t.Fatal("unrelated hash hit")
		}
		time.Sleep(metadataTTL)
		if _, ok := f.CachedTorrentMetadataByHash(original.InfoHash); ok {
			t.Fatal("expired hash hit")
		}
		if _, err := f.FetchTorrentMetadata(context.Background(), "https://example.test/a.torrent"); err != nil {
			t.Fatal(err)
		}
		if calls.Load() != 2 {
			t.Fatal("expired URL was not fetched")
		}
	})
}

func TestMetadataCacheCoalescesAndWaiterCanCancel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := NewFetcher()
		calls := 0
		release := make(chan struct{})
		f.Client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			calls++
			<-release
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(cacheTestTorrent))}, nil
		})}
		var wg sync.WaitGroup
		var got [8]torrentmeta.Metadata
		var errs [8]error
		for i := range got {
			wg.Go(func() {
				got[i], errs[i] = f.FetchTorrentMetadata(context.Background(), "https://example.test/a.torrent")
			})
		}
		synctest.Wait()
		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan error, 1)
		go func() { _, err := f.FetchTorrentMetadata(ctx, "https://example.test/a.torrent"); result <- err }()
		synctest.Wait()
		cancel()
		if err := <-result; !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		close(release)
		wg.Wait()
		if calls != 1 {
			t.Fatalf("requests=%d", calls)
		}
		for i := range got {
			if errs[i] != nil || !reflect.DeepEqual(got[i], got[0]) {
				t.Fatalf("result %d: %+v %v", i, got[i], errs[i])
			}
		}
	})
}

func TestMetadataCacheDoesNotKeepFailures(t *testing.T) {
	for _, body := range []string{"invalid torrent", "d4:infoee"} {
		t.Run(body, func(t *testing.T) {
			f, calls := cacheTestFetcher(t, body)
			for range 2 {
				if _, err := f.FetchTorrentMetadata(context.Background(), "https://example.test/a.torrent"); err == nil {
					t.Fatal("expected parse error")
				}
			}
			if calls.Load() != 2 || len(f.metadata.entries) != 0 {
				t.Fatal("failed parsing was cached")
			}
		})
	}
}

func TestMetadataCacheBounds(t *testing.T) {
	f := NewFetcher()
	for i := 0; i < metadataCacheEntries+1; i++ {
		f.metadata.put(fmt.Sprintf("https://example.test/%04d", i), torrentmeta.Metadata{Name: "abcabc146", InfoHash: fmt.Sprint(i)})
	}
	if len(f.metadata.entries) != metadataCacheEntries {
		t.Fatal("entry bound exceeded")
	}
	if _, ok := f.CachedTorrentMetadata("https://example.test/0000"); ok {
		t.Fatal("oldest entry not evicted")
	}
	oversized := torrentmeta.Metadata{Name: strings.Repeat("x", metadataCacheBytes+1)}
	f.metadata.put("oversized", oversized)
	if _, ok := f.CachedTorrentMetadata("oversized"); ok {
		t.Fatal("oversized metadata retained")
	}
	for i := range 20 {
		f.metadata.put(fmt.Sprint(i), torrentmeta.Metadata{Name: strings.Repeat("x", 1<<20)})
	}
	if f.metadata.bytes > metadataCacheBytes {
		t.Fatal("byte bound exceeded")
	}
}

func TestMetadataCacheCancellationDoesNotBecomeSuccess(t *testing.T) {
	f := NewFetcher()
	calls := 0
	f.Client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return nil, context.Canceled
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(cacheTestTorrent))}, nil
	})}
	if _, err := f.FetchTorrentMetadata(context.Background(), "https://example.test/a.torrent"); !errors.Is(err, context.Canceled) {
		t.Fatalf("lost cancellation: %v", err)
	}
	if _, err := f.FetchTorrentMetadata(context.Background(), "https://example.test/a.torrent"); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatal("cancelled request was cached")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.FetchTorrentMetadata(ctx, "https://example.test/a.torrent"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cache ignored cancellation: %v", err)
	}
}

func TestNewFetcherKeepsConcurrentProviderConnectionsWarm(t *testing.T) {
	f := NewFetcher()
	if f.Client == nil {
		t.Fatal("NewFetcher returned nil HTTP client")
	}
	transport, ok := f.Client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport=%T", f.Client.Transport)
	}
	if transport.MaxIdleConns < 64 || transport.MaxIdleConnsPerHost < 16 {
		t.Fatalf("idle pool too small: total=%d per_host=%d", transport.MaxIdleConns, transport.MaxIdleConnsPerHost)
	}
}
