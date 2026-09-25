// Package mediafile contains the small set of filesystem rules shared by
// source publication and library inventory.
package mediafile

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// IsIncompleteArtifact reports filenames that are downloader bookkeeping or
// partial-file names, rather than final media.
func IsIncompleteArtifact(path string) bool {
	lower := strings.ToLower(filepath.Base(path))
	return strings.HasSuffix(lower, ".!qb") || strings.HasSuffix(lower, ".part") || strings.HasSuffix(lower, ".aria2")
}

func ReadyPath(path string, extensions map[string]bool) (bool, error) {
	if IsIncompleteArtifact(path) || !HasAllowedExtension(path, extensions) {
		return false, nil
	}
	_, err := os.Stat(path + ".aria2")
	if err == nil {
		return false, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	return true, nil
}

// Ready reports whether path has an allowed media extension and no known
// downloader incomplete evidence. aria2 creates the final filename early, so
// its adjacent control file suppresses that file until the next observation.
func Ready(fsys fs.FS, path string, extensions map[string]bool) (bool, error) {
	if IsIncompleteArtifact(path) || !HasAllowedExtension(path, extensions) {
		return false, nil
	}
	_, err := fs.Stat(fsys, path+".aria2")
	if err == nil {
		return false, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	return true, nil
}

// HasAllowedExtension reports whether path has one of the normalized configured media extensions.
func HasAllowedExtension(path string, extensions map[string]bool) bool {
	return extensions[strings.ToLower(filepath.Ext(path))]
}

// ExtensionSet normalizes configured media extensions.
func ExtensionSet(values []string) map[string]bool {
	set := make(map[string]bool, len(values))
	for _, value := range values {
		set[strings.ToLower("."+strings.TrimPrefix(value, "."))] = true
	}
	return set
}
