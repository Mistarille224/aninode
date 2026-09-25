package rss

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"aninode/internal/torrentmeta"
)

const maxFeedSize = 8 << 20
const maxPageSize = 16 << 20
const transientRetryDelay = 300 * time.Millisecond

type Entry struct {
	Title       string    `json:"title"`
	GUID        string    `json:"guid,omitempty"`
	Link        string    `json:"link,omitempty"`
	DownloadURL string    `json:"download_url,omitempty"`
	PublishedAt time.Time `json:"published_at,omitempty"`
	Description string    `json:"description,omitempty"`
}

type Fetcher struct {
	Client   *http.Client
	metadata *metadataCache
	feeds    *feedCache
	cooldown *requestCooldown
}

func (fetcher Fetcher) Fetch(ctx context.Context, feedURL string) ([]Entry, error) {
	client := fetcher.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, feedURL, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/rss+xml, application/atom+xml, application/xml, text/xml;q=0.9")
	request.Header.Set("User-Agent", "aninode/0")
	if etag, modified := fetcher.feeds.validators(feedURL); etag != "" || modified != "" {
		if etag != "" {
			request.Header.Set("If-None-Match", etag)
		}
		if modified != "" {
			request.Header.Set("If-Modified-Since", modified)
		}
	}
	response, err := fetcher.readRequest(ctx, client, request)
	if err != nil {
		return nil, redactRequestError(err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotModified {
		if entries, ok := fetcher.feeds.notModified(feedURL); ok {
			return entries, nil
		}
		return nil, &HTTPStatusError{Resource: "feed", StatusCode: response.StatusCode}
	}
	if response.StatusCode != http.StatusOK {
		return nil, &HTTPStatusError{Resource: "feed", StatusCode: response.StatusCode}
	}
	entries, err := Parse(io.LimitReader(response.Body, maxFeedSize+1))
	if err != nil {
		return nil, err
	}
	fetcher.feeds.put(feedURL, response.Header, entries)
	return entries, nil
}

// FetchPage retrieves a bounded HTML/text page for provider adapters that need
// a site-native search page before resolving concrete torrent URLs. Media
// semantics must still come from torrent metadata, not from page presentation.
func (fetcher Fetcher) FetchPage(ctx context.Context, pageURL string) ([]byte, error) {
	client := fetcher.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "text/html,application/xhtml+xml;q=0.9,*/*;q=0.8")
	request.Header.Set("User-Agent", "aninode/0")
	response, err := fetcher.readRequest(ctx, client, request)
	if err != nil {
		return nil, redactRequestError(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, &HTTPStatusError{Resource: "page", StatusCode: response.StatusCode}
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxPageSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxPageSize {
		return nil, fmt.Errorf("page exceeds %d bytes", maxPageSize)
	}
	return data, nil
}

func Parse(reader io.Reader) ([]Entry, error) {
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	if len(data) > maxFeedSize {
		return nil, fmt.Errorf("feed exceeds %d bytes", maxFeedSize)
	}
	var root struct {
		XMLName xml.Name
	}
	if err := xml.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("decode feed root: %w", err)
	}
	switch strings.ToLower(root.XMLName.Local) {
	case "rss":
		return parseRSS(data)
	case "feed":
		return parseAtom(data)
	default:
		return nil, fmt.Errorf("unsupported feed root %q", root.XMLName.Local)
	}
}

type rssDocument struct {
	Channel struct {
		Items []struct {
			Title       string `xml:"title"`
			GUID        string `xml:"guid"`
			Link        string `xml:"link"`
			PubDate     string `xml:"pubDate"`
			Description string `xml:"description"`
			Enclosure   struct {
				URL string `xml:"url,attr"`
			} `xml:"enclosure"`
			MagnetURI string `xml:"magnetURI"`
		} `xml:"item"`
	} `xml:"channel"`
}

func parseRSS(data []byte) ([]Entry, error) {
	var document rssDocument
	if err := xml.Unmarshal(data, &document); err != nil {
		return nil, err
	}
	entries := make([]Entry, 0, len(document.Channel.Items))
	for _, item := range document.Channel.Items {
		downloadURL := strings.TrimSpace(item.Enclosure.URL)
		if strings.TrimSpace(item.MagnetURI) != "" {
			downloadURL = strings.TrimSpace(item.MagnetURI)
		}
		if downloadURL == "" && strings.HasPrefix(strings.ToLower(strings.TrimSpace(item.Link)), "magnet:") {
			downloadURL = strings.TrimSpace(item.Link)
		}
		if downloadURL == "" && strings.HasPrefix(strings.ToLower(strings.TrimSpace(item.GUID)), "magnet:") {
			downloadURL = strings.TrimSpace(item.GUID)
		}
		if downloadURL == "" && isTorrentURL(item.Link) {
			downloadURL = strings.TrimSpace(item.Link)
		}
		entries = append(entries, Entry{
			Title: strings.TrimSpace(item.Title), GUID: strings.TrimSpace(item.GUID),
			Link: strings.TrimSpace(item.Link), DownloadURL: downloadURL,
			PublishedAt: parseTime(item.PubDate), Description: strings.TrimSpace(item.Description),
		})
	}
	return entries, nil
}

type atomDocument struct {
	Entries []struct {
		Title   string `xml:"title"`
		ID      string `xml:"id"`
		Updated string `xml:"updated"`
		Summary string `xml:"summary"`
		Content string `xml:"content"`
		Links   []struct {
			Rel  string `xml:"rel,attr"`
			Href string `xml:"href,attr"`
		} `xml:"link"`
	} `xml:"entry"`
}

func parseAtom(data []byte) ([]Entry, error) {
	var document atomDocument
	if err := xml.Unmarshal(data, &document); err != nil {
		return nil, err
	}
	entries := make([]Entry, 0, len(document.Entries))
	for _, item := range document.Entries {
		var pageURL, downloadURL string
		for _, link := range item.Links {
			switch strings.ToLower(link.Rel) {
			case "enclosure":
				downloadURL = strings.TrimSpace(link.Href)
			case "", "alternate":
				if pageURL == "" {
					pageURL = strings.TrimSpace(link.Href)
				}
			}
		}
		if downloadURL == "" && isTorrentURL(pageURL) {
			downloadURL = pageURL
		}
		description := strings.TrimSpace(item.Summary)
		if description == "" {
			description = strings.TrimSpace(item.Content)
		}
		entries = append(entries, Entry{
			Title: strings.TrimSpace(item.Title), GUID: strings.TrimSpace(item.ID),
			Link: pageURL, DownloadURL: downloadURL,
			PublishedAt: parseTime(item.Updated), Description: description,
		})
	}
	return entries, nil
}

func isTorrentURL(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return false
	}
	if question := strings.IndexByte(value, '?'); question >= 0 {
		value = value[:question]
	}
	return strings.HasSuffix(value, ".torrent")
}

func parseTime(value string) time.Time {
	value = strings.TrimSpace(value)
	for _, layout := range []string{time.RFC3339, time.RFC3339Nano, time.RFC1123Z, time.RFC1123, time.RFC822Z, time.RFC822} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed
		}
	}
	return time.Time{}
}

