package ghclient_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"testing"
	"time"

	"github.com/google/go-github/v68/github"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BlackbirdWorks/copilot-autocode/ghclient"
)

// TestInvokeCopilotAgent verifies that InvokeCopilotAgent sends a correctly
// formed POST request to the Copilot API and handles success/error responses.
func TestInvokeCopilotAgent(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		prompt        string
		issueTitle    string
		issueNum      int
		issueURL      string
		serverStatus  int
		serverBody    string // JSON response body from mock server
		wantErr       bool
		wantJobID     string
		wantTitle     string
		wantPrompt    string
		wantEventType string
		wantIssueNum  int
		wantIssueURL  string
	}{
		{
			name:          "success – 201 Created with job ID",
			prompt:        "Please implement issue #42",
			issueTitle:    "Add CloudWatch support",
			issueNum:      42,
			issueURL:      "https://github.com/org/repo/issues/42",
			serverStatus:  http.StatusCreated,
			serverBody:    `{"id":"abc-123","status":"queued"}`,
			wantErr:       false,
			wantJobID:     "abc-123",
			wantTitle:     "[copilot-autocode] #42: Add CloudWatch support",
			wantPrompt:    "Please implement issue #42",
			wantEventType: "copilot-autocode",
			wantIssueNum:  42,
			wantIssueURL:  "https://github.com/org/repo/issues/42",
		},
		{
			name:          "success – 200 OK with job_id field",
			prompt:        "Fix the bug in issue #7",
			issueTitle:    "Fix nil pointer",
			issueNum:      7,
			issueURL:      "https://github.com/org/repo/issues/7",
			serverStatus:  http.StatusOK,
			serverBody:    `{"job_id":"xyz-789"}`,
			wantErr:       false,
			wantJobID:     "xyz-789",
			wantTitle:     "[copilot-autocode] #7: Fix nil pointer",
			wantPrompt:    "Fix the bug in issue #7",
			wantEventType: "copilot-autocode",
			wantIssueNum:  7,
			wantIssueURL:  "https://github.com/org/repo/issues/7",
		},
		{
			name:          "success – empty response body returns empty job ID",
			prompt:        "Do something",
			issueTitle:    "Task",
			issueNum:      1,
			serverStatus:  http.StatusCreated,
			serverBody:    `{}`,
			wantErr:       false,
			wantJobID:     "",
			wantTitle:     "[copilot-autocode] #1: Task",
			wantPrompt:    "Do something",
			wantEventType: "copilot-autocode",
			wantIssueNum:  1,
		},
		{
			name:         "unauthorized – 401 returns error",
			prompt:       "some task",
			serverStatus: http.StatusUnauthorized,
			wantErr:      true,
		},
		{
			name:         "server error – 500 returns error",
			prompt:       "some task",
			serverStatus: http.StatusInternalServerError,
			wantErr:      true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var gotReq ghclient.CopilotAgentJobRequest
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, http.MethodPost, r.Method)
				assert.NoError(t, json.NewDecoder(r.Body).Decode(&gotReq))
				w.WriteHeader(tc.serverStatus)
				if tc.serverBody != "" {
					_, _ = w.Write([]byte(tc.serverBody))
				}
			}))
			defer srv.Close()

			c := ghclient.NewTestClient("test-owner", "test-repo", "test-token")

			jobID, err := c.InvokeAgentAt(
				context.Background(),
				srv.URL+"/agents/swe/v1/jobs/test-owner/test-repo",
				tc.prompt, tc.issueTitle, tc.issueNum, tc.issueURL,
			)

			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantJobID, jobID)
			assert.Equal(t, tc.wantTitle, gotReq.Title)
			assert.Equal(t, tc.wantPrompt, gotReq.ProblemStatement)
			assert.Equal(t, tc.wantEventType, gotReq.EventType)
			assert.Equal(t, tc.wantIssueNum, gotReq.IssueNumber)
			assert.Equal(t, tc.wantIssueURL, gotReq.IssueURL)
		})
	}
}

