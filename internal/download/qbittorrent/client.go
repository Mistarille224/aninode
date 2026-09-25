package qbittorrent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"aninode/internal/acquisition"
	"aninode/internal/download"
)

type Client struct {
	name       string
	baseURL    string
	username   string
	password   string
	httpClient *http.Client
	mappings   []download.PathMapping
	authMu     sync.Mutex
	authed     bool
}

type Options struct {
	Name         string
	BaseURL      string
	Username     string
	Password     string
	HTTPClient   *http.Client
	PathMappings []download.PathMapping
}

func New(options Options) (*Client, error) {
	if options.BaseURL == "" {
		return nil, errors.New("qBittorrent base URL is required")
	}
	parsed, err := url.Parse(options.BaseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("invalid qBittorrent base URL %q", options.BaseURL)
	}
	httpClient := options.HTTPClient
	if httpClient == nil {
		jar, _ := cookiejar.New(nil)
		transport := http.DefaultTransport
		if base, ok := http.DefaultTransport.(*http.Transport); ok {
			clone := base.Clone()
			// Migration may issue many /torrents/files requests concurrently. The
			// standard transport only keeps two idle connections per host, which
			// causes repeated TCP/TLS handshakes on high-latency qBittorrent links.
			// Keep one warm connection per migration worker instead.
			if clone.MaxIdleConns < 32 {
				clone.MaxIdleConns = 32
			}
			if clone.MaxIdleConnsPerHost < 16 {
				clone.MaxIdleConnsPerHost = 16
			}
			transport = clone
		}
		httpClient = &http.Client{Timeout: 30 * time.Second, Jar: jar, Transport: transport}
	} else if httpClient.Jar == nil {
		jar, _ := cookiejar.New(nil)
		httpClient.Jar = jar
	}
	return &Client{
		name: options.Name, baseURL: strings.TrimRight(options.BaseURL, "/"),
		username: options.Username, password: options.Password,
		httpClient: httpClient, mappings: options.PathMappings,
	}, nil
}

func (client *Client) Name() string { return client.name }

func (client *Client) Add(ctx context.Context, request download.AddRequest) (download.Task, error) {
	fields := map[string]string{
		"urls": request.URL, "savepath": request.SavePath, "category": request.Category,
		"tags": strings.Join(request.Tags, ","), "paused": strconv.FormatBool(request.Paused),
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for key, value := range fields {
		if value != "" {
			if err := writer.WriteField(key, value); err != nil {
				return download.Task{}, err
			}
		}
	}
	if err := writer.Close(); err != nil {
		return download.Task{}, err
	}
	if _, err := client.do(ctx, http.MethodPost, "/api/v2/torrents/add", body.Bytes(), writer.FormDataContentType()); err != nil {
		return download.Task{}, err
	}
	task := download.Task{State: download.StateQueued, SavePath: request.SavePath, Sources: []string{request.URL}}
	if identity, err := acquisition.CanonicalIdentity(acquisition.IdentityInput{MagnetURL: request.URL}); err == nil && identity.Kind == "btih" {
		task.ID, task.InfoHash = strings.TrimPrefix(identity.ID, "btih-"), identity.Canonical
	}
	return task, nil
}

func (client *Client) Get(ctx context.Context, id string) (download.Task, error) {
	values := url.Values{"hashes": {id}}
	data, err := client.do(ctx, http.MethodGet, "/api/v2/torrents/info?"+values.Encode(), nil, "")
	if err != nil {
		return download.Task{}, err
	}
	var torrents []torrent
	if err := json.Unmarshal(data, &torrents); err != nil {
		return download.Task{}, err
	}
	if len(torrents) == 0 {
		return download.Task{}, fmt.Errorf("qBittorrent task %q not found", id)
	}
	return client.task(torrents[0]), nil
}

func (client *Client) List(ctx context.Context) ([]download.Task, error) {
	data, err := client.do(ctx, http.MethodGet, "/api/v2/torrents/info", nil, "")
	if err != nil {
		return nil, err
	}
	var torrents []torrent
	if err := json.Unmarshal(data, &torrents); err != nil {
		return nil, err
	}
	tasks := make([]download.Task, 0, len(torrents))
	for _, value := range torrents {
		tasks = append(tasks, client.task(value))
	}
	return tasks, nil
}

func (client *Client) Files(ctx context.Context, id string) ([]download.File, error) {
	task, err := client.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return client.FilesForTask(ctx, task)
}

// FilesForTask reuses task metadata already returned by List. Migration scans
// therefore need one qBittorrent request per task instead of fetching it again.
func (client *Client) FilesForTask(ctx context.Context, task download.Task) ([]download.File, error) {
	data, err := client.do(ctx, http.MethodGet, "/api/v2/torrents/files?"+url.Values{"hash": {task.ID}}.Encode(), nil, "")
	if err != nil {
		return nil, err
	}
	var values []struct {
		Name     string  `json:"name"`
		Size     int64   `json:"size"`
		Progress float64 `json:"progress"`
		Priority int     `json:"priority"`
	}
	if err := json.Unmarshal(data, &values); err != nil {
		return nil, err
	}
	files := make([]download.File, 0, len(values))
	for _, value := range values {
		path, err := taskFilePath(task, value.Name)
		if err != nil {
			return nil, err
		}
		files = append(files, download.File{Path: path, Size: value.Size, Completed: int64(float64(value.Size) * value.Progress), Progress: value.Progress, Wanted: value.Priority > 0})
	}
	return files, nil
}

// qBittorrent normally reports file names relative to save_path (including the
// torrent root component for multi-file torrents). Some versions/layouts omit
// that component. content_path lets us select the only candidate consistent
// with the downloader-observed topology without guessing from task.Name.
func taskFilePath(task download.Task, name string) (string, error) {
	local := filepath.FromSlash(name)
	if local == "" || filepath.IsAbs(local) {
		return "", fmt.Errorf("unsafe qBittorrent file name %q", name)
	}
	clean := filepath.Clean(local)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("unsafe qBittorrent file name %q", name)
	}
	saveCandidate := filepath.Join(task.SavePath, clean)
	if task.ContentPath == "" {
		return saveCandidate, nil
	}
	content := filepath.Clean(task.ContentPath)
	// A single-file content_path names the file, not a directory.
	if samePath(saveCandidate, content) {
		return content, nil
	}
	if pathInside(content, saveCandidate) {
		return saveCandidate, nil
	}
	contentCandidate := filepath.Join(content, clean)
	if pathInside(content, contentCandidate) {
		return contentCandidate, nil
	}
	return "", fmt.Errorf("qBittorrent file name %q escapes task content path %q", name, task.ContentPath)
}

