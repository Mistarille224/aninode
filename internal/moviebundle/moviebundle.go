// Package moviebundle observes and classifies a movie source as one logical
// publication unit. It deliberately uses filesystem and filename evidence
// only; disc trees are opaque and are never interpreted internally.
package moviebundle

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

type Kind string

const (
	Files    Kind = "files"
	BluRay   Kind = "bluray"
	DVD      Kind = "dvd"
	ISO      Kind = "iso"
	Conflict Kind = "conflict"
)

type AssetKind string

const (
	Video    AssetKind = "video"
	Audio    AssetKind = "audio"
	Subtitle AssetKind = "subtitle"
	Extra    AssetKind = "extra"
	Opaque   AssetKind = "opaque"
	Excluded AssetKind = "excluded"
	Unknown  AssetKind = "unknown"
)

type ExtraKind string

const (
	Trailer      ExtraKind = "trailer"
	Interview    ExtraKind = "interview"
	Featurette   ExtraKind = "featurette"
	BehindScenes ExtraKind = "behind_the_scenes"
	DeletedScene ExtraKind = "deleted_scene"
	Scene        ExtraKind = "scene"
	Short        ExtraKind = "short"
	OtherExtra   ExtraKind = "extra"
)

type Identity struct {
	Title string `json:"title"`
	Year  int    `json:"year,omitempty"`
}
type Asset struct {
	Path     string    `json:"path"`
	Relative string    `json:"relative"`
	Kind     AssetKind `json:"kind"`
	Detail   string    `json:"detail,omitempty"`
}
type Version struct {
	Video     Asset   `json:"video"`
	Audios    []Asset `json:"audios,omitempty"`
	Subtitles []Asset `json:"subtitles,omitempty"`
	Label     string  `json:"label,omitempty"`
}
type MovieExtra struct {
	Kind  ExtraKind `json:"kind"`
	Asset Asset     `json:"asset"`
}
type OpaqueTree struct {
	Root   string  `json:"root"`
	Assets []Asset `json:"assets"`
}
type Bundle struct {
	Recognized bool         `json:"recognized"`
	Kind       Kind         `json:"kind"`
	Identity   Identity     `json:"identity"`
	Versions   []Version    `json:"versions,omitempty"`
	Extras     []MovieExtra `json:"extras,omitempty"`
	OpaqueTree *OpaqueTree  `json:"opaque_tree,omitempty"`
	Excluded   []Asset      `json:"excluded,omitempty"`
	Unknown    []Asset      `json:"unknown,omitempty"`
	Warnings   []string     `json:"warnings,omitempty"`
	Conflict   string       `json:"conflict,omitempty"`
}
type Options struct {
	Title     string
	Year      int
	Blacklist []string
	Overrides map[string]string
}

var suffixRE = regexp.MustCompile(`(?i)(\.(?:[a-z]{2,3}(?:-[a-z]{2})?|forced|default|sdh|cc))+$`)
var tokenRE = regexp.MustCompile(`[\pL\pN]+`)

func Observe(root string, opts Options) (Bundle, error) { return AnalyzePaths(root, nil, opts) }

