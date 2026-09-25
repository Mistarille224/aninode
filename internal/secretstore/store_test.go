package secretstore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRecoverableSecretLifecycle(t *testing.T) {
	root := t.TempDir()
	store := Store{Root: root}
	id := DownloaderPassword("client-a")
	if _, set, err := store.Lookup(id); err != nil || set {
		t.Fatalf("initial lookup set=%v err=%v", set, err)
	}
	if err := store.Put(id, "top-secret"); err != nil {
		t.Fatal(err)
	}
	got, set, err := store.Lookup(id)
	if err != nil || !set || got != "top-secret" {
		t.Fatalf("got=%q set=%v err=%v", got, set, err)
	}
	path := filepath.Join(root, "secrets", "integrations", "downloader", "client-a", "password.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "top-secret") || !strings.Contains(string(data), "ciphertext") {
		t.Fatalf("secret not encrypted on disk: %s", data)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("secret mode=%v err=%v", info.Mode().Perm(), err)
	}
	key := filepath.Join(root, "secrets", "master.key")
	if info, err := os.Stat(key); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("key mode=%v err=%v", info.Mode().Perm(), err)
	}
	if err := store.Delete(id); err != nil {
		t.Fatal(err)
	}
	if _, set, err := store.Lookup(id); err != nil || set {
		t.Fatalf("deleted lookup set=%v err=%v", set, err)
	}
}

func TestSecretIdentityIsAuthenticated(t *testing.T) {
	root := t.TempDir()
	store := Store{Root: root}
	source := Integration("downloader", "a", "password")
	target := Integration("tmdb", "default", "token")
	if err := store.Put(source, "value"); err != nil {
		t.Fatal(err)
	}
	src, _ := store.path(source)
	dst, _ := store.path(target)
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Lookup(target); err == nil || !strings.Contains(err.Error(), "authentication failed") {
		t.Fatalf("copied ciphertext unexpectedly accepted: %v", err)
	}
}

func TestInvalidSecretIdentityRejected(t *testing.T) {
	store := Store{Root: t.TempDir()}
	for _, id := range []ID{
		Integration("../tmdb", "default", "token"),
		Integration("tmdb", "../default", "token"),
		Integration("tmdb", "default", "../token"),
	} {
		if err := store.Put(id, "value"); err == nil {
			t.Fatalf("invalid id accepted: %+v", id)
		}
	}
}
