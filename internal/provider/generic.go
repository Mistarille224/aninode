package provider

import (
	"aninode/internal/backfill"
	"aninode/internal/release"
	"context"
	"errors"
	"strings"
)

type genericRSS struct {
	base
	searchURLTemplate string
}

func (genericRSS) Name() string { return "generic" }
func (p genericRSS) Capabilities() Capability {
	return Capability{RSS: true, Search: strings.TrimSpace(p.searchURLTemplate) != "", Historical: strings.TrimSpace(p.searchURLTemplate) != ""}
}
func (p genericRSS) Poll(ctx context.Context, url string) ([]release.Release, error) {
	if strings.TrimSpace(url) == "" {
		return nil, backfill.ErrUnsupported
	}
	return p.fetch(ctx, p.Name(), url)
}
func (p genericRSS) Search(ctx context.Context, r SearchRequest) ([]release.Release, error) {
	q := queryWithEpisode(r, true)
	if q == "" {
		return nil, errors.New("search query is required")
	}
	if strings.TrimSpace(p.searchURLTemplate) == "" {
		return nil, backfill.ErrUnsupported
	}
	ep, e := endpoint(p.searchURLTemplate, q)
	if e != nil {
		return nil, e
	}
	return p.fetchSearch(ctx, p.Name(), ep, r)
}
