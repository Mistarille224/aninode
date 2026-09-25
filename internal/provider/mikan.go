package provider

import (
	"aninode/internal/backfill"
	"aninode/internal/release"
	"aninode/internal/rss"
	"context"
	"errors"
	"fmt"
	"html"
	"net/url"
	"regexp"
	"strings"
)

type mikan struct{ base }

// Mikan's RSS search is a convenient recent-result feed, but historical
// backfill must not depend on its result window. The HTML search endpoint is
// paginated, so walk it to exhaustion and only then let aninode's normal
// release/candidate filtering choose among the complete candidate set.
const defaultMikanSearchURL = "https://mikanani.me/Home/Search?searchstr={query}"

const maxMikanSearchPages = 100

var (
	mikanRowRE     = regexp.MustCompile(`(?is)<tr\b[^>]*>(.*?)</tr>`)
	mikanAnchorRE  = regexp.MustCompile(`(?is)<a\b([^>]*)>(.*?)</a>`)
	mikanHrefRE    = regexp.MustCompile(`(?is)\bhref\s*=\s*["']([^"']+)["']`)
	mikanTagRE     = regexp.MustCompile(`(?s)<[^>]+>`)
	mikanEpisodeRE = regexp.MustCompile(`(?i)/Home/Episode/`)
)

type pageFetcher interface {
	FetchPage(context.Context, string) ([]byte, error)
}

func (mikan) Name() string { return "mikan" }
func (mikan) Capabilities() Capability {
	return Capability{RSS: true, Search: true, Historical: true}
}
func (p mikan) Poll(ctx context.Context, rawURL string) ([]release.Release, error) {
	if strings.TrimSpace(rawURL) == "" {
		return nil, backfill.ErrUnsupported
	}
	return p.fetch(ctx, p.Name(), rawURL)
}
func (p mikan) Search(ctx context.Context, r SearchRequest) ([]release.Release, error) {
	q := queryWithEpisode(r, false)
	if q == "" {
		return nil, errors.New("search query is required")
	}
	pf, ok := p.fetcher.(pageFetcher)
	if !ok || p.fetcher == nil {
		return nil, errors.New("mikan search requires page fetching support")
	}
	searchURL, err := endpoint(defaultMikanSearchURL, q)
	if err != nil {
		return nil, err
	}
	items, err := fetchAllMikanSearchItems(ctx, pf, searchURL)
	if err != nil {
		return nil, err
	}

	// Deliberately do not apply SearchRequest.Limit here. Mikan orders search
	// results by publication time, while filtering/ranking is applied later. Truncating at
	// the provider boundary can keep (say) one group's episode 07 and discard a
	// later acceptable episode 07 from another group. Higher layers may limit
	// final UI output after filtering/grouping, but provider search returns every
	// candidate Mikan exposed for this query.
	out, resolveErr := p.resolveSearchItems(ctx, items, r)
	if len(out) == 0 && resolveErr != nil {
		return nil, resolveErr
	}
	if len(items) > 0 && len(out) == 0 {
		return nil, fmt.Errorf("mikan search resolved %d result links but none yielded usable native torrent metadata", len(items))
	}
	return out, resolveErr
}

func fetchAllMikanSearchItems(ctx context.Context, pf pageFetcher, searchURL string) ([]mikanSearchItem, error) {
	seen := map[string]bool{}
	out := make([]mikanSearchItem, 0, 32)
	for page := 1; page <= maxMikanSearchPages; page++ {
		pageURL, err := mikanSearchPageURL(searchURL, page)
		if err != nil {
			return nil, err
		}
		data, err := pf.FetchPage(ctx, pageURL)
		if err != nil {
			return nil, fmt.Errorf("mikan search page %d: %w", page, err)
		}
		items, err := parseMikanSearchPage(pageURL, data)
		if err != nil {
			return nil, fmt.Errorf("mikan search page %d: %w", page, err)
		}
		if len(items) == 0 {
			break
		}
		added := 0
		for _, item := range items {
			key := strings.TrimSpace(item.PageURL)
			if key == "" {
				key = strings.TrimSpace(item.TorrentURL)
			}
			if key == "" || seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, item)
			added++
		}
		// Some sites repeat the last valid page for an out-of-range page. Stop
		// on the first page that contributes nothing new instead of looping.
		if added == 0 {
			return out, nil
		}
		if page == maxMikanSearchPages {
			return nil, fmt.Errorf("mikan search exceeded safety bound of %d non-empty pages; refusing to return a silently truncated result set", maxMikanSearchPages)
		}
	}
	return out, nil
}