// TestTimeAgo verifies that TimeAgo produces the correct relative-time label
// for representative durations across all four branches of the function.
func TestTimeAgo(t *testing.T) {
	t.Parallel()
	now := time.Now()

	tests := []struct {
		name string
		t    time.Time
		want string
	}{
		// < 1 minute → "just now"
		{"1 second ago", now.Add(-1 * time.Second), "just now"},
		{"30 seconds ago", now.Add(-30 * time.Second), "just now"},
		{"59 seconds ago", now.Add(-59 * time.Second), "just now"},

		// >= 1 minute, < 1 hour → "Nm ago"
		{"exactly 1 minute ago", now.Add(-1 * time.Minute), "1m ago"},
		{"5 minutes ago", now.Add(-5 * time.Minute), "5m ago"},
		{"59 minutes ago", now.Add(-59 * time.Minute), "59m ago"},

		// >= 1 hour, < 24 hours → "Nh ago"
		{"exactly 1 hour ago", now.Add(-1 * time.Hour), "1h ago"},
		{"3 hours ago", now.Add(-3 * time.Hour), "3h ago"},
		{"23 hours ago", now.Add(-23 * time.Hour), "23h ago"},

		// >= 24 hours → "Nd ago"
		{"exactly 1 day ago", now.Add(-24 * time.Hour), "1d ago"},
		{"2 days ago", now.Add(-48 * time.Hour), "2d ago"},
		{"10 days ago", now.Add(-240 * time.Hour), "10d ago"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, ghclient.TimeAgo(tc.t))
		})
	}
}

// setupMockGitHubAPI creates an [httptest.Server] and a corresponding ghclient.Client
// configured to communicate with it. Providing a mock handler lets us simulate
// arbitrary GitHub API responses.
func setupMockGitHubAPI(t *testing.T, handler http.HandlerFunc) *ghclient.Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	gh := github.NewClient(nil)
	gh.BaseURL, _ = url.Parse(srv.URL + "/")
	return ghclient.NewTestClientWithGH(gh, "test-owner", "test-repo")
}

// TestAnyWorkflowRunActive verifies that waiting/action_required states
// do not trigger a 'true' return (preventing deadlocks).
func TestAnyWorkflowRunActive(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		runs       []*github.WorkflowRun
		wantActive bool
	}{
		{
			name:       "no runs",
			runs:       nil,
			wantActive: false,
		},
		{
			name:       "completed runs only",
			runs:       []*github.WorkflowRun{{Status: github.Ptr("completed")}},
			wantActive: false,
		},
		{
			name: "waiting and action_required runs",
			runs: []*github.WorkflowRun{
				{Status: github.Ptr("waiting")},
				{Status: github.Ptr("action_required")},
			},
			wantActive: false, // Core fix: these should NOT be considered "active"
		},
		{
			name:       "in_progress runs",
			runs:       []*github.WorkflowRun{{Status: github.Ptr("in_progress")}},
			wantActive: true,
		},
		{
			name:       "queued runs",
			runs:       []*github.WorkflowRun{{Status: github.Ptr("queued")}},
			wantActive: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := setupMockGitHubAPI(t, func(w http.ResponseWriter, _ *http.Request) {
				resp := struct {
					WorkflowRuns []*github.WorkflowRun `json:"workflow_runs"`
				}{WorkflowRuns: tc.runs}
				_ = json.NewEncoder(w).Encode(resp)
			})

			active, err := c.AnyWorkflowRunActive(context.Background(), "dummy-sha")
			require.NoError(t, err)
			assert.Equal(t, tc.wantActive, active)
		})
	}
}

// TestAllRunsSucceeded verifies the 0-runs bypass and the generic failure catch.
func TestAllRunsSucceeded(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		runs        []*github.WorkflowRun
		wantSuccess bool
		wantAnyFail bool
	}{
		{
			name:        "zero runs (0-run repo fix)",
			runs:        nil,
			wantSuccess: true,
			wantAnyFail: false,
		},
		{
			name: "one successful run",
			runs: []*github.WorkflowRun{
				{Status: github.Ptr("completed"), Conclusion: github.Ptr("success")},
			},
			wantSuccess: true,
			wantAnyFail: false,
		},
		{
			name: "one skipped run",
			runs: []*github.WorkflowRun{
				{Status: github.Ptr("completed"), Conclusion: github.Ptr("skipped")},
			},
			wantSuccess: true,
			wantAnyFail: false,
		},
		{
			name: "run still in progress",
			runs: []*github.WorkflowRun{
				{Status: github.Ptr("in_progress")},
			},
			wantSuccess: false,
			wantAnyFail: false,
		},
		{
			name: "generic failure (restored CI failure logic)",
			runs: []*github.WorkflowRun{
				{Status: github.Ptr("completed"), Conclusion: github.Ptr("failure")},
			},
			wantSuccess: false,
			wantAnyFail: true,
		},
		{
			name: "timed out failure",
			runs: []*github.WorkflowRun{
				{Status: github.Ptr("completed"), Conclusion: github.Ptr("timed_out")},
			},
			wantSuccess: false,
			wantAnyFail: true,
		},
		{
			name: "mixed success and failure",
			runs: []*github.WorkflowRun{
				{Status: github.Ptr("completed"), Conclusion: github.Ptr("success")},
				{Status: github.Ptr("completed"), Conclusion: github.Ptr("failure")},
			},
			wantSuccess: false,
			wantAnyFail: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := setupMockGitHubAPI(t, func(w http.ResponseWriter, _ *http.Request) {
				resp := struct {
					WorkflowRuns []*github.WorkflowRun `json:"workflow_runs"`
				}{WorkflowRuns: tc.runs}
				_ = json.NewEncoder(w).Encode(resp)
			})

			success, fail, err := c.AllRunsSucceeded(context.Background(), "dummy-sha")
			require.NoError(t, err)
			assert.Equal(t, tc.wantSuccess, success)
			assert.Equal(t, tc.wantAnyFail, fail)
		})
	}
}

