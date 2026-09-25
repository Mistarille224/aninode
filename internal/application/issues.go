package application

import (
	"fmt"
	"strings"

	"aninode/internal/automation"
)

func cycleIssues(result CycleResult, operationErr error) []OperationIssue {
	out := append([]OperationIssue(nil), result.Issues...)
	for _, value := range result.Acquisitions {
		if value.Decision == automation.AcquireFailed {
			out = appendIssue(out, OperationIssue{Severity: "error", Stage: "acquisition", Code: "acquisition_failed", Message: value.Error, Entry: value.EntryKey, Client: value.Client, Task: value.TaskID, Acquisition: value.AcquisitionID, Hint: "check the downloader response and path mapping, then retry"})
		} else if value.Decision == automation.AcquireSubmitted && value.TaskID == "" && value.Reason != "" {
			out = appendIssue(out, OperationIssue{Severity: "warning", Stage: "acquisition", Code: "task_observation_pending", Message: value.Reason, Entry: value.EntryKey, Client: value.Client, Acquisition: value.AcquisitionID, Hint: "the add was accepted; refresh after the downloader exposes the task"})
		}
	}
	for _, value := range result.CompletedTasks {
		switch value.Decision {
		case "conflict":
			out = appendIssue(out, OperationIssue{Severity: "warning", Stage: "publication", Code: "publication_conflict", Message: value.Reason, Entry: value.EntryKey, Client: value.Client, Task: value.TaskID, Hint: "inspect the named source and destination; aninode will not overwrite a different filesystem object"})
		case "failed":
			out = appendIssue(out, OperationIssue{Severity: "error", Stage: "publication", Code: "publication_failed", Message: value.Error, Entry: value.EntryKey, Client: value.Client, Task: value.TaskID})
		}
	}
	if operationErr != nil {
		out = appendIssue(out, OperationIssue{Severity: "error", Stage: "cycle", Code: "cycle_failed", Message: operationErr.Error()})
	}
	return out
}

func backfillIssues(result BackfillActionResult, operationErr error) []OperationIssue {
	cycle := CycleResult{Issues: result.Issues, Acquisitions: result.Acquisitions}
	out := cycleIssues(cycle, nil)
	if operationErr != nil {
		out = appendIssue(out, OperationIssue{Severity: "error", Stage: "backfill", Code: "backfill_failed", Message: operationErr.Error(), Entry: result.State.EntryKey})
	}
	return out
}

func appendMessageIssues(values []OperationIssue, severity, stage, code string, messages []string) []OperationIssue {
	for _, message := range messages {
		values = appendIssue(values, OperationIssue{Severity: severity, Stage: stage, Code: code, Message: message})
	}
	return values
}

func appendIssue(values []OperationIssue, issue OperationIssue) []OperationIssue {
	issue.Message = strings.TrimSpace(issue.Message)
	if issue.Message == "" {
		issue.Message = fmt.Sprintf("%s failed without a diagnostic message", issue.Stage)
	}
	for _, existing := range values {
		if existing.Severity == issue.Severity && existing.Stage == issue.Stage && existing.Code == issue.Code && existing.Message == issue.Message && existing.Entry == issue.Entry && existing.Client == issue.Client && existing.Task == issue.Task {
			return values
		}
		if existing.Severity == issue.Severity && existing.Message != "" && (strings.Contains(issue.Message, existing.Message) || strings.Contains(existing.Message, issue.Message)) {
			return values
		}
	}
	return append(values, issue)
}

func operationIssueStatus(issues []OperationIssue) string {
	status := "ok"
	for _, issue := range issues {
		if issue.Severity == "error" {
			return "error"
		}
		status = "warning"
	}
	return status
}
