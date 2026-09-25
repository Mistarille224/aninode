package server

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"aninode/internal/application"
	"aninode/internal/automation"
	"aninode/internal/candidate"
	"aninode/internal/catalog"
	"aninode/internal/configstore"
	"aninode/internal/organizer"
	"aninode/internal/releasefilter"
)

//go:embed web/index.html web/favicon.svg web/app.css web/app.js web/controls.js web/i18n.js web/locales/*.json
var webAssets embed.FS

type Service struct {
	App           *application.App
	Auth          *WebAuth
	Logger        *slog.Logger
	LocalInterval time.Duration

	reconcileMu        sync.Mutex
	reconcileRunning   reconcileMode
	reconcilePending   reconcileMode
	reconcileCtx       context.Context
	reconcileWG        sync.WaitGroup
	lifecycleOnce      sync.Once
	reconcileCompleted chan reconcileMode
}

type APIError struct {
	Error ErrorBody `json:"error"`
}
type ErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
type BackfillRequest struct {
	From    int `json:"from"`
	Through int `json:"through"`
}
type EmptyRequest struct{}
type MigrationApplyRequest struct {
	Key       string                              `json:"key"`
	EntryKey  string                              `json:"entry_key,omitempty"`
	Title     string                              `json:"title"`
	Season    int                                 `json:"season"`
	Year      int                                 `json:"year,omitempty"`
	MediaType string                              `json:"media_type"`
	Filters   releasefilter.Filters               `json:"filters,omitempty"`
	Movie     catalog.MovieDeclaration            `json:"movie,omitempty"`
	Tasks     []application.MigrationTaskIdentity `json:"tasks"`
}

func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	assets, err := fs.Sub(webAssets, "web")
	if err != nil {
		panic(err)
	}
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", http.FileServer(http.FS(assets))))
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		serveSPA(w, r, assets)
	})
	mux.HandleFunc("GET /auth/status", s.authStatus)
	for _, path := range []string{"/auth/setup", "/auth/login", "/auth/logout", "/auth/password"} {
		mux.HandleFunc("POST "+path, s.authWrite)
	}
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /readyz", s.ready)
	mux.HandleFunc("GET /ui/diagnostics", s.auth(s.diagnostics))
	mux.HandleFunc("POST /ui/automation/run", s.auth(s.automationRun))
	mux.HandleFunc("GET /ui/config", s.auth(s.getConfig))
	mux.HandleFunc("POST /ui/config/validate", s.auth(s.validateConfig))
	mux.HandleFunc("PUT /ui/config/organizer", s.auth(s.putOrganizer))
	mux.HandleFunc("GET /ui/clients/status", s.auth(s.clientStatuses))
	mux.HandleFunc("PUT /ui/clients/{id}", s.auth(s.putClient))
	mux.HandleFunc("DELETE /ui/clients/{id}", s.auth(s.deleteClient))
	mux.HandleFunc("PUT /ui/sources/{id}", s.auth(s.putSource))
	mux.HandleFunc("DELETE /ui/sources/{id}", s.auth(s.deleteSource))
	mux.HandleFunc("GET /ui/discovery/rss", s.auth(s.rssDiscoveries))
	mux.HandleFunc("POST /ui/discovery/rss/refresh", s.auth(s.refreshRSS))
	mux.HandleFunc("GET /ui/discovery/search", s.auth(s.searchDiscovery))
	mux.HandleFunc("POST /ui/discovery/acquire", s.auth(s.acquireDiscovery))
	mux.HandleFunc("GET /ui/catalog", s.auth(s.listEntries))
	mux.HandleFunc("POST /ui/catalog/{media}", s.auth(s.createEntry))
	mux.HandleFunc("GET /ui/catalog/{media}/{name}", s.auth(s.getEntry))
	mux.HandleFunc("PUT /ui/catalog/{media}/{name}/declaration", s.auth(s.putEntry))
	mux.HandleFunc("POST /ui/catalog/{media}/{name}/preview", s.auth(s.previewEntry))
	mux.HandleFunc("PUT /ui/catalog/{media}/{name}/folders/{folder}/projection", s.auth(s.putFolderProjection))
	mux.HandleFunc("POST /ui/catalog/{media}/{name}/seasons/{season}/backfill", s.auth(s.backfill))
	mux.HandleFunc("POST /ui/catalog/{media}/{name}/seasons/{season}/repairs/{review}/confirm", s.auth(s.confirmRepair))
	mux.HandleFunc("POST /ui/migrations/plan", s.auth(s.migrationPlan))
	mux.HandleFunc("POST /ui/migrations/apply", s.auth(s.migrationApply))
	return browserRequests(mux)
}

