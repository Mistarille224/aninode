package observation

import (
	"os"
	"path/filepath"
	"testing"

	"aninode/internal/catalog"
	"aninode/internal/filesystem"
)

func TestGraphProjectsOnePhysicalObjectAcrossNamespaces(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	library := filepath.Join(root, "library")
	sourceWork := filepath.Join(source, "TV", "abcabc146")
	libraryWork := filepath.Join(library, "TV", "abcabc146")
	if err := os.MkdirAll(sourceWork, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(libraryWork, 0o755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(sourceWork, "abcabc146 - 01.mkv")
	dst := filepath.Join(libraryWork, "abcabc146 S01E01.mkv")
	if err := os.WriteFile(src, []byte("media"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(src, dst); err != nil {
		t.Fatal(err)
	}
	w := catalog.Entry{Key: "w", DeclarationPath: sourceWork, Path: libraryWork}
	g, err := Build(source, library, map[string]catalog.Entry{"w": w})
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Sources["w"].Objects) != 1 || len(g.Libraries["w"].Objects) != 1 {
		t.Fatalf("unexpected projections: %+v", g)
	}
	var sourceID filesystem.ObjectID
	for id := range g.Sources["w"].Objects {
		sourceID = id
	}
	if _, ok := g.Libraries["w"].Objects[sourceID]; !ok {
		t.Fatal("hardlink object identity was not preserved across graph projections")
	}
	if g.SourceRoot.RootObject.ID == (filesystem.ObjectID{}) {
		t.Fatal("source directory identity was not observed")
	}
}

func TestGraphLocalizesSymlinkAsUnknownObservation(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	library := filepath.Join(root, "library")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(root, filepath.Join(source, "escape")); err != nil {
		t.Fatal(err)
	}
	g, err := Build(source, library, nil)
	if err != nil {
		t.Fatal(err)
	}
	if g.SourceRoot.Complete() || len(g.SourceRoot.Issues) != 1 {
		t.Fatalf("symlink should be localized as observation issue: %+v", g.SourceRoot.Issues)
	}
}

func TestDirectoryStableAcrossRename(t *testing.T) {
	root := t.TempDir()
	beforePath := filepath.Join(root, "before")
	afterPath := filepath.Join(root, "after")
	if err := os.Mkdir(beforePath, 0o755); err != nil {
		t.Fatal(err)
	}
	before, err := filesystem.ObserveTree(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(beforePath, afterPath); err != nil {
		t.Fatal(err)
	}
	after, err := filesystem.ObserveTree(root)
	if err != nil {
		t.Fatal(err)
	}
	if !DirectoryStable(before, after, beforePath, afterPath) {
		t.Fatal("directory rename should preserve object identity")
	}
}

func TestGraphIssueOutsideWorkDoesNotPoisonWorkProjection(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	library := filepath.Join(root, "library")
	workSource := filepath.Join(source, "TV", "abcabc146")
	workLibrary := filepath.Join(library, "TV", "abcabc146")
	if err := os.MkdirAll(workSource, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(workLibrary, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(root, filepath.Join(source, "unrelated-link")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	w := catalog.Entry{Key: "w", DeclarationPath: workSource, Path: workLibrary}
	g, err := Build(source, library, map[string]catalog.Entry{"w": w})
	if err != nil {
		t.Fatal(err)
	}
	if g.SourceRoot.Complete() {
		t.Fatal("root should record unrelated issue")
	}
	if !g.Sources["w"].Complete() {
		t.Fatalf("entry projection poisoned: %+v", g.Sources["w"].Issues)
	}
}
