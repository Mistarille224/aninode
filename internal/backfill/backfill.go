package backfill

import (
	"context"
	"errors"

	"aninode/internal/episode"
	"aninode/internal/release"
)

var ErrUnsupported = errors.New("provider does not support targeted historical search")

// QueryEvidence is one high-recall tracker search clue derived from facts aninode
// already owns. Title is a tracker-facing title observed from the current filesystem when possible. Group is optional provenance evidence and must never
// become a hard filter. Tier distinguishes source-derived primary recall from
// declared Entry-title fallback. Origin exists for diagnostics; providers
// do not interpret either field.
type QueryTier string

const (
	QueryPrimary  QueryTier = "primary"
	QueryFallback QueryTier = "fallback"
)

type QueryEvidence struct {
	Title  string    `json:"title"`
	Group  string    `json:"group,omitempty"`
	Origin string    `json:"origin"`
	Tier   QueryTier `json:"tier"`
}

type Request struct {
	EntryKey      string          `json:"entry_key"`
	Queries       []QueryEvidence `json:"queries"`
	SourceMissing []episode.Key   `json:"source_missing"`
}

type Searcher interface {
	SearchHistory(context.Context, string, Request) ([]release.Release, error)
}
