package configstore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBootstrapCreatesFileFirstSkeleton(t *testing.T) {
	r := t.TempDir()
	created, e := Bootstrap(r, BootstrapOptions{})
	if e != nil || !created {
		t.Fatalf("%v %v", created, e)
	}
	for _, p := range []string{"organizer.json"} {
		if _, e := os.Stat(filepath.Join(r, p)); e != nil {
			t.Fatal(e)
		}
	}
}

func TestBootstrapRepairsPartialSkeletonWithoutOverwriting(t *testing.T) {
	r := t.TempDir()
	organizer := filepath.Join(r, "organizer.json")
	const existing = "{\"source\":\"/custom\"}\n"
	if err := os.WriteFile(organizer, []byte(existing), 0o640); err != nil {
		t.Fatal(err)
	}
	_, err := Bootstrap(r, BootstrapOptions{})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(organizer)
	if err != nil || string(data) != existing {
		t.Fatalf("existing organizer was changed: %q err=%v", data, err)
	}
	for _, path := range []string{"clients", "sources"} {
		if _, err := os.Stat(filepath.Join(r, path)); err != nil {
			t.Fatalf("missing %s: %v", path, err)
		}
	}
}

func TestBootstrapOptionsApplyOnlyToNewOrganizer(t *testing.T) {
	r := t.TempDir()
	created, err := Bootstrap(r, BootstrapOptions{Source: "/media/incoming", Target: "/media/ready", Extensions: []string{"mkv", ".mp4"}})
	if err != nil || !created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	data, err := os.ReadFile(filepath.Join(r, "organizer.json"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	for _, want := range []string{`"source": "/media/incoming"`, `"target": "/media/ready"`, `".mkv"`, `".mp4"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("organizer missing %s: %s", want, got)
		}
	}
	_, err = Bootstrap(r, BootstrapOptions{Source: "/media/ignored", Target: "/media/also-ignored", Extensions: []string{".avi"}})
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(r, "organizer.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != got {
		t.Fatalf("existing organizer was overwritten: before=%s after=%s", got, after)
	}
}
