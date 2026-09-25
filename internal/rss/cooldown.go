package rss

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// HTTPStatusError preserves remote status and rate-limit timing for callers.
type HTTPStatusError struct {
	Resource   string
	StatusCode int
	RetryAt    time.Time
}

func (e *HTTPStatusError) Error() string {
	resource := e.Resource
	if resource == "" {
		resource = "request"
	}
	if !e.RetryAt.IsZero() {
		return fmt.Sprintf("%s returned HTTP %d; retry after %s", resource, e.StatusCode, e.RetryAt.UTC().Format(time.RFC3339))
	}
	return fmt.Sprintf("%s returned HTTP %d", resource, e.StatusCode)
}

type requestCooldown struct {
	mu    sync.Mutex
	hosts map[string]time.Time
}

// readRequest does not retry 429 responses. All reads through the application
// fetcher share a host cooldown, so queued work and immediate manual refreshes
// cannot amplify a rate limit. Nothing is scheduled or persisted here.
func (fetcher Fetcher) readRequest(ctx context.Context, client *http.Client, request *http.Request) (*http.Response, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c := fetcher.cooldown
	host := strings.ToLower(request.URL.Host)
	if c != nil {
		c.mu.Lock()
		retryAt := c.hosts[host]
		c.mu.Unlock()
		if time.Now().Before(retryAt) {
			return nil, &HTTPStatusError{StatusCode: http.StatusTooManyRequests, RetryAt: retryAt}
		}
	}
	response, err := doReadRequest(ctx, client, request)
	if err != nil || response.StatusCode != http.StatusTooManyRequests {
		return response, err
	}
	retryAt := rateLimitDeadline(response.Header.Get("Retry-After"), time.Now())
	if c != nil {
		c.mu.Lock()
		// Bound memory even with many custom RSS origins.
		for key, until := range c.hosts {
			if !time.Now().Before(until) {
				delete(c.hosts, key)
			}
		}
		if _, exists := c.hosts[host]; !exists && len(c.hosts) >= 128 {
			var oldest string
			for key, until := range c.hosts {
				if oldest == "" || until.Before(c.hosts[oldest]) || (until.Equal(c.hosts[oldest]) && key < oldest) {
					oldest = key
				}
			}
			delete(c.hosts, oldest)
		}
		if retryAt.After(c.hosts[host]) {
			c.hosts[host] = retryAt
		}
		c.mu.Unlock()
	}
	response.Body.Close()
	return nil, &HTTPStatusError{StatusCode: http.StatusTooManyRequests, RetryAt: retryAt}
}

func rateLimitDeadline(value string, now time.Time) time.Time {
	if seconds, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err == nil && seconds >= 0 && seconds <= int64((1<<63-1)/time.Second) {
		return now.Add(time.Duration(seconds) * time.Second)
	}
	if date, err := http.ParseTime(value); err == nil && date.After(now) {
		return date
	}
	return now.Add(time.Minute)
}
