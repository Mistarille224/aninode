package medianame

import (
	"regexp"
	"strings"
)

// TechnicalTraits are coarse, filename-observable release properties used to
// compare historical review candidates with already-managed neighboring
// episodes. They are intentionally small and reconstructible: no provider or
// group reputation is persisted or inferred here.
type TechnicalTraits struct {
	Source     string
	SourceKind string
	VideoCodec string
	BitDepth   string
	AudioCodec string
}

var technicalSpaceRE = regexp.MustCompile(`\s+`)

// TechnicalTraitsOf recognizes only common, unambiguous release tokens. An
// unknown value stays empty so callers can prefer stronger evidence instead of
// manufacturing a match from free-form metadata.
func TechnicalTraitsOf(input string) TechnicalTraits {
	value := strings.ToLower(input)
	normalized := technicalSpaceRE.ReplaceAllString(strings.NewReplacer("_", " ", ".", " ").Replace(value), " ")
	has := func(tokens ...string) bool {
		for _, token := range tokens {
			if strings.Contains(value, token) || strings.Contains(normalized, token) {
				return true
			}
		}
		return false
	}

	var out TechnicalTraits
	switch {
	case has("web-dl", "webdl", "web dl"):
		out.Source, out.SourceKind = "web-dl", "web"
	case has("webrip", "web-rip", "web rip"):
		out.Source, out.SourceKind = "webrip", "web"
	case has("blu-ray", "bluray", "blu ray", "bdremux", "bd-remux"):
		out.Source, out.SourceKind = "bluray", "disc"
	case has("bdrip", "bd-rip", "bd rip"):
		out.Source, out.SourceKind = "bdrip", "disc"
	case has("hdtv"):
		out.Source, out.SourceKind = "hdtv", "broadcast"
	case has("tvrip", "tv-rip", "tv rip"):
		out.Source, out.SourceKind = "tvrip", "broadcast"
	}

	switch {
	case has("av1"):
		out.VideoCodec = "av1"
	case has("hevc", "h265", "h.265", "x265"):
		out.VideoCodec = "hevc"
	case has("avc", "h264", "h.264", "x264"):
		out.VideoCodec = "avc"
	}

	switch {
	case has("10bit", "10-bit", "10 bit", "hi10p"):
		out.BitDepth = "10bit"
	case has("12bit", "12-bit", "12 bit"):
		out.BitDepth = "12bit"
	case has("8bit", "8-bit", "8 bit"):
		out.BitDepth = "8bit"
	}

	switch {
	case has("e-ac-3", "eac3", "e-ac3", "ddp", "dd+"):
		out.AudioCodec = "eac3"
	case has("flac"):
		out.AudioCodec = "flac"
	case has("opus"):
		out.AudioCodec = "opus"
	case has("aac"):
		out.AudioCodec = "aac"
	case has("ac3", "ac-3"):
		out.AudioCodec = "ac3"
	}
	return out
}
