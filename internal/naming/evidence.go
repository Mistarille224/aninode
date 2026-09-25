// Package naming derives transient external naming evidence from the live
// filesystem. Nothing in this package is persisted: directory and media names
// are observations that can be rebuilt on every operation.
package naming

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"aninode/internal/catalog"
	"aninode/internal/filesystem"
	"aninode/internal/mediafile"
	"aninode/internal/medianame"
)

type Evidence struct {
	Title  string
	Group  string
	Origin string
	Rank   int
}

type EntryEvidence struct {
	Names  []string
	Search []Evidence
}

type Index map[string]EntryEvidence

func BuildIndex(entries map[string]catalog.Entry, sources, libraries map[string]filesystem.Snapshot, extensions []string) Index {
	out := make(Index, len(entries))
	for key, entry := range entries {
		out[key] = BuildEntry(entry, sources[key], libraries[key], extensions)
	}
	return out
}

func BuildEntry(entry catalog.Entry, source, library filesystem.Snapshot, extensions []string) EntryEvidence {
	type scored struct {
		Evidence
		key string
	}
	byKey := map[string]scored{}
	add := func(title, group, origin string, rank int) {
		title = normalize(title)
		group = normalize(group)
		key := catalog.Comparable(title)
		if key == "" {
			return
		}
		compound := key + "\x00" + catalog.Comparable(group)
		current, exists := byKey[compound]
		candidate := scored{Evidence: Evidence{Title: title, Group: group, Origin: origin, Rank: rank}, key: key}
		if !exists || candidate.Rank > current.Rank || (candidate.Rank == current.Rank && candidate.Title < current.Title) {
			byKey[compound] = candidate
		}
	}

	// The configured Entry title is durable output intent, but also useful as
	// the weakest naming observation when the filesystem is empty.
	add(entry.Title, "", "entry_title", 10)
	addDirectory := func(root, origin string, rank int) {
		base := strings.TrimSpace(filepath.Base(filepath.Clean(root)))
		if base == "" || base == "." || base == string(filepath.Separator) {
			return
		}
		parsed := medianame.AnalyzeMany([]string{base}).Items[0].Components
		if normalize(parsed.Title) != "" {
			add(parsed.Title, parsed.ReleaseGroup, origin, rank)
			return
		}
		add(base, "", origin, rank)
	}
	if source.RootExists {
		addDirectory(source.Root, "source_directory", 70)
	}
	if library.RootExists {
		addDirectory(library.Root, "library_directory", 60)
	}

	extset := mediafile.ExtensionSet(extensions)
	addFiles := func(snapshot filesystem.Snapshot, origin string, rank int) {
		paths := make([]string, 0, len(snapshot.Paths))
		for path := range snapshot.Paths {
			if mediafile.HasAllowedExtension(path, extset) {
				paths = append(paths, path)
			}
		}
		sort.Strings(paths)
		if len(paths) == 0 {
			return
		}
		names := make([]string, len(paths))
		for i, path := range paths {
			names[i] = filepath.Base(path)
		}
		batch := medianame.AnalyzeFiles(names)
		for _, item := range batch.Items {
			title := normalize(item.Components.Title)
			if title == "" {
				continue
			}
			// Episode files are strongest because differential parsing can
			// recover tracker-facing titles even when one filename is weak.
			if item.Components.EpisodeStart > 0 {
				if reliable, ok := medianame.ReliableEpisodeTitle(item); ok {
					title = reliable
				}
			}
			add(title, item.Components.ReleaseGroup, origin, rank)
			add(title, "", origin, rank-1)
		}
	}
	addFiles(source, "source_media", 100)
	addFiles(library, "library_media", 90)

	values := make([]scored, 0, len(byKey))
	for _, value := range byKey {
		values = append(values, value)
	}
	sort.Slice(values, func(i, j int) bool {
		if values[i].Rank != values[j].Rank {
			return values[i].Rank > values[j].Rank
		}
		if values[i].key != values[j].key {
			return values[i].key < values[j].key
		}
		return strings.ToLower(values[i].Group) < strings.ToLower(values[j].Group)
	})

	seenName := map[string]bool{}
	out := EntryEvidence{}
	for _, value := range values {
		out.Search = append(out.Search, value.Evidence)
		key := catalog.Comparable(value.Title)
		if !seenName[key] {
			seenName[key] = true
			out.Names = append(out.Names, value.Title)
		}
	}
	return out
}

func Match(entry catalog.Entry, evidence EntryEvidence, title string, year int) bool {
	if year != 0 && entry.Year != 0 && year != entry.Year {
		return false
	}
	queries := []string{catalog.Comparable(title)}
	// A bare trailing year is intentionally not removed by the context-free
	// media parser because it may be part of a real title (for example
	// "Blade Runner 2049"). Once a concrete Entry supplies the expected year,
	// however, that exact suffix can be tested without guessing.
	if entry.Year != 0 {
		if stripped := stripKnownYearSuffix(title, entry.Year); stripped != strings.TrimSpace(title) {
			queries = append(queries, catalog.Comparable(stripped))
		}
	}
	for _, query := range queries {
		if query == "" {
			continue
		}
		for _, name := range evidence.Names {
			if catalog.Comparable(name) == query {
				return true
			}
		}
	}
	return false
}

func stripKnownYearSuffix(title string, year int) string {
	value := strings.TrimSpace(title)
	y := fmt.Sprintf("%d", year)
	for _, suffix := range []string{" (" + y + ")", "." + y, "_" + y, " " + y} {
		if strings.HasSuffix(value, suffix) {
			base := strings.Trim(strings.TrimSpace(strings.TrimSuffix(value, suffix)), " ._-")
			if base != "" {
				return base
			}
		}
	}
	return value
}

func normalize(value string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(value)), " ")
}
