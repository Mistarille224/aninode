// Package filesystem is aninode's physical observation layer.
//
// Paths are names. ObjectID identifies the filesystem object currently named
// by a path inside one observation snapshot. Nothing in this package is
// persisted: every fact can be rebuilt from the filesystem.
package filesystem

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

// ObjectID is a stable identity only while the underlying filesystem object
// exists. Inode numbers are unique only within their device/filesystem.
type ObjectID struct {
	Device uint64
	Inode  uint64
}

// Metadata is physical filesystem metadata. It is observation, never user
// intent, and must never be persisted as aninode state.
type Metadata struct {
	ID    ObjectID
	Size  int64
	Mode  fs.FileMode
	NLink uint64
	UID   uint32
	GID   uint32
}

func (m Metadata) IsRegular() bool { return m.Mode.IsRegular() }
func (m Metadata) IsDir() bool     { return m.Mode.IsDir() }

// Object groups every observed path naming the same physical object.
type Object struct {
	Metadata
	Paths []string
}

// Issue records a path whose contents could not be safely observed. Issues are
// ephemeral uncertainty, not persisted state. A caller may still use positive
// facts elsewhere in the snapshot, but absence below an issue is not proof.
type Issue struct {
	Path string
	Err  error
}

// Snapshot is an inode-first view of one or more configured namespaces.
// Objects includes regular files; Directories gives directory identity as a
// first-class observation. Root is populated by ObserveTree.
type Snapshot struct {
	Root        string
	RootExists  bool
	RootObject  Metadata
	Objects     map[ObjectID]Object
	Paths       map[string]ObjectID
	Directories map[string]Metadata
	Issues      []Issue
}

func EmptySnapshot() Snapshot {
	return Snapshot{Objects: map[ObjectID]Object{}, Paths: map[string]ObjectID{}, Directories: map[string]Metadata{}}
}

// Complete reports whether the snapshot contains no known observation gaps.
func (s Snapshot) Complete() bool { return len(s.Issues) == 0 }

func Identity(info fs.FileInfo) (ObjectID, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return ObjectID{}, false
	}
	return ObjectID{Device: uint64(stat.Dev), Inode: uint64(stat.Ino)}, true
}

func MetadataOf(info fs.FileInfo) (Metadata, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return Metadata{}, false
	}
	return Metadata{
		ID:   ObjectID{Device: uint64(stat.Dev), Inode: uint64(stat.Ino)},
		Size: info.Size(), Mode: info.Mode(), NLink: uint64(stat.Nlink),
		UID: stat.Uid, GID: stat.Gid,
	}, true
}

