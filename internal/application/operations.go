package application

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"aninode/internal/configstore"
	"aninode/internal/filesystem"
)

type Readiness struct {
	Ready  bool   `json:"ready"`
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

type OperationSummary struct {
	Kind           string           `json:"kind"`
	Status         string           `json:"status"`
	StartedAt      time.Time        `json:"started_at"`
	FinishedAt     time.Time        `json:"finished_at"`
	DurationMS     int64            `json:"duration_ms"`
	Selected       int              `json:"selected,omitempty"`
	Acquisitions   int              `json:"acquisitions,omitempty"`
	Reconciled     int              `json:"reconciled,omitempty"`
	OrganizedFiles int              `json:"organized_files,omitempty"`
	Records        int              `json:"records,omitempty"`
	Applied        bool             `json:"applied,omitempty"`
	Issues         []OperationIssue `json:"issues,omitempty"`
}

// OperationIssue is the shared diagnostics contract consumed by API clients,
// the embedded WebUI, and structured logs. Message is user-readable; the other
// fields retain enough stable context to locate the failing operation.
type OperationIssue struct {
	Severity    string `json:"severity"`
	Stage       string `json:"stage"`
	Code        string `json:"code"`
	Message     string `json:"message"`
	Entry       string `json:"entry,omitempty"`
	Source      string `json:"source,omitempty"`
	Client      string `json:"client,omitempty"`
	Task        string `json:"task,omitempty"`
	Acquisition string `json:"acquisition,omitempty"`
	Hint        string `json:"hint,omitempty"`
}

type Diagnostics struct {
	StartedAt     time.Time         `json:"started_at"`
	RuntimeConfig string            `json:"runtime_config"`
	LastCycle     *OperationSummary `json:"last_cycle,omitempty"`
	LastMigration *OperationSummary `json:"last_migration,omitempty"`
	LastBackfill  *OperationSummary `json:"last_backfill,omitempty"`
}

type operationsState struct {
	mu            sync.RWMutex
	startedAt     time.Time
	lastCycle     *OperationSummary
	lastMigration *OperationSummary
	lastBackfill  *OperationSummary
}

func (a *App) Readiness(ctx context.Context) Readiness {
	if a == nil {
		return Readiness{Ready: false, Status: "not_ready", Reason: "application_unavailable"}
	}
	select {
	case <-ctx.Done():
		return Readiness{Ready: false, Status: "not_ready", Reason: "request_canceled"}
	default:
	}
	bundle, err := configstore.Load(a.configRoot)
	if err != nil {
		return Readiness{Ready: false, Status: "not_ready", Reason: "invalid_configuration"}
	}
	sourceMeta, sourceErr := filesystem.ObserveDirectory(bundle.Organizer.Source)
	targetMeta, targetErr := filesystem.ObserveDirectory(bundle.Organizer.Target)
	if len(bundle.Entries) > 0 && (errors.Is(sourceErr, os.ErrNotExist) || errors.Is(targetErr, os.ErrNotExist)) {
		return Readiness{Ready: false, Status: "not_ready", Reason: "filesystem_unavailable"}
	}
	if sourceErr != nil && !errors.Is(sourceErr, os.ErrNotExist) {
		return Readiness{Ready: false, Status: "not_ready", Reason: "filesystem_unavailable"}
	}
	if targetErr != nil && !errors.Is(targetErr, os.ErrNotExist) {
		return Readiness{Ready: false, Status: "not_ready", Reason: "filesystem_unavailable"}
	}
	if sourceErr == nil && targetErr == nil && sourceMeta.ID.Device != targetMeta.ID.Device {
		return Readiness{Ready: false, Status: "not_ready", Reason: "hardlink_domain_mismatch"}
	}
	a.runtimeMu.RLock()
	reloadFailed := a.reloadFailed
	a.runtimeMu.RUnlock()
	if reloadFailed {
		return Readiness{Ready: false, Status: "not_ready", Reason: "runtime_reload_failed"}
	}
	return Readiness{Ready: true, Status: "ready"}
}

func (a *App) Diagnostics(context.Context) Diagnostics {
	if a == nil {
		return Diagnostics{}
	}
	a.ops.mu.RLock()
	defer a.ops.mu.RUnlock()
	a.runtimeMu.RLock()
	failed := a.reloadFailed
	a.runtimeMu.RUnlock()
	runtimeStatus := "ready"
	if failed {
		runtimeStatus = "reload_failed"
	}
	return Diagnostics{StartedAt: a.ops.startedAt, RuntimeConfig: runtimeStatus, LastCycle: cloneSummary(a.ops.lastCycle), LastMigration: cloneSummary(a.ops.lastMigration), LastBackfill: cloneSummary(a.ops.lastBackfill)}
}

func cloneSummary(in *OperationSummary) *OperationSummary {
	if in == nil {
		return nil
	}
	out := *in
	out.Issues = append([]OperationIssue(nil), in.Issues...)
	return &out
}

func (a *App) recordCycle(start time.Time, result CycleResult) {
	status := operationIssueStatus(result.Issues)
	s := &OperationSummary{Kind: "cycle", Status: status, StartedAt: start, FinishedAt: a.clock(), Selected: len(result.SelectedReleases), Acquisitions: len(result.Acquisitions), Reconciled: len(result.CompletedTasks), OrganizedFiles: result.OrganizedFiles, Issues: append([]OperationIssue(nil), result.Issues...)}
	s.DurationMS = max64(0, s.FinishedAt.Sub(start).Milliseconds())
	a.ops.mu.Lock()
	a.ops.lastCycle = s
	a.ops.mu.Unlock()
}

func (a *App) recordMigration(start time.Time, result MigrationResult, apply bool, err error) {
	s := &OperationSummary{Kind: "migration", Status: "ok", StartedAt: start, FinishedAt: a.clock(), Records: len(result.Plans) + len(result.Applied), Applied: apply}
	if err != nil {
		s.Status = "error"
		s.Issues = []OperationIssue{{Severity: "error", Stage: "migration", Code: "migration_failed", Message: err.Error(), Hint: "review the current migration plan and resolve the named conflict before retrying"}}
	}
	s.DurationMS = max64(0, s.FinishedAt.Sub(start).Milliseconds())
	a.ops.mu.Lock()
	a.ops.lastMigration = s
	a.ops.mu.Unlock()
}

func (a *App) recordBackfill(start time.Time, result BackfillActionResult) {
	status := operationIssueStatus(result.Issues)
	s := &OperationSummary{Kind: "backfill", Status: status, StartedAt: start, FinishedAt: a.clock(), Selected: len(result.Selected), Acquisitions: len(result.Acquisitions), Issues: append([]OperationIssue(nil), result.Issues...)}
	s.DurationMS = max64(0, s.FinishedAt.Sub(start).Milliseconds())
	a.ops.mu.Lock()
	a.ops.lastBackfill = s
	a.ops.mu.Unlock()
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func (r Readiness) Error() error {
	if r.Ready {
		return nil
	}
	return fmt.Errorf("%s", r.Reason)
}