// TestHasActiveCopilotRun verifies that the Copilot-run guard correctly
// identifies active runs from Copilot actors and ignores non-Copilot runs.
func TestHasActiveCopilotRun(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		runs       []*github.WorkflowRun
		wantActive bool
	}{
		{
			name:       "no runs",
			runs:       nil,
			wantActive: false,
		},
		{
			name: "completed copilot run",
			runs: []*github.WorkflowRun{
				{
					Status: github.Ptr("completed"),
					Actor:  &github.User{Login: github.Ptr("copilot-swe-agent[bot]")},
				},
			},
			wantActive: false,
		},
		{
			name: "in_progress copilot run",
			runs: []*github.WorkflowRun{
				{
					Status: github.Ptr("in_progress"),
					Actor:  &github.User{Login: github.Ptr("copilot-swe-agent[bot]")},
				},
			},
			wantActive: true,
		},
		{
			name: "queued copilot run",
			runs: []*github.WorkflowRun{
				{
					Status: github.Ptr("queued"),
					Actor:  &github.User{Login: github.Ptr("Copilot")},
				},
			},
			wantActive: true,
		},
		{
			name: "in_progress non-copilot run",
			runs: []*github.WorkflowRun{
				{
					Status: github.Ptr("in_progress"),
					Actor:  &github.User{Login: github.Ptr("github-actions[bot]")},
					Name:   github.Ptr("CI"),
				},
			},
			wantActive: false,
		},
		{
			name: "in_progress run with copilot in workflow name",
			runs: []*github.WorkflowRun{
				{
					Status: github.Ptr("in_progress"),
					Actor:  &github.User{Login: github.Ptr("github-actions[bot]")},
					Name:   github.Ptr("Copilot Coding Agent"),
				},
			},
			wantActive: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := setupMockGitHubAPI(t, func(w http.ResponseWriter, _ *http.Request) {
				resp := struct {
					WorkflowRuns []*github.WorkflowRun `json:"workflow_runs"`
				}{WorkflowRuns: tc.runs}
				_ = json.NewEncoder(w).Encode(resp)
			})

			active, err := c.HasActiveCopilotRun(context.Background())
			require.NoError(t, err)
			assert.Equal(t, tc.wantActive, active)
		})
	}
}

// TestPRRegexMatching validates the bodyRe, titleRe, and branchRe behavior
// used in OpenPRForIssue and MergedPRForIssue.
func TestPRRegexMatching(t *testing.T) {
	t.Parallel()
	issueNum := 329

	bodyRe := regexp.MustCompile(fmt.Sprintf(`(?i)#%d\b`, issueNum))
	titleRe := regexp.MustCompile(fmt.Sprintf(`(?i)#%d\b`, issueNum))
	branchRe := regexp.MustCompile(fmt.Sprintf(`(?i)(?:^|/)(?:issue-?)?%d(?:[-/]|$)`, issueNum))

	// Body match tests
	assert.True(t, bodyRe.MatchString("Fixes #329"), "bodyRe should match exact string")
	assert.True(t, bodyRe.MatchString("Addresses #329\nWith more text"), "bodyRe should match loosened mention")
	assert.False(t, bodyRe.MatchString("Fixes #3291"), "bodyRe should NOT match #3291")

	// Title match tests
	assert.True(t, titleRe.MatchString("#329 Fix database bug"), "titleRe should match starting number")
	assert.True(t, titleRe.MatchString("API Fixes for #329"), "titleRe should match ending number")
	assert.False(t, titleRe.MatchString("Fixes #3295"), "titleRe should NOT match #3295")

	// Branch match tests
	assert.True(t, branchRe.MatchString("issue-329"), "branchRe should match basic issue branch")
	assert.True(t, branchRe.MatchString("copilot/issue-329-fix-bug"), "branchRe should match copilot prefixed branch")
	assert.True(t, branchRe.MatchString("issue329"), "branchRe should match unhyphenated issue branch")
	assert.True(t, branchRe.MatchString("copilot/329-fix-bug"), "branchRe should match bare number after slash")
	assert.True(t, branchRe.MatchString("copilot-swe-agent/issue-329/fix-bug"), "branchRe should match slash after number")
	assert.False(t, branchRe.MatchString("issue-3295-fix"), "branchRe should NOT match issue branch with extra digits")
	assert.False(t, branchRe.MatchString("issue-3295/fix"), "branchRe should NOT match wrong issue with slash")
}

