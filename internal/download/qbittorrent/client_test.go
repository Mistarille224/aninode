package qbittorrent

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"aninode/internal/download"
)

func TestDefaultHTTPTransportKeepsMigrationConnectionsWarm(t *testing.T) {
	client, err := New(Options{Name: "qbit", BaseURL: "http://example.invalid"})
	if err != nil {
		t.Fatal(err)
	}
	transport, ok := client.httpClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T", client.httpClient.Transport)
	}
	if transport.MaxIdleConnsPerHost < 16 || transport.MaxIdleConns < 32 {
		t.Fatalf("idle connection pool too small: perHost=%d total=%d", transport.MaxIdleConnsPerHost, transport.MaxIdleConns)
	}
}

func TestClientAuthListFilesAndFallback(t *testing.T) {
	var logins atomic.Int32
	var relocate, verify, labels atomic.Int32
	handler := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v2/auth/login":
			logins.Add(1)
			http.SetCookie(response, &http.Cookie{Name: "SID", Value: "ok", Path: "/"})
			fmt.Fprint(response, "Ok.")
		case "/api/v2/torrents/info":
			if cookie, err := request.Cookie("SID"); err != nil || cookie.Value != "ok" {
				http.Error(response, "Forbidden", http.StatusForbidden)
				return
			}
			fmt.Fprint(response, `[{"hash":"0123456789abcdef0123456789abcdef01234567","name":"abcabc146","state":"uploading","progress":1,"save_path":"/remote/anime","content_path":"/remote/anime/abcabc146","size":100}]`)
		case "/api/v2/torrents/files":
			fmt.Fprint(response, `[{"name":"abcabc146/01.mkv","size":100,"progress":1,"priority":1}]`)
		case "/api/v2/torrents/stop":
			http.NotFound(response, request)
		case "/api/v2/torrents/pause":
			response.WriteHeader(http.StatusOK)
		case "/api/v2/torrents/setLocation":
			if err := request.ParseForm(); err != nil || request.FormValue("location") != "/remote/adopted" {
				http.Error(response, "bad location", http.StatusBadRequest)
				return
			}
			relocate.Add(1)
		case "/api/v2/torrents/recheck":
			verify.Add(1)
		case "/api/v2/torrents/addTags":
			if err := request.ParseForm(); err != nil || request.FormValue("tags") != "aninode,subscription:show" {
				http.Error(response, "bad tags", http.StatusBadRequest)
				return
			}
			labels.Add(1)
		default:
			http.NotFound(response, request)
		}
	})
	client, err := New(Options{Name: "qbit", BaseURL: "http://test", Username: "u", Password: "p", HTTPClient: memoryClient(handler), PathMappings: []download.PathMapping{{Remote: "/remote", Local: "/media"}}})
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := client.List(context.Background())
	if err != nil || len(tasks) != 1 || tasks[0].State != download.StateSeeding || tasks[0].SavePath != "/media/anime" || tasks[0].ContentPath != "/media/anime/abcabc146" {
		t.Fatalf("tasks = %+v, %v", tasks, err)
	}
	files, err := client.Files(context.Background(), tasks[0].ID)
	if err != nil || len(files) != 1 || files[0].Path != "/media/anime/abcabc146/01.mkv" {
		t.Fatalf("files = %+v, %v", files, err)
	}
	if err := client.Pause(context.Background(), tasks[0].ID); err != nil {
		t.Fatal(err)
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
	if relocate.Load() != 1 || verify.Load() != 1 || labels.Load() != 1 {
		t.Fatalf("relocate=%d verify=%d labels=%d", relocate.Load(), verify.Load(), labels.Load())
	}
	if logins.Load() != 1 {
		t.Fatalf("logins = %d", logins.Load())
	}
}

func TestTaskFilePathUsesObservedQbitTopology(t *testing.T) {
	tests := []struct {
		name string
		task download.Task
		file string
		want string
		bad  bool
	}{
		{"multifile save-relative", download.Task{SavePath: "/downloads/movie", ContentPath: "/downloads/movie/Torrent Folder"}, "Torrent Folder/CDs/disc.flac", "/downloads/movie/Torrent Folder/CDs/disc.flac", false},
		{"multifile content-relative", download.Task{SavePath: "/downloads/movie", ContentPath: "/downloads/movie/Torrent Folder"}, "Scans/001.jpg", "/downloads/movie/Torrent Folder/Scans/001.jpg", false},
		{"single file", download.Task{SavePath: "/downloads/movie", ContentPath: "/downloads/movie/abcabc147.mkv"}, "abcabc147.mkv", "/downloads/movie/abcabc147.mkv", false},
		{"escape", download.Task{SavePath: "/downloads/movie", ContentPath: "/downloads/movie/Torrent Folder"}, "../secret", "", true},
		{"absolute", download.Task{SavePath: "/downloads/movie", ContentPath: "/downloads/movie/Torrent Folder"}, "/etc/passwd", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := taskFilePath(tc.task, tc.file)
			if tc.bad && err == nil {
				t.Fatalf("path=%q, expected error", got)
			}
			if !tc.bad && (err != nil || got != tc.want) {
				t.Fatalf("path=%q err=%v want=%q", got, err, tc.want)
			}
		})
	}
}