func samePath(a, b string) bool { return filepath.Clean(a) == filepath.Clean(b) }

func pathInside(root, path string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func (client *Client) Pause(ctx context.Context, id string) error {
	return client.commandAcrossAPIVersions(ctx, "stop", "pause", id)
}

func (client *Client) Resume(ctx context.Context, id string) error {
	return client.commandAcrossAPIVersions(ctx, "start", "resume", id)
}

func (client *Client) Remove(ctx context.Context, id string, deleteFiles bool) error {
	values := url.Values{"hashes": {id}, "deleteFiles": {strconv.FormatBool(deleteFiles)}}
	_, err := client.do(ctx, http.MethodPost, "/api/v2/torrents/delete", []byte(values.Encode()), "application/x-www-form-urlencoded")
	return err
}

func (client *Client) Relocate(ctx context.Context, id, savePath string) error {
	values := url.Values{"hashes": {id}, "location": {download.UnmapPath(savePath, client.mappings)}}
	_, err := client.do(ctx, http.MethodPost, "/api/v2/torrents/setLocation", []byte(values.Encode()), "application/x-www-form-urlencoded")
	return err
}

func (client *Client) Verify(ctx context.Context, id string) error {
	values := url.Values{"hashes": {id}}
	_, err := client.do(ctx, http.MethodPost, "/api/v2/torrents/recheck", []byte(values.Encode()), "application/x-www-form-urlencoded")
	return err
}

func (client *Client) SetLabels(ctx context.Context, id string, labels []string) error {
	values := url.Values{"hashes": {id}, "tags": {strings.Join(labels, ",")}}
	_, err := client.do(ctx, http.MethodPost, "/api/v2/torrents/addTags", []byte(values.Encode()), "application/x-www-form-urlencoded")
	return err
}

type torrent struct {
	Hash          string  `json:"hash"`
	Name          string  `json:"name"`
	State         string  `json:"state"`
	Progress      float64 `json:"progress"`
	DownloadSpeed int64   `json:"dlspeed"`
	UploadSpeed   int64   `json:"upspeed"`
	ETA           int64   `json:"eta"`
	Size          int64   `json:"size"`
	SavePath      string  `json:"save_path"`
	ContentPath   string  `json:"content_path"`
	Category      string  `json:"category"`
	Tags          string  `json:"tags"`
	CompletionOn  int64   `json:"completion_on"`
}

func (client *Client) task(value torrent) download.Task {
	labels := splitLabels(value.Tags)
	if value.Category != "" {
		labels = append(labels, value.Category)
	}
	task := download.Task{
		ID: value.Hash, InfoHash: strings.ToLower(value.Hash), Name: value.Name,
		State: qbitState(value.State, value.Progress), Progress: value.Progress,
		DownloadSpeed: value.DownloadSpeed, UploadSpeed: value.UploadSpeed,
		ETA: value.ETA, TotalSize: value.Size,
		SavePath:    download.MapPath(value.SavePath, client.mappings),
		ContentPath: download.MapPath(value.ContentPath, client.mappings), Labels: labels,
	}
	if value.CompletionOn > 0 {
		task.CompletedAt = time.Unix(value.CompletionOn, 0).UTC()
	}
	return task
}

func qbitState(state string, progress float64) download.State {
	lower := strings.ToLower(state)
	switch {
	case strings.Contains(lower, "error"), strings.Contains(lower, "missing"):
		return download.StateFailed
	case strings.Contains(lower, "check"):
		return download.StateChecking
	case strings.HasPrefix(lower, "paused"), strings.HasPrefix(lower, "stopped"):
		return download.StatePaused
	case strings.Contains(lower, "queued"):
		return download.StateQueued
	case strings.Contains(lower, "upload"), strings.Contains(lower, "seed"), strings.HasSuffix(lower, "up"):
		return download.StateSeeding
	case strings.Contains(lower, "download"), strings.Contains(lower, "meta"), strings.HasSuffix(lower, "dl"), strings.Contains(lower, "allocat"):
		return download.StateDownloading
	case progress >= 1:
		return download.StateCompleted
	default:
		return download.StateUnknown
	}
}

func splitLabels(value string) []string {
	var labels []string
	for _, label := range strings.Split(value, ",") {
		if label = strings.TrimSpace(label); label != "" {
			labels = append(labels, label)
		}
	}
	return labels
}

func (client *Client) commandAcrossAPIVersions(ctx context.Context, current, fallback, id string) error {
	values := url.Values{"hashes": {id}}
	_, err := client.do(ctx, http.MethodPost, "/api/v2/torrents/"+current, []byte(values.Encode()), "application/x-www-form-urlencoded")
	var statusError *httpStatusError
	if errors.As(err, &statusError) && statusError.Code == http.StatusNotFound {
		_, err = client.do(ctx, http.MethodPost, "/api/v2/torrents/"+fallback, []byte(values.Encode()), "application/x-www-form-urlencoded")
	}
	return err
}

func (client *Client) ensureAuth(ctx context.Context) error {
	client.authMu.Lock()
	defer client.authMu.Unlock()
	if client.authed || (client.username == "" && client.password == "") {
		return nil
	}
	values := url.Values{"username": {client.username}, "password": {client.password}}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, client.baseURL+"/api/v2/auth/login", strings.NewReader(values.Encode()))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := client.httpClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(response.Body, 1024))
	body := strings.TrimSpace(string(data))
	// qBittorrent normally returns 200 with "Ok.". Some releases and reverse
	// proxies normalize a successful empty login response to 204. Do not accept
	// arbitrary 2xx bodies: qBittorrent also reports bad credentials as 200
	// with "Fails.".
	if !((response.StatusCode == http.StatusOK && body == "Ok.") || (response.StatusCode == http.StatusNoContent && body == "")) {
		return fmt.Errorf("qBittorrent login failed with HTTP %d: %s", response.StatusCode, body)
	}
	client.authed = true
	return nil
}

