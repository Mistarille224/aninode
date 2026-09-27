package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"aninode/internal/application"
)

// BackfillJob keeps a manual range search independent of the browser request.
// Only one job per entry and season may run at a time.
type BackfillJob struct {
	Status    string                            `json:"status"`
	From      int                               `json:"from"`
	Through   int                               `json:"through"`
	StartedAt time.Time                         `json:"started_at"`
	Result    *application.BackfillActionResult `json:"result,omitempty"`
	Error     string                            `json:"error,omitempty"`
}

func backfillJobKey(entry string, season int) string {
	return fmt.Sprintf("%s\x00%d", entry, season)
}

func (s *Service) startBackfill(w http.ResponseWriter, r *http.Request, entry string, season int, req BackfillRequest, logger *slog.Logger) {
	key := backfillJobKey(entry, season)
	s.backfillMu.Lock()
	if running, ok := s.backfillJobs[key]; ok && running.Status == "running" {
		s.backfillMu.Unlock()
		if running.From != req.From || running.Through != req.Through {
			writeAPIError(w, http.StatusConflict, "backfill_running", "a different completion range is already running for this season")
			return
		}
		writeJSON(w, http.StatusAccepted, running)
		return
	}
	if s.backfillJobs == nil {
		s.backfillJobs = make(map[string]BackfillJob)
	}
	job := BackfillJob{Status: "running", From: req.From, Through: req.Through, StartedAt: time.Now().UTC()}
	s.backfillJobs[key] = job
	// The service lifecycle cancels background work on shutdown. Without an
	// active lifecycle, detach only from this request's cancellation.
	runCtx := s.reconcileCtx
	if runCtx == nil {
		runCtx = context.WithoutCancel(r.Context())
	}
	s.backfillMu.Unlock()

	logger.Info("manual range completion started", "entry", entry, "season", season, "from", req.From, "through", req.Through)
	go func() {
		ctx, cancel := context.WithTimeout(runCtx, 30*time.Minute)
		defer cancel()
		result, err := s.App.Backfill(ctx, entry, season, req.From, req.Through)
		logOperationIssues(ctx, logger, result.Issues)
		logAcquisitionResults(ctx, logger, result.Acquisitions)
		finished := job
		finished.Result = &result
		if err != nil {
			finished.Status = "failed"
			finished.Error = err.Error()
			logger.Error("manual range completion failed", "entry", entry, "season", season, "from", req.From, "through", req.Through, "missing", result.State.Missing, "error", err)
		} else {
			finished.Status = "completed"
			logger.Info("manual range completion completed", "entry", entry, "season", season, "from", req.From, "through", req.Through, "missing", result.State.Missing, "selected", len(result.Selected), "acquisitions", len(result.Acquisitions), "status", issueStatus(result.Issues))
		}
		s.backfillMu.Lock()
		s.backfillJobs[key] = finished
		s.backfillMu.Unlock()
	}()
	writeJSON(w, http.StatusAccepted, job)
}

func (s *Service) backfillStatus(w http.ResponseWriter, r *http.Request) {
	if !s.requireApp(w) {
		return
	}
	entry, err := entryKeyFromRequest(r)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	season, err := seasonFromRequest(r)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	s.backfillMu.Lock()
	job, ok := s.backfillJobs[backfillJobKey(entry, season)]
	s.backfillMu.Unlock()
	if !ok {
		writeAPIError(w, http.StatusNotFound, "not_found", "no range completion has been started for this season")
		return
	}
	writeJSON(w, http.StatusOK, job)
}
