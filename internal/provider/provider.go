package provider

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"aninode/internal/medianame"
	"aninode/internal/release"
	"aninode/internal/rss"
	"aninode/internal/torrentmeta"
)

type Fetcher interface {
	Fetch(context.Context, string) ([]rss.Entry, error)
}

type Capability struct {
	RSS        bool `json:"rss"`
	Search     bool `json:"search"`
	Historical bool `json:"historical"`
}

type SearchEpisode struct {
	Season  int `json:"season,omitempty"`
	Episode int `json:"episode"`
}

type SearchRequest struct {
	Query          string          `json:"query"`
	Season         int             `json:"season,omitempty"`
	SeasonExplicit bool            `json:"season_explicit,omitempty"`
	EpisodeStart   int             `json:"episode_start,omitempty"`
	EpisodeEnd     int             `json:"episode_end,omitempty"`
	Targets        []SearchEpisode `json:"targets,omitempty"`
	Limit          int             `json:"limit,omitempty"`
}

type Provider interface {
	Name() string
	Capabilities() Capability
	Poll(context.Context, string) ([]release.Release, error)
	Search(context.Context, SearchRequest) ([]release.Release, error)
}

type Options struct{ SearchURLTemplate string }

const (
	defaultDMHYRSSURL  = "https://share.dmhy.org/topics/rss/rss.xml"
	defaultNyaaRSSURL  = "https://nyaa.si/?page=rss"
	defaultMikanRSSURL = "https://mikanani.me/RSS/Classic"
)

func DefaultRSSURL(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "dmhy":
		return defaultDMHYRSSURL
	case "nyaa":
		return defaultNyaaRSSURL
	case "mikan":
		return defaultMikanRSSURL
	default:
		return ""
	}
}

type Registry struct{ Fetcher Fetcher }

func (r Registry) New(name string, options Options) (Provider, error) {
	b := base{fetcher: r.Fetcher}
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "dmhy":
		return dmhy{base: b}, nil
	case "nyaa":
		return nyaa{base: b}, nil
	case "mikan":
		return mikan{base: b}, nil
	case "generic":
		return genericRSS{base: b, searchURLTemplate: options.SearchURLTemplate}, nil
	default:
		return nil, fmt.Errorf("unsupported content provider %q", name)
	}
}

type base struct{ fetcher Fetcher }

