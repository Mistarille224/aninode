package naming

import (
	"os"
	"path/filepath"
	"testing"

	"aninode/internal/catalog"
	"aninode/internal/filesystem"
)

func TestBuildEntryUsesSourceLibraryDirectoriesAndAllMediaNames(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "downloads", "TV", "abcabc 甲乙甲乙")
	lib := filepath.Join(root, "library", "TV", "abcabc (2004)")
	for _, dir := range []string{filepath.Join(src, "Season 02"), filepath.Join(lib, "Season 02")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	files := []string{
		filepath.Join(src, "Season 02", "[grpabc166] abcabc 甲乙甲乙 丙丁丙丁 - 43 [1080P].mp4"),
		filepath.Join(src, "Season 02", "nested", "[grpabc154] abcabc defdef ghighi - 46 [1080p].mkv"),
		filepath.Join(src, "Season 02", "nested", "[grpabc167] abcabc dd - 47.avi"),
		filepath.Join(lib, "Season 02", "abcabc (2004) S02E42.mkv"),
		filepath.Join(lib, "Season 02", "abcabc defdef ghighi S02E41.webm"),
	}
	for _, path := range files {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ss, err := filesystem.ObserveTree(src)
	if err != nil {
		t.Fatal(err)
	}
	ls, err := filesystem.ObserveTree(lib)
	if err != nil {
		t.Fatal(err)
	}
	got := BuildEntry(catalog.Entry{Title: "abcabc", Year: 2004}, ss, ls, []string{".mkv", ".mp4", ".avi", ".webm"})
	want := []string{"abcabc 甲乙甲乙 丙丁丙丁", "abcabc defdef ghighi", "abcabc dd", "abcabc defdef ghighi", "abcabc", "abcabc 甲乙甲乙"}
	for _, name := range want {
		found := false
		for _, value := range got.Names {
			if catalog.Comparable(value) == catalog.Comparable(name) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("missing %q in %#v", name, got.Names)
		}
	}
	if got.Search[0].Origin != "source_media" {
		t.Fatalf("search order=%+v", got.Search)
	}
}

func TestMatchUsesKnownEntryYearWithoutRewritingAmbiguousTitle(t *testing.T) {
	evidence := EntryEvidence{Names: []string{"abcabc147", "abcabc defdef 2049"}}
	if !Match(catalog.Entry{Title: "abcabc147", Year: 2024}, evidence, "abcabc147 2024", 0) {
		t.Fatal("known entry year did not qualify bare year suffix")
	}
	if !Match(catalog.Entry{Title: "abcabc defdef 2049", Year: 1982}, evidence, "abcabc defdef 2049", 0) {
		t.Fatal("numeric title was not preserved")
	}
	if Match(catalog.Entry{Title: "abcabc defdef", Year: 1982}, EntryEvidence{Names: []string{"abcabc defdef"}}, "abcabc defdef 2049", 0) {
		t.Fatal("unrelated numeric title suffix was treated as the entry year")
	}
}
