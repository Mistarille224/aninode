package transmission

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"aninode/internal/download"
)

type Client struct {
	name       string
	endpoint   string
	username   string
	password   string
	httpClient *http.Client
	mappings   []download.PathMapping
	sessionMu  sync.RWMutex
	sessionID  string
	requestID  atomic.Int64
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
	parsed, err := url.Parse(options.BaseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("invalid Transmission base URL %q", options.BaseURL)
	}
	endpoint := strings.TrimRight(options.BaseURL, "/")
	if !strings.HasSuffix(endpoint, "/transmission/rpc") {
		endpoint += "/transmission/rpc"
	}
	httpClient := options.HTTPClient
	if httpClient == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.MaxIdleConns = max(transport.MaxIdleConns, 32)
		transport.MaxIdleConnsPerHost = max(transport.MaxIdleConnsPerHost, 16)
		httpClient = &http.Client{Timeout: 30 * time.Second, Transport: transport}
	}
	return &Client{name: options.Name, endpoint: endpoint, username: options.Username, password: options.Password, httpClient: httpClient, mappings: options.PathMappings}, nil
}

func (client *Client) Name() string { return client.name }

func (client *Client) Add(ctx context.Context, request download.AddRequest) (download.Task, error) {
	arguments := map[string]any{"filename": request.URL, "paused": request.Paused}
	if request.SavePath != "" {
		arguments["download-dir"] = request.SavePath
	}
	labels := append([]string(nil), request.Labels...)
	if len(labels) > 0 {
		arguments["labels"] = labels
	}
	var response struct {
		Added     *torrent `json:"torrent-added"`
		Duplicate *torrent `json:"torrent-duplicate"`
	}
	if err := client.call(ctx, "torrent-add", arguments, &response); err != nil {
		return download.Task{}, err
	}
	value := response.Added
	if value == nil {
		value = response.Duplicate
	}
	if value == nil {
		return download.Task{}, errors.New("Transmission returned neither added nor duplicate torrent")
	}
	return client.task(*value), nil
}

func (client *Client) Get(ctx context.Context, id string) (download.Task, error) {
	values, err := client.get(ctx, []any{id})
	if err != nil {
		return download.Task{}, err
	}
	if len(values) == 0 {
		return download.Task{}, fmt.Errorf("Transmission task %q not found", id)
	}
	return client.task(values[0]), nil
}

func (client *Client) List(ctx context.Context) ([]download.Task, error) {
	values, err := client.get(ctx, nil)
	if err != nil {
		return nil, err
	}
	tasks := make([]download.Task, 0, len(values))
	for _, value := range values {
		tasks = append(tasks, client.task(value))
	}
	return tasks, nil
}

func (client *Client) ListObservations(ctx context.Context) ([]download.Observation, error) {
	values, err := client.get(ctx, nil)
	if err != nil {
		return nil, err
	}
	out := make([]download.Observation, 0, len(values))
	for _, value := range values {
		out = append(out, download.Observation{Task: client.task(value), Files: client.files(value)})
	}
	return out, nil
}

func (client *Client) Files(ctx context.Context, id string) ([]download.File, error) {
	values, err := client.get(ctx, []any{id})
	if err != nil {
		return nil, err
	}
	if len(values) == 0 {
		return nil, fmt.Errorf("Transmission task %q not found", id)
	}
	return client.files(values[0]), nil
}

func (client *Client) files(value torrent) []download.File {
	files := make([]download.File, 0, len(value.Files))
	for index, item := range value.Files {
		wanted := true
		if index < len(value.FileStats) {
			wanted = bool(value.FileStats[index].Wanted)
		}
		progress := float64(0)
		if item.Length > 0 {
			progress = float64(item.BytesCompleted) / float64(item.Length)
		}
		files = append(files, download.File{
			Path: download.MapPath(filepath.Join(value.DownloadDir, filepath.FromSlash(item.Name)), client.mappings),
			Size: item.Length, Completed: item.BytesCompleted, Progress: progress, Wanted: wanted,
		})
	}
	return files
}

func (client *Client) Pause(ctx context.Context, id string) error {
	return client.call(ctx, "torrent-stop", map[string]any{"ids": []any{id}}, nil)
}

func (client *Client) Resume(ctx context.Context, id string) error {
	return client.call(ctx, "torrent-start", map[string]any{"ids": []any{id}}, nil)
}

func (client *Client) Remove(ctx context.Context, id string, deleteFiles bool) error {
	return client.call(ctx, "torrent-remove", map[string]any{"ids": []any{id}, "delete-local-data": deleteFiles}, nil)
}

func (client *Client) Relocate(ctx context.Context, id, savePath string) error {
	remote := download.UnmapPath(savePath, client.mappings)
	return client.call(ctx, "torrent-set-location", map[string]any{"ids": []any{id}, "location": remote, "move": true}, nil)
}

func (client *Client) Verify(ctx context.Context, id string) error {
	return client.call(ctx, "torrent-verify", map[string]any{"ids": []any{id}}, nil)
}

func (client *Client) SetLabels(ctx context.Context, id string, labels []string) error {
	return client.call(ctx, "torrent-set", map[string]any{"ids": []any{id}, "labels": labels}, nil)
}

