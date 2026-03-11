// Package agent defines the CodingAgent interface that abstracts the coding
// agent used by the orchestrator.  The default implementation wraps the GitHub
// Copilot cloud API; alternative implementations (e.g. a local CLI agent) can
// be swapped in via configuration.
package agent

import (
	"context"
	"time"

	"github.com/google/go-github/v68/github"
)

// CodingAgent is the interface every coding-agent backend must implement.
type CodingAgent interface {
	// InvokeTask starts the agent working on an issue.
	// Returns a job/task ID that can be used with GetTaskStatus.
	InvokeTask(
		ctx context.Context,
		prompt, issueTitle string,
		issueNum int,
		issueURL string,
	) (string, error)

	// GetTaskStatus returns the status of a previously created task.
	GetTaskStatus(ctx context.Context, jobID string) (*TaskStatus, error)

	// IsActive checks if the agent has any work in progress for the given issue.
	IsActive(ctx context.Context, issueNum int) bool

	// SendPrompt sends a follow-up prompt to the agent (refinement, CI fix,
	// merge conflict resolution, or continue).  For cloud agents this posts a
	// comment; for local agents this may invoke a CLI.
	SendPrompt(ctx context.Context, req PromptRequest) error

	// HasRespondedSince checks if the agent has done anything since the given time.
	HasRespondedSince(ctx context.Context, prNum int, since time.Time) (bool, error)

	// DiscoverPR attempts to find a PR created by the agent for an issue.
	// Returns (nil, nil) if no PR is found or the implementation doesn't support this.
	DiscoverPR(ctx context.Context, issueNum int) (*github.PullRequest, error)

	// RetryTask re-invokes the agent for an issue that previously failed.
	// For CLI agents this reuses the existing persistent working directory.
	RetryTask(ctx context.Context, issueNum int) error

	// CleanupWorkdir removes any persistent working directory associated with
	// an issue.  Called after a successful merge.  No-op for cloud agents.
	CleanupWorkdir(ctx context.Context, issueNum int)

	// CleanupAllWorkdirs removes all persistent working directories.
	// No-op for cloud agents.
	CleanupAllWorkdirs(ctx context.Context) int
}

// TaskStatus describes the current state of an agent task.
type TaskStatus struct {
	JobID    string
	Status   string // "pending", "in_progress", "completed", "failed"
	PRNumber int    // 0 if not yet created
}

// PromptRequest describes a follow-up prompt to send to the coding agent.
type PromptRequest struct {
	IssueNum   int
	PRNum      int
	PromptType string // "merge-conflict", "refinement", "ci-fix", "continue"
	Body       string
	AsReview   bool // true = post as PR review, false = post as issue comment
}