func (s *Service) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "live"})
}

func (s *Service) ready(w http.ResponseWriter, r *http.Request) {
	if s.App == nil {
		writeJSON(w, http.StatusServiceUnavailable, application.Readiness{Ready: false, Status: "not_ready", Reason: "application_unavailable"})
		return
	}
	value := s.App.Readiness(r.Context())
	status := http.StatusOK
	if !value.Ready {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, value)
}

func (s *Service) diagnostics(w http.ResponseWriter, r *http.Request) {
	if !s.requireApp(w) {
		return
	}
	writeJSON(w, http.StatusOK, s.App.Diagnostics(r.Context()))
}

func (s *Service) automationRun(w http.ResponseWriter, r *http.Request) {
	if !s.requireApp(w) {
		return
	}
	if e := decodeStrict(r, &EmptyRequest{}); e != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", e.Error())
		return
	}
	status := s.requestAutomation(r.Context())
	writeJSON(w, http.StatusAccepted, map[string]string{"status": status})
}

type reconcileMode uint8

const (
	reconcileNone  reconcileMode = 0
	reconcileLocal reconcileMode = 1
	reconcileRSS   reconcileMode = 2
	reconcileFull  reconcileMode = 4
)

func (s *Service) initLifecycle() {
	s.lifecycleOnce.Do(func() { s.reconcileCompleted = make(chan reconcileMode, 1) })
}

func (s *Service) requestAutomation(requestCtx context.Context) string {
	s.initLifecycle()
	return s.requestReconcile(requestCtx, reconcileFull)
}

func (s *Service) requestLocalReconcile(requestCtx context.Context) string {
	return s.requestReconcile(requestCtx, reconcileLocal)
}

func (s *Service) requestReconcile(requestCtx context.Context, mode reconcileMode) string {
	logger := s.Logger
	if logger == nil {
		logger = slog.Default()
	}
	s.reconcileMu.Lock()
	if s.reconcileRunning != reconcileNone {
		// Full work subsumes remote refresh. Local follow-ups remain necessary
		// when the namespace changes during a running observation. These are
		// coalesced intents in the one runner, not independent feature workers.
		pending := mode
		if s.reconcileRunning == reconcileFull && mode != reconcileLocal {
			pending = reconcileNone
		}
		if mode == reconcileRSS && s.reconcileRunning&reconcileRSS != 0 {
			pending = reconcileNone
		}
		if s.reconcilePending != reconcileFull {
			if pending == reconcileFull {
				s.reconcilePending = reconcileFull
			} else {
				s.reconcilePending |= pending
			}
		}
		s.reconcileMu.Unlock()
		logger.Debug("reconcile trigger coalesced", "mode", mode.String())
		return "coalesced"
	}
	s.reconcileRunning = mode
	runCtx := s.reconcileCtx
	if runCtx == nil {
		runCtx = context.WithoutCancel(requestCtx)
	}
	s.reconcileWG.Add(1)
	s.reconcileMu.Unlock()

	logger.Debug("reconcile trigger accepted", "mode", mode.String())
	go s.reconcileLoop(runCtx, mode)
	return "started"
}

func (s *Service) reconcileLoop(ctx context.Context, mode reconcileMode) {
	defer s.reconcileWG.Done()
	for {
		s.runReconcileCycle(ctx, mode)

		s.reconcileMu.Lock()
		if s.reconcilePending != reconcileNone && ctx.Err() == nil {
			mode = s.reconcilePending
			s.reconcilePending = reconcileNone
			s.reconcileRunning = mode
			s.reconcileMu.Unlock()
			continue
		}
		s.reconcileRunning = reconcileNone
		s.reconcilePending = reconcileNone
		s.reconcileMu.Unlock()
		return
	}
}