const maxTorrentSize = 8 << 20

// fetchTorrentMetadata resolves the native torrent name and info hash from a
// .torrent URL. Feed titles are presentation text and are intentionally not
// treated as authoritative media names when native torrent metadata exists.
func (fetcher Fetcher) fetchTorrentMetadata(ctx context.Context, torrentURL string) (torrentmeta.Metadata, error) {
	client := fetcher.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, torrentURL, nil)
	if err != nil {
		return torrentmeta.Metadata{}, err
	}
	request.Header.Set("Accept", "application/x-bittorrent, application/octet-stream;q=0.9")
	request.Header.Set("User-Agent", "aninode/0")
	response, err := fetcher.readRequest(ctx, client, request)
	if err != nil {
		return torrentmeta.Metadata{}, redactRequestError(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return torrentmeta.Metadata{}, &HTTPStatusError{Resource: "torrent", StatusCode: response.StatusCode}
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxTorrentSize+1))
	if err != nil {
		return torrentmeta.Metadata{}, err
	}
	if len(data) > maxTorrentSize {
		return torrentmeta.Metadata{}, fmt.Errorf("torrent exceeds %d bytes", maxTorrentSize)
	}
	return torrentmeta.Parse(data)
}

// doReadRequest retries one transient gateway response. Provider reads are
// idempotent, and a single bounded retry smooths short-lived 502/503/504 errors
// without turning a degraded upstream into an unbounded retry queue.
func doReadRequest(ctx context.Context, client *http.Client, request *http.Request) (*http.Response, error) {
	response, err := client.Do(request)
	if err != nil || response == nil || !transientGatewayStatus(response.StatusCode) {
		return response, err
	}
	response.Body.Close()
	timer := time.NewTimer(transientRetryDelay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
	}
	return client.Do(request.Clone(ctx))
}

func transientGatewayStatus(status int) bool {
	return status == http.StatusBadGateway || status == http.StatusServiceUnavailable || status == http.StatusGatewayTimeout
}

// redactRequestError preserves error identity for cancellation and timeout
// checks while ensuring net/http cannot echo configured URL credentials into
// logs or API error bodies.
func redactRequestError(err error) error {
	var requestErr *url.Error
	if !errors.As(err, &requestErr) {
		return err
	}
	return &url.Error{Op: requestErr.Op, URL: redactURL(requestErr.URL), Err: requestErr.Err}
}

func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "[redacted URL]"
	}
	if u.User != nil {
		u.User = url.User("[redacted]")
	}
	if u.RawQuery != "" {
		u.RawQuery = "[redacted]"
		u.ForceQuery = true
	}
	return u.String()
}
