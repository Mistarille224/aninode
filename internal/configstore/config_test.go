package configstore

import (
	"aninode/internal/catalog"
	"aninode/internal/organizer"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOrganizerForPublicationReusesObservedFolderProjection(t *testing.T) {
	root := t.TempDir()
	entryRoot := filepath.Join(root, "source", "TV", "abcabc")
	seasonRoot := filepath.Join(entryRoot, "Season 02")
	id := "series/abcabc"
	b := Bundle{
		Organizer: organizer.Config{Source: filepath.Join(root, "source"), Target: filepath.Join(root, "library"), Extensions: []string{".mkv"}},
		Entries: map[string]catalog.Entry{id: {
			Key: id, MediaType: catalog.MediaSeries, Title: "abcabc", Enabled: true,
			DeclarationPath: entryRoot, Path: filepath.Join(root, "library", "TV", "abcabc"), Seasons: []int{1, 2},
			FolderProjections: map[string]catalog.FolderProjection{"Season 02": {Season: 2, EpisodeOffset: -40}},
		}},
	}
	cfg, err := b.OrganizerForPublication(id, 1, 2, seasonRoot)
	if err != nil {
		t.Fatal(err)
	}
	destination, matched, err := organizer.PreviewDestination(cfg, "abcabc - 41.mkv")
	if err != nil || !matched {
		t.Fatalf("preview matched=%v err=%v", matched, err)
	}
	want := filepath.Join(root, "library", "TV", "abcabc", "Season 02", "abcabc S02E01.mkv")
	if destination != want {
		t.Fatalf("destination=%q want=%q", destination, want)
	}
}

func TestLoadDiscoversDeclaredFilesystemEntriesAndSingleClient(t *testing.T) {
	root := t.TempDir()
	lib := filepath.Join(root, "library")
	src := filepath.Join(root, "downloads")
	cfg := filepath.Join(root, "config")
	must := func(p, s string) {
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(s), 0644); err != nil {
			t.Fatal(err)
		}
	}
	must(filepath.Join(cfg, "organizer.json"), `{"source":"`+src+`","target":"`+lib+`","extensions":[".mkv"]}`)
	must(filepath.Join(cfg, "clients/c.json"), `{"id":"c","type":"aria2","url":"http://x","enabled":true}`)
	if err := os.MkdirAll(filepath.Join(lib, "TV", "abcabc146 (2026)", "Season 01"), 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.AdoptUndeclaredAt(filepath.Join(lib, "TV"), filepath.Join(src, "TV")); err != nil {
		t.Fatal(err)
	}
	b, err := Load(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Entries) != 1 {
		t.Fatalf("entries=%d", len(b.Entries))
	}
	for _, w := range b.Entries {
		if !w.Enabled {
			t.Fatal("empty declaration should be enabled")
		}
	}
}

func TestBuiltInSourceAcceptsRSSOverrideButRejectsSearchEndpoint(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "downloads")
	lib := filepath.Join(root, "library")
	cfg := filepath.Join(root, "config")
	for _, dir := range []string{src, lib, filepath.Join(cfg, "sources")} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(cfg, "organizer.json"), []byte(`{"source":"`+src+`","target":"`+lib+`","extensions":[".mkv"]}`), 0644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cfg, "sources", "mikan.json")
	if err := os.WriteFile(path, []byte(`{"id":"mikan","provider":"mikan","rss":[{"url":"https://example.test/rss"}],"enabled":true}`), 0644); err != nil {
		t.Fatal(err)
	}
	b, err := Load(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Sources["mikan"].RSS) != 1 {
		t.Fatalf("rss=%+v", b.Sources["mikan"].RSS)
	}
	if err := os.WriteFile(path, []byte(`{"id":"mikan","provider":"mikan","search":{"url_template":"https://example.test/?q={query}"},"enabled":true}`), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(cfg); err == nil {
		t.Fatal("expected built-in search endpoint configuration to be rejected")
	}
}

func TestContentSourceRejectsDuplicateRSSURLs(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "downloads")
	lib := filepath.Join(root, "library")
	cfg := filepath.Join(root, "config")
	for _, dir := range []string{src, lib, filepath.Join(cfg, "sources")} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(cfg, "organizer.json"), []byte(`{"source":"`+src+`","target":"`+lib+`","extensions":[".mkv"]}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg, "sources", "mikan.json"), []byte(`{"id":"mikan","provider":"mikan","rss":[{"url":"https://example.test/rss"},{"url":"https://example.test/rss"}],"enabled":true}`), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(cfg); err == nil || !strings.Contains(err.Error(), "duplicate RSS URL") {
		t.Fatalf("err=%v", err)
	}
}

func TestOrganizerForPreviewPreservesSeasonZero(t *testing.T) {
	root := t.TempDir()
	id := "series/abcabc042"
	entryRoot := filepath.Join(root, "downloads", "TV", "abcabc042")
	b := Bundle{
		Organizer: organizer.Config{Source: filepath.Join(root, "downloads"), Target: filepath.Join(root, "library"), Extensions: []string{".mkv"}},
		Entries: map[string]catalog.Entry{id: {
			Key: id, MediaType: catalog.MediaSeries, Title: "abcabc042", Enabled: true,
			DeclarationPath: entryRoot, Path: filepath.Join(root, "library", "TV", "abcabc042"), Seasons: []int{0},
		}},
	}
	cfg, err := b.OrganizerForPreview(id, 0)
	if err != nil {
		t.Fatal(err)
	}
	destination, matched, err := organizer.PreviewDestination(cfg, "abcabc042 S00E03.mkv")
	if err != nil || !matched {
		t.Fatalf("preview matched=%v err=%v", matched, err)
	}
	want := filepath.Join(root, "library", "TV", "abcabc042", "Season 00", "abcabc042 S00E03.mkv")
	if destination != want {
		t.Fatalf("destination=%q want=%q", destination, want)
	}
}

func TestClientURLRejectsEmbeddedCredentials(t *testing.T) {
	root := t.TempDir()
	cfg := filepath.Join(root, "config")
	src := filepath.Join(root, "downloads")
	lib := filepath.Join(root, "library")
	if err := os.MkdirAll(filepath.Join(cfg, "clients"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg, "organizer.json"), []byte(`{"source":"`+src+`","target":"`+lib+`","extensions":[".mkv"]}`), 0644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cfg, "clients", "c.json")
	if err := os.WriteFile(path, []byte(`{"id":"c","type":"qbittorrent","url":"http://alice:secret@example.test:8080","enabled":true}`), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(cfg); err == nil || !strings.Contains(err.Error(), "embedded credentials") {
		t.Fatalf("err=%v", err)
	}
}