func TestFilesForTaskMapsMultifileAndSingleFileContentPaths(t *testing.T) {
	tests := []struct {
		name        string
		contentPath string
		filesJSON   string
		want        []string
	}{
		{
			name:        "multifile",
			contentPath: "/remote/movie/[grpabc202] abcabc113 [tagabc_816p]",
			filesJSON:   `[{"name":"[grpabc202] abcabc113 [tagabc_816p]/movie.mkv","priority":1},{"name":"[grpabc202] abcabc113 [tagabc_816p]/CDs/disc.flac","priority":1},{"name":"[grpabc202] abcabc113 [tagabc_816p]/Scans/001.jpg","priority":1}]`,
			want:        []string{"/usb/downloads/video/movie/[grpabc202] abcabc113 [tagabc_816p]/movie.mkv", "/usb/downloads/video/movie/[grpabc202] abcabc113 [tagabc_816p]/CDs/disc.flac", "/usb/downloads/video/movie/[grpabc202] abcabc113 [tagabc_816p]/Scans/001.jpg"},
		},
		{name: "single", contentPath: "/remote/movie/abcabc147.mkv", filesJSON: `[{"name":"abcabc147.mkv","priority":1}]`, want: []string{"/usb/downloads/video/movie/abcabc147.mkv"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v2/torrents/info":
					fmt.Fprintf(w, `[{"hash":"abc","name":"fixture","save_path":"/remote/movie","content_path":%q}]`, tc.contentPath)
				case "/api/v2/torrents/files":
					fmt.Fprint(w, tc.filesJSON)
				default:
					http.NotFound(w, r)
				}
			})
			client, err := New(Options{Name: "qbit", BaseURL: "http://test", HTTPClient: memoryClient(handler), PathMappings: []download.PathMapping{{Remote: "/remote", Local: "/usb/downloads/video"}}})
			if err != nil {
				t.Fatal(err)
			}
			files, err := client.Files(context.Background(), "abc")
			if err != nil || len(files) != len(tc.want) {
				t.Fatalf("files=%+v err=%v", files, err)
			}
			for i := range tc.want {
				if files[i].Path != tc.want[i] {
					t.Fatalf("file[%d]=%q want=%q", i, files[i].Path, tc.want[i])
				}
			}
		})
	}
}

func TestLoginAcceptsNoContentWithSessionCookie(t *testing.T) {
	handler := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v2/auth/login":
			http.SetCookie(response, &http.Cookie{Name: "SID", Value: "ok", Path: "/"})
			response.WriteHeader(http.StatusNoContent)
		case "/api/v2/torrents/info":
			if cookie, err := request.Cookie("SID"); err != nil || cookie.Value != "ok" {
				http.Error(response, "Forbidden", http.StatusForbidden)
				return
			}
			fmt.Fprint(response, `[]`)
		default:
			http.NotFound(response, request)
		}
	})
	client, err := New(Options{Name: "qbit", BaseURL: "http://test", Username: "u", Password: "p", HTTPClient: memoryClient(handler)})
	if err != nil {
		t.Fatal(err)
	}
	if tasks, err := client.List(context.Background()); err != nil || len(tasks) != 0 {
		t.Fatalf("tasks=%+v err=%v", tasks, err)
	}
}

func TestLoginStillRejectsCredentialFailureWithHTTP200(t *testing.T) {
	handler := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		fmt.Fprint(response, "Fails.")
	})
	client, err := New(Options{Name: "qbit", BaseURL: "http://test", Username: "bad", Password: "bad", HTTPClient: memoryClient(handler)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.List(context.Background()); err == nil || !strings.Contains(err.Error(), "login failed with HTTP 200") {
		t.Fatalf("credential failure = %v", err)
	}
}

func TestAddMagnetReturnsHash(t *testing.T) {
	handler := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/v2/torrents/add" {
			http.NotFound(response, request)
			return
		}
		if err := request.ParseMultipartForm(1 << 20); err != nil || !strings.HasPrefix(request.FormValue("urls"), "magnet:") {
			http.Error(response, "bad form", http.StatusBadRequest)
		}
	})
	client, _ := New(Options{Name: "qbit", BaseURL: "http://test", HTTPClient: memoryClient(handler)})
	task, err := client.Add(context.Background(), download.AddRequest{URL: "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567"})
	if err != nil || task.ID != "0123456789abcdef0123456789abcdef01234567" {
		t.Fatalf("task = %+v, %v", task, err)
	}
}

func TestQbitStateVariants(t *testing.T) {
	tests := map[string]download.State{
		"queuedDL":     download.StateQueued,
		"stalledDL":    download.StateDownloading,
		"stalledUP":    download.StateSeeding,
		"stoppedDL":    download.StatePaused,
		"checkingUP":   download.StateChecking,
		"missingFiles": download.StateFailed,
	}
	for input, want := range tests {
		if got := qbitState(input, 0.5); got != want {
			t.Errorf("qbitState(%q) = %q, want %q", input, got, want)
		}
	}
}
