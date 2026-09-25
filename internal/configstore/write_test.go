package configstore

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStorePutAndRollback(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "sources"), 0755)
	os.MkdirAll(filepath.Join(root, "clients"), 0755)
	os.WriteFile(filepath.Join(root, "organizer.json"), []byte(`{"source":"`+filepath.Join(root, "media", "downloads")+`","target":"`+filepath.Join(root, "media", "library")+`","extensions":[".mkv"]}`), 0644)
	st := Store{Root: root}
	good := []byte(`{"id":"f","provider":"generic","rss":[{"url":"https://e/rss"}],"enabled":true}`)
	if e := st.Put("sources", "f", good); e != nil {
		t.Fatal(e)
	}
	bad := []byte(`{"id":"x","provider":"generic","rss":[{"url":"https://e/rss"}],"enabled":true}`)
	if e := st.Put("sources", "f", bad); e == nil {
		t.Fatal("expected rejection")
	}
}
