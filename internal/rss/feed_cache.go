package rss

import (
	"net/http"
	"slices"
	"sync"
)

const feedCacheEntries = 128

type cachedFeed struct {
	etag         string
	lastModified string
	entries      []Entry
}

// feedCache stores validators and the last successfully parsed representation.
// The origin remains authoritative: a hit is used only after the server answers
// 304 Not Modified. Deleting this cache merely turns the next request into a
// full 200 response.
type feedCache struct {
	mu      sync.Mutex
	entries map[string]cachedFeed
}

func (c *feedCache) validators(rawURL string) (string, string) {
	if c == nil {
		return "", ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry := c.entries[rawURL]
	return entry.etag, entry.lastModified
}

func (c *feedCache) notModified(rawURL string) ([]Entry, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[rawURL]
	if !ok {
		return nil, false
	}
	return slices.Clone(entry.entries), true
}

func (c *feedCache) put(rawURL string, header http.Header, entries []Entry) {
	if c == nil {
		return
	}
	etag, modified := header.Get("ETag"), header.Get("Last-Modified")
	if etag == "" && modified == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= feedCacheEntries {
		// Validators are an optimization, not state. A coarse reset is cheaper
		// and simpler than maintaining eviction order for a tiny source set.
		c.entries = make(map[string]cachedFeed, feedCacheEntries)
	}
	c.entries[rawURL] = cachedFeed{etag: etag, lastModified: modified, entries: slices.Clone(entries)}
}