func (m reconcileMode) String() string {
	if m == reconcileFull {
		return "full"
	}
	if m == reconcileLocal {
		return "local"
	}
	if m == reconcileRSS {
		return "rss"
	}
	if m == reconcileLocal|reconcileRSS {
		return "local+rss"
	}
	return "none"
}

func (s *Service) runReconcileCycle(ctx context.Context, mode reconcileMode) {
	logger := s.Logger
	if logger == nil {
		logger = slog.Default()
	}
	defer s.notifyReconcileCompleted(mode)
	if mode&reconcileRSS != 0 {
		started := time.Now()
		err := s.App.RefreshRSS(ctx)
		if err != nil {
			logger.WarnContext(ctx, "RSS refresh failed or partial", "error", err, "duration_ms", time.Since(started).Milliseconds())
		} else {
			logger.InfoContext(ctx, "RSS refresh completed", "duration_ms", time.Since(started).Milliseconds())
		}
		if mode&reconcileLocal == 0 || ctx.Err() != nil {
			return
		}
	}
	started := time.Now()
	logger.Debug("reconcile cycle started", "mode", mode.String())
	options := application.CycleOptions{Acquire: mode == reconcileFull, Reconcile: true}
	if mode&reconcileLocal != 0 {
		// Local reconciliation intentionally avoids a broad RSS refresh, but a
		// filesystem-proven internal episode gap is local truth and should be
		// repaired immediately. RepairGaps enables only targeted historical
		// searches for those exact holes; it does not turn the one-minute local
		// monitor into an RSS poller.
		options.LocalOnly = true
		options.Acquire = true
		options.RepairGaps = true
	}
	result, err := s.App.Cycle(ctx, options)
	logAutomationDetails(ctx, logger, result, err)
	status := issueStatus(result.Issues)
	submitted, recovered, skipped, failed := acquisitionCounts(result.Acquisitions)
	level := slog.LevelDebug
	if mode == reconcileFull || submitted+recovered+failed+result.OrganizedFiles+len(result.Issues) > 0 || err != nil {
		level = slog.LevelInfo
	}
	logger.Log(ctx, level, "reconcile cycle completed",
		"mode", mode.String(),
		"status", status,
		"duration_ms", time.Since(started).Milliseconds(),
		"selected", len(result.SelectedReleases),
		"candidate_skipped", len(result.SkippedReleases),
		"candidate_duplicates", len(result.DuplicateReleases),
		"acquisitions", len(result.Acquisitions),
		"submitted", submitted,
		"recovered", recovered,
		"acquisition_skipped", skipped,
		"reconciled", reconciledCount(result.CompletedTasks),
		"organized_files", result.OrganizedFiles)
}

// notifyReconcileCompleted coalesces completion kinds, not just wakeups. A local
// completion must not hide an RSS completion and leave the old remote deadline
// armed. The serialized runner is the sole sender; the scheduler only receives.
func (s *Service) notifyReconcileCompleted(mode reconcileMode) {
	s.initLifecycle()
	select {
	case s.reconcileCompleted <- mode:
		return
	default:
	}
	select {
	case previous := <-s.reconcileCompleted:
		mode |= previous
	default:
		// The scheduler already consumed the previous completion.
	}
	s.reconcileCompleted <- mode
}

func reconciledCount(values []automation.ReconcileResult) int {
	count := 0
	for _, value := range values {
		if value.Decision == "reconciled" || value.Decision == "local-reconciled" {
			count++
		}
	}
	return count
}