// ObservePaths observes named regular files without following symlinks.
func ObservePaths(paths []string) (Snapshot, error) {
	s := EmptySnapshot()
	clean := append([]string(nil), paths...)
	sort.Strings(clean)
	for _, path := range clean {
		info, err := os.Lstat(path)
		if err != nil {
			return Snapshot{}, fmt.Errorf("lstat %s: %w", path, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return Snapshot{}, fmt.Errorf("%s is not a regular filesystem object", path)
		}
		meta, ok := MetadataOf(info)
		if !ok {
			return Snapshot{}, fmt.Errorf("filesystem identity unavailable for %s", path)
		}
		s.addRegular(filepath.Clean(path), meta)
	}
	return s, nil
}

// ObservePresentPaths observes the subset of named regular files that exists
// now. Missing paths are ignored, while symlinks and non-regular objects are
// rejected. It is useful for mutation guards when a downloader may still be
// materializing part of a task.
func ObservePresentPaths(paths []string) (Snapshot, error) {
	s := EmptySnapshot()
	clean := append([]string(nil), paths...)
	sort.Strings(clean)
	for _, path := range clean {
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return Snapshot{}, fmt.Errorf("lstat %s: %w", path, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return Snapshot{}, fmt.Errorf("%s is not a regular filesystem object", path)
		}
		meta, ok := MetadataOf(info)
		if !ok {
			return Snapshot{}, fmt.Errorf("filesystem identity unavailable for %s", path)
		}
		s.addRegular(filepath.Clean(path), meta)
	}
	return s, nil
}

// ObserveTree observes a namespace without following symlinks. Positive facts
// outside an unreadable/symlinked subtree remain useful; unsafe paths are
// recorded as Issues so consumers can downgrade only the affected Entry to
// unknown instead of failing the entire daemon cycle.
func ObserveTree(root string) (Snapshot, error) {
	root = filepath.Clean(root)
	s := EmptySnapshot()
	s.Root = root
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		path = filepath.Clean(path)
		if walkErr != nil {
			if path == root {
				return walkErr
			}
			s.Issues = append(s.Issues, Issue{Path: path, Err: walkErr})
			if entry != nil && entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		// DirEntry.Type is available from readdir and lets us reject a symlink
		// without following it via Info.
		if entry.Type()&os.ModeSymlink != 0 {
			if path == root {
				return fmt.Errorf("configured namespace root is a symlink: %s", path)
			}
			s.Issues = append(s.Issues, Issue{Path: path, Err: fmt.Errorf("symlink is not a reliable filesystem observation")})
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			if path == root {
				return fmt.Errorf("stat %s: %w", path, err)
			}
			s.Issues = append(s.Issues, Issue{Path: path, Err: fmt.Errorf("stat %s: %w", path, err)})
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		meta, ok := MetadataOf(info)
		if !ok {
			if path == root {
				return fmt.Errorf("filesystem identity unavailable for %s", path)
			}
			s.Issues = append(s.Issues, Issue{Path: path, Err: fmt.Errorf("filesystem identity unavailable")})
			if info.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if path == root && !info.IsDir() {
			return fmt.Errorf("configured namespace root is not a directory: %s", path)
		}
		if info.IsDir() {
			s.Directories[path] = meta
			if path == root {
				s.RootExists = true
				s.RootObject = meta
			}
			return nil
		}
		if info.Mode().IsRegular() {
			s.addRegular(path, meta)
		}
		return nil
	})
	if err != nil {
		return Snapshot{}, err
	}
	s.sortObjectPaths()
	return s, nil
}

func ObserveTreeIfPresent(root string) (Snapshot, error) {
	s, err := ObserveTree(root)
	if errors.Is(err, os.ErrNotExist) {
		empty := EmptySnapshot()
		empty.Root = filepath.Clean(root)
		return empty, nil
	}
	return s, err
}

func (s *Snapshot) addRegular(path string, meta Metadata) {
	obj := s.Objects[meta.ID]
	if len(obj.Paths) == 0 {
		obj.Metadata = meta
	}
	obj.Paths = append(obj.Paths, path)
	s.Objects[meta.ID] = obj
	s.Paths[path] = meta.ID
}

func (s *Snapshot) sortObjectPaths() {
	for id, obj := range s.Objects {
		if len(obj.Paths) > 1 {
			sort.Strings(obj.Paths)
			s.Objects[id] = obj
		}
	}
}

// Subtree projects an already observed snapshot onto root without touching the
// filesystem again. It is the basic operation used to share one physical
// observation across claim, inventory and publication planning.
func (s Snapshot) Subtree(root string) Snapshot {
	root = filepath.Clean(root)
	out := EmptySnapshot()
	out.Root = root
	if meta, ok := s.Directories[root]; ok {
		out.RootExists = true
		out.RootObject = meta
	}
	for path, meta := range s.Directories {
		if within(root, path) {
			out.Directories[path] = meta
		}
	}
	for _, object := range s.Objects {
		for _, path := range object.Paths {
			if within(root, path) {
				out.addRegular(path, object.Metadata)
			}
		}
	}
	for _, issue := range s.Issues {
		if within(root, issue.Path) {
			out.Issues = append(out.Issues, issue)
		}
	}
	return out
}

// ProjectSubtrees projects many roots in one pass over a snapshot. This avoids
// the O(entries * entries) behavior of calling Subtree independently for every
// Entry while preserving support for nested/overlapping Entry roots.
func (s Snapshot) ProjectSubtrees(roots map[string]string) map[string]Snapshot {
	out := make(map[string]Snapshot, len(roots))
	byRoot := map[string][]string{}
	for id, root := range roots {
		root = filepath.Clean(root)
		v := EmptySnapshot()
		v.Root = root
		if meta, ok := s.Directories[root]; ok {
			v.RootExists = true
			v.RootObject = meta
		}
		out[id] = v
		byRoot[root] = append(byRoot[root], id)
	}
	owners := func(path string) []string {
		path = filepath.Clean(path)
		var ids []string
		for current := path; ; current = filepath.Dir(current) {
			ids = append(ids, byRoot[current]...)
			parent := filepath.Dir(current)
			if parent == current {
				break
			}
		}
		return ids
	}
	for path, meta := range s.Directories {
		for _, id := range owners(path) {
			v := out[id]
			v.Directories[path] = meta
			out[id] = v
		}
	}
	for _, object := range s.Objects {
		for _, path := range object.Paths {
			for _, id := range owners(path) {
				v := out[id]
				v.addRegular(path, object.Metadata)
				out[id] = v
			}
		}
	}
	for _, issue := range s.Issues {
		for _, id := range owners(issue.Path) {
			v := out[id]
			v.Issues = append(v.Issues, issue)
			out[id] = v
		}
	}
	return out
}

// CommonNamespaceRoot returns the deepest directory that contains both roots.
// aninode's Docker contract keeps source and library inside one media namespace;
// observing that namespace once lets every projection share the same inode graph.
func CommonNamespaceRoot(first, second string) (string, error) {
	a, err := filepath.Abs(first)
	if err != nil {
		return "", err
	}
	b, err := filepath.Abs(second)
	if err != nil {
		return "", err
	}
	a, b = filepath.Clean(a), filepath.Clean(b)
	for current := a; ; current = filepath.Dir(current) {
		if within(current, b) {
			if current == string(filepath.Separator) {
				return "", fmt.Errorf("source and library must share a dedicated media namespace below filesystem root")
			}
			return current, nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
	}
	return "", fmt.Errorf("source and library do not share a media namespace")
}

// SameHardlinkDomain reports whether two observed roots can possibly share a
// hardlink. Equality of the underlying device is a necessary kernel property;
// link(2) remains the final authority at mutation time.
func SameHardlinkDomain(a, b Snapshot) bool {
	return a.RootExists && b.RootExists && a.RootObject.ID.Device == b.RootObject.ID.Device
}

func SameObject(first, second string) bool {
	a, err := os.Lstat(first)
	if err != nil || a.Mode()&os.ModeSymlink != 0 || !a.Mode().IsRegular() {
		return false
	}
	b, err := os.Lstat(second)
	if err != nil || b.Mode()&os.ModeSymlink != 0 || !b.Mode().IsRegular() {
		return false
	}
	aid, aok := Identity(a)
	bid, bok := Identity(b)
	return aok && bok && aid == bid
}

func (s Snapshot) ContainsAll(other Snapshot) bool {
	if len(other.Objects) == 0 {
		return false
	}
	for id := range other.Objects {
		if _, ok := s.Objects[id]; !ok {
			return false
		}
	}
	return true
}

func within(root, path string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// ObserveDirectory reads one structural directory without walking it or
// following a symlink. It is suitable for cheap readiness checks.
func ObserveDirectory(path string) (Metadata, error) {
	path = filepath.Clean(path)
	info, err := os.Lstat(path)
	if err != nil {
		return Metadata{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return Metadata{}, fmt.Errorf("%s is not a real directory", path)
	}
	meta, ok := MetadataOf(info)
	if !ok {
		return Metadata{}, fmt.Errorf("filesystem identity unavailable for %s", path)
	}
	return meta, nil
}

// ValidateHardlinkDomain performs a side-effect-free preflight. Device equality
// is necessary for hardlinks; link(2) during Apply remains the definitive test.
// EnsureHardlinkDomain materializes the two configured namespace roots for a
// mutation and verifies that the kernel reports them in the same hardlink
// domain. Observation-only paths should use ValidateHardlinkDomain instead so
// reads never create directories.
func EnsureHardlinkDomain(sourceRoot, targetRoot string, mode fs.FileMode) error {
	if mode == 0 {
		mode = 0o755
	}
	if err := os.MkdirAll(sourceRoot, mode); err != nil {
		return fmt.Errorf("create source root: %w", err)
	}
	if err := os.MkdirAll(targetRoot, mode); err != nil {
		return fmt.Errorf("create library root: %w", err)
	}
	return ValidateHardlinkDomain(sourceRoot, targetRoot)
}

func ValidateHardlinkDomain(sourceRoot, targetRoot string) error {
	source, err := ObserveDirectory(sourceRoot)
	if err != nil {
		return fmt.Errorf("source root: %w", err)
	}
	target, err := ObserveDirectory(targetRoot)
	if err != nil {
		return fmt.Errorf("library root: %w", err)
	}
	if source.ID.Device != target.ID.Device {
		return fmt.Errorf("source and library are not in the same hardlink domain (device %d != %d)", source.ID.Device, target.ID.Device)
	}
	return nil
}

// SyncDirectory makes prior namespace mutations durable on filesystems that
// implement directory fsync. It carries no state; callers use it after atomic
// rename/link/unlink sequences when crash durability matters.
func SyncDirectory(path string) error {
	dir, err := os.Open(filepath.Clean(path))
	if err != nil {
		return fmt.Errorf("open directory for sync: %w", err)
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("sync directory: %w", err)
	}
	return nil
}