// TestGetCopilotJobStatus verifies polling the job status endpoint.
func TestGetCopilotJobStatus(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		serverStatus int
		serverBody   string
		jobID        string
		wantErr      bool
		wantStatus   string
	}{
		{
			name:         "success - running",
			serverStatus: http.StatusOK,
			serverBody:   `{"job_id": "job-123", "status": "running"}`,
			jobID:        "job-123",
			wantErr:      false,
			wantStatus:   "running",
		},
		{
			name:         "not found - 404",
			serverStatus: http.StatusNotFound,
			serverBody:   `{}`,
			jobID:        "job-404",
			wantErr:      true,
			wantStatus:   "",
		},
		{
			name:         "server error - 500",
			serverStatus: http.StatusInternalServerError,
			serverBody:   `{"message":"Internal Server Error"}`,
			jobID:        "job-500",
			wantErr:      true,
			wantStatus:   "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// Create a regular HTTP server purely to provide the URL,
			// since setupMockGitHubAPI returns a ghclient that overrides github
			// client but the auth token logic is separate.
			// Actually, setupMockGitHubAPI creates an httptest.Server internally
			// but doesn't expose its URL on the returned ghclient object directly.
			// Let's just create our own server so we can pass its URL to GetJobStatusAt.

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, http.MethodGet, r.Method)
				assert.Contains(t, r.URL.Path, tc.jobID)
				w.WriteHeader(tc.serverStatus)
				_, _ = w.Write([]byte(tc.serverBody))
			}))
			defer srv.Close()

			client := ghclient.NewTestClient("test-owner", "test-repo", "test-token")
			endpoint := srv.URL + "/agents/swe/v1/jobs/test-owner/test-repo/" + tc.jobID

			status, err := client.GetJobStatusAt(context.Background(), endpoint, tc.jobID)
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tc.wantStatus, status.Status)
				assert.Equal(t, tc.jobID, status.JobID)
			}
		})
	}
}

// TestLatestCopilotJobID verifies extraction of the job ID from issue comments.
func TestLatestCopilotJobID(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		comments  []*github.IssueComment
		wantJobID string
	}{
		{
			name:      "no comments",
			comments:  nil,
			wantJobID: "",
		},
		{
			name: "comments without marker",
			comments: []*github.IssueComment{
				{Body: github.Ptr("Some comment"), CreatedAt: &github.Timestamp{Time: time.Now()}},
			},
			wantJobID: "",
		},
		{
			name: "single marker comment",
			comments: []*github.IssueComment{
				{
					Body:      github.Ptr(fmt.Sprintf("Tracking task.\n%smy-job-123 -->", ghclient.CopilotJobIDCommentMarker)),
					CreatedAt: &github.Timestamp{Time: time.Now()},
				},
			},
			wantJobID: "my-job-123",
		},
		{
			name: "multiple markers returns latest",
			comments: []*github.IssueComment{
				{
					Body:      github.Ptr(fmt.Sprintf("Old task.\n%sold-job -->", ghclient.CopilotJobIDCommentMarker)),
					CreatedAt: &github.Timestamp{Time: time.Now().Add(-1 * time.Hour)},
				},
				{
					Body:      github.Ptr(fmt.Sprintf("New task.\n%snew-job -->", ghclient.CopilotJobIDCommentMarker)),
					CreatedAt: &github.Timestamp{Time: time.Now()},
				},
			},
			wantJobID: "new-job",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := setupMockGitHubAPI(t, func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(tc.comments)
			})

			jobID, err := c.LatestCopilotJobID(context.Background(), 1)
			require.NoError(t, err)
			assert.Equal(t, tc.wantJobID, jobID)
		})
	}
}