type httpStatusError struct {
	Code int
	Body string
}

func (err *httpStatusError) Error() string {
	return fmt.Sprintf("qBittorrent HTTP %d: %s", err.Code, err.Body)
}

func (client *Client) do(ctx context.Context, method, endpoint string, body []byte, contentType string) ([]byte, error) {
	for attempt := 0; attempt < 2; attempt++ {
		if err := client.ensureAuth(ctx); err != nil {
			return nil, err
		}
		request, err := http.NewRequestWithContext(ctx, method, client.baseURL+endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		if contentType != "" {
			request.Header.Set("Content-Type", contentType)
		}
		response, err := client.httpClient.Do(request)
		if err != nil {
			return nil, err
		}
		data, readErr := io.ReadAll(io.LimitReader(response.Body, 8<<20))
		response.Body.Close()
		if readErr != nil {
			return nil, readErr
		}
		if response.StatusCode == http.StatusForbidden && attempt == 0 && (client.username != "" || client.password != "") {
			client.authMu.Lock()
			client.authed = false
			client.authMu.Unlock()
			continue
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return nil, &httpStatusError{Code: response.StatusCode, Body: strings.TrimSpace(string(data))}
		}
		return data, nil
	}
	return nil, errors.New("qBittorrent authentication retry failed")
}
