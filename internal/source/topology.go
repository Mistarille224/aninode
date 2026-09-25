// Package source defines downloader placement and source directory topology.
// Source names are deliberately absent from every correctness decision here.
package source

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"aninode/internal/download"
)

var ErrNoWantedTaskFiles = errors.New("no wanted task files")

type MediaType string

const (
	Series MediaType = "series"
	Movie  MediaType = "movie"
)

type Shape string

const (
	SingleFile Shape = "single-file"
	MultiFile  Shape = "multi-file"
)

type Placement struct {
	SavePath string
}

// Observation is the normalized, local view of one downloader task. ContentRoot
// is the directory which owns the observed assets even when ContentPath names a
// single file. It is deliberately derived here so migration, claim and
// reconciliation cannot develop different path rules.
type Observation struct {
	Task        download.Task
	ContentRoot string
	Paths       []string
	Shape       Shape
}

// Observe maps downloader paths and resolves the actual content container.
// ContentPath is authoritative. Without it, SavePath is accepted only when all
// wanted files are directly inside it; a nested common directory is not guessed.
func Observe(task download.Task, files []download.File, mappings []download.PathMapping) (Observation, error) {
	task.SavePath = download.MapPath(task.SavePath, mappings)
	task.ContentPath = download.MapPath(task.ContentPath, mappings)
	var paths []string
	for _, file := range files {
		if file.Wanted {
			paths = append(paths, filepath.Clean(download.MapPath(file.Path, mappings)))
		}
	}
	if len(paths) == 0 {
		return Observation{}, ErrNoWantedTaskFiles
	}
	observation := Observation{Task: task, Paths: paths}
	if task.ContentPath != "" {
		content := filepath.Clean(task.ContentPath)
		if len(paths) == 1 && samePath(paths[0], content) {
			observation.ContentRoot, observation.Shape = filepath.Dir(content), SingleFile
			return observation, nil
		}
		for _, path := range paths {
			if !within(content, path) || samePath(content, path) {
				return Observation{}, fmt.Errorf("task file %q is outside content path %q", path, content)
			}
		}
		observation.ContentRoot, observation.Shape = content, MultiFile
		return observation, nil
	}
	if task.SavePath == "" {
		return Observation{}, fmt.Errorf("task has neither content path nor save path")
	}
	save := filepath.Clean(task.SavePath)
	for _, path := range paths {
		if !samePath(filepath.Dir(path), save) {
			return Observation{}, fmt.Errorf("content path is missing and files are not flat in save path")
		}
	}
	observation.ContentRoot = save
	if len(paths) == 1 {
		observation.Shape = SingleFile
	} else {
		observation.Shape = MultiFile
	}
	return observation, nil
}

// PlacementFor computes the downloader save path. It never creates a directory.
// containerName is only an operational name for content that does not create its
// own top-level directory; it is not an ownership invariant.
func PlacementFor(media MediaType, shape Shape, entryRoot, sourceRoot, containerName string) (Placement, error) {
	if shape != SingleFile && shape != MultiFile {
		return Placement{}, fmt.Errorf("source shape is required")
	}
	switch media {
	case Series:
		if entryRoot == "" {
			return Placement{}, fmt.Errorf("series entry root is required")
		}
	case Movie:
		if sourceRoot == "" {
			return Placement{}, fmt.Errorf("movie source root is required")
		}
		if shape == MultiFile {
			return Placement{SavePath: filepath.Clean(sourceRoot)}, nil
		}
	default:
		return Placement{}, fmt.Errorf("unknown media type %q", media)
	}
	containerName = strings.TrimSpace(containerName)
	if containerName == "" || containerName == "." || filepath.Base(containerName) != containerName {
		return Placement{}, fmt.Errorf("safe source container name is required")
	}
	base := entryRoot
	if media == Movie {
		base = sourceRoot
		if shape == MultiFile {
			// A multifile movie torrent creates its own top-level content
			// container, so the downloader is placed at the movie source root.
			return Placement{SavePath: filepath.Clean(base)}, nil
		}
	}
	// Series placement is deliberately different: the first-level directory
	// below an Entry is the user-visible season projection container. A
	// multifile torrent may then create its own opaque/grouping directory
	// below it. Directory names below the season container never create media
	// identity; leaf release/file names remain the only parser input.
	return Placement{SavePath: filepath.Join(base, containerName)}, nil
}

type Topology struct {
	Valid         bool
	RelativeDepth int
	Detail        string
}

