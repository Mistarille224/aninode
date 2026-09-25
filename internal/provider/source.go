package provider

import (
	"aninode/internal/backfill"
	"aninode/internal/configstore"
	"aninode/internal/release"
	"context"
	"errors"
	"fmt"
)

// Source binds durable source configuration to a provider adapter. This is the
// only provider-layer type aware of configstore; concrete providers are pure
// remote adapters and know nothing about aninode persistence.
type Source struct {
	Config   configstore.ContentSource
	Provider Provider
}

func NewSource(cfg configstore.ContentSource, fetcher Fetcher) (Source, error) {
	options := Options{}
	if cfg.Provider == "generic" && cfg.Search != nil {
		options.SearchURLTemplate = cfg.Search.URLTemplate
	}
	p, err := (Registry{Fetcher: fetcher}).New(cfg.Provider, options)
	return Source{Config: cfg, Provider: p}, err
}
func (s Source) Capabilities() Capability {
	if s.Provider == nil {
		return Capability{}
	}
	c := s.Provider.Capabilities()
	if s.Config.Provider == "generic" {
		c.RSS = c.RSS && len(s.Config.RSS) > 0
	}
	return c
}
func (s Source) HistoricalCapable() bool { return s.Capabilities().Historical }
func (s Source) DiscoveryCapable() bool  { return s.Capabilities().Search }
func (s Source) Current(ctx context.Context) ([]release.Release, error) {
	if s.Provider == nil {
		return nil, errors.New("content provider unavailable")
	}
	feeds := append([]configstore.RSSSource(nil), s.Config.RSS...)
	if len(feeds) == 0 {
		if endpoint := DefaultRSSURL(s.Config.Provider); endpoint != "" {
			feeds = []configstore.RSSSource{{Name: "default", URL: endpoint}}
		}
	}
	if len(feeds) == 0 {
		return nil, backfill.ErrUnsupported
	}
	var out []release.Release
	var errs []error
	seen := map[string]bool{}
	for _, feed := range feeds {
		values, err := s.Provider.Poll(ctx, feed.URL)
		if err != nil {
			name := feed.Name
			if name == "" {
				name = feed.URL
			}
			errs = append(errs, fmt.Errorf("RSS %s: %w", name, err))
		}
		for _, value := range values {
			key := value.InfoHash
			if key == "" {
				key = value.DownloadURL
			}
			if key == "" {
				key = value.MediaName + "\x00" + value.Title
			}
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, value)
		}
	}
	return out, errors.Join(errs...)
}
func (s Source) Search(ctx context.Context, q SearchRequest) ([]release.Release, error) {
	if s.Provider == nil {
		return nil, errors.New("content provider unavailable")
	}
	return s.Provider.Search(ctx, q)
}
