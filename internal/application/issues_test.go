package application

import (
	"errors"
	"testing"

	"aninode/internal/automation"
)

func TestCycleIssuesPreserveActionableContextAndDeduplicateJoinedErrors(t *testing.T) {
	result := CycleResult{
		Acquisitions: []automation.AcquireResult{{Decision: automation.AcquireFailed, EntryKey: "series/abcabc", Client: "qbit", AcquisitionID: "btih-1", Error: "downloader rejected Add"}},
	}
	issues := cycleIssues(result, nil)
	if len(issues) != 1 {
		t.Fatalf("issues=%+v", issues)
	}
	got := issues[0]
	if got.Stage != "acquisition" || got.Code != "acquisition_failed" || got.Entry != "series/abcabc" || got.Client != "qbit" || got.Hint == "" {
		t.Fatalf("issue lost actionable context: %+v", got)
	}
}

func TestBackfillIssuesExposeReturnedError(t *testing.T) {
	result := BackfillActionResult{}
	issues := backfillIssues(result, errors.New("mikan returned HTTP 504"))
	if len(issues) != 1 || issues[0].Stage != "backfill" || issues[0].Message != "mikan returned HTTP 504" {
		t.Fatalf("issues=%+v", issues)
	}
}

func TestPublicationConflictIsWarningNotCycleError(t *testing.T) {
	result := CycleResult{CompletedTasks: []automation.ReconcileResult{{
		Decision: "conflict", EntryKey: "series/abcabc145", Reason: "destination differs",
	}}}
	issues := cycleIssues(result, nil)
	if len(issues) != 1 {
		t.Fatalf("issues=%+v", issues)
	}
	if issues[0].Severity != "warning" || issues[0].Code != "publication_conflict" {
		t.Fatalf("publication conflict severity changed: %+v", issues[0])
	}
	if got := operationIssueStatus(issues); got != "warning" {
		t.Fatalf("status=%q issues=%+v", got, issues)
	}
}
