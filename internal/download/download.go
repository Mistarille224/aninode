package download

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"strings"
	"time"
)

var ErrUnsupported = errors.New("download client capability is unsupported")

type State string

const (
	StateQueued      State = "queued"
	StateDownloading State = "downloading"
	StateCompleted   State = "completed"
	StateSeeding     State = "seeding"
	StatePaused      State = "paused"
	StateChecking    State = "checking"
	StateFailed      State = "failed"
	StateUnknown     State = "unknown"
)

type AddRequest struct {
	URL         string   `json:"url"`
	SavePath    string   `json:"save_path,omitempty"`
	Paused      bool     `json:"paused,omitempty"`
	Category    string   `json:"category,omitempty"`
	Labels      []string `json:"labels,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	Correlation string   `json:"correlation,omitempty"`
}

type Task struct {
	ID            string  `json:"id"`
	InfoHash      string  `json:"info_hash,omitempty"`
	Name          string  `json:"name"`
	State         State   `json:"state"`
	Progress      float64 `json:"progress"`
	DownloadSpeed int64   `json:"download_speed,omitempty"`
	UploadSpeed   int64   `json:"upload_speed,omitempty"`
	ETA           int64   `json:"eta_seconds,omitempty"`
	TotalSize     int64   `json:"total_size,omitempty"`
	SavePath      string  `json:"save_path,omitempty"`
	// ContentPath is the downloader-observed path of this task's content. For a
	// multi-file torrent it is the torrent root directory; for a single-file
	// torrent it is the file itself. It is deliberately distinct from SavePath,
	// which is the location accepted by downloader relocation APIs.
	ContentPath string    `json:"content_path,omitempty"`
	Labels      []string  `json:"labels,omitempty"`
	Sources     []string  `json:"sources,omitempty"`
	Error       string    `json:"error,omitempty"`
	CompletedAt time.Time `json:"completed_at,omitempty"`
}

type File struct {
	Path      string  `json:"path"`
	Size      int64   `json:"size"`
	Completed int64   `json:"completed"`
	Progress  float64 `json:"progress"`
	Wanted    bool    `json:"wanted"`
}

// Observation is a request-local snapshot of a downloader task and its files.
// Backends whose list RPC already returns file metadata can expose it without
// forcing callers to issue one additional request per task.
type Observation struct {
	Task  Task
	Files []File
}

type ObservationLister interface {
	ListObservations(context.Context) ([]Observation, error)
}

// TaskFileObserver returns file metadata for a task whose bulk task metadata is
// already available. Backends such as qBittorrent can use this to avoid an
// otherwise redundant per-task Get request before their files endpoint.
type TaskFileObserver interface {
	FilesForTask(context.Context, Task) ([]File, error)
}

type Backend interface {
	Name() string
}

type Relocator interface {
	Relocate(ctx context.Context, id, savePath string) error
}

type Verifier interface {
	Verify(ctx context.Context, id string) error
}

type Labeler interface {
	SetLabels(ctx context.Context, id string, labels []string) error
}

type PathMapping struct {
	Remote string `json:"remote"`
	Local  string `json:"local"`
}

func MapPath(path string, mappings []PathMapping) string {
	return mapPath(path, mappings, false)
}

// UnmapPath converts an aninode-local path back to the path expected by a
// remote download client.
func UnmapPath(path string, mappings []PathMapping) string {
	return mapPath(path, mappings, true)
}

func mapPath(path string, mappings []PathMapping, reverse bool) string {
	bestRemote, bestLocal := "", ""
	cleanedPath := filepath.Clean(path)
	for _, mapping := range mappings {
		remote, local := mapping.Remote, mapping.Local
		if reverse {
			remote, local = local, remote
		}
		remote = filepath.Clean(remote)
		if remote == "." || remote == "" || local == "" {
			continue
		}
		relative, err := filepath.Rel(remote, cleanedPath)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			continue
		}
		if len(remote) > len(bestRemote) {
			bestRemote, bestLocal = remote, filepath.Clean(local)
		}
	}
	if bestRemote == "" {
		return path
	}
	relative, _ := filepath.Rel(bestRemote, cleanedPath)
	return filepath.Join(bestLocal, relative)
}

func CorrelationTaskID(correlation string) string {
	sum := sha256.Sum256([]byte(correlation))
	return hex.EncodeToString(sum[:8])
}