// ValidateSeries validates a claimed downloader source below one Entry root.
// The first-level child of EntryRoot is the season projection container. A
// multifile torrent may create one or more grouping directories below it;
// those directory names are structural only and never contribute season or
// episode identity.
func ValidateSeries(entryRoot, contentRoot string, mediaPaths []string) Topology {
	if entryRoot == "" || contentRoot == "" || len(mediaPaths) == 0 {
		return Topology{Detail: "root, content root, and observed media are required"}
	}
	topology := ValidateSeriesRoot(entryRoot, contentRoot)
	if !topology.Valid {
		return topology
	}
	return validatePaths(contentRoot, mediaPaths, topology, "asset")
}

// ValidateSeriesRoot validates only the structural content-root placement for
// one durable Entry. It is safe for downloader prefiltering when ContentPath
// itself has already been verified against the live filesystem.
func ValidateSeriesRoot(entryRoot, contentRoot string) Topology {
	return validateSeriesRoot(entryRoot, contentRoot, 1, 2)
}

// ValidateSeriesAtSource validates the same invariant from the configured
// series source root: Source/<entry>/<season>/[grouping...]/media. The Entry and
// season container names are locators/projection keys only. Media identity is
// always parsed from leaf release/file names.
func ValidateSeriesAtSource(sourceRoot, contentRoot string, mediaPaths []string) Topology {
	if sourceRoot == "" || contentRoot == "" || len(mediaPaths) == 0 {
		return Topology{Detail: "root, content root, and observed media are required"}
	}
	topology := ValidateSeriesAtSourceRoot(sourceRoot, contentRoot)
	if !topology.Valid {
		return topology
	}
	return validatePaths(contentRoot, mediaPaths, topology, "asset")
}

// ValidateSeriesAtSourceRoot validates only series content-root depth below the
// configured series source root.
func ValidateSeriesAtSourceRoot(sourceRoot, contentRoot string) Topology {
	return validateSeriesRoot(sourceRoot, contentRoot, 2, 3)
}

func validateSeriesRoot(parentRoot, contentRoot string, minDepth, maxDepth int) Topology {
	root, content := filepath.Clean(parentRoot), filepath.Clean(contentRoot)
	if parentRoot == "" || contentRoot == "" {
		return Topology{Detail: "root and content root are required"}
	}
	rel, err := filepath.Rel(root, content)
	if err != nil || rel == "." || escapes(rel) {
		return Topology{Detail: "series content root is outside the configured source"}
	}
	depth := len(strings.Split(filepath.ToSlash(rel), "/"))
	if depth < minDepth || depth > maxDepth {
		return Topology{RelativeDepth: depth, Detail: fmt.Sprintf("series content root depth is %d; want %d..%d", depth, minDepth, maxDepth)}
	}
	return Topology{Valid: true, RelativeDepth: depth, Detail: "valid source topology"}
}

// ValidateMovie requires SourceRoot/<exactly one movie container>/recognizable
// bundle assets. Movie semantics remain the responsibility of moviebundle.
func ValidateMovie(sourceRoot, contentRoot string, assetPaths []string) Topology {
	if sourceRoot == "" || contentRoot == "" || len(assetPaths) == 0 {
		return Topology{Detail: "root, content root, and observed assets are required"}
	}
	topology := ValidateMovieRoot(sourceRoot, contentRoot)
	if !topology.Valid {
		return topology
	}
	return validatePaths(contentRoot, assetPaths, topology, "asset")
}

// ValidateMovieRoot validates only the movie container placement below the
// configured movie source root.
func ValidateMovieRoot(sourceRoot, contentRoot string) Topology {
	root, content := filepath.Clean(sourceRoot), filepath.Clean(contentRoot)
	if sourceRoot == "" || contentRoot == "" {
		return Topology{Detail: "root and content root are required"}
	}
	rel, err := filepath.Rel(root, content)
	if err != nil || rel == "." || escapes(rel) {
		return Topology{Detail: "content root is not one container below the required root"}
	}
	depth := len(strings.Split(filepath.ToSlash(rel), "/"))
	if depth != 1 {
		return Topology{RelativeDepth: depth, Detail: fmt.Sprintf("content root depth is %d; want exactly 1", depth)}
	}
	return Topology{Valid: true, RelativeDepth: depth, Detail: "valid source topology"}
}

func validatePaths(contentRoot string, paths []string, topology Topology, noun string) Topology {
	content := filepath.Clean(contentRoot)
	for _, path := range paths {
		assetRel, err := filepath.Rel(content, filepath.Clean(path))
		if err != nil || assetRel == "." || escapes(assetRel) {
			topology.Valid = false
			topology.Detail = fmt.Sprintf("%s %q escapes content root", noun, path)
			return topology
		}
	}
	return topology
}

func escapes(rel string) bool {
	return filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func samePath(a, b string) bool { return filepath.Clean(a) == filepath.Clean(b) }

func within(root, path string) bool {
	if root == "" || path == "" {
		return false
	}
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	return err == nil && !escapes(rel)
}