func mikanSearchPageURL(searchURL string, page int) (string, error) {
	u, err := url.Parse(searchURL)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("page", fmt.Sprintf("%d", page))
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func (p mikan) resolveSearchItems(ctx context.Context, items []mikanSearchItem, req SearchRequest) ([]release.Release, error) {
	entries := make([]rss.Entry, 0, len(items))
	for _, item := range items {
		if item.TorrentURL == "" {
			continue
		}
		entries = append(entries, rss.Entry{Title: item.Title, Link: item.PageURL, GUID: item.PageURL, DownloadURL: item.TorrentURL})
	}
	if len(entries) == 0 && len(items) > 0 {
		return nil, errors.New("mikan search results contain no direct torrent links; site layout may have changed")
	}
	if len(entries) == 0 {
		return nil, nil
	}
	entries = filterSearchEntries(entries, req)
	if len(entries) == 0 {
		return nil, nil
	}
	values, err := p.enrich(ctx, p.Name(), entries, req)
	usable := values[:0]
	for _, value := range values {
		if strings.TrimSpace(value.MediaName) != "" {
			usable = append(usable, value)
		}
	}
	return usable, err
}

type mikanSearchItem struct {
	Title      string
	PageURL    string
	TorrentURL string
}

// parseMikanSearchPage intentionally avoids depending on Mikan's table class or
// fixed column numbers. A result row is identified by the semantic links aninode
// needs: one /Home/Episode/ link plus one direct .torrent link in the same row.
func parseMikanSearchPage(baseURL string, data []byte) ([]mikanSearchItem, error) {
	base, err := url.Parse(baseURL)
	if err != nil {
		return nil, err
	}
	out := make([]mikanSearchItem, 0, 16)
	seen := map[string]bool{}
	for _, row := range mikanRowRE.FindAllSubmatch(data, -1) {
		var item mikanSearchItem
		for _, anchor := range mikanAnchorRE.FindAllSubmatch(row[1], -1) {
			rawHref := attrHref(anchor[1])
			if rawHref == "" {
				continue
			}
			resolved := resolveMikanHref(base, rawHref)
			if resolved == "" {
				continue
			}
			if item.PageURL == "" && mikanEpisodeRE.MatchString(rawHref) {
				item.PageURL = resolved
				item.Title = cleanMikanText(anchor[2])
			}
			if item.TorrentURL == "" && torrentURL(resolved) {
				item.TorrentURL = resolved
			}
		}
		if item.PageURL == "" || item.TorrentURL == "" || seen[item.PageURL] {
			continue
		}
		seen[item.PageURL] = true
		out = append(out, item)
	}

	// Distinguish a genuine empty result page from a layout change. If the page
	// visibly contains episode/torrent links but none could be paired into rows,
	// silently treating that as "no more pages" would reintroduce truncation.
	text := string(data)
	if len(out) == 0 && (mikanEpisodeRE.MatchString(text) || strings.Contains(strings.ToLower(text), ".torrent")) {
		return nil, errors.New("mikan search page contains release links but no usable result rows; site layout may have changed")
	}
	return out, nil
}

func attrHref(attrs []byte) string {
	match := mikanHrefRE.FindSubmatch(attrs)
	if len(match) < 2 {
		return ""
	}
	return html.UnescapeString(strings.TrimSpace(string(match[1])))
}

func resolveMikanHref(base *url.URL, raw string) string {
	raw = html.UnescapeString(strings.TrimSpace(raw))
	if raw == "" {
		return ""
	}
	ref, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return base.ResolveReference(ref).String()
}

func cleanMikanText(raw []byte) string {
	return strings.TrimSpace(html.UnescapeString(mikanTagRE.ReplaceAllString(string(raw), "")))
}
