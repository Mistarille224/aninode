package releasefilter

import (
	"testing"

	"aninode/internal/medianame"
	"aninode/internal/release"
)

func TestEmptyFiltersAreOpenAndExplicitDimensionsRestrict(t *testing.T) {
	r := release.Release{Group: "grpabc166", Resolution: "1080p", Subtitle: "CHT"}
	if reason := (Filters{}).RejectReason(r); reason != "" {
		t.Fatalf("empty filters rejected release: %s", reason)
	}
	f := Filters{Groups: []string{"grpabc154", "grpabc166"}, Resolutions: []string{"2160p"}}
	if reason := f.RejectReason(r); reason == "" {
		t.Fatal("explicit resolution filter did not reject release")
	}
	f.Resolutions = nil
	if reason := f.RejectReason(r); reason != "" {
		t.Fatalf("group allowlist should accept grpabc166: %s", reason)
	}
}

func TestOptionsMergeAndNormalizeReuseCaseInsensitiveIdentity(t *testing.T) {
	a := NewOptions([]string{" grpabc166 ", "GRPABC166"}, []string{"1080P"}, nil)
	b := OptionsFromReleases([]release.Release{{Group: "grpabc154", Resolution: "1080p", Subtitle: "CHT"}})
	got := a.Merge(b)
	if len(got.Groups) != 2 || len(got.Resolutions) != 1 || len(got.Subtitles) != 1 {
		t.Fatalf("options=%+v", got)
	}
}

func TestOptionsFromNamesUsesSameTraitVocabularyAsReleases(t *testing.T) {
	options := OptionsFromNames([]medianame.Name{
		{Components: medianame.Components{ReleaseGroup: "grpabc166", Resolution: "1080p", Subtitle: "zh-Hans"}},
		{Components: medianame.Components{ReleaseGroup: "GRPABC166", Resolution: "2160p", Subtitle: "zh-Hant"}},
	})
	if len(options.Groups) != 1 || options.Groups[0] != "grpabc166" {
		t.Fatalf("groups=%v", options.Groups)
	}
	if len(options.Resolutions) != 2 || len(options.Subtitles) != 2 {
		t.Fatalf("options=%+v", options)
	}
}

func TestFiltersEqualIgnoresOrderingCaseAndWhitespace(t *testing.T) {
	a := Filters{Groups: []string{" grpabc166 ", "grpabc154"}, Resolutions: []string{"1080P"}}
	b := Filters{Groups: []string{"GRPABC154", "GRPABC166"}, Resolutions: []string{"1080p"}}
	if !a.Equal(b) {
		t.Fatalf("filters should be equal: a=%+v b=%+v", a, b)
	}
}
