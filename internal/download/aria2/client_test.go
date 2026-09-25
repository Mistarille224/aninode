package aria2

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"aninode/internal/download"
)

func TestClientUsesSecretAndNormalizesTasks(t *testing.T) {
	handler := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		var input struct {
			ID     int64             `json:"id"`
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
			t.Fatal(err)
		}
		var token string
		if len(input.Params) == 0 || json.Unmarshal(input.Params[0], &token) != nil || token != "token:secret" {
			http.Error(response, "missing token", http.StatusUnauthorized)
			return
		}
		response.Header().Set("Content-Type", "application/json")
		switch input.Method {
		case "aria2.tellActive":
			fmt.Fprintf(response, `{"jsonrpc":"2.0","id":%d,"result":[{"gid":"a","status":"active","totalLength":"100","completedLength":"50","downloadSpeed":"10","dir":"/remote","files":[{"path":"/remote/01.mkv","length":"100","completedLength":"50","selected":"true","uris":[{"uri":"https://example.test/01.mkv"}]}]}]}`, input.ID)
		case "aria2.tellWaiting", "aria2.tellStopped":
			fmt.Fprintf(response, `{"jsonrpc":"2.0","id":%d,"result":[]}`, input.ID)
		case "aria2.tellStatus":
			fmt.Fprintf(response, `{"jsonrpc":"2.0","id":%d,"result":{"gid":"a","status":"complete","totalLength":"100","completedLength":"100","dir":"/remote","files":[{"path":"/remote/01.mkv","length":"100","completedLength":"100","selected":"true"}]}}`, input.ID)
		case "aria2.addUri":
			fmt.Fprintf(response, `{"jsonrpc":"2.0","id":%d,"result":"new-gid"}`, input.ID)
		default:
			fmt.Fprintf(response, `{"jsonrpc":"2.0","id":%d,"result":"OK"}`, input.ID)
		}
	})
	client, err := New(Options{Name: "aria", Endpoint: "http://test/jsonrpc", Secret: "secret", HTTPClient: memoryClient(handler), PathMappings: []download.PathMapping{{Remote: "/remote", Local: "/media"}}})
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := client.List(context.Background())
	if err != nil || len(tasks) != 1 || tasks[0].State != download.StateDownloading || tasks[0].SavePath != "/media" {
		t.Fatalf("tasks = %+v, %v", tasks, err)
	}
	files, err := client.Files(context.Background(), "a")
	if err != nil || files[0].Path != "/media/01.mkv" {
		t.Fatalf("files = %+v, %v", files, err)
	}
	added, err := client.Add(context.Background(), download.AddRequest{URL: "https://example.test/file"})
	if err != nil || added.ID != "new-gid" {
		t.Fatalf("added = %+v, %v", added, err)
	}
	if err := client.Remove(context.Background(), "a", true); !errors.Is(err, download.ErrUnsupported) {
		t.Fatalf("Remove error = %v", err)
	}
}

func TestBitTorrentContentPathIsProvenFromReturnedFiles(t *testing.T) {
	value := status{GID: "a", Dir: "/remote"}
	value.BitTorrent.Info.Name = "abcabc146"
	value.Files = append(value.Files, struct {
		Path            string `json:"path"`
		Length          string `json:"length"`
		CompletedLength string `json:"completedLength"`
		Selected        string `json:"selected"`
		URIs            []struct {
			URI string `json:"uri"`
		} `json:"uris"`
	}{Path: "/remote/abcabc146/01.mkv", Selected: "true"})
	client, err := New(Options{Name: "aria", Endpoint: "http://test", HTTPClient: memoryClient(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})), PathMappings: []download.PathMapping{{Remote: "/remote", Local: "/media"}}})
	if err != nil {
		t.Fatal(err)
	}
	task := client.task(value)
	if task.ContentPath != "/media/abcabc146" {
		t.Fatalf("content path=%q", task.ContentPath)
	}
	value.Files[0].Path = "/remote/grpabc167/01.mkv"
	if got := client.task(value).ContentPath; got != "" {
		t.Fatalf("unproven content path=%q", got)
	}
}

func TestListObservationsDoesNotCallTellStatus(t *testing.T) {
	counts := map[string]int{}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			ID     int64  `json:"id"`
			Method string `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Fatal(err)
		}
		counts[input.Method]++
		w.Header().Set("Content-Type", "application/json")
		if input.Method == "aria2.tellActive" {
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":[{"gid":"a","dir":"/d","files":[{"path":"/d/a.mkv","length":"1","completedLength":"1","selected":"true"}]},{"gid":"b","dir":"/d","files":[{"path":"/d/b.mkv","length":"1","completedLength":"1","selected":"true"}]}]}`, input.ID)
		} else {
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":[]}`, input.ID)
		}
	})
	client, err := New(Options{Name: "aria", Endpoint: "http://test", HTTPClient: memoryClient(handler)})
	if err != nil {
		t.Fatal(err)
	}
	values, err := client.ListObservations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 2 || counts["aria2.tellStatus"] != 0 || counts["aria2.tellActive"] != 1 || counts["aria2.tellWaiting"] != 1 || counts["aria2.tellStopped"] != 1 {
		t.Fatalf("counts=%v observations=%+v", counts, values)
	}
}

func TestDefaultClientKeepsMigrationConnectionsWarm(t *testing.T) {
	client, err := New(Options{Endpoint: "http://127.0.0.1:6800/jsonrpc"})
	if err != nil {
		t.Fatal(err)
	}
	transport, ok := client.httpClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport=%T", client.httpClient.Transport)
	}
	if transport.MaxIdleConnsPerHost < 16 {
		t.Fatalf("MaxIdleConnsPerHost=%d", transport.MaxIdleConnsPerHost)
	}
}