func logAutomationDetails(ctx context.Context, logger *slog.Logger, result application.CycleResult, cycleErr error) {
	for sourceID, count := range result.SourceReleases {
		logger.Debug("content source observed", "source", sourceID, "releases", count)
	}
	for _, title := range result.DiscoveredEntries {
		logger.Debug("unmanaged work discovered", "title", title)
	}
	for _, value := range result.SelectedReleases {
		logger.Info("release selected for acquisition",
			"entry", value.EntryKey,
			"source", value.SourceID,
			"media_name", value.Release.MediaName,
			"season", value.Episode.Season,
			"episode_start", value.Episode.EpisodeStart,
			"episode_end", value.Episode.EpisodeEnd,
			"acquisition", value.Acquisition.ID)
	}
	for _, value := range result.SkippedReleases {
		level := slog.LevelWarn
		if value.Decision == candidate.DecisionSuperseded || value.Decision == candidate.DecisionFiltered {
			level = slog.LevelDebug
		}
		logger.Log(ctx, level, "release rejected before acquisition",
			"entry", value.EntryKey,
			"source", value.SourceID,
			"media_name", value.Release.MediaName,
			"decision", value.Decision,
			"reason", value.Reason)
	}
	for _, value := range result.DuplicateReleases {
		logger.Debug("release duplicate suppressed",
			"entry", value.EntryKey,
			"source", value.SourceID,
			"media_name", value.Release.MediaName,
			"reason", value.Reason)
	}
	logAcquisitionResults(ctx, logger, result.Acquisitions)
	for _, value := range result.CompletedTasks {
		switch value.Decision {
		case "pending":
			logger.Debug("reconciliation pending", "client", value.Client, "task", value.TaskID, "entry", value.EntryKey, "reason", value.Reason)
		case "ignored":
			logger.Debug("reconciliation ignored unmanaged task", "client", value.Client, "task", value.TaskID, "reason", value.Reason)
		}
	}
	logOperationIssues(ctx, logger, result.Issues)
}

func logAcquisitionResults(ctx context.Context, logger *slog.Logger, values []automation.AcquireResult) {
	for _, value := range values {
		if value.Decision == automation.AcquireFailed || (value.Decision == automation.AcquireSubmitted && value.TaskID == "" && value.Reason != "") {
			continue
		}
		level := slog.LevelInfo
		message := "acquisition completed"
		if value.Decision == automation.AcquireSkipped {
			level = slog.LevelDebug
			message = "acquisition skipped"
		}
		logger.Log(ctx, level, message,
			"entry", value.EntryKey,
			"client", value.Client,
			"task", value.TaskID,
			"acquisition", value.AcquisitionID,
			"decision", value.Decision,
			"reason", value.Reason,
			"error", value.Error)
	}
}

func logOperationIssues(ctx context.Context, logger *slog.Logger, issues []application.OperationIssue) {
	for _, issue := range issues {
		level := slog.LevelWarn
		if issue.Severity == "error" {
			level = slog.LevelError
		}
		logger.Log(ctx, level, issue.Message,
			"stage", issue.Stage,
			"code", issue.Code,
			"entry", issue.Entry,
			"source", issue.Source,
			"client", issue.Client,
			"task", issue.Task,
			"acquisition", issue.Acquisition,
			"hint", issue.Hint)
	}
}

func acquisitionCounts(values []automation.AcquireResult) (submitted, recovered, skipped, failed int) {
	for _, value := range values {
		switch value.Decision {
		case automation.AcquireSubmitted:
			submitted++
		case automation.AcquireRecovered:
			recovered++
		case automation.AcquireSkipped:
			skipped++
		case automation.AcquireFailed:
			failed++
		}
	}
	return
}

func (s *Service) Serve(ctx context.Context, listen string, interval time.Duration) error {
	if s.App == nil {
		return errors.New("application is required")
	}
	if ctx.Err() != nil {
		return nil
	}
	logger := s.Logger
	if logger == nil {
		logger = slog.Default()
	}
	created, err := s.App.EnsureSourceDeclarations(ctx)
	if err != nil {
		return fmt.Errorf("ensure source declarations before serving: %w", err)
	}
	if len(created) > 0 {
		logger.Info("source declarations adopted before serving", "created", len(created))
	}
	httpServer := &http.Server{Addr: listen, Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 15 * time.Second}
	runCtx, stopScheduler := context.WithCancel(ctx)
	defer stopScheduler()
	s.reconcileMu.Lock()
	s.reconcileCtx = runCtx
	s.reconcileMu.Unlock()
	errCh := make(chan error, 1)
	go func() {
		err := httpServer.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errCh <- err
	}()
	logger.Info("daemon started", "listen", listen, "remote_interval", interval.String())
	schedulerDone := make(chan struct{})
	go func() {
		defer close(schedulerDone)
		s.scheduler(runCtx, interval)
	}()
	waitScheduler := func() {
		<-schedulerDone
	}
	select {
	case <-ctx.Done():
		logger.Info("shutdown requested")
		stopScheduler()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		shutdownErr := httpServer.Shutdown(shutdownCtx)
		serveErr := <-errCh
		waitScheduler()
		s.reconcileWG.Wait()
		logger.Info("daemon stopped")
		return errors.Join(shutdownErr, serveErr)
	case err := <-errCh:
		stopScheduler()
		waitScheduler()
		s.reconcileWG.Wait()
		return err
	}
}

