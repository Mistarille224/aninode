package server

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const testPassword = "a-long-test-password"

func authRequest(h http.Handler, path string, body any, cookie *http.Cookie) *httptest.ResponseRecorder {
	return authRequestFrom(h, path, body, cookie, "192.0.2.1:1234")
}

func authRequestFrom(h http.Handler, path string, body any, cookie *http.Cookie, remote string) *httptest.ResponseRecorder {
	data, _ := json.Marshal(body)
	r := httptest.NewRequest("POST", path, bytes.NewReader(data))
	r.RemoteAddr = remote
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Aninode-Request", "1")
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func testWebAuth(t *testing.T) (*WebAuth, *http.Cookie) {
	t.Helper()
	root := t.TempDir()
	a, err := OpenWebAuth(root)
	if err != nil {
		t.Fatal(err)
	}
	code, _, err := SetupCode(root)
	if err != nil {
		t.Fatal(err)
	}
	w := authRequest((&Service{Auth: a}).Handler(), "/auth/setup", map[string]string{"password": testPassword, "setup_code": code}, nil)
	if w.Code != 200 {
		t.Fatalf("setup: %d %s", w.Code, w.Body.String())
	}
	return a, w.Result().Cookies()[0]
}
func authenticatedHandler(t *testing.T, s *Service) http.Handler {
	t.Helper()
	auth, cookie := testWebAuth(t)
	s.Auth = auth
	h := s.Handler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = r.Clone(r.Context())
		r.AddCookie(cookie)
		r.Header.Set("X-Aninode-Request", "1")
		h.ServeHTTP(w, r)
	})
}
func getWithCookie(h http.Handler, path string, cookie *http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", path, nil)
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestWebLoginLifecycle(t *testing.T) {
	root := t.TempDir()
	auth, err := OpenWebAuth(root)
	if err != nil {
		t.Fatal(err)
	}
	app, _ := webApp(t)
	h := (&Service{App: app, Auth: auth}).Handler()
	status := getWithCookie(h, "/auth/status", nil)
	if !strings.Contains(status.Body.String(), `"setup_required":true`) {
		t.Fatal(status.Body.String())
	}
	if got := getWithCookie(h, "/ui/config", nil).Code; got != 401 {
		t.Fatalf("unauthenticated config: %d", got)
	}
	code, expires, err := SetupCode(root)
	if err != nil || !expires.After(time.Now()) {
		t.Fatalf("setup code: expires=%v err=%v", expires, err)
	}
	if got := authRequest(h, "/auth/setup", map[string]string{"password": testPassword, "setup_code": "wrong"}, nil).Code; got != 401 {
		t.Fatalf("invalid setup code accepted: %d", got)
	}
	if got := authRequest(h, "/auth/setup", map[string]string{"password": "short", "setup_code": code}, nil).Code; got != 400 {
		t.Fatalf("weak password accepted: %d", got)
	}
	setup := authRequest(h, "/auth/setup", map[string]string{"password": testPassword, "setup_code": code}, nil)
	if setup.Code != 200 {
		t.Fatalf("setup: %d %s", setup.Code, setup.Body.String())
	}
	cookie := setup.Result().Cookies()[0]
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != "/" || cookie.MaxAge <= 0 {
		t.Fatalf("unsafe cookie: %+v", cookie)
	}
	if got := getWithCookie(h, "/ui/config", cookie).Code; got != 200 {
		t.Fatalf("session config: %d", got)
	}
	if _, err := os.Stat(filepath.Join(root, "secrets", "auth", "setup.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("setup code was not deleted: %v", err)
	}
	if got := authRequest(h, "/auth/setup", map[string]string{"password": "another-password", "setup_code": code}, nil).Code; got != 409 {
		t.Fatalf("repeated setup: %d", got)
	}
	data, err := os.ReadFile(filepath.Join(root, "secrets", "auth", "login.json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(testPassword)) {
		t.Fatal("plaintext password persisted")
	}
	if info, _ := os.Stat(filepath.Join(root, "secrets", "auth", "login.json")); info.Mode().Perm() != 0600 {
		t.Fatal("unsafe login file permissions")
	}
	if got := authRequest(h, "/auth/login", map[string]string{"password": "wrong"}, nil).Code; got != 401 {
		t.Fatalf("wrong password: %d", got)
	}
	second := authRequest(h, "/auth/login", map[string]string{"password": testPassword}, nil)
	if second.Code != 200 {
		t.Fatal(second.Body.String())
	}
	secondCookie := second.Result().Cookies()[0]
	changed := authRequest(h, "/auth/password", map[string]string{"current_password": testPassword, "password": "the-new-long-password"}, cookie)
	if changed.Code != 200 {
		t.Fatal(changed.Body.String())
	}
	newCookie := changed.Result().Cookies()[0]
	for _, old := range []*http.Cookie{cookie, secondCookie} {
		if getWithCookie(h, "/ui/config", old).Code != 401 {
			t.Fatal("password change did not revoke old session")
		}
	}
	if getWithCookie(h, "/ui/config", newCookie).Code != 200 {
		t.Fatal("replacement session invalid")
	}
	if authRequest(h, "/auth/logout", map[string]string{}, newCookie).Code != 200 || getWithCookie(h, "/ui/config", newCookie).Code != 401 {
		t.Fatal("logout failed to revoke session")
	}
	reloaded, err := OpenWebAuth(root)
	if err != nil {
		t.Fatal(err)
	}
	h = (&Service{App: app, Auth: reloaded}).Handler()
	if authRequest(h, "/auth/login", map[string]string{"password": testPassword}, nil).Code != 401 {
		t.Fatal("old password still works")
	}
	login := authRequest(h, "/auth/login", map[string]string{"password": "the-new-long-password"}, nil)
	if login.Code != 200 {
		t.Fatal("new password not persisted")
	}
	reloaded.now = func() time.Time { return time.Now().Add(sessionLifetime + time.Second) }
	if getWithCookie(h, "/ui/config", login.Result().Cookies()[0]).Code != 401 {
		t.Fatal("expired session accepted")
	}
}

func TestLoginCSRFAndRetiredAPI(t *testing.T) {
	auth, cookie := testWebAuth(t)
	app, _ := webApp(t)
	h := (&Service{App: app, Auth: auth}).Handler()
	for _, path := range []string{"/api/v1/config", "/api/v1/diagnostics", "/api/health", "/api/ready"} {
		if getWithCookie(h, path, cookie).Code != 404 {
			t.Fatalf("retired endpoint still exists: %s", path)
		}
	}
	for _, path := range []string{"/auth/setup", "/auth/login", "/auth/logout", "/auth/password", "/ui/clients/c"} {
		for _, custom := range []bool{false, true} {
			r := httptest.NewRequest("POST", path, strings.NewReader(`{"password":"a-long-password"}`))
			r.AddCookie(cookie)
			r.Header.Set("Origin", "https://attacker.test")
			if custom {
				r.Header.Set("X-Aninode-Request", "1")
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != 403 {
				t.Fatalf("cross-origin request accepted: %s %d", path, w.Code)
			}
		}
	}
	r := httptest.NewRequest("GET", "/ui/config", nil)
	r.Header.Set("Authorization", "Bearer old-api-token")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("Bearer accepted without session")
	}
	r = httptest.NewRequest("POST", "/auth/login", strings.NewReader(`{"password":"`+testPassword+`"}`))
	r.Header.Set("X-Aninode-Request", "1")
	r.TLS = &tls.ConnectionState{}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 || !w.Result().Cookies()[0].Secure {
		t.Fatal("HTTPS session cookie must be Secure")
	}
	if getWithCookie((&Service{App: app}).Handler(), "/ui/config", nil).Code != 401 {
		t.Fatal("missing auth allowed access")
	}
}

func TestLoginRateLimitAndCorruptState(t *testing.T) {
	auth, _ := testWebAuth(t)
	h := (&Service{Auth: auth}).Handler()
	for i := 0; i < 5; i++ {
		if authRequest(h, "/auth/login", map[string]string{"password": "wrong"}, nil).Code != 401 {
			t.Fatal("unexpected login result")
		}
	}
	if authRequest(h, "/auth/login", map[string]string{"password": testPassword}, nil).Code != 429 {
		t.Fatal("missing per-source rate limit")
	}
	if authRequestFrom(h, "/auth/login", map[string]string{"password": testPassword}, nil, "192.0.2.2:4321").Code != 200 {
		t.Fatal("one source blocked an unrelated login")
	}
	auth.now = func() time.Time { return time.Now().Add(31 * time.Second) }
	if authRequest(h, "/auth/login", map[string]string{"password": testPassword}, nil).Code != 200 {
		t.Fatal("rate limit did not expire")
	}
	if err := os.WriteFile(auth.path, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenWebAuth(filepath.Dir(filepath.Dir(filepath.Dir(auth.path)))); err == nil {
		t.Fatal("corrupt auth reset to setup mode")
	}
}

func TestConcurrentSetupHasOneWinner(t *testing.T) {
	root := t.TempDir()
	a, _ := OpenWebAuth(root)
	b, _ := OpenWebAuth(root)
	code, _, err := SetupCode(root)
	if err != nil {
		t.Fatal(err)
	}
	handlers := []http.Handler{(&Service{Auth: a}).Handler(), (&Service{Auth: b}).Handler()}
	var wg sync.WaitGroup
	results := make(chan int, 2)
	for _, h := range handlers {
		wg.Go(func() {
			results <- authRequest(h, "/auth/setup", map[string]string{"password": testPassword, "setup_code": code}, nil).Code
		})
	}
	wg.Wait()
	close(results)
	success := 0
	for code := range results {
		if code == 200 {
			success++
		}
	}
	if success != 1 {
		t.Fatalf("setup successes: %d", success)
	}
}

func TestPasswordMinimumIsEightCharacters(t *testing.T) {
	if _, err := passwordRecord("12345678"); err != nil {
		t.Fatalf("8-character password rejected: %v", err)
	}
	if _, err := passwordRecord("1234567"); err == nil {
		t.Fatal("7-character password accepted")
	}
}
