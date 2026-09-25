package release

import (
	"fmt"
	"strings"
	"time"

	"aninode/internal/acquisition"
	"aninode/internal/medianame"
	"aninode/internal/rss"
)

type Release struct {
	MediaType       string                 `json:"media_type"`
	Provider        string                 `json:"provider"`
	Title           string                 `json:"title"`
	NormalizedTitle string                 `json:"normalized_title"`
	MediaName       string                 `json:"media_name,omitempty"`
	NameSource      string                 `json:"name_source,omitempty"`
	InfoHash        string                 `json:"info_hash,omitempty"`
	Parsed          medianame.Parsed       `json:"parsed"`
	Components      medianame.Components   `json:"components"`
	Differences     []medianame.Difference `json:"differences,omitempty"`
	GUID            string                 `json:"guid,omitempty"`
	PageURL         string                 `json:"page_url,omitempty"`
	DownloadURL     string                 `json:"download_url,omitempty"`
	PublishedAt     time.Time              `json:"published_at,omitempty"`
	Group           string                 `json:"group,omitempty"`
	Episode         string                 `json:"episode,omitempty"`
	Resolution      string                 `json:"resolution,omitempty"`
	Subtitle        string                 `json:"subtitle,omitempty"`
}

// InferMediaType classifies an individual release instead of its source. RSS
// sources are commonly mixed. A season qualifier or reliable episode evidence
// makes a release a series; a lone ambiguous numeric tail remains movie-shaped
// until publication structure or sibling variation raises its confidence.

// AcquisitionIdentity returns the strongest native identity carried by a
// release. BitTorrent info hashes from magnet links intentionally outrank
// provider URLs and GUIDs so downloader observations can reconcile with feed
// observations without inventing an aninode-specific binding.
func AcquisitionIdentity(r Release) (acquisition.Identity, error) {
	in := acquisition.IdentityInput{GUID: r.GUID, InfoHash: r.InfoHash}
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(r.DownloadURL)), "magnet:") {
		in.MagnetURL = r.DownloadURL
	} else {
		in.DownloadURL = r.DownloadURL
	}
	return acquisition.CanonicalIdentity(in)
}

func InferMediaType(value Release) string {
	if value.Components.SeasonExplicit || value.Components.Season > 0 || medianame.ReliableEpisodeEvidence(value.Components) {
		return "series"
	}
	return "movie"
}

type NameObservation struct {
	Value    string
	Source   string
	InfoHash string
}

// EnrichManyObserved is the only release-construction boundary. It runs one
// canonical filename/release-name parser for every content-source path. RSS <title> is not a separate naming grammar: adapters
// first resolve the strongest native torrent name they can observe. Missing
// native naming evidence stays unknown instead of reinterpreting RSS text.
func EnrichManyObserved(provider string, entries []rss.Entry, observed []NameObservation) ([]Release, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider != "generic" && provider != "mikan" && provider != "dmhy" && provider != "nyaa" {
		return nil, fmt.Errorf("unsupported feed provider %q", provider)
	}
	if len(observed) != len(entries) {
		return nil, fmt.Errorf("name observation count %d does not match entry count %d", len(observed), len(entries))
	}
	names := make([]string, len(entries))
	for i := range entries {
		names[i] = strings.TrimSpace(observed[i].Value)
	}
	analysis := medianame.AnalyzeMany(names)
	values := make([]Release, len(entries))
	for i := range entries {
		values[i] = enrichParsed(provider, entries[i], analysis.Items[i])
		values[i].MediaName = names[i]
		values[i].NameSource = observed[i].Source
		values[i].InfoHash = strings.ToLower(strings.TrimSpace(observed[i].InfoHash))
		values[i].NormalizedTitle = values[i].Components.Title
		values[i].Differences = analysis.DifferencesFor(i)
	}
	return values, nil
}

func enrichParsed(provider string, entry rss.Entry, name medianame.Name) Release {
	parsed, components := name.Parsed, name.Components
	value := Release{
		Provider: provider, Title: entry.Title, NormalizedTitle: entry.Title, Parsed: parsed, Components: components,
		GUID: entry.GUID, PageURL: entry.Link, DownloadURL: entry.DownloadURL,
		PublishedAt: entry.PublishedAt,
	}
	value.Group = components.ReleaseGroup
	if parsed.EpisodeStart > 0 {
		if parsed.SeasonExplicit || parsed.Season > 0 {
			value.Episode = fmt.Sprintf("S%dE%d", parsed.Season, parsed.EpisodeStart)
		} else {
			value.Episode = fmt.Sprintf("E%d", parsed.EpisodeStart)
		}
		if parsed.EpisodeEnd > 0 && parsed.EpisodeEnd != parsed.EpisodeStart {
			value.Episode += fmt.Sprintf("-E%d", parsed.EpisodeEnd)
		}
		if parsed.Version > 1 {
			value.Episode += fmt.Sprintf("v%d", parsed.Version)
		}
	}
	value.Resolution = components.Resolution
	value.Subtitle = components.Subtitle
	value.MediaType = InferMediaType(value)
	return value
}
