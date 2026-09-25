package rss

import (
	"context"
	"net/http"
	"slices"
	"sync"
	"time"

	"aninode/internal/torrentmeta"
)

const metadataTTL = 6 * time.Hour
const metadataCacheEntries = 2048
const metadataCacheBytes = 16 << 20

type cachedMetadata struct {
	value   torrentmeta.Metadata
	expires time.Time
	size    int
}

type metadataFlight struct {
	done  chan struct{}
	value torrentmeta.Metadata
	err   error
}

// metadataCache contains only successfully parsed native metainfo. It is never
// media availability evidence. URL entries expire even if they remain popular;
// infohash lookups reuse only the hash computed from the actual info dictionary.
type metadataCache struct {
	mu      sync.Mutex
	entries map[string]cachedMetadata
	byHash  map[string]string
	flights map[string]*metadataFlight
	bytes   int
}

// NewFetcher creates a fetcher with a bounded, disposable metainfo cache shared
// by RSS and search. The zero-value Fetcher remains usable without caching.
func NewFetcher() Fetcher {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// Metadata enrichment is intentionally concurrent. Keep enough idle sockets
	// to reuse those connections on the next RSS/search pass instead of paying
	// repeated TCP/TLS setup costs (the standard per-host idle limit is two).
	transport.MaxIdleConns = max(transport.MaxIdleConns, 64)
	transport.MaxIdleConnsPerHost = max(transport.MaxIdleConnsPerHost, 16)
	return Fetcher{
		Client:   &http.Client{Timeout: 30 * time.Second, Transport: transport},
		cooldown: &requestCooldown{hosts: make(map[string]time.Time)},
		metadata: &metadataCache{entries: make(map[string]cachedMetadata), byHash: make(map[string]string), flights: make(map[string]*metadataFlight)},
		feeds:    &feedCache{entries: make(map[string]cachedFeed)},
	}
}

func cloneMetadata(value torrentmeta.Metadata) torrentmeta.Metadata {
	value.Files = slices.Clone(value.Files)
	return value
}

// CachedTorrentMetadata returns unexpired metainfo for this exact download URL.
func (fetcher Fetcher) CachedTorrentMetadata(rawURL string) (torrentmeta.Metadata, bool) {
	c := fetcher.metadata
	if c == nil {
		return torrentmeta.Metadata{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lookup(rawURL)
}

func (c *metadataCache) lookup(rawURL string) (torrentmeta.Metadata, bool) {
	entry, ok := c.entries[rawURL]
	if !ok {
		return torrentmeta.Metadata{}, false
	}
	if !time.Now().Before(entry.expires) {
		c.remove(rawURL)
		return torrentmeta.Metadata{}, false
	}
	return cloneMetadata(entry.value), true
}

// CachedTorrentMetadataByHash reuses metainfo only by its computed native hash,
// allowing a DMHY magnet to avoid both the detail page and torrent download.
func (fetcher Fetcher) CachedTorrentMetadataByHash(infoHash string) (torrentmeta.Metadata, bool) {
	c := fetcher.metadata
	if c == nil || infoHash == "" {
		return torrentmeta.Metadata{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	key, ok := c.byHash[infoHash]
	if !ok {
		return torrentmeta.Metadata{}, false
	}
	value, ok := c.lookup(key)
	if !ok {
		delete(c.byHash, infoHash)
	}
	return value, ok
}

func (c *metadataCache) remove(key string) {
	entry, ok := c.entries[key]
	if !ok {
		return
	}
	c.bytes -= entry.size
	delete(c.entries, key)
	if entry.value.InfoHash != "" && c.byHash[entry.value.InfoHash] == key {
		delete(c.byHash, entry.value.InfoHash)
	}
}

func (c *metadataCache) put(key string, value torrentmeta.Metadata) {
	size := len(key) + len(value.Name) + len(value.InfoHash) + 128
	for _, path := range value.Files {
		size += len(path) + 16
	}
	if size > metadataCacheBytes {
		return
	}
	c.remove(key)
	for len(c.entries) >= metadataCacheEntries || c.bytes+size > metadataCacheBytes {
		var oldest string
		var expires time.Time
		for k, entry := range c.entries {
			if oldest == "" || entry.expires.Before(expires) || (entry.expires.Equal(expires) && k < oldest) {
				oldest, expires = k, entry.expires
			}
		}
		c.remove(oldest)
	}
	c.entries[key] = cachedMetadata{value: cloneMetadata(value), expires: time.Now().Add(metadataTTL), size: size}
	if value.InfoHash != "" {
		c.byHash[value.InfoHash] = key
	}
	c.bytes += size
}

// FetchTorrentMetadata coalesces concurrent reads of the same URL. A cancelled
// waiter leaves the owning request alone; errors are shared only with current
// waiters and never retained as successful metadata.
func (fetcher Fetcher) FetchTorrentMetadata(ctx context.Context, rawURL string) (torrentmeta.Metadata, error) {
	if err := ctx.Err(); err != nil {
		return torrentmeta.Metadata{}, err
	}
	c := fetcher.metadata
	if c == nil {
		return fetcher.fetchTorrentMetadata(ctx, rawURL)
	}
	c.mu.Lock()
	if value, ok := c.lookup(rawURL); ok {
		c.mu.Unlock()
		return value, nil
	}
	if flight, ok := c.flights[rawURL]; ok {
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return torrentmeta.Metadata{}, ctx.Err()
		case <-flight.done:
			return cloneMetadata(flight.value), flight.err
		}
	}
	flight := &metadataFlight{done: make(chan struct{})}
	c.flights[rawURL] = flight
	c.mu.Unlock()
	value, err := fetcher.fetchTorrentMetadata(ctx, rawURL)
	c.mu.Lock()
	if err == nil {
		c.put(rawURL, value)
	}
	flight.value, flight.err = cloneMetadata(value), err
	delete(c.flights, rawURL)
	close(flight.done)
	c.mu.Unlock()
	return value, err
}
