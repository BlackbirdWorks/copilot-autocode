//nolint:gocritic,goimports
package poller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/BlackbirdWorks/copilot-autodev/agent"
	"github.com/BlackbirdWorks/copilot-autodev/config"
	"github.com/BlackbirdWorks/copilot-autodev/ghclient"
	"github.com/google/go-github/v68/github"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type internalFakeRoundTripper struct {
	handler func(*http.Request) (*http.Response, error)
}

func (f *internalFakeRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	return f.handler(r)
}

func setupInternalPoller(t *testing.T, handler http.HandlerFunc) *Poller {
	t.Helper()
	rt := &internalFakeRoundTripper{
		handler: func(r *http.Request) (*http.Response, error) {
			rec := httptest.NewRecorder()
			handler(rec, r)
			return rec.Result(), nil
		},
	}

	cfg := config.DefaultConfig()
	cfg.GitHubOwner = "test-owner"
	cfg.GitHubRepo = "test-repo"
	cfg.LabelQueue = "ai-todo"
	cfg.LabelCoding = "ai-coding"
	cfg.LabelReview = "ai-review"
	cfg.CopilotInvokeTimeoutSeconds = 60
	cfg.PollIntervalSeconds = 1

	client := ghclient.NewWithTransport("test-token", cfg, rt)
	ag := agent.NewCloudAgent(client, cfg)
	return New(cfg, client, "test-token", ag)
}

func TestInternalHelpers(t *testing.T) {
	t.Parallel()
	p := setupInternalPoller(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/issues/") && strings.Contains(r.URL.Path, "/comments") {
			_ = json.NewEncoder(w).Encode([]*github.IssueComment{})
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	cfg := p.continueTimeoutCfg("timeout prompt", "notice %d")
	require.NotNil(t, cfg.CountFn)
	assert.Equal(t, ghclient.AgentContinueCommentMarker, cfg.NudgeMarker)
	assert.Equal(t, "timeout prompt", cfg.PromptKind)
	assert.Equal(t, "notice %d", cfg.NoticeFormat)
	assert.Equal(t, "retries", cfg.StatusVerb)

	now := time.Now()
	assert.Equal(t, now, latestOf(now, now.Add(-time.Minute)))
	assert.Equal(t, now.Add(time.Minute), latestOf(now, now.Add(time.Minute)))

	runs := []*github.WorkflowRun{{Name: github.Ptr("lint")}, {Name: github.Ptr("test")}}
	assert.Equal(t, []string{"lint", "test"}, runNames(runs))

	assert.Equal(t, 1, p.incApproveRetry(101))
	assert.Equal(t, 2, p.incApproveRetry(101))
	p.clearApproveRetry(101)
	assert.Equal(t, 1, p.incApproveRetry(101))
}

func TestWaitForAgentCycleSetsPendingState(t *testing.T) {
	t.Parallel()

	p := setupInternalPoller(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/comments") {
			_ = json.NewEncoder(w).Encode([]*github.IssueComment{})
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	p.cfg.CopilotInvokeTimeoutSeconds = 600

	displayInfo := make(map[int]*IssueDisplayInfo)
	pr := &github.PullRequest{Number: github.Ptr(123)}
	postedAt := time.Now().Add(-5 * time.Second)

	p.WaitForAgentCycle(context.Background(), pr, 123, postedAt, AgentTimeoutCfg{
		CountFn: func(context.Context, int) (int, error) { return 0, nil },
	}, "Agent running", displayInfo)

	require.Contains(t, displayInfo, 123)
	assert.Equal(t, "Agent running", displayInfo[123].Current)
	assert.Equal(t, "Waiting for agent to push", displayInfo[123].Next)
	assert.Equal(t, "pending", displayInfo[123].AgentStatus)
	assert.Equal(t, pr, displayInfo[123].PR)
}

func TestHandleAgentTimeoutSendsContinuePrompt(t *testing.T) {
	t.Parallel()

	var posted []string
	p := setupInternalPoller(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/issues/123/comments") && r.Method == http.MethodPost:
			var body struct {
				Body string `json:"body"`
			}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			posted = append(posted, body.Body)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(&github.IssueComment{ID: github.Ptr(int64(9))})
		default:
			w.WriteHeader(http.StatusOK)
		}
	})
	p.cfg.CopilotInvokeTimeoutSeconds = 1
	p.cfg.MaxAgentContinueRetries = 3

	handled := p.HandleAgentTimeout(
		context.Background(),
		&github.PullRequest{Number: github.Ptr(123)},
		123,
		time.Now().Add(-2*time.Hour),
		AgentTimeoutCfg{
			CountFn:     func(context.Context, int) (int, error) { return 1, nil },
			NudgeMarker: ghclient.AgentContinueCommentMarker,
		},
		make(map[int]*IssueDisplayInfo),
	)

	assert.False(t, handled)
	require.Len(t, posted, 1)
	assert.Contains(t, posted[0], "@copilot")
	assert.Contains(t, posted[0], ghclient.AgentContinueCommentMarker)
}

