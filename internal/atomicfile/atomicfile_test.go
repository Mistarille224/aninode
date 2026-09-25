package atomicfile

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteReplacesCompleteFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Write(path, []byte("new\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "new\n" {
		t.Fatalf("content=%q", data)
	}
}

func TestCreateDoesNotReplaceExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "receipt.json")
	if err := Create(path, []byte("first"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := Create(path, []byte("second"), 0o640); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("Create error=%v, want fs.ErrExist", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "first" {
		t.Fatalf("existing content changed: %q", data)
	}
}
