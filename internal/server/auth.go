package server

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"aninode/internal/atomicfile"
)

const passwordIterations = 600000
const sessionLifetime = 7 * 24 * time.Hour
const setupCodeLifetime = 10 * time.Minute
const loginBlockDuration = 30 * time.Second
const loginFailureLimit = 5
const sessionCookie = "aninode_session"

type loginRecord struct {
	Version int    `json:"version"`
	Salt    []byte `json:"salt"`
	Hash    []byte `json:"hash"`
}

type setupRecord struct {
	Version   int       `json:"version"`
	Code      string    `json:"code"`
	ExpiresAt time.Time `json:"expires_at"`
}

type loginThrottle struct {
	Failures     int
	BlockedUntil time.Time
	LastSeen     time.Time
}

// WebAuth stores only a salted password hash on disk. Sessions and login
// throttles are ephemeral: logout, password changes, expiry and process restart
// revoke/reset them. The first-run setup code is a short-lived host-held file
// and is deleted as soon as the initial password is committed.
type WebAuth struct {
	mu        sync.Mutex
	path      string
	setupPath string
	record    *loginRecord
	sessions  map[[32]byte]time.Time
	throttles map[string]loginThrottle
	hashSlots chan struct{}
	now       func() time.Time
}

func OpenWebAuth(configRoot string) (*WebAuth, error) {
	if configRoot == "" {
		return nil, errors.New("login configuration root is required")
	}
	a := &WebAuth{
		path:      filepath.Join(configRoot, "secrets", "auth", "login.json"),
		setupPath: filepath.Join(configRoot, "secrets", "auth", "setup.json"),
		sessions:  make(map[[32]byte]time.Time),
		throttles: make(map[string]loginThrottle),
		hashSlots: make(chan struct{}, 2),
		now:       time.Now,
	}
	info, err := os.Lstat(a.path)
	if errors.Is(err, fs.ErrNotExist) {
		if _, err := ensureSetupCodeAt(a.setupPath, a.now(), false); err != nil {
			return nil, fmt.Errorf("prepare setup code: %w", err)
		}
		return a, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("login configuration must be a private regular file (0600)")
	}
	data, err := os.ReadFile(a.path)
	if err != nil {
		return nil, err
	}
	var record loginRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return nil, errors.New("invalid login configuration")
	}
	if record.Version != 1 || len(record.Salt) != 32 || len(record.Hash) != 32 {
		return nil, errors.New("invalid login password hash")
	}
	a.record = &record
	_ = os.Remove(a.setupPath)
	return a, nil
}

// SetupCode returns the current one-time first-run code. If the previous code
// expired, a fresh ten-minute code is created. This is intended for host-side
// commands such as `docker compose exec aninode aninode setup-code`.
func SetupCode(configRoot string) (string, time.Time, error) {
	if configRoot == "" {
		return "", time.Time{}, errors.New("login configuration root is required")
	}
	loginPath := filepath.Join(configRoot, "secrets", "auth", "login.json")
	if _, err := os.Lstat(loginPath); err == nil {
		return "", time.Time{}, errors.New("login password is already configured")
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", time.Time{}, err
	}
	record, err := ensureSetupCodeAt(filepath.Join(configRoot, "secrets", "auth", "setup.json"), time.Now(), false)
	if err != nil {
		return "", time.Time{}, err
	}
	return record.Code, record.ExpiresAt, nil
}

func ensureSetupCodeAt(path string, now time.Time, force bool) (setupRecord, error) {
	if !force {
		if record, err := readSetupCodeAt(path); err == nil && now.Before(record.ExpiresAt) {
			return record, nil
		} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return setupRecord{}, err
		}
	}
	data := make([]byte, 18)
	if _, err := rand.Read(data); err != nil {
		return setupRecord{}, err
	}
	record := setupRecord{Version: 1, Code: base64.RawURLEncoding.EncodeToString(data), ExpiresAt: now.Add(setupCodeLifetime)}
	encoded, err := json.Marshal(record)
	if err != nil {
		return setupRecord{}, err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return setupRecord{}, err
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return setupRecord{}, err
	}
	if err := atomicfile.Write(path, encoded, 0600); err != nil {
		return setupRecord{}, err
	}
	return record, nil
}