func (s *Service) scheduler(ctx context.Context, interval time.Duration) {
	logger := s.Logger
	if logger == nil {
		logger = slog.Default()
	}
	s.initLifecycle()

	// Local convergence is deliberately timer-driven. Linux filesystem state is
	// the authority; no event stream is required for correctness or remembered
	// as lifecycle state. A short periodic observation repairs internal gaps,
	// publication drift and external namespace edits. Remote RSS observation has
	// its own slower deadline because it is discovery, not filesystem truth.
	localInterval := s.LocalInterval
	if localInterval <= 0 {
		localInterval = time.Minute
	}

	now := time.Now()
	nextLocal := now.Add(localInterval)
	var nextFull time.Time
	if interval > 0 {
		nextFull = now // startup full observation
	} else {
		nextLocal = now // RSS disabled: still prove local filesystem truth at startup
	}

	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()

	resetTimer := func() {
		next := nextLocal
		if !nextFull.IsZero() && nextFull.Before(next) {
			next = nextFull
		}
		d := time.Until(next)
		if d < 0 {
			d = 0
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(d)
	}

	resetTimer()
	for {
		select {
		case <-ctx.Done():
			return
		case mode := <-s.reconcileCompleted:
			now = time.Now()
			if mode&(reconcileFull|reconcileRSS) != 0 && interval > 0 {
				nextFull = now.Add(interval)
			}
			// Only local/full reconciliation has re-proved filesystem truth.
			if mode&(reconcileFull|reconcileLocal) != 0 {
				nextLocal = now.Add(localInterval)
			}
			resetTimer()
		case <-timer.C:
			now = time.Now()
			if !nextFull.IsZero() && !now.Before(nextFull) {
				nextFull = now.Add(interval)
				s.requestAutomation(ctx)
				nextLocal = now.Add(localInterval)
			} else if !now.Before(nextLocal) {
				s.requestLocalReconcile(ctx)
				nextLocal = now.Add(localInterval)
			}
			resetTimer()
		}
	}
}

func serveSPA(w http.ResponseWriter, r *http.Request, assets fs.FS) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		http.NotFound(w, r)
		return
	}
	if r.URL.Path != "/" && r.URL.Path != "/login" && !strings.HasPrefix(r.URL.Path, "/catalog/") && r.URL.Path != "/catalog" && r.URL.Path != "/sources" && r.URL.Path != "/settings" && r.URL.Path != "/help" {
		http.NotFound(w, r)
		return
	}
	data, err := fs.ReadFile(assets, "index.html")
	if err != nil {
		http.Error(w, "web UI unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (s *Service) requireApp(w http.ResponseWriter) bool {
	if s.App == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "unavailable", "application is required")
		return false
	}
	return true
}
func (s *Service) getConfig(w http.ResponseWriter, r *http.Request) {
	if !s.requireApp(w) {
		return
	}
	v, e := s.App.Config(r.Context())
	respond(w, v, e)
}
func (s *Service) validateConfig(w http.ResponseWriter, r *http.Request) {
	if !s.requireApp(w) {
		return
	}
	if e := decodeStrict(r, &EmptyRequest{}); e != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", e.Error())
		return
	}
	respond(w, map[string]bool{"valid": true}, s.App.ValidateConfig(r.Context()))
}
func (s *Service) putOrganizer(w http.ResponseWriter, r *http.Request) {
	if !s.requireApp(w) {
		return
	}
	var v organizer.Config
	if e := decodeStrict(r, &v); e != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", e.Error())
		return
	}
	out, e := s.App.PutOrganizer(r.Context(), v)
	respond(w, out.Organizer, e)
}
func (s *Service) clientStatuses(w http.ResponseWriter, r *http.Request) {
	if !s.requireApp(w) {
		return
	}
	respond(w, s.App.ClientStatuses(r.Context()), nil)
}
func (s *Service) putClient(w http.ResponseWriter, r *http.Request) {
	if !s.requireApp(w) {
		return
	}
	var v struct {
		ID           string                    `json:"id"`
		Type         string                    `json:"type"`
		URL          string                    `json:"url"`
		Username     string                    `json:"username,omitempty"`
		Password     *string                   `json:"password,omitempty"`
		PathMappings []configstore.PathMapping `json:"path_mappings,omitempty"`
		Enabled      bool                      `json:"enabled"`
	}
	if e := decodeStrict(r, &v); e != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", e.Error())
		return
	}
	out, e := s.App.SaveClient(r.Context(), r.PathValue("id"), configstore.Client{ID: v.ID, Type: v.Type, URL: v.URL, Username: v.Username, PathMappings: v.PathMappings, Enabled: v.Enabled}, v.Password)
	respond(w, out, e)
}
func (s *Service) deleteClient(w http.ResponseWriter, r *http.Request) {
	if !s.requireApp(w) {
		return
	}
	e := s.App.DeleteClient(r.Context(), r.PathValue("id"))
	respond(w, map[string]bool{"deleted": e == nil}, e)
}
func (s *Service) putSource(w http.ResponseWriter, r *http.Request) {
	if !s.requireApp(w) {
		return
	}
	var v configstore.ContentSource
	if e := decodeStrict(r, &v); e != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", e.Error())
		return
	}
	out, e := s.App.PutSource(r.Context(), r.PathValue("id"), v)
	respond(w, out, e)
}
func (s *Service) deleteSource(w http.ResponseWriter, r *http.Request) {
	if !s.requireApp(w) {
		return
	}
	e := s.App.DeleteSource(r.Context(), r.PathValue("id"))
	respond(w, map[string]bool{"deleted": e == nil}, e)
}
func (s *Service) searchDiscovery(w http.ResponseWriter, r *http.Request) {
	if !s.requireApp(w) {
		return
	}
	sourceID := strings.TrimSpace(r.URL.Query().Get("source"))
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if sourceID == "" || query == "" {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "source and q are required")
		return
	}
	v, e := s.App.SearchDiscovery(r.Context(), sourceID, query, 30)
	respond(w, v, e)
}
func (s *Service) acquireDiscovery(w http.ResponseWriter, r *http.Request) {
	if !s.requireApp(w) {
		return
	}
	var req application.DiscoveryAcquireRequest
	if e := decodeStrict(r, &req); e != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", e.Error())
		return
	}
	if strings.TrimSpace(req.EntryKey) == "" || strings.TrimSpace(req.SourceID) == "" || strings.TrimSpace(req.Query) == "" {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "entry_key, source_id and query are required")
		return
	}
	v, e := s.App.AcquireDiscovery(r.Context(), req)
	logger := s.Logger
	if logger == nil {
		logger = slog.Default()
	}
	if e != nil {
		logger.Error("interactive acquisition failed", "entry", req.EntryKey, "source", req.SourceID, "error", e)
	} else {
		logger.Info("interactive release selected",
			"entry", v.Candidate.EntryKey,
			"source", v.Candidate.SourceID,
			"media_name", v.Candidate.Release.MediaName,
			"season", v.Candidate.Episode.Season,
			"episode_start", v.Candidate.Episode.EpisodeStart,
			"episode_end", v.Candidate.Episode.EpisodeEnd)
		logAcquisitionResults(r.Context(), logger, []automation.AcquireResult{v.Acquisition})
	}
	respond(w, v, e)
}

