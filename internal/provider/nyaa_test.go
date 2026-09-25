package provider

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	"aninode/internal/rss"
	"aninode/internal/torrentmeta"
)

type nyaaTestMetadata struct {
	calls  []time.Time
	failAt int
}

func (f *nyaaTestMetadata) FetchTorrentMetadata(context.Context, string) (torrentmeta.Metadata, error) {
	f.calls = append(f.calls, time.Now())
	if len(f.calls) == f.failAt {
		return torrentmeta.Metadata{}, &rss.HTTPStatusError{StatusCode: http.StatusTooManyRequests}
	}
	return torrentmeta.Metadata{Name: "abcabc145 S01E01.mkv"}, nil
}

func TestNyaaMetadataPacingAndRateLimit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		resolver := &nyaaTestMetadata{failAt: 3}
		f := &nyaaMetadataFetcher{resolver: resolver}
		entries := make([]rss.Entry, 20)
		for i := range entries {
			entries[i].DownloadURL = "https://example.test/file.torrent"
		}
		names, err := resolveMediaNames(context.Background(), f, entries, SearchRequest{})
		var status *rss.HTTPStatusError
		if !errors.As(err, &status) || status.StatusCode != 429 {
			t.Fatalf("lost rate limit: %v", err)
		}
		if len(resolver.calls) != 3 {
			t.Fatalf("made %d requests", len(resolver.calls))
		}
		for i := 1; i < len(resolver.calls); i++ {
			if resolver.calls[i].Sub(resolver.calls[i-1]) < time.Second {
				t.Fatal("unpaced requests")
			}
		}
		successes := 0
		for _, name := range names {
			if name.Value != "" {
				successes++
			}
		}
		if successes != 2 {
			t.Fatalf("retained %d successful names", successes)
		}
	})
}

func TestNyaaMetadataCancellationDuringSpacing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		resolver := &nyaaTestMetadata{}
		f := &nyaaMetadataFetcher{resolver: resolver}
		if _, err := f.FetchTorrentMetadata(context.Background(), "first"); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
		defer cancel()
		_, err := f.FetchTorrentMetadata(ctx, "second")
		if !errors.Is(err, context.DeadlineExceeded) || len(resolver.calls) != 1 {
			t.Fatalf("calls=%d err=%v", len(resolver.calls), err)
		}
	})
}