func (b base) fetch(ctx context.Context, provider, endpoint string) ([]release.Release, error) {
	if b.fetcher == nil {
		return nil, errors.New("content fetcher is unavailable")
	}
	entries, err := b.fetcher.Fetch(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	return b.enrich(ctx, provider, entries, SearchRequest{})
}

// fetchSearch deliberately separates cheap tracker evidence from expensive
// torrent metainfo. Search-page/RSS titles are never trusted as final media
// identity, but explicit contradictory episode evidence can safely eliminate a
// candidate before aninode spends a network request on its .torrent file.
func (b base) fetchSearch(ctx context.Context, provider, endpoint string, req SearchRequest) ([]release.Release, error) {
	if b.fetcher == nil {
		return nil, errors.New("content fetcher is unavailable")
	}
	entries, err := b.fetcher.Fetch(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	entries = filterSearchEntries(entries, req)
	return b.enrich(ctx, provider, entries, req)
}

func (b base) enrich(ctx context.Context, provider string, entries []rss.Entry, req SearchRequest) ([]release.Release, error) {
	if b.fetcher == nil {
		return nil, errors.New("content fetcher is unavailable")
	}
	fetcher := b.fetcher
	if provider == "nyaa" {
		if resolver, ok := fetcher.(torrentMetadataFetcher); ok {
			fetcher = &nyaaMetadataFetcher{Fetcher: fetcher, resolver: resolver}
		}
	}
	observed, observationErr := resolveMediaNames(ctx, fetcher, entries, req)
	values, err := release.EnrichManyObserved(provider, entries, observed)
	promoteTargetedEpisodeReleases(values, req)
	if err != nil {
		return values, errors.Join(observationErr, fmt.Errorf("normalize releases: %w", err))
	}
	return values, observationErr
}

// promoteTargetedEpisodeReleases carries exact historical-search context across
// the release normalization boundary. A native single-file name such as
// "Show - 06" is intentionally weak when observed in an untargeted RSS stream,
// but once a provider request explicitly asks for episode 6 and the native name
// covers that target, it is safe to classify the result as episodic. The parser's
// syntactic evidence remains unchanged; only the search-scoped media type is
// strengthened.
func promoteTargetedEpisodeReleases(values []release.Release, req SearchRequest) {
	if len(searchTargets(req)) == 0 {
		return
	}
	for i := range values {
		if values[i].MediaType == "series" || strings.TrimSpace(values[i].MediaName) == "" {
			continue
		}
		// Promotion is allowed only for native BitTorrent naming evidence. The
		// tracker/search presentation title remains recall-only and must never turn
		// a weak numeric suffix into episodic identity by itself.
		switch values[i].NameSource {
		case "torrent-info", "magnet-dn":
		default:
			continue
		}
		if componentsCoverSearchTarget(values[i].Components, req) {
			values[i].MediaType = "series"
		}
	}
}

type torrentMetadataFetcher interface {
	FetchTorrentMetadata(context.Context, string) (torrentmeta.Metadata, error)
}

type metadataCacheReader interface {
	CachedTorrentMetadata(string) (torrentmeta.Metadata, bool)
}

// resolveMediaNames adapts feed/search transport into the one naming domain
// understood by medianame. Presentation titles are only cheap recall evidence;
// final identity comes from native BitTorrent naming. For a multi-file torrent,
// targeted search may select the real media path matching the requested episode
// instead of incorrectly treating info.name (often just a directory) as a file.
func resolveMediaNames(ctx context.Context, fetcher Fetcher, entries []rss.Entry, req SearchRequest) ([]release.NameObservation, error) {
	out := make([]release.NameObservation, len(entries))
	if err := ctx.Err(); err != nil {
		return out, err
	}
	resolver, canFetchTorrent := fetcher.(torrentMetadataFetcher)
	var wg sync.WaitGroup
	concurrency := 8
	if _, ok := fetcher.(*nyaaMetadataFetcher); ok {
		concurrency = 1
	}
	sem := make(chan struct{}, concurrency)
	errs := make([]error, len(entries))
	for i, entry := range entries {
		downloadURL := strings.TrimSpace(entry.DownloadURL)
		if downloadURL == "" {
			continue
		}
		if strings.HasPrefix(strings.ToLower(downloadURL), "magnet:") {
			if parsed, err := url.Parse(downloadURL); err == nil {
				if name := strings.TrimSpace(parsed.Query().Get("dn")); name != "" {
					out[i] = release.NameObservation{Value: name, Source: "magnet-dn"}
				}
			}
			continue
		}
		if cache, ok := fetcher.(metadataCacheReader); ok {
			if meta, found := cache.CachedTorrentMetadata(downloadURL); found {
				out[i] = release.NameObservation{Value: strings.TrimSpace(nativeSearchName(meta, req)), Source: "torrent-info", InfoHash: meta.InfoHash}
				continue
			}
		}
		if !canFetchTorrent || !torrentURL(downloadURL) {
			continue
		}
		wg.Add(1)
		go func(index int, rawURL string) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				errs[index] = ctx.Err()
				return
			}
			metadataCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			meta, err := resolver.FetchTorrentMetadata(metadataCtx, rawURL)
			if err != nil {
				errs[index] = fmt.Errorf("torrent metadata %s: %w", rawURL, err)
				return
			}
			name := nativeSearchName(meta, req)
			out[index] = release.NameObservation{Value: strings.TrimSpace(name), Source: "torrent-info", InfoHash: meta.InfoHash}
		}(i, downloadURL)
	}
	wg.Wait()
	return out, summarizeLookupErrors("torrent metadata", errs)
}

func filterSearchEntries(entries []rss.Entry, req SearchRequest) []rss.Entry {
	if len(entries) == 0 || len(searchTargets(req)) == 0 {
		return entries
	}
	names := make([]string, len(entries))
	for i := range entries {
		names[i] = entries[i].Title
	}
	analysis := medianame.AnalyzeMany(names)
	out := make([]rss.Entry, 0, len(entries))
	for i, entry := range entries {
		c := analysis.Items[i].Components
		// Weak ordinal syntax is intentionally retained: tracker presentation
		// titles are noisy, and cheap filtering is allowed to reject only explicit
		// contradictory episode syntax. Differential/weak inference stays in the
		// recall set until native torrent metadata is available.
		if c.EpisodeStart > 0 && c.EpisodeEvidence == "explicit" && !componentsCoverSearchTarget(c, req) {
			continue
		}
		out = append(out, entry)
	}
	return out
}

