package organizer

import (
	"aninode/internal/filesystem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type Config struct {
	Source          string   `json:"source"`
	Target          string   `json:"target"`
	Extensions      []string `json:"extensions"`
	emby            *embyConfig
	movie           *movieConfig
	Managed         bool   `json:"-"`
	PublicationRoot string `json:"-"`
	blacklist       []string
}

func WithEntryBlacklist(cfg Config, patterns []string) Config {
	cfg.blacklist = append([]string(nil), patterns...)
	return cfg
}

type Result struct {
	Scanned, Linked, Existing, Removed, Excluded, Unmatched, Conflicts int
}

func validate(cfg Config) error {
	if cfg.Source == "" || cfg.Target == "" {
		return errors.New("source and target are required")
	}
	source, err := filepath.Abs(cfg.Source)
	if err != nil {
		return err
	}
	target, err := filepath.Abs(cfg.Target)
	if err != nil {
		return err
	}
	if rootsOverlap(source, target) {
		return errors.New("source and target must be disjoint directory trees")
	}
	if _, err := filesystem.CommonNamespaceRoot(source, target); err != nil {
		return fmt.Errorf("source and target must live under one dedicated media namespace: %w", err)
	}
	return nil
}

func rootsOverlap(a, b string) bool {
	if a == b {
		return true
	}
	sep := string(os.PathSeparator)
	return strings.HasPrefix(a+sep, b+sep) || strings.HasPrefix(b+sep, a+sep)
}

// ValidateConfig validates organizer settings without scanning or modifying the
// filesystem.
func ValidateConfig(cfg Config) error {
	return validate(cfg)
}

func ValidateRuntimeConfig(cfg Config) error { return validate(cfg) }

func (cfg Config) SeriesSource() string { return filepath.Join(cfg.Source, "TV") }
func (cfg Config) SeriesTarget() string { return filepath.Join(cfg.Target, "TV") }
func (cfg Config) MovieSource() string  { return filepath.Join(cfg.Source, "Movies") }
func (cfg Config) MovieTarget() string  { return filepath.Join(cfg.Target, "Movies") }

func safeDestination(root, relative string) (string, error) {
	cleaned := filepath.Clean(filepath.FromSlash(relative))
	if cleaned == "." || filepath.IsAbs(cleaned) || cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("unsafe relative path %q", relative)
	}
	return filepath.Join(root, cleaned), nil
}
