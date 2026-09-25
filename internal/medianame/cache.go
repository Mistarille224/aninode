package medianame

import "sync"

// parseCache stores only context-free parser results. It is deliberately
// disposable: parser output is a pure function of the stem, so dropping the
// cache changes performance only, never semantics or recoverability.
//
// The cache uses a coarse bounded reset instead of an LRU. Media libraries
// repeatedly parse a relatively small working set, while an LRU would add a
// linked-list mutation and lock write on every hit. Resetting at the bound
// keeps memory bounded and makes the common hit path read-only.
const parseCacheLimit = 8192

type parserCache struct {
	mu     sync.RWMutex
	values map[string]Parsed
}

var contextFreeParseCache = parserCache{values: make(map[string]Parsed, parseCacheLimit)}

func (c *parserCache) get(stem string) (Parsed, bool) {
	c.mu.RLock()
	value, ok := c.values[stem]
	c.mu.RUnlock()
	return value, ok
}

func (c *parserCache) put(stem string, value Parsed) {
	c.mu.Lock()
	if len(c.values) >= parseCacheLimit {
		// This is a cache, not state. A full reset has trivial correctness
		// semantics and avoids maintaining eviction metadata.
		c.values = make(map[string]Parsed, parseCacheLimit)
	}
	c.values[stem] = value
	c.mu.Unlock()
}

func resetParseCacheForTest() {
	contextFreeParseCache.mu.Lock()
	contextFreeParseCache.values = make(map[string]Parsed, parseCacheLimit)
	contextFreeParseCache.mu.Unlock()
}