func nativeSearchName(meta torrentmeta.Metadata, req SearchRequest) string {
	targeted := len(searchTargets(req)) > 0
	if len(meta.Files) == 0 {
		name := strings.TrimSpace(meta.Name)
		if !targeted || name == "" {
			return name
		}
		// Native single-file metadata is authoritative enough to reject an
		// explicitly contradictory episode before it becomes a movie/series noise
		// candidate. Ambiguous names remain in the recall set.
		c := medianame.AnalyzeMany([]string{name}).Items[0].Components
		if c.EpisodeStart > 0 && c.EpisodeEvidence == "explicit" && !componentsCoverSearchTarget(c, req) {
			return ""
		}
		return name
	}
	media := make([]string, 0, len(meta.Files))
	for _, file := range meta.Files {
		name := path.Base(strings.TrimSpace(file))
		switch strings.ToLower(path.Ext(name)) {
		case ".mkv", ".mp4", ".avi", ".mov", ".m4v", ".ts", ".webm":
			media = append(media, name)
		}
	}
	if len(media) == 0 {
		return meta.Name
	}
	if !targeted {
		if len(media) == 1 {
			return media[0]
		}
		return meta.Name
	}
	analysis := medianame.AnalyzeFiles(media)
	for i := range media {
		if componentsCoverSearchTarget(analysis.Items[i].Components, req) {
			return media[i]
		}
	}
	// For a targeted multi-file search, info.name is commonly only the pack
	// directory. If none of the actual media paths covers the requested episode,
	// the torrent is conclusively not useful for this target and must not be
	// reinterpreted as a movie-shaped release.
	return ""
}

func componentsCoverSearchTarget(c medianame.Components, req SearchRequest) bool {
	if c.EpisodeStart <= 0 {
		return false
	}
	end := c.EpisodeEnd
	if end < c.EpisodeStart {
		end = c.EpisodeStart
	}
	seasonTargeted := len(req.Targets) > 0 || req.SeasonExplicit || req.Season > 0
	for _, target := range searchTargets(req) {
		if target.Episode < c.EpisodeStart || target.Episode > end {
			continue
		}
		if (c.SeasonExplicit || c.Season > 0) && seasonTargeted && c.Season != target.Season {
			continue
		}
		return true
	}
	return false
}

func searchTargets(req SearchRequest) []SearchEpisode {
	if len(req.Targets) > 0 {
		return req.Targets
	}
	if req.EpisodeStart <= 0 {
		return nil
	}
	end := req.EpisodeEnd
	if end < req.EpisodeStart {
		end = req.EpisodeStart
	}
	out := make([]SearchEpisode, 0, end-req.EpisodeStart+1)
	for ep := req.EpisodeStart; ep <= end; ep++ {
		out = append(out, SearchEpisode{Season: req.Season, Episode: ep})
	}
	return out
}

func summarizeLookupErrors(kind string, errs []error) error {
	causes := make([]error, 0, len(errs))
	for _, err := range errs {
		if err != nil {
			causes = append(causes, err)
		}
	}
	errs = causes
	if len(errs) == 0 {
		return nil
	}
	const maxSamples = 3
	samples := make([]string, 0, min(maxSamples, len(errs)))
	for i, err := range errs {
		if i >= maxSamples {
			break
		}
		samples = append(samples, err.Error())
	}
	return &lookupErrors{message: fmt.Sprintf("%s: %d lookup(s) failed; samples: %s", kind, len(errs), strings.Join(samples, " | ")), causes: errs}
}

func torrentURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	return strings.EqualFold(path.Ext(u.Path), ".torrent")
}
func firstTitle(req SearchRequest) string { return strings.TrimSpace(req.Query) }
func queryWithEpisode(req SearchRequest, sxe bool) string {
	title := firstTitle(req)
	if title == "" {
		return ""
	}
	if req.EpisodeStart <= 0 {
		return title
	}
	if !sxe {
		return fmt.Sprintf("%s %02d", title, req.EpisodeStart)
	}
	season := req.Season
	if season <= 0 && !req.SeasonExplicit {
		season = 1
	}
	return fmt.Sprintf("%s S%02dE%02d", title, season, req.EpisodeStart)
}
func endpoint(tpl, q string) (string, error) {
	if strings.Count(tpl, "{query}") != 1 {
		return "", errors.New("search URL template must contain exactly one {query}")
	}
	return strings.Replace(tpl, "{query}", url.QueryEscape(q), 1), nil
}

type lookupErrors struct {
	message string
	causes  []error
}

func (e *lookupErrors) Error() string   { return e.message }
func (e *lookupErrors) Unwrap() []error { return e.causes }
