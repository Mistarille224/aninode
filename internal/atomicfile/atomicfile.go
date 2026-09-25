package atomicfile

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Write durably replaces path with data using a temporary file in the same
// directory. The file is synced before rename and the parent directory is
// synced after the rename becomes visible.
func Write(path string, data []byte, mode fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	tmp, err := writeTemp(path, data, mode)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

// Create durably creates path without replacing an existing file.
func Create(path string, data []byte, mode fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	tmp, err := writeTemp(path, data, mode)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	if err := os.Link(tmp, path); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fs.ErrExist
		}
		return err
	}
	if err := os.Remove(tmp); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

func writeTemp(path string, data []byte, mode fs.FileMode) (string, error) {
	f, err := os.CreateTemp(filepath.Dir(path), ".aninode-*.tmp")
	if err != nil {
		return "", err
	}
	name := f.Name()
	failed := true
	defer func() {
		if failed {
			_ = f.Close()
			_ = os.Remove(name)
		}
	}()
	if err := f.Chmod(mode); err != nil {
		return "", err
	}
	if _, err := f.Write(data); err != nil {
		return "", err
	}
	if err := f.Sync(); err != nil {
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	failed = false
	return name, nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open parent directory: %w", err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("sync parent directory: %w", err)
	}
	return nil
}