func TestStartEmitsEvent(t *testing.T) {
	t.Parallel()

	p := setupInternalPoller(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/labels") && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode([]*github.Label{})
		case strings.Contains(r.URL.Path, "/labels") && r.Method == http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(&github.Label{})
		case strings.Contains(r.URL.Path, "/issues") && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode([]*github.Issue{})
		default:
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{})
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p.Start(ctx)
	select {
	case <-p.Events:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for poller event")
	}

	p.Commands <- Command{Action: "unknown"}
	time.Sleep(20 * time.Millisecond)
}

func TestRerunCI_RerunsOnlyFailedRuns(t *testing.T) {
	t.Parallel()

	rerunCount := 0
	p := setupInternalPoller(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/pulls/42") && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(&github.PullRequest{
				Number: github.Ptr(42),
				Head:   &github.PullRequestBranch{SHA: github.Ptr("sha-42")},
			})
		case strings.Contains(r.URL.Path, "/actions/runs") && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(&github.WorkflowRuns{WorkflowRuns: []*github.WorkflowRun{
				{ID: github.Ptr(int64(1)), Conclusion: github.Ptr("failure")},
				{ID: github.Ptr(int64(2)), Conclusion: github.Ptr("success")},
				{ID: github.Ptr(int64(3)), Conclusion: github.Ptr("cancelled")},
			}})
		case strings.Contains(r.URL.Path, "/actions/runs/1/rerun") && r.Method == http.MethodPost:
			rerunCount++
			w.WriteHeader(http.StatusCreated)
		case strings.Contains(r.URL.Path, "/actions/runs/3/rerun") && r.Method == http.MethodPost:
			rerunCount++
			w.WriteHeader(http.StatusCreated)
		default:
			w.WriteHeader(http.StatusOK)
		}
	})

	p.rerunCI(context.Background(), 42)
	assert.Equal(t, 2, rerunCount)
}

func TestHandleMissingPR_MergedPath(t *testing.T) {
	t.Parallel()

	removeCalls := 0
	closeCalls := 0
	p := setupInternalPoller(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/search/issues"):
			_ = json.NewEncoder(w).Encode(&github.IssuesSearchResult{Issues: []*github.Issue{{
				Number:           github.Ptr(77),
				PullRequestLinks: &github.PullRequestLinks{},
			}}})
		case strings.Contains(r.URL.Path, "/pulls/77") && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(&github.PullRequest{
				Number: github.Ptr(77),
				Merged: github.Ptr(true),
				Body:   github.Ptr("Fixes #123"),
			})
		case strings.Contains(r.URL.Path, "/issues/123/labels/") && r.Method == http.MethodDelete:
			removeCalls++
			w.WriteHeader(http.StatusOK)
		case strings.Contains(r.URL.Path, "/issues/123") && r.Method == http.MethodPatch:
			closeCalls++
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).
				Encode(&github.Issue{Number: github.Ptr(123), State: github.Ptr("closed")})
		default:
			w.WriteHeader(http.StatusOK)
		}
	})

	displayInfo := make(map[int]*IssueDisplayInfo)
	err := p.HandleMissingPR(
		context.Background(),
		&github.Issue{Number: github.Ptr(123)},
		123,
		displayInfo,
	)
	require.NoError(t, err)
	assert.Equal(t, 3, removeCalls)
	assert.Equal(t, 1, closeCalls)
	require.Contains(t, displayInfo, 123)
	assert.Contains(t, displayInfo[123].Current, "PR merged manually")
}

