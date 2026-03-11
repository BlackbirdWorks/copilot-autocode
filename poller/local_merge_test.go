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
			expectStatus: "Merge conflicts unresolved — needs manual fix",
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
			expectStatus: "Merge conflicts unresolved — needs manual fix", // Will attempt and fail
		},
		{
			name:     "already resolved successfully",
			attempts: 2,
			setupMock: func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "/repos/test-owner/test-repo/issues/123/comments") {
					marker := ghclient.SHAMarker("local-resolution-success", "head-sha")
					comments := []*github.IssueComment{
						{Body: github.Ptr(marker)},
					}
					json.NewEncoder(w).Encode(comments)
				}
			},
			expectStatus: "Merge resolved locally",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rt := &fakeRoundTripper{
				handler: func(r *http.Request) (*http.Response, error) {
					// GitHub API requests usually have query params, so we check for prefix.
					if strings.Contains(
						r.URL.Path,
						"/repos/test-owner/test-repo/issues/123/comments",
					) {
						rec := httptest.NewRecorder()
						tt.setupMock(rec, r)
						return rec.Result(), nil
					}
					// Return empty JSON for everything else to avoid 404/decode errors
					return &http.Response{
						StatusCode: http.StatusOK,
						Body:       http.NoBody,
					}, nil
				},
			}
			cfg := config.DefaultConfig()
			cfg.GitHubOwner = "test-owner"
			cfg.GitHubRepo = "test-repo"
			cfg.LocalMergeAttempts = tt.attempts
			cfg.LocalMergeDelayMinutes = tt.delay

			client := ghclient.NewWithTransport("test-token", cfg, rt)
			ag := agent.NewCloudAgent(client)
			p := poller.New(cfg, client, "test-token", ag)

			task := &poller.PRTask{
				P:           p,
				PR:          &github.PullRequest{Number: github.Ptr(123)},
				Num:         123,
				Sha:         "head-sha", // Set a SHA
				DisplayInfo: make(map[int]*poller.IssueDisplayInfo),
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
			_ = json.NewEncoder(w).Encode([]*github.IssueComment{{Body: github.Ptr(marker)}})
		}
	})
	task := &poller.PRTask{
		P:           p,
		PR:          &github.PullRequest{Number: github.Ptr(123)},
		Num:         123,
		Sha:         "head-sha",
		DisplayInfo: make(map[int]*poller.IssueDisplayInfo),
	}

	done, err := task.ResolveConflictsLocally(ctx, 0)
	require.NoError(t, err)
	assert.True(t, done)
	assert.Contains(t, task.DisplayInfo[123].Current, "Merge resolved locally")
}

// fakeRoundTripper and setupMockPoller are now in common_test.go
