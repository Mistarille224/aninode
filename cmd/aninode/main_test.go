package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func minimalConfig(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	cfg := filepath.Join(root, "config")
	if err := os.MkdirAll(cfg, 0755); err != nil {
		t.Fatal(err)
	}
	body := `{"source":"` + filepath.Join(root, "downloads") + `","target":"` + filepath.Join(root, "library") + `","extensions":[".mkv"]}`
	if err := os.WriteFile(filepath.Join(cfg, "organizer.json"), []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	return cfg
}
func TestServeRejectsRemovedTokenOptions(t *testing.T) {
	for _, option := range []string{"-api-token-file", "-allow-unauthenticated-management"} {
		err := runServe([]string{option})
		if err == nil || !strings.Contains(err.Error(), "flag provided but not defined") {
			t.Fatalf("removed option accepted: %v", err)
		}
	}
}
func TestInitMakesExplicitSkeleton(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "config")
	if err := runInit([]string{"-config-dir", cfg}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cfg, "organizer.json")); err != nil {
		t.Fatal(err)
	}
}

func TestInitAcceptsFirstRunOrganizerDefaults(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "config")
	if err := runInit([]string{"-config-dir", cfg, "-source", "/media/incoming", "-target", "/media/ready", "-extensions", "mkv,.m2ts"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(cfg, "organizer.json"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	for _, want := range []string{`"source": "/media/incoming"`, `"target": "/media/ready"`, `".mkv"`, `".m2ts"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("organizer missing %s: %s", want, got)
		}
	}
	if err := runInit([]string{"-config-dir", cfg, "-source", "/media/ignored", "-target", "/media/ignored-library"}); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(cfg, "organizer.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != got {
		t.Fatalf("init overwrote persisted organizer: before=%s after=%s", got, after)
	}
}

func TestPreflightVerifiesExistingRootsWithoutLeavingProbes(t *testing.T) {
	root := t.TempDir()
	cfg := filepath.Join(root, "config")
	source := filepath.Join(root, "downloads")
	target := filepath.Join(root, "library")
	for _, path := range []string{cfg, source, target} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	body := `{"source":"` + source + `","target":"` + target + `","extensions":[".mkv"]}`
	if err := os.WriteFile(filepath.Join(cfg, "organizer.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runPreflight([]string{"-config-dir", cfg}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{source, target} {
		entries, err := os.ReadDir(path)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Fatalf("preflight left probe files in %s: %v", path, entries)
		}
	}
}

func TestPreflightDefersMissingFirstRunRootsWithoutCreatingThem(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "downloads")
	target := filepath.Join(root, "library")
	ready, err := verifyMediaPaths(source, target)
	if err != nil {
		t.Fatal(err)
	}
	if ready {
		t.Fatal("missing first-run media roots reported ready")
	}
	for _, path := range []string{source, target} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("preflight created %s: %v", path, err)
		}
	}
}

func TestPreflightRejectsNonDirectoryMediaPath(t *testing.T) {
	root := t.TempDir()
	blocker := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	ready, err := verifyMediaPaths(blocker, filepath.Join(root, "library"))
	if ready || err == nil || !strings.Contains(err.Error(), "is not a real directory") {
		t.Fatalf("unexpected result ready=%v err=%v", ready, err)
	}
}
