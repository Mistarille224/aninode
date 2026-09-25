package aria2

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"aninode/internal/download"
)

type Client struct {
	name       string
	endpoint   string
	secret     string
	httpClient *http.Client
	mappings   []download.PathMapping
	requestID  atomic.Int64
}

type Options struct {
	Name         string
	Endpoint     string
	Secret       string
	HTTPClient   *http.Client
	PathMappings []download.PathMapping
}

func New(options Options) (*Client, error) {
	parsed, err := url.Parse(options.Endpoint)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("invalid aria2 RPC endpoint %q", options.Endpoint)
	}
	httpClient := options.HTTPClient
	if httpClient == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.MaxIdleConns = max(transport.MaxIdleConns, 32)
		transport.MaxIdleConnsPerHost = max(transport.MaxIdleConnsPerHost, 16)
		httpClient = &http.Client{Timeout: 30 * time.Second, Transport: transport}
	}
	return &Client{name: options.Name, endpoint: options.Endpoint, secret: options.Secret, httpClient: httpClient, mappings: options.PathMappings}, nil
}

func (client *Client) Name() string { return client.name }

func (client *Client) Add(ctx context.Context, request download.AddRequest) (download.Task, error) {
	options := map[string]string{}
	if request.SavePath != "" {
		options["dir"] = request.SavePath
	}
	if request.Paused {
		options["pause"] = "true"
	}
	if request.Correlation != "" {
		options["gid"] = download.CorrelationTaskID(request.Correlation)
	}
	var gid string
	if err := client.call(ctx, "aria2.addUri", []any{[]string{request.URL}, options}, &gid); err != nil {
		return download.Task{}, err
	}
	return download.Task{ID: gid, State: download.StateQueued, SavePath: request.SavePath, Sources: []string{request.URL}}, nil
}

func (client *Client) Get(ctx context.Context, id string) (download.Task, error) {
	value, err := client.tellStatus(ctx, id)
	if err != nil {
		return download.Task{}, err
	}
	return client.task(value), nil
}

func (client *Client) List(ctx context.Context) ([]download.Task, error) {
	observations, err := client.ListObservations(ctx)
	if err != nil {
		return nil, err
	}
	tasks := make([]download.Task, 0, len(observations))
	for _, observation := range observations {
		tasks = append(tasks, observation.Task)
	}
	return tasks, nil
}

func (client *Client) ListObservations(ctx context.Context) ([]download.Observation, error) {
	keys := statusKeys()
	var active []status
	if err := client.call(ctx, "aria2.tellActive", []any{keys}, &active); err != nil {
		return nil, err
	}
	waiting, err := client.listPage(ctx, "aria2.tellWaiting", keys)
	if err != nil {
		return nil, err
	}
	// Stopped entries are unbounded historical results in aria2, not the current
	// task set. A recent window is sufficient for stateless filesystem adoption.
	stopped, err := client.listStopped(ctx, keys)
	if err != nil {
		return nil, err
	}
	all := append(append(active, waiting...), stopped...)
	observations := make([]download.Observation, 0, len(all))
	for _, value := range all {
		observations = append(observations, download.Observation{Task: client.task(value), Files: client.files(value)})
	}
	return observations, nil
}

func (client *Client) listStopped(ctx context.Context, keys []string) ([]status, error) {
	const recentStopped = 1000
	var values []status
	if err := client.call(ctx, "aria2.tellStopped", []any{0, recentStopped, keys}, &values); err != nil {
		return nil, err
	}
	return values, nil
}

// listPage enumerates current waiting entries. Stopped results use a separate,
// bounded recent window because aria2 retains an unbounded result history.
func (client *Client) listPage(ctx context.Context, method string, keys []string) ([]status, error) {
	const pageSize = 1000
	var all []status
	for offset := 0; ; offset += pageSize {
		var page []status
		if err := client.call(ctx, method, []any{offset, pageSize, keys}, &page); err != nil {
			return nil, err
		}
		all = append(all, page...)
		if len(page) < pageSize {
			return all, nil
		}
	}
}

func (client *Client) Files(ctx context.Context, id string) ([]download.File, error) {
	value, err := client.tellStatus(ctx, id)
	if err != nil {
		return nil, err
	}
	return client.files(value), nil
}

func (client *Client) files(value status) []download.File {
	files := make([]download.File, 0, len(value.Files))
	for _, item := range value.Files {
		size, _ := strconv.ParseInt(item.Length, 10, 64)
		completed, _ := strconv.ParseInt(item.CompletedLength, 10, 64)
		progress := float64(0)
		if size > 0 {
			progress = float64(completed) / float64(size)
		}
		files = append(files, download.File{Path: download.MapPath(item.Path, client.mappings), Size: size, Completed: completed, Progress: progress, Wanted: item.Selected == "true"})
	}
	return files
}

func (client *Client) Pause(ctx context.Context, id string) error {
	return client.call(ctx, "aria2.pause", []any{id}, nil)
}

func (client *Client) Resume(ctx context.Context, id string) error {
	return client.call(ctx, "aria2.unpause", []any{id}, nil)
}

