//nolint:gocritic,goimports
package poller_test

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
	"github.com/BlackbirdWorks/copilot-autodev/poller"
	"github.com/google/go-github/v68/github"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPRTask_ResolveConflictsLocally(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		setupMock    func(w http.ResponseWriter, r *http.Request)
		maxRetries   int
		attempts     int
		delay        int
		expectStatus string
		expectFailed bool
	}{
		{
			name:     "immediate local resolution (maxRetries=0)",
			attempts: 2,
			setupMock: func(w http.ResponseWriter, r *http.Request) {
				// No prior failure comments
				if r.URL.Path == "/repos/test-owner/test-repo/issues/123/comments" {
					json.NewEncoder(w).Encode([]*github.IssueComment{})
				}
			},
			expectStatus: "Running local AI merge resolution",
		},
		{
			name:     "exhausted retries",
			attempts: 2,
			setupMock: func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "/repos/test-owner/test-repo/issues/123/comments") {
					comments := []*github.IssueComment{
						{Body: github.Ptr(ghclient.LocalResolutionFailedMarker)},
						{Body: github.Ptr(ghclient.LocalResolutionFailedMarker)},
					}
					json.NewEncoder(w).Encode(comments)
				}
			},
			expectStatus: "Merge conflicts unresolved — needs manual fix",
		},
		{
			name:     "within delay period",
			attempts: 2,
			delay:    10,
			setupMock: func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "/repos/test-owner/test-repo/issues/123/comments") {
					comments := []*github.IssueComment{
						{
							Body:      github.Ptr(ghclient.LocalResolutionFailedMarker),
							CreatedAt: &github.Timestamp{Time: time.Now().Add(-5 * time.Minute)},
						},
					}
					json.NewEncoder(w).Encode(comments)
				}
			},
			expectStatus: "Waiting for local AI merge retry delay (10 min)",
		},
		{
			name:     "delay passed - ready to retry",
			attempts: 2,
			delay:    10,
			setupMock: func(w http.ResponseWriter, r *http.Request) {
				// Initial check for comments
				if strings.Contains(r.URL.Path, "/repos/test-owner/test-repo/issues/123/comments") {
					comments := []*github.IssueComment{
						{
							Body:      github.Ptr(ghclient.LocalResolutionFailedMarker),
							CreatedAt: &github.Timestamp{Time: time.Now().Add(-15 * time.Minute)},
						},
					}
					json.NewEncoder(w).Encode(comments)
				}
			},
			expectStatus: "Running local AI merge resolution",
		},
		{
			name:     "already resolved successfully (fresh)",
			attempts: 2,
			setupMock: func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "/repos/test-owner/test-repo/issues/123/comments") {
					marker := ghclient.SHAMarker("local-resolution-success", "head-sha")
					comments := []*github.IssueComment{
						{
							Body:      github.Ptr(marker),
							CreatedAt: &github.Timestamp{Time: time.Now()},
						},
					}
					json.NewEncoder(w).Encode(comments)
				}
			},
			expectStatus: "Merge resolved locally",
		},
		{
			name:     "resolved successfully but timed out (stuck dirty)",
			attempts: 2,
			setupMock: func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "/repos/test-owner/test-repo/issues/123/comments") {
					marker := ghclient.SHAMarker("local-resolution-success", "head-sha")
					comments := []*github.IssueComment{
						{
							Body:      github.Ptr(marker),
							CreatedAt: &github.Timestamp{Time: time.Now().Add(-5 * time.Minute)},
						},
					}
					json.NewEncoder(w).Encode(comments)
				}
			},
			expectStatus: "Running local AI merge resolution",
		},
		{
			name:     "agent active on CAPI",
			attempts: 2,
			setupMock: func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "/repos/test-owner/test-repo/issues/123/comments") {
					comments := []*github.IssueComment{
						{
							Body:      github.Ptr("<!-- copilot-autodev:job-id:job-1 -->"),
							CreatedAt: &github.Timestamp{Time: time.Now()},
						},
					}
					json.NewEncoder(w).Encode(comments)
				}
				if strings.Contains(r.URL.Path, "/agents/swe/v1/jobs") {
					json.NewEncoder(w).Encode(map[string]any{
						"job_id": "job-1",
						"status": "in_progress",
					})
				}
			},
			expectStatus: "Agent active — waiting for idle before resolving",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rt := &fakeRoundTripper{
				handler: func(r *http.Request) (*http.Response, error) {
					rec := httptest.NewRecorder()
					if tt.setupMock != nil {
						tt.setupMock(rec, r)
					}
					res := rec.Result()
					if res.StatusCode == 0 {
						res.StatusCode = http.StatusOK
					}
					if res.Body == nil || res.ContentLength == 0 {
						res.Body = http.NoBody
					}
					return res, nil
				},
			}
			cfg := config.DefaultConfig()
			cfg.GitHubOwner = "test-owner"
			cfg.GitHubRepo = "test-repo"
			cfg.LocalMergeAttempts = tt.attempts
			cfg.LocalMergeDelayMinutes = tt.delay

			client := ghclient.NewWithTransport("test-token", cfg, rt)
			ag := agent.NewCloudAgent(client, cfg)
			p := poller.New(cfg, client, "test-token", ag)

			task := &poller.PRTask{
				P:           p,
				PR:          &github.PullRequest{Number: github.Ptr(123)},
				Num:         123,
				Sha:         "head-sha", // Set a SHA
				DisplayInfo: make(map[int]*poller.IssueDisplayInfo),
				Manager:     &poller.AISessionManager{Slots: 1, Active: make(map[int]bool)},
			}

			// We call ResolveConflictsLocally and check the DisplayInfo.
			done, err := task.ResolveConflictsLocally(context.Background(), 0)

			// If it proceeds to actual resolution, it might fail because we didn't mock resolver.
			// That's fine as long as it passed the guards we are testing.
			if tt.expectStatus != "" {
				require.NoError(t, err)
				assert.True(t, done)
				assert.Contains(t, task.DisplayInfo[123].Current, tt.expectStatus)
			} else {
				// If we expect it to proceed (done=false), it might error out in resolver.
				// For the guards test, we just want to know if it reached that point.
				assert.False(t, done)
			}
		})
	}
}

func TestPRTask_ResolveConflictsLocally_Success(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	p := setupMockPoller(t, func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if strings.Contains(path, "/issues/123/comments") {
			marker := ghclient.SHAMarker("local-resolution-success", "head-sha")
			_ = json.NewEncoder(w).Encode([]*github.IssueComment{
				{
					Body:      github.Ptr(marker),
					CreatedAt: &github.Timestamp{Time: time.Now()},
				},
			})
		}
	})
	task := &poller.PRTask{
		P:           p,
		PR:          &github.PullRequest{Number: github.Ptr(123)},
		Num:         123,
		Sha:         "head-sha",
		DisplayInfo: make(map[int]*poller.IssueDisplayInfo),
		Manager:     &poller.AISessionManager{Slots: 1, Active: make(map[int]bool)},
	}

	done, err := task.ResolveConflictsLocally(ctx, 0)
	require.NoError(t, err)
	assert.True(t, done)
	assert.Contains(t, task.DisplayInfo[123].Current, "Merge resolved locally")
	assert.NotEmpty(t, task.DisplayInfo[123].MergeLogPath)
}

// fakeRoundTripper and setupMockPoller are now in common_test.go
