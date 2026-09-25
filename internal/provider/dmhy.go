package provider

import (
	"aninode/internal/acquisition"
	"aninode/internal/backfill"
	"aninode/internal/release"
	"aninode/internal/rss"
	"aninode/internal/torrentmeta"
	"context"
	"errors"
	"fmt"
	"html"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

type dmhy struct{ base }

var dmhyTorrentLinkRE = regexp.MustCompile(`(?i)(?:https?:)?//[^"'<>[:space:]]+\.torrent(?:\?[^"'<>[:space:]]*)?`)

func (dmhy) Name() string             { return "dmhy" }
func (dmhy) Capabilities() Capability { return Capability{RSS: true, Search: true, Historical: true} }
func (p dmhy) Poll(ctx context.Context, rawURL string) ([]release.Release, error) {
	if strings.TrimSpace(rawURL) == "" {
		return nil, backfill.ErrUnsupported
	}
	return p.fetchNative(ctx, rawURL, SearchRequest{})
}
func (p dmhy) Search(ctx context.Context, r SearchRequest) ([]release.Release, error) {
	q := queryWithEpisode(r, false)
	if q == "" {
		return nil, errors.New("search query is required")
	}
	ep, e := endpoint("https://share.dmhy.org/topics/rss/rss.xml?keyword={query}", q)
	if e != nil {
		return nil, e
	}
	return p.fetchNative(ctx, ep, r)
}

// fetchNative preserves the shared release-normalization path while adding the
// one transport-specific step DMHY needs. Its RSS enclosure commonly carries a
// BTIH magnet without a dn parameter, so the native torrent filename cannot be
// learned from the feed alone. In that case resolve the item page's concrete
// .torrent URL and read info.name instead of treating the RSS display title as
// a filename.
func (p dmhy) fetchNative(ctx context.Context, endpoint string, req SearchRequest) ([]release.Release, error) {
	if p.fetcher == nil {
		return nil, errors.New("content fetcher is unavailable")
	}
	entries, err := p.fetcher.Fetch(ctx, dmhyHTTPSURL(endpoint))
	if err != nil {
		return nil, err
	}
	if len(searchTargets(req)) > 0 {
		entries = filterSearchEntries(entries, req)
	}
	observed, observationErr := resolveMediaNames(ctx, p.fetcher, entries, req)
	observed, fallbackErr := resolveDMHYPageMediaNames(ctx, p.fetcher, entries, observed, req)
	values, normalizeErr := release.EnrichManyObserved(p.Name(), entries, observed)
	promoteTargetedEpisodeReleases(values, req)
	if normalizeErr != nil {
		return values, errors.Join(observationErr, fallbackErr, fmt.Errorf("normalize releases: %w", normalizeErr))
	}
	return values, errors.Join(observationErr, fallbackErr)
}

func resolveDMHYPageMediaNames(ctx context.Context, fetcher Fetcher, entries []rss.Entry, observed []release.NameObservation, req SearchRequest) ([]release.NameObservation, error) {
	if len(entries) != len(observed) {
		return observed, fmt.Errorf("dmhy observation count %d does not match entry count %d", len(observed), len(entries))
	}
	pageResolver, canFetchPage := fetcher.(pageFetcher)
	metadataResolver, canFetchTorrent := fetcher.(torrentMetadataFetcher)
	if !canFetchPage || !canFetchTorrent {
		return observed, nil
	}

	out := append([]release.NameObservation(nil), observed...)
	if err := ctx.Err(); err != nil {
		return out, err
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	errs := make([]error, len(entries))
	for i, entry := range entries {
		if strings.TrimSpace(out[i].Value) != "" || strings.TrimSpace(entry.Link) == "" {
			continue
		}
		if cache, ok := fetcher.(interface {
			CachedTorrentMetadataByHash(string) (torrentmeta.Metadata, bool)
		}); ok {
			identity, err := acquisition.CanonicalIdentity(acquisition.IdentityInput{MagnetURL: entry.DownloadURL})
			if err == nil {
				if meta, found := cache.CachedTorrentMetadataByHash(identity.Canonical); found {
					out[i] = release.NameObservation{Value: strings.TrimSpace(nativeSearchName(meta, req)), Source: "torrent-info", InfoHash: meta.InfoHash}
					continue
				}
			}
		}
		wg.Add(1)
		go func(index int, pageURL string) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				errs[index] = ctx.Err()
				return
			}

			pageCtx, cancelPage := context.WithTimeout(ctx, 10*time.Second)
			data, err := pageResolver.FetchPage(pageCtx, pageURL)
			cancelPage()
			if err != nil {
				errs[index] = fmt.Errorf("dmhy item page %s: %w", pageURL, err)
				return
			}
			torrentURL, err := dmhyTorrentURL(pageURL, data)
			if err != nil {
				errs[index] = fmt.Errorf("dmhy item page %s: %w", pageURL, err)
				return
			}

			metadataCtx, cancelMetadata := context.WithTimeout(ctx, 10*time.Second)
			meta, err := metadataResolver.FetchTorrentMetadata(metadataCtx, torrentURL)
			cancelMetadata()
			if err != nil {
				errs[index] = fmt.Errorf("dmhy torrent metadata %s: %w", torrentURL, err)
				return
			}
			if name := strings.TrimSpace(nativeSearchName(meta, req)); name != "" {
				out[index] = release.NameObservation{Value: name, Source: "torrent-info", InfoHash: meta.InfoHash}
			}
		}(i, dmhyHTTPSURL(strings.TrimSpace(entry.Link)))
	}
	wg.Wait()
	return out, summarizeLookupErrors("dmhy native metadata", errs)
}

func dmhyTorrentURL(pageURL string, data []byte) (string, error) {
	base, err := url.Parse(pageURL)
	if err != nil {
		return "", err
	}
	text := html.UnescapeString(string(data))
	match := dmhyTorrentLinkRE.FindString(text)
	if match == "" {
		return "", errors.New("page contains no direct .torrent URL")
	}
	ref, err := url.Parse(match)
	if err != nil {
		return "", err
	}
	return dmhyHTTPSURL(base.ResolveReference(ref).String()), nil
}

// Upgrade only the two built-in DMHY origins whose HTTPS endpoints are known.
// Custom mirrors, explicit ports and credentials retain their original meaning.
func dmhyHTTPSURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.User != nil {
		return raw
	}
	switch strings.ToLower(u.Host) {
	case "share.dmhy.org", "dl.dmhy.org":
		u.Scheme = "https"
		return u.String()
	default:
		return raw
	}
}