func readSetupCodeAt(path string) (setupRecord, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return setupRecord{}, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return setupRecord{}, errors.New("setup code must be a private regular file (0600)")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return setupRecord{}, err
	}
	var record setupRecord
	if err := json.Unmarshal(data, &record); err != nil || record.Version != 1 || record.Code == "" || record.ExpiresAt.IsZero() {
		return setupRecord{}, errors.New("invalid setup code")
	}
	return record, nil
}

func passwordRecord(password string) (*loginRecord, error) {
	if utf8.RuneCountInString(password) < 8 || len(password) > 1024 {
		return nil, errors.New("The sign-in password must be at least 8 characters and at most 1024 bytes")
	}
	salt := make([]byte, 32)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	hash, err := pbkdf2.Key(sha256.New, password, salt, passwordIterations, 32)
	if err != nil {
		return nil, err
	}
	return &loginRecord{Version: 1, Salt: salt, Hash: hash}, nil
}

func matchesRecord(password string, record *loginRecord) bool {
	if record == nil || len(password) > 1024 {
		return false
	}
	hash, err := pbkdf2.Key(sha256.New, password, record.Salt, passwordIterations, 32)
	return err == nil && subtle.ConstantTimeCompare(hash, record.Hash) == 1
}

func sameRecord(a, b *loginRecord) bool {
	return a != nil && b != nil && subtle.ConstantTimeCompare(a.Salt, b.Salt) == 1 && subtle.ConstantTimeCompare(a.Hash, b.Hash) == 1
}

func (a *WebAuth) saveLocked(record *loginRecord, initial bool) error {
	dir := filepath.Dir(a.path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return err
	}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if initial {
		err = atomicfile.Create(a.path, data, 0600)
	} else {
		err = atomicfile.Write(a.path, data, 0600)
	}
	if err != nil {
		return err
	}
	a.record = record
	return nil
}

func (a *WebAuth) validLocked(r *http.Request) bool {
	c, err := r.Cookie(sessionCookie)
	if err != nil || len(c.Value) != 43 {
		return false
	}
	key := sha256.Sum256([]byte(c.Value))
	expires, ok := a.sessions[key]
	if !ok {
		return false
	}
	if !a.now().Before(expires) {
		delete(a.sessions, key)
		return false
	}
	return true
}

func (a *WebAuth) valid(r *http.Request) bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.validLocked(r)
}

func secureCookie(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	origin, err := url.Parse(r.Header.Get("Origin"))
	return err == nil && origin.Scheme == "https" && origin.Host == r.Host
}

func setSessionCookie(w http.ResponseWriter, r *http.Request, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: value, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: secureCookie(r), MaxAge: maxAge})
}

func (a *WebAuth) revokeLocked(r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		delete(a.sessions, sha256.Sum256([]byte(c.Value)))
	}
}

func (a *WebAuth) sessionLocked(w http.ResponseWriter, r *http.Request) error {
	data := make([]byte, 32)
	if _, err := rand.Read(data); err != nil {
		return err
	}
	a.revokeLocked(r)
	now := a.now()
	for key, expiry := range a.sessions {
		if !now.Before(expiry) {
			delete(a.sessions, key)
		}
	}
	if len(a.sessions) >= 32 {
		var oldest [32]byte
		var expiry time.Time
		for key, end := range a.sessions {
			if expiry.IsZero() || end.Before(expiry) {
				oldest, expiry = key, end
			}
		}
		delete(a.sessions, oldest)
	}
	value := base64.RawURLEncoding.EncodeToString(data)
	a.sessions[sha256.Sum256([]byte(value))] = now.Add(sessionLifetime)
	setSessionCookie(w, r, value, int(sessionLifetime.Seconds()))
	return nil
}

func loginSource(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil || host == "" {
		host = r.RemoteAddr
	}
	// The documented/default reverse proxy runs on the same host. Only trust
	// its forwarded client address when the immediate peer is loopback, so a
	// directly exposed client cannot rotate X-Forwarded-For to evade limits.
	if peer := net.ParseIP(host); peer != nil && peer.IsLoopback() {
		if forwarded := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-For"), ",")[0]); net.ParseIP(forwarded) != nil {
			return forwarded
		}
	}
	if host != "" {
		return host
	}
	return "unknown"
}

