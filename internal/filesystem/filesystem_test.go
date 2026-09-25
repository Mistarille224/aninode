package filesystem

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestSnapshotCollapsesHardlinksIntoOneObject(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "a.mkv")
	b := filepath.Join(root, "b.mkv")
	if err := os.WriteFile(a, []byte("media"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(a, b); err != nil {
		t.Fatal(err)
	}
	snapshot, err := ObserveTree(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Objects) != 1 || len(snapshot.Paths) != 2 {
		t.Fatalf("snapshot=%+v", snapshot)
	}
	for _, object := range snapshot.Objects {
		if object.NLink < 2 || len(object.Paths) != 2 {
			t.Fatalf("object=%+v", object)
		}
	}
}

func TestSameObjectRejectsDistinctFiles(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "a.mkv")
	b := filepath.Join(root, "b.mkv")
	if err := os.WriteFile(a, []byte("media"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("media"), 0o644); err != nil {
		t.Fatal(err)
	}
	if SameObject(a, b) {
		t.Fatal("distinct files reported as one object")
	}
}

func TestLinkObservedRejectsSymlinkTraversalAndPreservesSourceIdentity(t *testing.T) {
	root := t.TempDir()
	sourceRoot := filepath.Join(root, "source")
	targetRoot := filepath.Join(root, "target")
	outside := filepath.Join(root, "outside")
	for _, dir := range []string{sourceRoot, targetRoot, outside} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	source := filepath.Join(sourceRoot, "episode.mkv")
	if err := os.WriteFile(source, []byte("media"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(source)
	if err != nil {
		t.Fatal(err)
	}
	id, ok := Identity(info)
	if !ok {
		t.Fatal("identity unavailable")
	}
	if err := os.Symlink(outside, filepath.Join(targetRoot, "Season 01")); err != nil {
		t.Fatal(err)
	}
	if _, err := LinkObserved(sourceRoot, source, targetRoot, filepath.Join(targetRoot, "Season 01", "episode.mkv"), id, 0o755); err == nil {
		t.Fatal("expected symlink traversal rejection")
	}
	if _, err := os.Stat(filepath.Join(outside, "episode.mkv")); !os.IsNotExist(err) {
		t.Fatalf("link escaped target root: %v", err)
	}
}

func TestLinkObservedIsIdempotentForSameObject(t *testing.T) {
	root := t.TempDir()
	sourceRoot := filepath.Join(root, "source")
	targetRoot := filepath.Join(root, "target")
	if err := os.MkdirAll(sourceRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(targetRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(sourceRoot, "episode.mkv")
	if err := os.WriteFile(source, []byte("media"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Lstat(source)
	id, _ := Identity(info)
	target := filepath.Join(targetRoot, "Season 01", "episode.mkv")
	created, err := LinkObserved(sourceRoot, source, targetRoot, target, id, 0o755)
	if err != nil || !created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	created, err = LinkObserved(sourceRoot, source, targetRoot, target, id, 0o755)
	if err != nil || created {
		t.Fatalf("idempotent created=%v err=%v", created, err)
	}
}

func TestRenameNoReplaceIsAtomicAgainstExistingDestination(t *testing.T) {
	root := t.TempDir()
	oldPath := filepath.Join(root, "old")
	newPath := filepath.Join(root, "new")
	if err := os.WriteFile(oldPath, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newPath, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RenameNoReplace(oldPath, newPath); err == nil {
		t.Fatal("expected no-replace conflict")
	}
	oldData, _ := os.ReadFile(oldPath)
	newData, _ := os.ReadFile(newPath)
	if string(oldData) != "old" || string(newData) != "new" {
		t.Fatalf("rename changed files: old=%q new=%q", oldData, newData)
	}
	if err := os.Remove(newPath); err != nil {
		t.Fatal(err)
	}
	if err := RenameNoReplace(oldPath, newPath); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("old path remains: %v", err)
	}
}

func TestObserveTreeLocalizesNestedSymlinkIssue(t *testing.T) {
	root := t.TempDir()
	good := filepath.Join(root, "good")
	bad := filepath.Join(root, "bad")
	if err := os.MkdirAll(good, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(bad, 0o755); err != nil {
		t.Fatal(err)
	}
	media := filepath.Join(good, "episode.mkv")
	if err := os.WriteFile(media, []byte("media"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(media, filepath.Join(bad, "link.mkv")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	s, err := ObserveTree(root)
	if err != nil {
		t.Fatal(err)
	}
	if s.Complete() || len(s.Issues) != 1 {
		t.Fatalf("issues=%+v", s.Issues)
	}
	if _, ok := s.Paths[media]; !ok {
		t.Fatal("positive observation outside bad subtree was lost")
	}
	if !s.Subtree(good).Complete() {
		t.Fatal("unrelated subtree inherited observation issue")
	}
	if s.Subtree(bad).Complete() {
		t.Fatal("affected subtree lost observation issue")
	}
}

func TestProjectSubtreesPreservesNestedOwnershipAndIssues(t *testing.T) {
	root := t.TempDir()
	outer := filepath.Join(root, "outer")
	inner := filepath.Join(outer, "inner")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(inner, "episode.mkv")
	if err := os.WriteFile(file, []byte("media"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(file, filepath.Join(inner, "unsafe.mkv")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	s, err := ObserveTree(root)
	if err != nil {
		t.Fatal(err)
	}

	projected := s.ProjectSubtrees(map[string]string{"outer": outer, "inner": inner})
	for _, id := range []string{"outer", "inner"} {
		if len(projected[id].Objects) != 1 || len(projected[id].Issues) != 1 {
			t.Fatalf("%s projection=%+v", id, projected[id])
		}
	}
}

func TestCommonNamespaceRootUsesDedicatedParent(t *testing.T) {
	root := t.TempDir()
	media := filepath.Join(root, "media")
	source := filepath.Join(media, "downloads")
	target := filepath.Join(media, "library")
	got, err := CommonNamespaceRoot(source, target)
	if err != nil {
		t.Fatal(err)
	}
	if got != media {
		t.Fatalf("common root=%q want %q", got, media)
	}
}

func TestCommonNamespaceRootRejectsFilesystemRootOnly(t *testing.T) {
	if _, err := CommonNamespaceRoot("/downloads", "/library"); err == nil {
		t.Fatal("expected filesystem-root-only namespace rejection")
	}
}

func BenchmarkSnapshotAddHardlinkPaths(b *testing.B) {
	meta := Metadata{ID: ObjectID{Device: 1, Inode: 1}, Mode: 0o644}
	paths := make([]string, 256)
	for i := range paths {
		paths[i] = fmt.Sprintf("/media/show/link-%04d.mkv", i)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s := EmptySnapshot()
		for _, path := range paths {
			s.addRegular(path, meta)
		}
		s.sortObjectPaths()
	}
}