func entryKeyFromRequest(r *http.Request) (string, error) {
	return catalog.Key(r.PathValue("media"), r.PathValue("name"))
}
func seasonFromRequest(r *http.Request) (int, error) {
	season, err := strconv.Atoi(r.PathValue("season"))
	if err != nil || season < 0 || season > 99 {
		return 0, errors.New("season must be 0-99")
	}
	return season, nil
}

func (s *Service) listEntries(w http.ResponseWriter, r *http.Request) {
	if !s.requireApp(w) {
		return
	}
	v, e := s.App.SearchEntries(r.Context(), r.URL.Query().Get("q"), 30)
	respond(w, v, e)
}
func (s *Service) getEntry(w http.ResponseWriter, r *http.Request) {
	if !s.requireApp(w) {
		return
	}
	key, err := entryKeyFromRequest(r)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	v, e := s.App.GetEntry(r.Context(), key)
	respond(w, v, e)
}
func (s *Service) createEntry(w http.ResponseWriter, r *http.Request) {
	if !s.requireApp(w) {
		return
	}
	var req application.CreateEntryRequest
	if e := decodeStrict(r, &req); e != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", e.Error())
		return
	}
	season := 1
	if req.Season != nil {
		season = *req.Season
	}
	v, e := s.App.CreateEntry(r.Context(), r.PathValue("media"), season, req.Declaration)
	respond(w, v, e)
}

