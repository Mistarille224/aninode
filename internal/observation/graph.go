// Package observation builds the ephemeral physical graph used by one
// reconciliation pass. It contains no lifecycle state and is safe to discard.
package observation

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"aninode/internal/catalog"
	"aninode/internal/filesystem"
)

// Graph is one coherent observation of aninode's configured source and
// library namespaces. Every consumer in a reconciliation pass should derive
// its view from this graph instead of independently rescanning paths.
type Graph struct {
	NamespaceRoot string
	Physical      filesystem.Snapshot
	SourceRoot    filesystem.Snapshot
	LibraryRoot   filesystem.Snapshot
	Sources       map[string]filesystem.Snapshot
	Libraries     map[string]filesystem.Snapshot
}

func Build(sourceRoot, libraryRoot string, entries map[string]catalog.Entry) (Graph, error) {
	namespaceRoot, err := filesystem.CommonNamespaceRoot(sourceRoot, libraryRoot)
	if err != nil {
		return Graph{}, err
	}
	physical, err := filesystem.ObserveTreeIfPresent(namespaceRoot)
	if err != nil {
		return Graph{}, fmt.Errorf("observe media namespace: %w", err)
	}
	source := physical.Subtree(sourceRoot)
	library := physical.Subtree(libraryRoot)
	g := Graph{
		NamespaceRoot: namespaceRoot, Physical: physical,
		SourceRoot: source, LibraryRoot: library,
		Sources: map[string]filesystem.Snapshot{}, Libraries: map[string]filesystem.Snapshot{},
	}
	ids := make([]string, 0, len(entries))
	sourceRoots := make(map[string]string, len(entries))
	libraryRoots := make(map[string]string, len(entries))
	for id, w := range entries {
		ids = append(ids, id)
		if strings.TrimSpace(w.DeclarationPath) != "" {
			sourceRoots[id] = w.DeclarationPath
		}
		if strings.TrimSpace(w.Path) != "" {
			libraryRoots[id] = w.Path
		}
	}
	sort.Strings(ids)
	projectedSources := source.ProjectSubtrees(sourceRoots)
	projectedLibraries := library.ProjectSubtrees(libraryRoots)
	for _, id := range ids {
		if snapshot, ok := projectedSources[id]; ok {
			g.Sources[id] = snapshot
		}
		if snapshot, ok := projectedLibraries[id]; ok {
			g.Libraries[id] = snapshot
		}
	}
	return g, nil
}

func (g Graph) HardlinkCapable() bool {
	return filesystem.SameHardlinkDomain(g.SourceRoot, g.LibraryRoot)
}

// SourceOwners returns the Entries whose current source namespace contains every
// physical object in task. Ownership is therefore an observed graph relation,
// not a persisted binding.
func (g Graph) SourceOwners(task filesystem.Snapshot) []string {
	var candidates map[string]bool
	for objectID := range task.Objects {
		next := map[string]bool{}
		for entryKey, snapshot := range g.Sources {
			if _, ok := snapshot.Objects[objectID]; ok && (candidates == nil || candidates[entryKey]) {
				next[entryKey] = true
			}
		}
		candidates = next
		if len(candidates) == 0 {
			return nil
		}
	}
	out := make([]string, 0, len(candidates))
	for id := range candidates {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// DirectoryStable reports whether a directory path in two observations names
// the same filesystem object. This is the namespace-level proof used around
// same-filesystem directory renames.
func DirectoryStable(before, after filesystem.Snapshot, beforePath, afterPath string) bool {
	b, bok := before.Directories[filepath.Clean(beforePath)]
	a, aok := after.Directories[filepath.Clean(afterPath)]
	return bok && aok && b.ID == a.ID
}
