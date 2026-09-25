package acquisition

import (
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

type IdentityInput struct {
	InfoHash    string
	MagnetURL   string
	DownloadURL string
	GUID        string
}

type Identity struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Canonical string `json:"canonical"`
}

// CanonicalIdentity prefers a BitTorrent info hash, then a normalized download
// URL, and finally the feed GUID. The resulting ID is stable and filename-safe.
func CanonicalIdentity(input IdentityInput) (Identity, error) {
	if input.InfoHash != "" {
		return identityFromInfoHash(input.InfoHash)
	}
	if input.MagnetURL != "" {
		infoHash, err := infoHashFromMagnet(input.MagnetURL)
		if err != nil {
			return Identity{}, err
		}
		return identityFromInfoHash(infoHash)
	}
	if input.DownloadURL != "" {
		canonical, err := normalizeURL(input.DownloadURL)
		if err != nil {
			return Identity{}, fmt.Errorf("normalize download URL: %w", err)
		}
		return hashedIdentity("url", canonical), nil
	}
	if strings.TrimSpace(input.GUID) != "" {
		return hashedIdentity("guid", strings.TrimSpace(input.GUID)), nil
	}
	return Identity{}, errors.New("info hash, magnet URL, download URL, or GUID is required")
}

func identityFromInfoHash(value string) (Identity, error) {
	normalized, err := normalizeInfoHash(value)
	if err != nil {
		return Identity{}, err
	}
	return Identity{ID: "btih-" + normalized, Kind: "btih", Canonical: normalized}, nil
}

func normalizeInfoHash(value string) (string, error) {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(strings.ToLower(value), "urn:btih:")
	if len(value) == 40 {
		if _, err := hex.DecodeString(value); err == nil {
			return value, nil
		}
	}
	if len(value) == 32 {
		decoded, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(value))
		if err == nil && len(decoded) == 20 {
			return hex.EncodeToString(decoded), nil
		}
	}
	return "", fmt.Errorf("invalid BitTorrent info hash %q", value)
}

func infoHashFromMagnet(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil || !strings.EqualFold(parsed.Scheme, "magnet") {
		return "", fmt.Errorf("invalid magnet URL %q", raw)
	}
	for _, exactTopic := range parsed.Query()["xt"] {
		if strings.HasPrefix(strings.ToLower(exactTopic), "urn:btih:") {
			return exactTopic, nil
		}
	}
	return "", errors.New("magnet URL has no urn:btih exact topic")
}

func normalizeURL(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", err
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("unsupported scheme %q", parsed.Scheme)
	}
	if parsed.Hostname() == "" {
		return "", errors.New("URL host is required")
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	hostname := strings.ToLower(parsed.Hostname())
	port := parsed.Port()
	if (parsed.Scheme == "http" && port == "80") || (parsed.Scheme == "https" && port == "443") {
		port = ""
	}
	if strings.Contains(hostname, ":") {
		hostname = "[" + hostname + "]"
	}
	if port != "" {
		parsed.Host = net.JoinHostPort(strings.Trim(hostname, "[]"), port)
	} else {
		parsed.Host = hostname
	}
	parsed.Fragment = ""
	if parsed.Path == "" {
		parsed.Path = "/"
	}
	parsed.RawQuery = parsed.Query().Encode()
	return parsed.String(), nil
}

func hashedIdentity(kind, canonical string) Identity {
	digest := sha256.Sum256([]byte(canonical))
	return Identity{
		ID:        kind + "-" + hex.EncodeToString(digest[:]),
		Kind:      kind,
		Canonical: canonical,
	}
}
