package rss

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestRateLimitCooldownSharedAcrossReadKinds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := NewFetcher()
		calls := 0
		f.Client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			calls++
			if calls == 1 {
				return &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": []string{"120"}}, Body: io.NopCloser(strings.NewReader("limited"))}, nil
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(cacheTestTorrent))}, nil
		})}
		_, err := f.FetchPage(context.Background(), "https://example.test/page")
		var status *HTTPStatusError
		if !errors.As(err, &status) || status.StatusCode != 429 {
			t.Fatal(err)
		}
		_, err = f.Fetch(context.Background(), "https://example.test/feed")
		if !errors.As(err, &status) || calls != 1 {
			t.Fatalf("feed cooldown: %v calls=%d", err, calls)
		}
		_, err = f.FetchTorrentMetadata(context.Background(), "https://example.test/a.torrent")
		if !errors.As(err, &status) || calls != 1 {
			t.Fatalf("torrent cooldown: %v calls=%d", err, calls)
		}
		if _, err = f.FetchTorrentMetadata(context.Background(), "https://other.test/a.torrent"); err != nil {
			t.Fatal(err)
		}
		time.Sleep(120 * time.Second)
		if _, err = f.FetchTorrentMetadata(context.Background(), "https://example.test/a.torrent"); err != nil {
			t.Fatal(err)
		}
		if calls != 3 {
			t.Fatalf("calls=%d", calls)
		}
	})
}

func TestRateLimitDeadline(t *testing.T) {
	now := time.Date(2026, 9, 19, 0, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name, value string
		want        time.Duration
	}{
		{"seconds", "120", 120 * time.Second},
		{"date", now.Add(2 * time.Minute).Format(http.TimeFormat), 2 * time.Minute},
		{"missing", "", time.Minute},
		{"invalid", "oops", time.Minute},
		{"negative", "-3", time.Minute},
		{"overflow", "9223372036854775807", time.Minute},
		{"past", now.Add(-time.Hour).Format(http.TimeFormat), time.Minute},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := rateLimitDeadline(tt.value, now).Sub(now); got != tt.want {
				t.Fatalf("got %s want %s", got, tt.want)
			}
		})
	}
}
