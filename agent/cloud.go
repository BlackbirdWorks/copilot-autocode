package agent

import (
	"context"
	"strings"
	"time"

	"github.com/google/go-github/v68/github"

	"github.com/BlackbirdWorks/copilot-autodev/config"
	"github.com/BlackbirdWorks/copilot-autodev/ghclient"
)

// CloudAgent implements CodingAgent using the GitHub Copilot cloud API.
// This is the default agent — it wraps the existing ghclient Copilot methods.
type CloudAgent struct {
	gh  *ghclient.Client
	cfg *config.Config
}

// NewCloudAgent creates a CloudAgent backed by the given GitHub client.
func NewCloudAgent(gh *ghclient.Client, cfg *config.Config) *CloudAgent {
	return &CloudAgent{gh: gh, cfg: cfg}
}

func (a *CloudAgent) InvokeTask(
	ctx context.Context, prompt, issueTitle string, issueNum int, issueURL string,
) (string, error) {
	return a.gh.InvokeCopilotAgent(ctx, prompt, issueTitle, issueNum, issueURL)
}

func (a *CloudAgent) GetTaskStatus(ctx context.Context, jobID string) (*TaskStatus, error) {
	if jobID == "" {
		// Assignment trigger doesn't have a job ID. Return a placeholder status.
		return &TaskStatus{Status: "requested"}, nil
	}
	status, err := a.gh.GetCopilotJobStatus(ctx, jobID)
	if err != nil {
		return nil, err
	}
	ts := &TaskStatus{
		JobID:  status.JobID,
		Status: status.Status,
	}
	if status.PullRequest != nil {
		ts.PRNumber = status.PullRequest.Number
	}
	return ts, nil
}

func (a *CloudAgent) IsActive(ctx context.Context, issueNum int, branch string) bool {
	jobID, startedAt, _ := a.gh.LatestCopilotJobID(ctx, issueNum)
	if jobID != "" {
		if !startedAt.IsZero() && time.Since(startedAt) > time.Hour {
			// Hard timeout: AI session lost track or stuck for > 1 hour
			return false
		}
		status, err := a.gh.GetCopilotJobStatus(ctx, jobID)
		if err == nil && status != nil {
			s := status.Status
			if s == "in_progress" || s == "running" || s == "queued" || s == "requested" ||
				s == "pending" {
				return true
			}
		}
	} else if a.cfg.AssignInsteadOfInvoke {
		// If no job ID but assignment trigger is enabled, check if the issue
		// is still assigned to copilot.
		if assigned, _ := a.gh.IsIssueAssignedTo(ctx, issueNum, "github-copilot"); assigned {
			return true
		}
	}
	if active, err := a.gh.HasActiveCopilotRunForBranch(ctx, branch); err == nil && active {
		// Note: we could also check the workflow run's age here, but usually GitHub caps them at 1h.
		return true
	}

	// Check if Copilot is engaged via reaction to a recent prompt but hasn't responded yet.
	if engaged, reactedAt, _ := a.gh.CopilotEngagementStatus(ctx, issueNum); engaged {
		if !reactedAt.IsZero() && time.Since(reactedAt) > time.Hour {
			return false // Hard timeout
		}
		return true
	}

	return false
}

func (a *CloudAgent) SendPrompt(ctx context.Context, req PromptRequest) error {
	body := req.Body
	// Cloud agent is addressed via @copilot in comments.
	if !strings.Contains(body, "@copilot") {
		body = "@copilot " + body
	}
	if req.AsReview {
		return a.gh.PostReviewComment(ctx, req.PRNum, body)
	}
	num := req.PRNum
	if num == 0 {
		num = req.IssueNum
	}
	return a.gh.PostComment(ctx, num, body)
}

func (a *CloudAgent) HasRespondedSince(
	ctx context.Context,
	prNum int,
	since time.Time,
) (bool, error) {
	return a.gh.HasAgentCommentSince(ctx, prNum, since)
}

func (a *CloudAgent) DiscoverPR(ctx context.Context, issueNum int) (*github.PullRequest, error) {
	pr := a.gh.DiscoverPRViaJobID(ctx, issueNum)
	return pr, nil
}

func (a *CloudAgent) RetryTask(ctx context.Context, issueNum int) error {
	// For cloud agents, retry is a no-op — the poller's nudge logic handles re-invocation.
	return nil
}

func (a *CloudAgent) CleanupWorkdir(_ context.Context, _ int) {
	// Cloud agent has no local working directories.
}

func (a *CloudAgent) CleanupAllWorkdirs(_ context.Context) int {
	return 0
}