func (a *WebAuth) blocked(source string) (bool, time.Duration) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	for key, item := range a.throttles {
		if now.Sub(item.LastSeen) > 10*time.Minute {
			delete(a.throttles, key)
		}
	}
	item := a.throttles[source]
	if now.Before(item.BlockedUntil) {
		return true, item.BlockedUntil.Sub(now)
	}
	return false, 0
}

func (a *WebAuth) failed(source string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	item := a.throttles[source]
	item.Failures++
	item.LastSeen = now
	if item.Failures >= loginFailureLimit {
		item.Failures = 0
		item.BlockedUntil = now.Add(loginBlockDuration)
	}
	a.throttles[source] = item
}

func (a *WebAuth) succeeded(source string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.throttles, source)
}

func (a *WebAuth) acquireHashSlot() bool {
	select {
	case a.hashSlots <- struct{}{}:
		return true
	default:
		return false
	}
}

func (a *WebAuth) releaseHashSlot() { <-a.hashSlots }

func (s *Service) authStatus(w http.ResponseWriter, r *http.Request) {
	if s.Auth == nil {
		writeAPIError(w, 503, "unavailable", "Sign-in service is unavailable")
		return
	}
	a := s.Auth
	a.mu.Lock()
	defer a.mu.Unlock()
	writeJSON(w, 200, map[string]bool{"setup_required": a.record == nil, "authenticated": a.validLocked(r)})
}

