package configstore

import (
	"aninode/internal/atomicfile"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
)

var writableIDRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

type Store struct {
	Root string
}

// PutOrganizer atomically updates the global filesystem roots and rules using
// the same rollback validation as directory resources.
func (store Store) PutOrganizer(data []byte) error {
	if store.Root == "" {
		return errors.New("configuration root is required")
	}
	return store.putValidated(filepath.Join(store.Root, "organizer.json"), data, true)
}

// Put atomically writes one strict JSON resource, validates the complete
// configuration graph, and rolls back if the new graph is invalid.
func (store Store) Put(kind, id string, data []byte) error {
	path, err := store.resourcePath(kind, id)
	if err != nil {
		return err
	}
	return store.putValidated(path, data, false)
}

func (store Store) putValidated(path string, data []byte, requireExisting bool) error {
	if len(data) > 1<<20 {
		return errors.New("configuration resource exceeds 1 MiB")
	}
	old, oldErr := os.ReadFile(path)
	if oldErr != nil && !errors.Is(oldErr, fs.ErrNotExist) {
		return oldErr
	}
	if requireExisting && errors.Is(oldErr, fs.ErrNotExist) {
		return oldErr
	}
	if err := atomicfile.Write(path, data, 0o640); err != nil {
		return err
	}
	if _, err := Load(store.Root); err != nil {
		if oldErr == nil {
			if rollbackErr := atomicfile.Write(path, old, 0o640); rollbackErr != nil {
				return errors.Join(fmt.Errorf("configuration rejected: %w", err), fmt.Errorf("restore previous configuration: %w", rollbackErr))
			}
		} else if rollbackErr := os.Remove(path); rollbackErr != nil && !errors.Is(rollbackErr, fs.ErrNotExist) {
			return errors.Join(fmt.Errorf("configuration rejected: %w", err), fmt.Errorf("remove rejected configuration: %w", rollbackErr))
		}
		return fmt.Errorf("configuration rejected: %w", err)
	}
	return nil
}

// Delete removes a resource only if the remaining configuration graph is
// valid. Otherwise the original file is restored atomically.
func (store Store) Delete(kind, id string) error {
	path, err := store.resourcePath(kind, id)
	if err != nil {
		return err
	}
	old, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	if _, err := Load(store.Root); err != nil {
		if rollbackErr := atomicfile.Write(path, old, 0o640); rollbackErr != nil {
			return errors.Join(fmt.Errorf("configuration deletion rejected: %w", err), fmt.Errorf("restore deleted configuration: %w", rollbackErr))
		}
		return fmt.Errorf("configuration deletion rejected: %w", err)
	}
	return nil
}

func (store Store) resourcePath(kind, id string) (string, error) {
	if store.Root == "" {
		return "", errors.New("configuration root is required")
	}
	if kind != "clients" && kind != "sources" {
		return "", fmt.Errorf("unsupported configuration resource %q", kind)
	}
	if !writableIDRE.MatchString(id) || id == "." || id == ".." {
		return "", fmt.Errorf("invalid configuration ID %q", id)
	}
	return filepath.Join(store.Root, kind, id+".json"), nil
}

// Backup captures the editable 1.0 configuration resources so an application
// runtime-construction failure can roll back an otherwise structurally valid
// file mutation without leaving disk and the last-known-good runtime split.
type Backup map[string][]byte

func (store Store) Backup() (Backup, error) {
	if store.Root == "" {
		return nil, errors.New("configuration root is required")
	}
	out := Backup{}
	for _, rel := range []string{"organizer.json"} {
		b, err := os.ReadFile(filepath.Join(store.Root, rel))
		if err == nil {
			out[rel] = b
		} else if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
	}
	for _, dir := range []string{"clients", "sources"} {
		entries, err := os.ReadDir(filepath.Join(store.Root, dir))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, ent := range entries {
			if ent.IsDir() || filepath.Ext(ent.Name()) != ".json" {
				continue
			}
			rel := filepath.Join(dir, ent.Name())
			b, err := os.ReadFile(filepath.Join(store.Root, rel))
			if err != nil {
				return nil, err
			}
			out[rel] = b
		}
	}
	return out, nil
}

func (store Store) Restore(backup Backup) error {
	if store.Root == "" {
		return errors.New("configuration root is required")
	}
	for _, rel := range []string{"organizer.json"} {
		if _, ok := backup[rel]; !ok {
			if err := os.Remove(filepath.Join(store.Root, rel)); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		}
	}
	for _, dir := range []string{"clients", "sources"} {
		root := filepath.Join(store.Root, dir)
		entries, err := os.ReadDir(root)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		for _, ent := range entries {
			if ent.IsDir() || filepath.Ext(ent.Name()) != ".json" {
				continue
			}
			rel := filepath.Join(dir, ent.Name())
			if _, ok := backup[rel]; !ok {
				if err := os.Remove(filepath.Join(store.Root, rel)); err != nil && !errors.Is(err, fs.ErrNotExist) {
					return err
				}
			}
		}
	}
	for rel, data := range backup {
		if err := atomicfile.Write(filepath.Join(store.Root, rel), data, 0o640); err != nil {
			return err
		}
	}
	return nil
}
