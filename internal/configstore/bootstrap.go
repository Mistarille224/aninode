package configstore

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"aninode/internal/organizer"
)

var defaultExtensions = []string{".mkv", ".mp4", ".avi", ".mov", ".m4v", ".ts", ".webm"}

type BootstrapOptions struct {
	Source     string
	Target     string
	Extensions []string
}

func bootstrapOrganizer(options BootstrapOptions) organizer.Config {
	source := strings.TrimSpace(options.Source)
	if source == "" {
		source = "/media/downloads"
	}
	target := strings.TrimSpace(options.Target)
	if target == "" {
		target = "/media/library"
	}
	extensions := options.Extensions
	if len(extensions) == 0 {
		extensions = defaultExtensions
	}
	normalized := make([]string, 0, len(extensions))
	for _, extension := range extensions {
		extension = strings.TrimSpace(extension)
		if extension == "" {
			continue
		}
		if !strings.HasPrefix(extension, ".") {
			extension = "." + extension
		}
		normalized = append(normalized, extension)
	}
	if len(normalized) == 0 {
		normalized = append(normalized, defaultExtensions...)
	}
	return organizer.Config{Source: source, Target: target, Extensions: normalized}
}

// Bootstrap creates only the non-operational configuration skeleton required
// to start the setup UI. Bootstrap options are initialization defaults: they
// are consulted only when organizer.json does not already exist and never
// override persisted configuration.
func Bootstrap(root string, options BootstrapOptions) (bool, error) {
	if root == "" {
		return false, errors.New("configuration directory is required")
	}
	created := false
	for _, directory := range []string{"clients", "sources"} {
		if err := os.MkdirAll(filepath.Join(root, directory), 0o750); err != nil {
			return false, err
		}
	}
	organizerPath := filepath.Join(root, "organizer.json")
	if _, err := os.Stat(organizerPath); errors.Is(err, fs.ErrNotExist) {
		cfg := bootstrapOrganizer(options)
		if err := organizer.ValidateConfig(cfg); err != nil {
			return false, err
		}
		data, err := json.MarshalIndent(cfg, "", "  ")
		if err != nil {
			return false, err
		}
		data = append(data, '\n')
		made, err := createBootstrapFile(organizerPath, string(data))
		if err != nil {
			return false, err
		}
		created = created || made
	} else if err != nil {
		return false, err
	}
	return created, nil
}

func createBootstrapFile(path, data string) (bool, error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if errors.Is(err, fs.ErrExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if _, err = file.WriteString(data); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		_ = os.Remove(path)
		return false, err
	}
	if closeErr != nil {
		return false, closeErr
	}
	return true, nil
}