func (client *Client) Remove(ctx context.Context, id string, deleteFiles bool) error {
	if deleteFiles {
		return download.ErrUnsupported
	}
	task, err := client.Get(ctx, id)
	if err != nil {
		return err
	}
	method := "aria2.remove"
	if task.State == download.StateCompleted || task.State == download.StateFailed {
		method = "aria2.removeDownloadResult"
	}
	return client.call(ctx, method, []any{id}, nil)
}

type status struct {
	GID             string `json:"gid"`
	Status          string `json:"status"`
	TotalLength     string `json:"totalLength"`
	CompletedLength string `json:"completedLength"`
	DownloadSpeed   string `json:"downloadSpeed"`
	UploadSpeed     string `json:"uploadSpeed"`
	ErrorMessage    string `json:"errorMessage"`
	Dir             string `json:"dir"`
	InfoHash        string `json:"infoHash"`
	BitTorrent      struct {
		Info struct {
			Name string `json:"name"`
		} `json:"info"`
	} `json:"bittorrent"`
	Files []struct {
		Path            string `json:"path"`
		Length          string `json:"length"`
		CompletedLength string `json:"completedLength"`
		Selected        string `json:"selected"`
		URIs            []struct {
			URI string `json:"uri"`
		} `json:"uris"`
	} `json:"files"`
}

func statusKeys() []string {
	return []string{"gid", "status", "totalLength", "completedLength", "downloadSpeed", "uploadSpeed", "errorMessage", "dir", "infoHash", "bittorrent", "files"}
}

func (client *Client) tellStatus(ctx context.Context, id string) (status, error) {
	var value status
	if err := client.call(ctx, "aria2.tellStatus", []any{id, statusKeys()}, &value); err != nil {
		return status{}, err
	}
	return value, nil
}

func (client *Client) task(value status) download.Task {
	total, _ := strconv.ParseInt(value.TotalLength, 10, 64)
	completed, _ := strconv.ParseInt(value.CompletedLength, 10, 64)
	downloadSpeed, _ := strconv.ParseInt(value.DownloadSpeed, 10, 64)
	uploadSpeed, _ := strconv.ParseInt(value.UploadSpeed, 10, 64)
	progress := float64(0)
	if total > 0 {
		progress = float64(completed) / float64(total)
	}
	name := value.BitTorrent.Info.Name
	if name == "" && len(value.Files) > 0 {
		name = filepath.Base(value.Files[0].Path)
	}
	var sources []string
	for _, item := range value.Files {
		for _, uri := range item.URIs {
			if uri.URI != "" {
				sources = append(sources, uri.URI)
			}
		}
	}
	return download.Task{
		ID: value.GID, InfoHash: strings.ToLower(value.InfoHash), Name: name,
		State: ariaState(value.Status, value.ErrorMessage, progress, value.InfoHash != ""), Progress: progress,
		DownloadSpeed: downloadSpeed, UploadSpeed: uploadSpeed, TotalSize: total,
		SavePath: download.MapPath(value.Dir, client.mappings), ContentPath: download.MapPath(ariaContentPath(value), client.mappings),
		Sources: sources, Error: value.ErrorMessage,
	}
}

func ariaContentPath(value status) string {
	if len(value.Files) == 0 {
		return ""
	}
	name := filepath.Clean(filepath.FromSlash(strings.TrimSpace(value.BitTorrent.Info.Name)))
	if name == "." || filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
		name = ""
	}
	if name == "" {
		if len(value.Files) == 1 {
			return filepath.Clean(value.Files[0].Path)
		}
		return ""
	}
	candidate := filepath.Join(value.Dir, name)
	for _, item := range value.Files {
		path := filepath.Clean(item.Path)
		if len(value.Files) == 1 && path == filepath.Clean(candidate) {
			continue
		}
		rel, err := filepath.Rel(candidate, path)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return ""
		}
	}
	return candidate
}

func ariaState(status, errorMessage string, progress float64, bitTorrent bool) download.State {
	if errorMessage != "" || status == "error" || status == "removed" {
		return download.StateFailed
	}
	switch status {
	case "active":
		if progress >= 1 && bitTorrent {
			return download.StateSeeding
		}
		return download.StateDownloading
	case "waiting":
		return download.StateQueued
	case "paused":
		return download.StatePaused
	case "complete":
		return download.StateCompleted
	default:
		return download.StateUnknown
	}
}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Method  string `json:"method"`
	Params  []any  `json:"params,omitempty"`
}

type rpcResponse struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (client *Client) call(ctx context.Context, method string, params []any, output any) error {
	if client.secret != "" {
		params = append([]any{"token:" + client.secret}, params...)
	}
	payload, err := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: client.requestID.Add(1), Method: method, Params: params})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, client.endpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.httpClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("aria2 HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(data)))
	}
	var envelope rpcResponse
	if err := json.Unmarshal(data, &envelope); err != nil {
		return err
	}
	if envelope.Error != nil {
		return fmt.Errorf("aria2 RPC %s (%d): %s", method, envelope.Error.Code, envelope.Error.Message)
	}
	if output != nil {
		if err := json.Unmarshal(envelope.Result, output); err != nil {
			return err
		}
	}
	return nil
}
