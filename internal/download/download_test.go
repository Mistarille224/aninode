package download

import "testing"

func TestMapPathUsesLongestBoundaryMatch(t *testing.T) {
	mappings := []PathMapping{{Remote: "/downloads", Local: "/media"}, {Remote: "/downloads/anime", Local: "/media/anime-special"}}
	if got := MapPath("/downloads/anime/show/file.mkv", mappings); got != "/media/anime-special/show/file.mkv" {
		t.Fatalf("MapPath = %q", got)
	}
	if got := MapPath("/downloads-other/file.mkv", mappings); got != "/downloads-other/file.mkv" {
		t.Fatalf("boundary MapPath = %q", got)
	}
	if got := UnmapPath("/media/anime-special/show/file.mkv", mappings); got != "/downloads/anime/show/file.mkv" {
		t.Fatalf("UnmapPath = %q", got)
	}
}