// AnalyzePaths analyzes an optional downloader-provided path cohort. With nil
// paths it walks root once. Every path must be a regular file below root.
func AnalyzePaths(root string, paths []string, opts Options) (Bundle, error) {
	b := Bundle{Kind: Files, Identity: Identity{Title: strings.TrimSpace(opts.Title), Year: opts.Year}}
	var files []string
	if paths == nil {
		err := filepath.WalkDir(root, func(path string, e fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if path == root || e.IsDir() {
				return nil
			}
			if e.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("symlink is not a reliable movie asset: %s", path)
			}
			if e.Type().IsRegular() {
				files = append(files, path)
			}
			return nil
		})
		if err != nil {
			return b, err
		}
	} else {
		files = append(files, paths...)
	}
	sort.Strings(files)
	type discRoot struct {
		kind Kind
		rel  string
	}
	roots := map[string]discRoot{}
	var iso, ordinary []Asset
	for _, path := range files {
		rel, err := filepath.Rel(root, path)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return b, fmt.Errorf("movie asset escapes source: %s", path)
		}
		rel = filepath.ToSlash(rel)
		parts := strings.Split(rel, "/")
		for i, p := range parts[:len(parts)-1] {
			if strings.EqualFold(p, "BDMV") || strings.EqualFold(p, "VIDEO_TS") {
				k := BluRay
				if strings.EqualFold(p, "VIDEO_TS") {
					k = DVD
				}
				rr := strings.Join(parts[:i+1], "/")
				roots[string(k)+":"+rr] = discRoot{k, rr}
				break
			}
		}
		a := Asset{Path: path, Relative: rel}
		if strings.EqualFold(filepath.Ext(path), ".iso") {
			a.Kind = Opaque
			iso = append(iso, a)
		} else {
			ordinary = append(ordinary, a)
		}
	}
	if len(roots) > 0 {
		b.Recognized = true
		if len(roots) != 1 || len(iso) > 0 {
			return conflict(b, "mixed or multiple opaque movie bundle roots"), nil
		}
		var rootInfo discRoot
		for _, r := range roots {
			rootInfo = r
		}
		var assets []Asset
		var outside bool
		prefix := rootInfo.rel + "/"
		for _, a := range ordinary {
			if strings.HasPrefix(a.Relative, prefix) {
				a.Kind = Opaque
				assets = append(assets, a)
			} else if filepath.Base(a.Relative) != ".aninode.json" {
				outside = true
			}
		}
		if outside {
			return conflict(b, "ordinary movie assets and an opaque disc tree were detected together"), nil
		}
		b.Kind = rootInfo.kind
		b.OpaqueTree = &OpaqueTree{Root: rootInfo.rel, Assets: assets}
		if len(assets) == 0 {
			return conflict(b, "opaque disc tree contains no regular files"), nil
		}
		return b, nil
	}
	ordinary = filterDeclaration(ordinary)
	if len(iso) > 0 {
		b.Recognized = true
		if len(iso) != 1 || len(ordinary) > 0 {
			return conflict(b, "ISO and ordinary movie assets were detected together"), nil
		}
		b.Kind = ISO
		b.OpaqueTree = &OpaqueTree{Assets: iso}
		return b, nil
	}
	return classifyFiles(b, ordinary, opts), nil
}

func filterDeclaration(in []Asset) []Asset {
	out := in[:0]
	for _, a := range in {
		if filepath.Base(a.Relative) != ".aninode.json" {
			out = append(out, a)
		}
	}
	return out
}
func conflict(b Bundle, reason string) Bundle { b.Kind = Conflict; b.Conflict = reason; return b }

func classifyFiles(b Bundle, assets []Asset, opts Options) Bundle {
	var videos, sidecars []Asset
	seen := map[string]bool{}
	overrides := map[string]string{}
	for path, kind := range opts.Overrides {
		overrides[filepath.ToSlash(filepath.Clean(path))] = kind
	}
	for _, a := range assets {
		seen[a.Relative] = true
		if p, ok := blacklist(opts.Blacklist, a.Relative); ok {
			a.Kind = Excluded
			a.Detail = "matched Entry blacklist pattern " + fmt.Sprintf("%q", p)
			b.Excluded = append(b.Excluded, a)
			continue
		}
		o := strings.ToLower(strings.TrimSpace(overrides[a.Relative]))
		ext := strings.ToLower(filepath.Ext(a.Relative))
		if o == "exclude" {
			a.Kind = Excluded
			a.Detail = "movie classification override"
			b.Excluded = append(b.Excluded, a)
			continue
		}
		if ek, ok := extraOverride(o); ok {
			a.Kind = Extra
			b.Extras = append(b.Extras, MovieExtra{ek, a})
			continue
		}
		switch {
		case isVideo(ext):
			b.Recognized = true
			if ek, ok := detectExtra(a.Relative, opts.Title, opts.Year); ok && o != "version" {
				a.Kind = Extra
				b.Extras = append(b.Extras, MovieExtra{ek, a})
			} else {
				a.Kind = Video
				videos = append(videos, a)
			}
		case isSubtitle(ext):
			a.Kind = Subtitle
			sidecars = append(sidecars, a)
		case isAudio(ext):
			a.Kind = Audio
			sidecars = append(sidecars, a)
		default:
			a.Kind = Excluded
			a.Detail = "non-media ancillary asset"
			b.Excluded = append(b.Excluded, a)
		}
	}
	for path := range overrides {
		if !seen[filepath.ToSlash(path)] {
			b.Warnings = append(b.Warnings, fmt.Sprintf("classification override references missing asset %s", path))
		}
	}
	for _, v := range videos {
		b.Versions = append(b.Versions, Version{Video: v, Label: versionLabel(v.Relative)})
	}
	for _, s := range sidecars {
		indexes := associated(s, b.Versions)
		if len(indexes) != 1 {
			s.Kind = Unknown
			s.Detail = "sidecar cannot be associated with exactly one movie version"
			b.Unknown = append(b.Unknown, s)
			continue
		}
		i := indexes[0]
		if s.Kind == Audio {
			b.Versions[i].Audios = append(b.Versions[i].Audios, s)
		} else {
			b.Versions[i].Subtitles = append(b.Versions[i].Subtitles, s)
		}
	}
	if len(b.Versions) == 0 {
		return conflict(b, "movie bundle contains no main version")
	}
	if len(b.Versions) > 1 {
		unlabelled := 0
		for i := range b.Versions {
			if b.Versions[i].Label == "" {
				unlabelled++
			}
		}
		for i := range b.Versions {
			if b.Versions[i].Label == "" && unlabelled > 1 {
				b.Versions[i].Video.Kind = Unknown
				b.Versions[i].Video.Detail = fmt.Sprintf("secondary video %q has no reliable version or extra marker", b.Versions[i].Video.Relative)
				b.Unknown = append(b.Unknown, b.Versions[i].Video)
			}
		}
		if len(b.Unknown) > 0 {
			b.Conflict = "unknown secondary movie assets require classification"
		}
	}
	if len(b.Unknown) > 0 && b.Conflict == "" {
		b.Conflict = "unknown movie assets require classification"
	}
	return b
}