func (s *Service) authWrite(w http.ResponseWriter, r *http.Request) {
	if s.Auth == nil {
		writeAPIError(w, 503, "unavailable", "Sign-in service is unavailable")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	var input struct {
		Password        string `json:"password"`
		CurrentPassword string `json:"current_password,omitempty"`
		SetupCode       string `json:"setup_code,omitempty"`
	}
	if r.URL.Path != "/auth/logout" {
		if err := decodeStrict(r, &input); err != nil {
			writeAPIError(w, 400, "invalid_request", "Unable to read sign-in request")
			return
		}
	}
	a := s.Auth

	if r.URL.Path == "/auth/logout" {
		a.mu.Lock()
		a.revokeLocked(r)
		a.mu.Unlock()
		setSessionCookie(w, r, "", -1)
		writeJSON(w, 200, map[string]bool{"authenticated": false})
		return
	}

	if r.URL.Path == "/auth/setup" {
		a.mu.Lock()
		configured := a.record != nil
		a.mu.Unlock()
		if configured {
			writeAPIError(w, 409, "already_configured", "A sign-in password is already set. Please sign in.")
			return
		}
		setup, err := readSetupCodeAt(a.setupPath)
		if err != nil {
			writeAPIError(w, 503, "setup_code_unavailable", "Setup code is unavailable. Get a new one from the host.")
			return
		}
		if !a.now().Before(setup.ExpiresAt) {
			writeAPIError(w, 401, "setup_code_expired", "Setup code has expired. Get a new one from the host.")
			return
		}
		provided, expected := sha256.Sum256([]byte(input.SetupCode)), sha256.Sum256([]byte(setup.Code))
		if subtle.ConstantTimeCompare(provided[:], expected[:]) != 1 {
			writeAPIError(w, 401, "invalid_setup_code", "Setup code is incorrect")
			return
		}
		record, err := passwordRecord(input.Password)
		if err != nil {
			writeAPIError(w, 400, "invalid_password", err.Error())
			return
		}
		a.mu.Lock()
		if a.record != nil {
			a.mu.Unlock()
			writeAPIError(w, 409, "already_configured", "A sign-in password is already set. Please sign in.")
			return
		}
		if err := a.saveLocked(record, true); err != nil {
			a.mu.Unlock()
			if errors.Is(err, fs.ErrExist) {
				writeAPIError(w, 409, "already_configured", "A sign-in password is already set. Please sign in.")
			} else {
				writeAPIError(w, 500, "save_failed", "Unable to save sign-in settings")
			}
			return
		}
		_ = os.Remove(a.setupPath)
		if err := a.sessionLocked(w, r); err != nil {
			a.mu.Unlock()
			writeAPIError(w, 500, "session_failed", "Unable to create sign-in session")
			return
		}
		a.mu.Unlock()
		writeJSON(w, 200, map[string]bool{"authenticated": true, "setup_required": false})
		return
	}

	if r.URL.Path != "/auth/login" && r.URL.Path != "/auth/password" {
		http.NotFound(w, r)
		return
	}

	source := loginSource(r)
	if blocked, retry := a.blocked(source); blocked {
		seconds := int(retry.Round(time.Second).Seconds())
		if seconds < 1 {
			seconds = 1
		}
		w.Header().Set("Retry-After", fmt.Sprintf("%d", seconds))
		writeAPIError(w, 429, "rate_limited", "Too many attempts from this source. Try again later.")
		return
	}

	a.mu.Lock()
	if a.record == nil {
		a.mu.Unlock()
		writeAPIError(w, 409, "setup_required", "Set a sign-in password first")
		return
	}
	if r.URL.Path == "/auth/password" && !a.validLocked(r) {
		a.mu.Unlock()
		writeAPIError(w, 401, "unauthorized", "Please sign in first")
		return
	}
	record := &loginRecord{Version: a.record.Version, Salt: append([]byte(nil), a.record.Salt...), Hash: append([]byte(nil), a.record.Hash...)}
	a.mu.Unlock()

	if !a.acquireHashSlot() {
		w.Header().Set("Retry-After", "1")
		writeAPIError(w, 429, "busy", "Sign-in verification is busy. Try again later.")
		return
	}
	password := input.Password
	if r.URL.Path == "/auth/password" {
		password = input.CurrentPassword
	}
	matched := matchesRecord(password, record)
	a.releaseHashSlot()
	if !matched {
		a.failed(source)
		writeAPIError(w, 401, "invalid_password", "Incorrect password")
		return
	}

	a.mu.Lock()
	if !sameRecord(a.record, record) {
		a.mu.Unlock()
		writeAPIError(w, 409, "credentials_changed", "Sign-in credentials changed. Try again.")
		return
	}
	a.mu.Unlock()

	if r.URL.Path == "/auth/password" {
		newRecord, err := passwordRecord(input.Password)
		if err != nil {
			writeAPIError(w, 400, "invalid_password", err.Error())
			return
		}
		a.mu.Lock()
		if !sameRecord(a.record, record) {
			a.mu.Unlock()
			writeAPIError(w, 409, "credentials_changed", "Sign-in credentials changed. Try again.")
			return
		}
		if err := a.saveLocked(newRecord, false); err != nil {
			a.mu.Unlock()
			writeAPIError(w, 500, "save_failed", "Unable to save sign-in settings")
			return
		}
		clear(a.sessions)
		if err := a.sessionLocked(w, r); err != nil {
			a.mu.Unlock()
			writeAPIError(w, 500, "session_failed", "Unable to create sign-in session")
			return
		}
		a.mu.Unlock()
	} else {
		a.mu.Lock()
		if err := a.sessionLocked(w, r); err != nil {
			a.mu.Unlock()
			writeAPIError(w, 500, "session_failed", "Unable to create sign-in session")
			return
		}
		a.mu.Unlock()
	}
	a.succeeded(source)
	writeJSON(w, 200, map[string]bool{"authenticated": true, "setup_required": false})
}

func (s *Service) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.Auth.valid(r) {
			writeAPIError(w, 401, "unauthorized", "Please sign in first")
			return
		}
		next(w, r)
	}
}

// A custom header blocks simple cross-origin form requests, including login
// CSRF in older browsers. Fetch Metadata/Origin checking also protects writes.
func browserRequests(next http.Handler) http.Handler {
	protected := http.NewCrossOriginProtection().Handler(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		if r.Method != "GET" && r.Method != "HEAD" && r.Method != "OPTIONS" && r.Header.Get("X-Aninode-Request") != "1" {
			writeAPIError(w, 403, "forbidden", "Invalid request origin")
			return
		}
		protected.ServeHTTP(w, r)
	})
}
