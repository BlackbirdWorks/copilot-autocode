//nolint:gocritic,goimports
package agent_test

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

func TestCloudAgent_IsActive(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	tests := []struct {
		name     string
		mock     func(w http.ResponseWriter, r *http.Request)
		expected bool
	}{
		{
			name: "job in progress",
			mock: func(w http.ResponseWriter, r *http.Request) {
				path := r.URL.Path
				if strings.Contains(path, "/comments") {
					_ = json.NewEncoder(w).Encode([]*github.IssueComment{
						{
							Body:      github.Ptr("<!-- copilot-autodev:job-id:job-123 -->"),
							CreatedAt: &github.Timestamp{Time: time.Now()},
						},
					})
				} else if strings.Contains(path, "/agents/swe/v1/jobs/") {
					_ = json.NewEncoder(w).Encode(map[string]interface{}{
						"job_id": "job-123",
						"status": "in_progress",
					})
				}
			},
			expected: true,
		},
		{
			name: "no active job",
			mock: func(w http.ResponseWriter, r *http.Request) {
				path := r.URL.Path
				if strings.Contains(path, "/comments") {
					_ = json.NewEncoder(w).Encode([]*github.IssueComment{})
				} else if strings.Contains(path, "/actions/runs") {
					_ = json.NewEncoder(w).Encode(&github.WorkflowRuns{WorkflowRuns: []*github.WorkflowRun{}})
				}
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rt := &fakeRoundTripper{
				handler: func(r *http.Request) (*http.Response, error) {
					rec := httptest.NewRecorder()
					tt.mock(rec, r)
					return rec.Result(), nil
				},
			}
			cfg := config.DefaultConfig()
			cfg.GitHubOwner = "owner"
			cfg.GitHubRepo = "repo"
			client := ghclient.NewWithTransport("token", cfg, rt)
			ag := agent.NewCloudAgent(client, cfg)

			assert.Equal(t, tt.expected, ag.IsActive(ctx, 123, "branch"))
		})
	}
}

func TestCloudAgent_InvokeTask(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	p := setupCloudAgent(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"job_id": "job-123",
		})
	})

	id, err := p.InvokeTask(ctx, "prompt", "title", 123, "url")
	require.NoError(t, err)
	assert.Equal(t, "job-123", id)
}

func TestCloudAgent_GetTaskStatus(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	p := setupCloudAgent(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"job_id": "job-123",
			"status": "completed",
			"pull_request": map[string]interface{}{
				"number": 456,
			},
		})
	})

	status, err := p.GetTaskStatus(ctx, "job-123")
	require.NoError(t, err)
	assert.Equal(t, "completed", status.Status)
	assert.Equal(t, 456, status.PRNumber)
}

func TestCloudAgent_SendPrompt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	tests := []struct {
		name     string
		req      agent.PromptRequest
		expected string
	}{
		{
			name: "issue comment",
			req:  agent.PromptRequest{IssueNum: 123, Body: "fix it"},
		},
		{
			name: "pr review",
			req:  agent.PromptRequest{PRNum: 456, Body: "looks good", AsReview: true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := setupCloudAgent(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusCreated)
			})
			err := p.SendPrompt(ctx, tt.req)
			require.NoError(t, err)
		})
	}
}

func TestCloudAgent_HasRespondedSince_And_DiscoverPR(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	rt := &fakeRoundTripper{
		handler: func(r *http.Request) (*http.Response, error) {
			rec := httptest.NewRecorder()
			switch {
			case strings.Contains(r.URL.Path, "/issues/123/comments") && r.Method == http.MethodGet:
				_ = json.NewEncoder(rec).Encode([]*github.IssueComment{
					{
						Body:      github.Ptr("<!-- copilot-autodev:job-id:job-xyz -->"),
						CreatedAt: &github.Timestamp{Time: time.Now()},
						User:      &github.User{Login: github.Ptr("github-copilot[bot]")},
					},
				})
			case strings.Contains(r.URL.Path, "/agents/swe/v1/jobs/owner/repo/job-xyz"):
				_ = json.NewEncoder(rec).Encode(map[string]any{
					"job_id": "job-xyz",
					"status": "completed",
					"pull_request": map[string]any{
						"number": 456,
					},
				})
			case strings.Contains(r.URL.Path, "/pulls/456") && r.Method == http.MethodGet:
				_ = json.NewEncoder(rec).Encode(&github.PullRequest{
					Number: github.Ptr(456),
					State:  github.Ptr("open"),
				})
			case strings.Contains(r.URL.Path, "/actions/runs"):
				_ = json.NewEncoder(rec).
					Encode(&github.WorkflowRuns{WorkflowRuns: []*github.WorkflowRun{}})
			default:
				rec.WriteHeader(http.StatusOK)
			}
			return rec.Result(), nil
		},
	}

	cfg := config.DefaultConfig()
	cfg.GitHubOwner = "owner"
	cfg.GitHubRepo = "repo"
	client := ghclient.NewWithTransport("token", cfg, rt)
	ag := agent.NewCloudAgent(client, cfg)

	has, err := ag.HasRespondedSince(ctx, 123, time.Now().Add(-time.Hour))
	require.NoError(t, err)
	assert.True(t, has)

	pr, err := ag.DiscoverPR(ctx, 123)
	require.NoError(t, err)
	require.NotNil(t, pr)
	assert.Equal(t, 456, pr.GetNumber())
}

func setupCloudAgent(t *testing.T, handler http.HandlerFunc) agent.CodingAgent {
	t.Helper()
	rt := &fakeRoundTripper{
		handler: func(r *http.Request) (*http.Response, error) {
			rec := httptest.NewRecorder()
			handler(rec, r)
			return rec.Result(), nil
		},
	}
	cfg := config.DefaultConfig()
	cfg.GitHubOwner = "owner"
	client := ghclient.NewWithTransport("token", cfg, rt)
	return agent.NewCloudAgent(client, cfg)
}
