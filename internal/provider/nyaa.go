package provider

import (
	"aninode/internal/backfill"
	"aninode/internal/release"
	"aninode/internal/rss"
	"aninode/internal/torrentmeta"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

type nyaa struct{ base }

func (nyaa) Name() string             { return "nyaa" }
func (nyaa) Capabilities() Capability { return Capability{RSS: true, Search: true, Historical: true} }
func (p nyaa) Poll(ctx context.Context, url string) ([]release.Release, error) {
	if strings.TrimSpace(url) == "" {
		return nil, backfill.ErrUnsupported
	}
	return p.fetch(ctx, p.Name(), url)
}
func (p nyaa) Search(ctx context.Context, r SearchRequest) ([]release.Release, error) {
	q := queryWithEpisode(r, false)
	if q == "" {
		return nil, errors.New("search query is required")
	}
	ep, e := endpoint("https://nyaa.si/?page=rss&q={query}", q)
	if e != nil {
		return nil, e
	}
	return p.fetchSearch(ctx, p.Name(), ep, r)
}

// nyaaMetadataFetcher spaces native metadata reads within one observation.
// A rate-limit response stops queued reads; another observation can try again.
// Keep native torrent names authoritative even when enrichment is incomplete.
type nyaaMetadataFetcher struct {
	Fetcher
	resolver torrentMetadataFetcher
	mu       sync.Mutex
	next     time.Time
	limited  error
}

func (f *nyaaMetadataFetcher) FetchTorrentMetadata(ctx context.Context, rawURL string) (torrentmeta.Metadata, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return torrentmeta.Metadata{}, err
	}
	if meta, ok := f.CachedTorrentMetadata(rawURL); ok {
		return meta, nil
	}
	if f.limited != nil {
		return torrentmeta.Metadata{}, fmt.Errorf("skipped after Nyaa rate limit: %w", f.limited)
	}
	if delay := time.Until(f.next); delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return torrentmeta.Metadata{}, ctx.Err()
		case <-timer.C:
		}
	}
	meta, err := f.resolver.FetchTorrentMetadata(ctx, rawURL)
	f.next = time.Now().Add(time.Second)
	var status *rss.HTTPStatusError
	if errors.As(err, &status) && status.StatusCode == http.StatusTooManyRequests {
		f.limited = err
	}
	return meta, err
}

func (f *nyaaMetadataFetcher) CachedTorrentMetadata(rawURL string) (torrentmeta.Metadata, bool) {
	if cache, ok := f.resolver.(metadataCacheReader); ok {
		return cache.CachedTorrentMetadata(rawURL)
	}
	return torrentmeta.Metadata{}, false
}