func (s *Service) rssDiscoveries(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.requireApp(w) {
		return
	}
	v, e := s.App.RSSDiscoveries(r.Context(), r.URL.Query().Get("source"), 0)
	s.reconcileMu.Lock()
	v.Refreshing = (s.reconcileRunning|s.reconcilePending)&(reconcileFull|reconcileRSS) != 0
	s.reconcileMu.Unlock()
	respond(w, v, e)
}

// refreshRSS acknowledges background observation before any network work finishes.
// It shares the reconciliation runner but cannot acquire or publish media.
func (s *Service) refreshRSS(w http.ResponseWriter, r *http.Request) {
	if !s.requireApp(w) {
		return
	}
	if err := decodeStrict(r, &EmptyRequest{}); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	s.initLifecycle()
	status := s.requestReconcile(r.Context(), reconcileRSS)
	writeJSON(w, http.StatusAccepted, map[string]string{"status": status})
}

func (s *Service) putEntry(w http.ResponseWriter, r *http.Request) {
	if !s.requireApp(w) {
		return
	}
	key, err := entryKeyFromRequest(r)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	var req application.EntryDeclaration
	if e := decodeStrict(r, &req); e != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", e.Error())
		return
	}
	v, e := s.App.PutEntry(r.Context(), key, req)
	respond(w, v, e)
}
func (s *Service) previewEntry(w http.ResponseWriter, r *http.Request) {
	if !s.requireApp(w) {
		return
	}
	key, err := entryKeyFromRequest(r)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	var req application.PreviewRequest
	if e := decodeStrict(r, &req); e != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", e.Error())
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "name is required")
		return
	}
	v, e := s.App.PreviewEntry(r.Context(), key, req)
	respond(w, v, e)
}
func (s *Service) putFolderProjection(w http.ResponseWriter, r *http.Request) {
	if !s.requireApp(w) {
		return
	}
	key, err := entryKeyFromRequest(r)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	folder := r.PathValue("folder")
	if strings.TrimSpace(folder) == "" {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "folder is required")
		return
	}
	var req catalog.FolderProjection
	if e := decodeStrict(r, &req); e != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", e.Error())
		return
	}
	v, e := s.App.PutFolderProjection(r.Context(), key, folder, req)
	respond(w, v, e)
}
func (s *Service) backfill(w http.ResponseWriter, r *http.Request) {
	if !s.requireApp(w) {
		return
	}
	key, err := entryKeyFromRequest(r)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	season, err := seasonFromRequest(r)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	var req BackfillRequest
	if e := decodeStrict(r, &req); e != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", e.Error())
		return
	}
	if req.From <= 0 || req.Through < req.From {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "from must be positive and through must be >= from")
		return
	}
	logger := s.Logger
	if logger == nil {
		logger = slog.Default()
	}
	v, e := s.App.Backfill(r.Context(), key, season, req.From, req.Through)
	logOperationIssues(r.Context(), logger, v.Issues)
	if e == nil {
		logger.Info("manual range completion completed", "entry", key, "season", season, "from", req.From, "through", req.Through, "selected", len(v.Selected), "acquisitions", len(v.Acquisitions), "status", issueStatus(v.Issues))
		logAcquisitionResults(r.Context(), logger, v.Acquisitions)
	}
	respond(w, v, e)
}