func TestFetchAllIssuesReturnsLabeledError(t *testing.T) {
	t.Parallel()

	p := setupInternalPoller(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/issues") && r.Method == http.MethodGet {
			if strings.Contains(r.URL.RawQuery, "labels=ai-coding") {
				w.WriteHeader(http.StatusInternalServerError)
				_ = json.NewEncoder(w).Encode(map[string]string{"message": "boom"})
				return
			}
			_ = json.NewEncoder(w).Encode([]*github.Issue{})
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	_, _, _, err := p.FetchAllIssues(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "fetch ai-coding issues")
}

func TestNudgeSingleCodingIssue_WithinTimeoutAndExhausted(t *testing.T) {
	t.Parallel()

	old := timeNow
	timeNow = time.Now
	t.Cleanup(func() { timeNow = old })

	p := setupInternalPoller(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/issues/123/events"):
			_ = json.NewEncoder(w).Encode([]*github.IssueEvent{{
				Event:     github.Ptr("labeled"),
				Label:     &github.Label{Name: github.Ptr("ai-coding")},
				CreatedAt: &github.Timestamp{Time: time.Now().Add(-5 * time.Minute)},
			}})
		case strings.Contains(r.URL.Path, "/issues/123/comments"):
			_ = json.NewEncoder(w).Encode([]*github.IssueComment{{
				Body:      github.Ptr(ghclient.CopilotNudgeCommentMarker),
				CreatedAt: &github.Timestamp{Time: time.Now().Add(-2 * time.Minute)},
			}})
		default:
			w.WriteHeader(http.StatusOK)
		}
	})

	displayInfo := map[int]*IssueDisplayInfo{}
	manager := &AISessionManager{Slots: 1, Active: make(map[int]bool)}
	err := p.NudgeSingleCodingIssue(
		context.Background(),
		&github.Issue{Number: github.Ptr(123)},
		displayInfo,
		10*time.Minute,
		manager,
	)
	require.NoError(t, err)
	require.Contains(t, displayInfo, 123)
	assert.Contains(t, displayInfo[123].Current, "Agent invoked via API")
	assert.Equal(t, "pending", displayInfo[123].AgentStatus)

	// Force exhausted retries branch.
	p.cfg.CopilotInvokeMaxRetries = 1
	p2 := setupInternalPoller(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/issues/123/events"):
			_ = json.NewEncoder(w).Encode([]*github.IssueEvent{{
				Event:     github.Ptr("labeled"),
				Label:     &github.Label{Name: github.Ptr("ai-coding")},
				CreatedAt: &github.Timestamp{Time: time.Now().Add(-2 * time.Hour)},
			}})
		case strings.Contains(r.URL.Path, "/issues/123/comments") && r.Method == http.MethodGet:
			comments := []*github.IssueComment{
				{
					Body:      github.Ptr(ghclient.CopilotNudgeCommentMarker),
					CreatedAt: &github.Timestamp{Time: time.Now().Add(-90 * time.Minute)},
				},
			}
			_ = json.NewEncoder(w).Encode(comments)
		case strings.Contains(r.URL.Path, "/issues/123") && r.Method == http.MethodPatch:
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(&github.Issue{Number: github.Ptr(123)})
		default:
			w.WriteHeader(http.StatusOK)
		}
	})
	p2.cfg.CopilotInvokeMaxRetries = 1

	displayInfo = map[int]*IssueDisplayInfo{}
	manager2 := &AISessionManager{Slots: 1, Active: make(map[int]bool)}
	err = p2.NudgeSingleCodingIssue(
		context.Background(),
		&github.Issue{Number: github.Ptr(123)},
		displayInfo,
		10*time.Minute,
		manager2,
	)
	require.NoError(t, err)
	assert.Contains(t, displayInfo[123].Current, "No response after")
}