func isVideo(e string) bool {
	return map[string]bool{".mkv": true, ".mp4": true, ".avi": true, ".mov": true, ".m4v": true, ".ts": true, ".webm": true}[e]
}
func isSubtitle(e string) bool {
	return map[string]bool{".srt": true, ".ass": true, ".ssa": true, ".vtt": true, ".sub": true, ".sup": true}[e]
}
func isAudio(e string) bool { return e == ".mka" }
func blacklist(patterns []string, rel string) (string, bool) {
	for _, p := range patterns {
		for _, v := range []string{filepath.Base(rel), filepath.ToSlash(rel)} {
			if ok, _ := filepath.Match(strings.ToLower(p), strings.ToLower(v)); ok {
				return p, true
			}
		}
	}
	return "", false
}
func extraOverride(v string) (ExtraKind, bool) {
	switch v {
	case "extra":
		return OtherExtra, true
	case "trailer":
		return Trailer, true
	case "interview":
		return Interview, true
	case "featurette":
		return Featurette, true
	case "deleted_scene":
		return DeletedScene, true
	case "behind_the_scenes":
		return BehindScenes, true
	}
	return "", false
}
func detectExtra(path, title string, year int) (ExtraKind, bool) {
	parts := strings.Split(filepath.ToSlash(path), "/")
	folders := map[string]ExtraKind{"trailer": Trailer, "trailers": Trailer, "interview": Interview, "interviews": Interview, "featurette": Featurette, "featurettes": Featurette, "behind the scenes": BehindScenes, "deleted scene": DeletedScene, "deleted scenes": DeletedScene, "scene": Scene, "scenes": Scene, "short": Short, "shorts": Short, "extra": OtherExtra, "extras": OtherExtra, "sp": OtherExtra, "sps": OtherExtra, "special": OtherExtra, "specials": OtherExtra}
	for _, part := range parts[:len(parts)-1] {
		if k, ok := folders[strings.Join(tokens(part), " ")]; ok {
			return k, true
		}
	}
	name := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	remaining := stripIdentityPrefix(tokens(name), tokens(title), year)
	markers := []struct {
		kind  ExtraKind
		words []string
	}{{Trailer, []string{"trailer"}}, {Interview, []string{"interview"}}, {DeletedScene, []string{"deleted", "scene"}}, {BehindScenes, []string{"behind", "the", "scenes"}}, {Featurette, []string{"featurette"}}, {Featurette, []string{"making", "of"}}}
	for _, marker := range markers {
		if containsTokenSequence(remaining, marker.words) {
			return marker.kind, true
		}
	}
	return "", false
}

func tokens(value string) []string {
	raw := tokenRE.FindAllString(strings.ToLower(value), -1)
	return raw
}
func stripIdentityPrefix(values, title []string, year int) []string {
	if len(title) > 0 && len(values) >= len(title) {
		same := true
		for i := range title {
			if values[i] != title[i] {
				same = false
				break
			}
		}
		if same {
			values = values[len(title):]
		}
	}
	if len(values) > 0 && year > 0 && values[0] == fmt.Sprint(year) {
		values = values[1:]
	}
	return values
}
func containsTokenSequence(values, marker []string) bool {
	for i := 0; i+len(marker) <= len(values); i++ {
		same := true
		for j := range marker {
			if values[i+j] != marker[j] {
				same = false
				break
			}
		}
		if same {
			return true
		}
	}
	return false
}

