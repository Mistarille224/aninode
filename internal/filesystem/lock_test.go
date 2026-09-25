package filesystem

import (
	"errors"
	"testing"
)

func TestWriterLockRejectsSecondWriterAndReleases(t *testing.T) {
	root := t.TempDir()
	first, err := AcquireWriterLock(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireWriterLock(root); !errors.Is(err, ErrWriterLocked) {
		t.Fatalf("second lock error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := AcquireWriterLock(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
}