type torrent struct {
	ID              int64    `json:"id"`
	HashString      string   `json:"hashString"`
	Name            string   `json:"name"`
	Status          int      `json:"status"`
	PercentComplete float64  `json:"percentComplete"`
	RateDownload    int64    `json:"rateDownload"`
	RateUpload      int64    `json:"rateUpload"`
	ETA             int64    `json:"eta"`
	TotalSize       int64    `json:"totalSize"`
	DownloadDir     string   `json:"downloadDir"`
	Labels          []string `json:"labels"`
	ErrorString     string   `json:"errorString"`
	DoneDate        int64    `json:"doneDate"`
	Files           []struct {
		Name           string `json:"name"`
		Length         int64  `json:"length"`
		BytesCompleted int64  `json:"bytesCompleted"`
	} `json:"files"`
	FileStats []struct {
		Wanted wanted `json:"wanted"`
	} `json:"fileStats"`
}

type wanted bool

func (value *wanted) UnmarshalJSON(data []byte) error {
	switch string(data) {
	case "true", "1":
		*value = true
		return nil
	case "false", "0":
		*value = false
		return nil
	default:
		return fmt.Errorf("invalid Transmission wanted value %s", data)
	}
}

var torrentFields = []string{"id", "hashString", "name", "status", "percentComplete", "rateDownload", "rateUpload", "eta", "totalSize", "downloadDir", "labels", "errorString", "doneDate", "files", "fileStats"}

func (client *Client) get(ctx context.Context, ids []any) ([]torrent, error) {
	arguments := map[string]any{"fields": torrentFields}
	if ids != nil {
		arguments["ids"] = ids
	}
	var response struct {
		Torrents []torrent `json:"torrents"`
	}
	if err := client.call(ctx, "torrent-get", arguments, &response); err != nil {
		return nil, err
	}
	return response.Torrents, nil
}

func (client *Client) task(value torrent) download.Task {
	id := strings.ToLower(value.HashString)
	if id == "" {
		id = fmt.Sprint(value.ID)
	}
	contentPath := transmissionContentPath(value)
	task := download.Task{
		ID: id, InfoHash: strings.ToLower(value.HashString), Name: value.Name,
		State: transmissionState(value.Status, value.ErrorString, value.PercentComplete), Progress: value.PercentComplete,
		DownloadSpeed: value.RateDownload, UploadSpeed: value.RateUpload, ETA: value.ETA,
		TotalSize: value.TotalSize, SavePath: download.MapPath(value.DownloadDir, client.mappings),
		ContentPath: download.MapPath(contentPath, client.mappings), Labels: value.Labels, Error: value.ErrorString,
	}
	if value.DoneDate > 0 {
		task.CompletedAt = time.Unix(value.DoneDate, 0).UTC()
	}
	return task
}

func transmissionContentPath(value torrent) string {
	name := filepath.Clean(filepath.FromSlash(strings.TrimSpace(value.Name)))
	if value.DownloadDir == "" || name == "." || filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
		return ""
	}
	candidate := filepath.Join(value.DownloadDir, name)
	if len(value.Files) == 0 {
		return ""
	}
	for _, item := range value.Files {
		rel := filepath.Clean(filepath.FromSlash(item.Name))
		if rel == "." || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return ""
		}
		path := filepath.Join(value.DownloadDir, rel)
		if len(value.Files) == 1 && filepath.Clean(path) == filepath.Clean(candidate) {
			continue
		}
		inside, err := filepath.Rel(candidate, path)
		if err != nil || inside == "." || inside == ".." || strings.HasPrefix(inside, ".."+string(filepath.Separator)) {
			return ""
		}
	}
	return candidate
}

func transmissionState(status int, errorString string, progress float64) download.State {
	if errorString != "" {
		return download.StateFailed
	}
	switch status {
	case 0:
		if progress >= 1 {
			return download.StateCompleted
		}
		return download.StatePaused
	case 1, 2:
		return download.StateChecking
	case 3:
		return download.StateQueued
	case 4:
		return download.StateDownloading
	case 5:
		return download.StateQueued
	case 6:
		return download.StateSeeding
	default:
		return download.StateUnknown
	}
}

type rpcRequest struct {
	Method    string         `json:"method"`
	Arguments map[string]any `json:"arguments,omitempty"`
	Tag       int64          `json:"tag"`
}

type rpcResponse struct {
	Result    string          `json:"result"`
	Arguments json.RawMessage `json:"arguments"`
}

func (client *Client) call(ctx context.Context, method string, arguments map[string]any, output any) error {
	payload, err := json.Marshal(rpcRequest{Method: method, Arguments: arguments, Tag: client.requestID.Add(1)})
	if err != nil {
		return err
	}
	for attempt := 0; attempt < 2; attempt++ {
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, client.endpoint, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		request.Header.Set("Content-Type", "application/json")
		client.sessionMu.RLock()
		request.Header.Set("X-Transmission-Session-Id", client.sessionID)
		client.sessionMu.RUnlock()
		if client.username != "" || client.password != "" {
			request.SetBasicAuth(client.username, client.password)
		}
		response, err := client.httpClient.Do(request)
		if err != nil {
			return err
		}
		data, readErr := io.ReadAll(io.LimitReader(response.Body, 8<<20))
		response.Body.Close()
		if readErr != nil {
			return readErr
		}
		if response.StatusCode == http.StatusConflict && attempt == 0 {
			client.sessionMu.Lock()
			client.sessionID = response.Header.Get("X-Transmission-Session-Id")
			client.sessionMu.Unlock()
			continue
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return fmt.Errorf("Transmission HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(data)))
		}
		var envelope rpcResponse
		if err := json.Unmarshal(data, &envelope); err != nil {
			return err
		}
		if envelope.Result != "success" {
			return fmt.Errorf("Transmission RPC %s: %s", method, envelope.Result)
		}
		if output != nil {
			if err := json.Unmarshal(envelope.Arguments, output); err != nil {
				return err
			}
		}
		return nil
	}
	return errors.New("Transmission session negotiation failed")
}
