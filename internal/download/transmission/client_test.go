package transmission

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"

	"aninode/internal/download"
)

func TestClientNegotiatesSessionAndNormalizesTasks(t *testing.T) {
	var calledMu sync.Mutex
	called := map[string]bool{}
	handler := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("X-Transmission-Session-Id") != "session" {
			response.Header().Set("X-Transmission-Session-Id", "session")
			response.WriteHeader(http.StatusConflict)
			return
		}
		var input struct {
			Method string `json:"method"`
		}
		if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
			t.Fatal(err)
		}
		calledMu.Lock()
		called[input.Method] = true
		calledMu.Unlock()
		switch input.Method {
		case "torrent-get":
			fmt.Fprint(response, `{"result":"success","arguments":{"torrents":[{"id":1,"hashString":"0123456789abcdef0123456789abcdef01234567","name":"abcabc146","status":4,"percentComplete":0.5,"downloadDir":"/remote","files":[{"name":"abcabc146/01.mkv","length":100,"bytesCompleted":50}],"fileStats":[{"wanted":1}]}]}}`)
		case "torrent-add":
			fmt.Fprint(response, `{"result":"success","arguments":{"torrent-added":{"id":2,"hashString":"abcdefabcdefabcdefabcdefabcdefabcdefabcd","name":"Added"}}}`)
		default:
			fmt.Fprint(response, `{"result":"success","arguments":{}}`)
		}
	})
	client, err := New(Options{Name: "transmission", BaseURL: "http://test", HTTPClient: memoryClient(handler), PathMappings: []download.PathMapping{{Remote: "/remote", Local: "/media"}}})
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := client.List(context.Background())
	if err != nil || len(tasks) != 1 || tasks[0].State != download.StateDownloading || tasks[0].ID == "1" || tasks[0].ContentPath != "/media/abcabc146" {
		t.Fatalf("tasks = %+v, %v", tasks, err)
	}
	files, err := client.Files(context.Background(), tasks[0].ID)
	if err != nil || files[0].Path != "/media/abcabc146/01.mkv" || files[0].Progress != 0.5 {
		t.Fatalf("files = %+v, %v", files, err)
	}
	added, err := client.Add(context.Background(), download.AddRequest{URL: "magnet:?xt=urn:btih:test"})
	if err != nil || added.Name != "Added" {
		t.Fatalf("added = %+v, %v", added, err)
	}
	if err := client.Relocate(context.Background(), tasks[0].ID, "/media/adopted"); err != nil {
		t.Fatal(err)
	}
	if err := client.Verify(context.Background(), tasks[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := client.SetLabels(context.Background(), tasks[0].ID, []string{"aninode", "subscription:show"}); err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{"torrent-set-location", "torrent-verify", "torrent-set"} {
		if !called[method] {
			t.Errorf("method %s was not called", method)
		}
	}
}

func TestListObservationsUsesOneTorrentGetForAllFiles(t *testing.T) {
	var calls int
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		fmt.Fprint(w, `{"result":"success","arguments":{"torrents":[{"id":1,"downloadDir":"/d","files":[{"name":"a.mkv","length":1,"bytesCompleted":1}],"fileStats":[{"wanted":1}]},{"id":2,"downloadDir":"/d","files":[{"name":"b.mkv","length":1,"bytesCompleted":1}],"fileStats":[{"wanted":1}]}]}}`)
	})
	client, err := New(Options{Name: "transmission", BaseURL: "http://test", HTTPClient: memoryClient(handler)})
	if err != nil {
		t.Fatal(err)
	}
	values, err := client.ListObservations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || len(values) != 2 || len(values[0].Files) != 1 || len(values[1].Files) != 1 {
		t.Fatalf("calls=%d observations=%+v", calls, values)
	}
}

func TestDefaultClientKeepsMigrationConnectionsWarm(t *testing.T) {
	client, err := New(Options{BaseURL: "http://127.0.0.1:9091"})
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