func versionLabel(path string) string {
	v := tokens(strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)))
	var labels []string
	add := func(s string) {
		for _, x := range labels {
			if x == s {
				return
			}
		}
		labels = append(labels, s)
	}
	for i := 0; i < len(v); i++ {
		switch v[i] {
		case "480p", "720p", "1080p", "2160p":
			add(v[i])
		case "4k":
			add("4K")
		case "director", "directors":
			if i+1 < len(v) && v[i+1] == "s" {
				i++
			}
			if i+1 < len(v) && v[i+1] == "cut" {
				add("Directors Cut")
				i++
			}
		case "theatrical":
			add("Theatrical")
		case "extended":
			add("Extended")
		case "unrated":
			add("Unrated")
		case "ultimate":
			add("Ultimate")
		case "special":
			if i+1 < len(v) && v[i+1] == "edition" {
				add("Special Edition")
				i++
			}
		case "remastered":
			add("Remastered")
		case "imax":
			add("IMAX")
		case "3d":
			add("3D")
		case "hsbs":
			add("HSBS")
		case "hou":
			add("HOU")
		case "mvc":
			add("MVC")
		}
	}
	return strings.Join(labels, " ")
}
func comparableStem(path string) string {
	s := strings.ToLower(strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)))
	s = suffixRE.ReplaceAllString(s, "")
	return strings.Trim(strings.Map(func(r rune) rune {
		if r == '.' || r == '_' || r == '-' || r == ' ' {
			return ' '
		}
		return r
	}, s), " ")
}
func associated(a Asset, versions []Version) []int {
	stem := comparableStem(a.Relative)
	var exact []int
	for i, v := range versions {
		vs := comparableStem(v.Video.Relative)
		if stem == vs {
			exact = append(exact, i)
		}
	}
	if len(exact) > 0 {
		return exact
	}
	var prefix []int
	for i, v := range versions {
		vs := comparableStem(v.Video.Relative)
		if strings.HasPrefix(stem, vs+" ") || strings.HasPrefix(vs, stem+" ") {
			prefix = append(prefix, i)
		}
	}
	return prefix
}

func sidecarSuffix(v Version, a Asset) string {
	ext := filepath.Ext(a.Relative)
	side := strings.TrimSuffix(filepath.Base(a.Relative), ext)
	video := strings.TrimSuffix(filepath.Base(v.Video.Relative), filepath.Ext(v.Video.Relative))
	if len(side) >= len(video) && strings.EqualFold(side[:len(video)], video) {
		suffix := side[len(video):]
		if suffix == "" || strings.ContainsAny(suffix[:1], "._- ") {
			return suffix
		}
	}
	return ""
}

func (b Bundle) Validate() error {
	if b.Kind == Conflict || b.Conflict != "" {
		return errors.New(b.Conflict)
	}
	return nil
}

func ExtraDirectory(k ExtraKind) string {
	switch k {
	case Trailer:
		return "trailers"
	case Interview:
		return "interviews"
	case Featurette:
		return "featurettes"
	case BehindScenes:
		return "behind the scenes"
	case DeletedScene:
		return "deleted scenes"
	case Scene:
		return "scenes"
	case Short:
		return "shorts"
	default:
		return "extras"
	}
}

func MovieBase(title string, year int) string {
	base := strings.TrimSpace(title)
	if year > 0 {
		base += fmt.Sprintf(" (%04d)", year)
	}
	return base
}

func ISOTargetName(title string, year int, a Asset) string {
	lower := strings.ToLower(filepath.Base(a.Relative))
	suffix := ".iso"
	if strings.HasSuffix(lower, ".bluray.iso") {
		suffix = ".bluray.iso"
	} else if strings.HasSuffix(lower, ".dvd.iso") {
		suffix = ".dvd.iso"
	}
	return MovieBase(title, year) + suffix
}

func ExtraTargetName(x MovieExtra) string {
	return filepath.Join(ExtraDirectory(x.Kind), filepath.Base(x.Asset.Relative))
}

// TargetName returns the Emby basename shared by a version and its sidecars.
func TargetName(title string, year int, v Version, a Asset) string {
	base := MovieBase(title, year)
	if v.Label != "" {
		base += " - " + v.Label
	}
	ext := filepath.Ext(a.Relative)
	if a.Kind == Subtitle || a.Kind == Audio {
		return base + sidecarSuffix(v, a) + ext
	}
	return base + ext
}