func TestHandleCommand_RoutesActions(t *testing.T) {
	t.Parallel()

	deletedComments := 0
	removedLabels := 0
	addedLabels := 0
	p := setupInternalPoller(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/issues/50/comments") && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode([]*github.IssueComment{{
				ID:   github.Ptr(int64(999)),
				Body: github.Ptr(ghclient.LocalResolutionFailedMarker),
			}})
		case strings.Contains(r.URL.Path, "/issues/comments/999") && r.Method == http.MethodDelete:
			deletedComments++
			w.WriteHeader(http.StatusNoContent)
		case strings.Contains(r.URL.Path, "/issues/77/labels") && r.Method == http.MethodPost:
			addedLabels++
			w.WriteHeader(http.StatusOK)
		case strings.Contains(r.URL.Path, "/issues/77/labels/") && r.Method == http.MethodDelete:
			removedLabels++
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{})
		}
	})

	ctx := context.Background()
	p.HandleCommand(ctx, Command{Action: "retry-merge", PRNum: 50})
	p.HandleCommand(ctx, Command{Action: "takeover", IssueNum: 77})
	p.HandleCommand(ctx, Command{Action: "priority-up", IssueNum: 123})
	p.HandleCommand(ctx, Command{Action: "priority-down", IssueNum: 123})
	p.HandleCommand(ctx, Command{Action: "unknown"})

	assert.Equal(t, 1, deletedComments)
	assert.Equal(t, 1, addedLabels)
	assert.Equal(t, 3, removedLabels)
	_, exists := p.priorities[123]
	assert.False(t, exists)
}

func TestBuildRefinementCIPrompt_IncludesFailedRunDetails(t *testing.T) {
	t.Parallel()

	p := setupInternalPoller(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/actions/runs") && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(&github.WorkflowRuns{WorkflowRuns: []*github.WorkflowRun{
				{
					ID:         github.Ptr(int64(10)),
					Name:       github.Ptr("CI"),
					Conclusion: github.Ptr("failure"),
				},
			}})
		case strings.Contains(r.URL.Path, "/actions/runs/10/jobs") && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(&github.Jobs{Jobs: []*github.WorkflowJob{
				{
					ID:         github.Ptr(int64(20)),
					Name:       github.Ptr("unit"),
					Conclusion: github.Ptr("failure"),
				},
			}})
		case strings.Contains(r.URL.Path, "/actions/jobs/20/logs") && r.Method == http.MethodGet:
			w.Header().Set("Location", "https://example.com/logs/20")
			w.WriteHeader(http.StatusFound)
		default:
			w.WriteHeader(http.StatusOK)
		}
	})

	prompt := p.BuildRefinementCIPrompt(context.Background(), 1, 3, 123, true, "sha123")
	assert.Contains(t, prompt, "refinement check 1 of 3")
	assert.Contains(t, prompt, "Failing workflow")
	assert.Contains(t, prompt, ghclient.RefinementCommentMarker)
}

func TestRerunCI_ErrorPaths(t *testing.T) {
	t.Parallel()

	// PR head SHA lookup fails.
	p := setupInternalPoller(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/pulls/88") && r.Method == http.MethodGet {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	p.rerunCI(context.Background(), 88)

	// Workflow listing fails after SHA lookup succeeds.
	p2 := setupInternalPoller(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/pulls/89") && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).
				Encode(&github.PullRequest{Head: &github.PullRequestBranch{SHA: github.Ptr("sha-89")}})
		case strings.Contains(r.URL.Path, "/actions/runs") && r.Method == http.MethodGet:
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusOK)
		}
	})
	p2.rerunCI(context.Background(), 89)
}