func (s *Service) confirmRepair(w http.ResponseWriter, r *http.Request) {
	if !s.requireApp(w) {
		return
	}
	key, err := entryKeyFromRequest(r)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	season, err := seasonFromRequest(r)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	review := strings.TrimSpace(r.PathValue("review"))
	if review == "" {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "review is required")
		return
	}
	if e := decodeStrict(r, &EmptyRequest{}); e != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", e.Error())
		return
	}
	logger := s.Logger
	if logger == nil {
		logger = slog.Default()
	}
	v, e := s.App.ConfirmRepair(r.Context(), key, season, review)
	logOperationIssues(r.Context(), logger, v.Issues)
	if e == nil {
		logger.Info("repair confirmation completed", "entry", key, "season", season, "review", review, "selected", len(v.Selected), "acquisitions", len(v.Acquisitions), "status", issueStatus(v.Issues))
		logAcquisitionResults(r.Context(), logger, v.Acquisitions)
	}
	respond(w, v, e)
}

func issueStatus(issues []application.OperationIssue) string {
	status := "ok"
	for _, issue := range issues {
		if issue.Severity == "error" {
			return "error"
		}
		status = "warning"
	}
	return status
}
func (s *Service) migrationPlan(w http.ResponseWriter, r *http.Request) {
	if !s.requireApp(w) {
		return
	}
	if e := decodeStrict(r, &EmptyRequest{}); e != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", e.Error())
		return
	}
	v, e := s.App.MigrationPlan(r.Context())
	respond(w, v, e)
}
func (s *Service) migrationApply(w http.ResponseWriter, r *http.Request) {
	if !s.requireApp(w) {
		return
	}
	var req MigrationApplyRequest
	if e := decodeStrict(r, &req); e != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", e.Error())
		return
	}
	v, e := s.App.MigrationApplyUnit(r.Context(), application.MigrationUnitInput{
		Key: req.Key, EntryKey: req.EntryKey, Title: req.Title, Season: req.Season, Year: req.Year, MediaType: req.MediaType, Filters: req.Filters, Movie: req.Movie, Tasks: req.Tasks,
	})
	respond(w, v, e)
}

func decodeStrict(r *http.Request, target any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values are not allowed")
		}
		return fmt.Errorf("invalid JSON: %w", err)
	}
	return nil
}
func respond(w http.ResponseWriter, v any, err error) {
	if err == nil {
		writeJSON(w, http.StatusOK, v)
		return
	}
	var me *application.ManagementError
	if errors.As(err, &me) {
		switch me.Kind {
		case application.ErrorNotFound:
			writeAPIError(w, http.StatusNotFound, string(me.Kind), me.Error())
		case application.ErrorInvalid:
			writeAPIError(w, http.StatusBadRequest, string(me.Kind), me.Error())
		case application.ErrorConflict:
			writeAPIError(w, http.StatusConflict, string(me.Kind), me.Error())
		case application.ErrorUnknown:
			writeAPIError(w, http.StatusServiceUnavailable, string(me.Kind), me.Error())
		default:
			writeAPIError(w, http.StatusInternalServerError, string(me.Kind), me.Error())
		}
		return
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		writeAPIError(w, http.StatusServiceUnavailable, "operation_unavailable", err.Error())
		return
	}
	writeAPIError(w, http.StatusInternalServerError, "internal", err.Error())
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil && sLogFallback != nil {
		sLogFallback.Error("encode HTTP response", "error", err)
	}
}
func writeAPIError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, APIError{Error: ErrorBody{Code: code, Message: strings.TrimSpace(message)}})
}

var sLogFallback = slog.Default()
