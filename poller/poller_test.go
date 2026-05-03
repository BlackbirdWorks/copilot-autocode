//nolint:gocritic,goimports
package poller_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/BlackbirdWorks/copilot-autodev/ghclient"
	"github.com/BlackbirdWorks/copilot-autodev/poller"
	"github.com/google/go-github/v68/github"
	"github.com/stretchr/testify/assert"
)

func TestSortIssuesAsc(t *testing.T) {
	t.Parallel()
	makeIssue := func(n int) *github.Issue { return &github.Issue{Number: &n} }

	tests := []struct {
		name     string
		input    []*github.Issue
		expected []int
	}{
		{"sorted", []*github.Issue{makeIssue(1), makeIssue(2), makeIssue(3)}, []int{1, 2, 3}},
		{"unsorted", []*github.Issue{makeIssue(3), makeIssue(1), makeIssue(2)}, []int{1, 2, 3}},
		{"reverse", []*github.Issue{makeIssue(5), makeIssue(4), makeIssue(3)}, []int{3, 4, 5}},
		{"empty", []*github.Issue{}, []int{}},
		{"single", []*github.Issue{makeIssue(42)}, []int{42}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			poller.SortIssuesAsc(tt.input)
			actual := make([]int, len(tt.input))
			for i, v := range tt.input {
				actual[i] = v.GetNumber()
			}
			assert.Equal(t, tt.expected, actual)
		})
	}
}

func TestFormatFallbackPrompt(t *testing.T) {
	t.Parallel()
	issue := &github.Issue{
		Number:  github.Ptr(123),
		Title:   github.Ptr("Fix the bug"),
		HTMLURL: github.Ptr("https://github.com/org/repo/issues/123"),
	}

	tpl := "Issue #{{.Number}}: {{.Title}} ({{.URL}})"
	expected := "Issue #123: Fix the bug (https://github.com/org/repo/issues/123)"
	actual := poller.FormatFallbackPrompt(tpl, issue)
	assert.Equal(t, expected, actual)
}

func TestPoller_IsAgentActive(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	type args struct {
		num int
	}
	type wants struct {
		active bool
	}
	tests := []struct {
		name  string
		args  args
		wants wants
	}{
		{
			name:  "agent active",
			args:  args{num: 123},
			wants: wants{active: true},
		},
		{
			name:  "agent not active",
			args:  args{num: 456},
			wants: wants{active: false},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := setupMockPoller(t, func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "comments") {
					if tt.args.num == 123 {
						_ = json.NewEncoder(w).Encode([]*github.IssueComment{
							{
								Body:      github.Ptr("<!-- copilot-autodev:job-id:job123 -->"),
								CreatedAt: &github.Timestamp{Time: time.Now()},
							},
						})
						return
					}
					_ = json.NewEncoder(w).Encode([]*github.IssueComment{})
				} else if strings.Contains(r.URL.Path, "job123") {
					_ = json.NewEncoder(w).Encode(&ghclient.CopilotJobStatus{Status: "in_progress"})
				} else if strings.Contains(r.URL.Path, "actions/runs") {
					_ = json.NewEncoder(w).Encode(&github.WorkflowRuns{WorkflowRuns: []*github.WorkflowRun{}})
				} else {
					w.WriteHeader(http.StatusNotFound)
				}
			})
			active := p.IsAgentActive(ctx, tt.args.num, "copilot-autodev/issue-123")
			assert.Equal(t, tt.wants.active, active)
		})
	}
}

func TestPoller_RequestNudge(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	type args struct {
		issue      *github.Issue
		num        int
		nudgeCount int
	}
	type wants struct {
		errNil bool
	}
	tests := []struct {
		name  string
		args  args
		wants wants
	}{
		{
			name: "request nudge success",
			args: args{
				issue:      &github.Issue{Number: github.Ptr(123), HTMLURL: github.Ptr("url")},
				num:        123,
				nudgeCount: 1,
			},
			wants: wants{errNil: true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := setupMockPoller(t, func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "/comments") {
					w.WriteHeader(http.StatusCreated)
					_ = json.NewEncoder(w).
						Encode(&github.IssueComment{ID: github.Ptr(int64(12345))})
				} else {
					w.WriteHeader(http.StatusCreated)
					_ = json.NewEncoder(w).Encode(map[string]string{"id": "job-abc"})
				}
			})
			displayInfo := make(map[int]*poller.IssueDisplayInfo)
			err := p.RequestNudge(
				ctx,
				tt.args.issue,
				tt.args.num,
				tt.args.nudgeCount,
				time.Second,
				displayInfo,
			)
			if tt.wants.errNil {
				assert.NoError(t, err)
			}
		})
	}
}

func TestPoller_ProcessCodingPR(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	tests := []struct {
		name         string
		active       bool
		nudge        bool
		lastNudgeAge time.Duration
		expectNudge  bool
	}{
		{
			name:   "agent active",
			active: true,
		},
		{
			name:         "should nudge",
			active:       false,
			nudge:        true,
			lastNudgeAge: 20 * time.Minute,
			expectNudge:  true,
		},
		{
			name:         "too soon to nudge",
			active:       false,
			nudge:        true,
			lastNudgeAge: 5 * time.Minute,
			expectNudge:  false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := setupMockPoller(t, func(w http.ResponseWriter, r *http.Request) {
				path := r.URL.Path
				switch {
				case strings.Contains(path, "/comments") && r.Method == http.MethodGet:
					comments := []*github.IssueComment{}
					if tt.active {
						comments = append(comments, &github.IssueComment{
							ID:        github.Ptr(int64(1)),
							Body:      github.Ptr("copilot job job-123 started"),
							CreatedAt: &github.Timestamp{Time: time.Now()},
						})
					}
					if tt.nudge {
						comments = append(comments, &github.IssueComment{
							ID:        github.Ptr(int64(2)),
							Body:      github.Ptr("copilot nudge"),
							CreatedAt: &github.Timestamp{Time: time.Now().Add(-tt.lastNudgeAge)},
						})
					}
					_ = json.NewEncoder(w).Encode(comments)
				case strings.Contains(path, "/actions/runs") && r.Method == http.MethodGet:
					runs := &github.WorkflowRuns{WorkflowRuns: []*github.WorkflowRun{}}
					if tt.active {
						runs.WorkflowRuns = append(runs.WorkflowRuns, &github.WorkflowRun{
							ID:     github.Ptr(int64(123)),
							Status: github.Ptr("in_progress"),
						})
					}
					_ = json.NewEncoder(w).Encode(runs)
				case strings.Contains(path, "/comments") && r.Method == http.MethodPost:
					w.WriteHeader(http.StatusCreated)
					_ = json.NewEncoder(w).Encode(&github.IssueComment{ID: github.Ptr(int64(456))})
				case strings.Contains(path, "/labels"):
					w.WriteHeader(http.StatusOK)
					_ = json.NewEncoder(w).Encode([]*github.Label{})
				default:
					w.WriteHeader(http.StatusOK)
					_ = json.NewEncoder(w).Encode(map[string]any{})
				}
			})
			pr := &github.PullRequest{
				Number: github.Ptr(123),
				Head:   &github.PullRequestBranch{SHA: github.Ptr("sha")},
			}
			displayInfo := make(map[int]*poller.IssueDisplayInfo)
			manager := &poller.AISessionManager{Slots: 1, Active: make(map[int]bool)}
			err := p.ProcessCodingPR(ctx, pr, 123, displayInfo, manager)
			assert.NoError(t, err)
		})
	}
}

func TestPoller_ProcessDraftPR(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	type args struct {
		pr *github.PullRequest
	}
	type wants struct {
		expectStatus string
	}
	tests := []struct {
		name  string
		args  args
		wants wants
	}{
		{
			name: "promote draft",
			args: args{
				pr: &github.PullRequest{
					Number: github.Ptr(123),
					NodeID: github.Ptr("node123"),
					Draft:  github.Ptr(true),
					Head:   &github.PullRequestBranch{SHA: github.Ptr("sha123")},
				},
			},
			wants: wants{expectStatus: "Agent completed — PR marked ready"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := setupMockPoller(t, func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "/graphql") {
					w.WriteHeader(http.StatusOK)
					return
				}
				w.WriteHeader(http.StatusOK)
			})
			displayInfo := make(map[int]*poller.IssueDisplayInfo)
			manager := &poller.AISessionManager{Slots: 1, Active: make(map[int]bool)}
			err := p.ProcessDraftPR(ctx, tt.args.pr, 123, displayInfo, manager)
			assert.NoError(t, err)
			assert.Contains(t, displayInfo[123].Current, tt.wants.expectStatus)
		})
	}
}

func TestPoller_PromoteFromQueue(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	type args struct {
		codingIssues  []*github.Issue
		reviewIssues  []*github.Issue
		queueIssues   []*github.Issue
		maxConcurrent int
	}
	type wants struct {
		actions int
	}

	tests := []struct {
		name  string
		args  args
		wants wants
	}{
		{
			name: "promote within limits",
			args: args{
				codingIssues:  []*github.Issue{{Number: github.Ptr(1)}},
				reviewIssues:  []*github.Issue{{Number: github.Ptr(2)}},
				queueIssues:   []*github.Issue{{Number: github.Ptr(3)}, {Number: github.Ptr(4)}},
				maxConcurrent: 4,
			},
			wants: wants{
				actions: 2,
			},
		},
		{
			name: "already at limit",
			args: args{
				codingIssues:  []*github.Issue{{Number: github.Ptr(1)}},
				reviewIssues:  []*github.Issue{{Number: github.Ptr(2)}},
				queueIssues:   []*github.Issue{{Number: github.Ptr(3)}},
				maxConcurrent: 2,
			},
			wants: wants{
				actions: 0,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := setupMockPoller(t, func(w http.ResponseWriter, r *http.Request) {
				path := r.URL.Path
				switch {
				case strings.Contains(path, "/issues") && r.Method == http.MethodGet:
					if strings.Contains(r.URL.Query().Get("labels"), "ai-coding") {
						_ = json.NewEncoder(w).Encode(tt.args.codingIssues)
					} else if strings.Contains(r.URL.Query().Get("labels"), "ai-review") {
						_ = json.NewEncoder(w).Encode(tt.args.reviewIssues)
					} else {
						_ = json.NewEncoder(w).Encode(tt.args.queueIssues)
					}
				case strings.Contains(path, "/labels") && r.Method == http.MethodPost:
					w.WriteHeader(http.StatusOK)
				case strings.Contains(path, "/comments") && r.Method == http.MethodPost:
					w.WriteHeader(http.StatusCreated)
				case strings.Contains(path, "/pulls") && r.Method == http.MethodGet:
					_ = json.NewEncoder(w).Encode([]*github.PullRequest{})
				}
			})
			p.Cfg().MaxConcurrentIssues = tt.args.maxConcurrent
			manager := &poller.AISessionManager{Slots: tt.args.maxConcurrent, Active: make(map[int]bool)}

			err := p.PromoteFromQueue(ctx, tt.args.queueIssues, len(tt.args.codingIssues), len(tt.args.reviewIssues), manager)
			assert.NoError(t, err)
		})
	}
}

func TestPoller_Tick(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	mockHandler := func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		labels := r.URL.Query().Get("labels")
		switch {
		case strings.Contains(path, "/repos/test-owner/test-repo/issues"):
			var issues []*github.Issue
			if strings.Contains(labels, "ai-todo") {
				issues = []*github.Issue{
					{Number: github.Ptr(101), Title: github.Ptr("Queue Issue")},
				}
			} else if strings.Contains(labels, "ai-coding") {
				issues = []*github.Issue{{Number: github.Ptr(102), Title: github.Ptr("Coding Issue")}}
			} else if strings.Contains(labels, "ai-review") {
				issues = []*github.Issue{{Number: github.Ptr(103), Title: github.Ptr("Review Issue")}}
			}
			_ = json.NewEncoder(w).Encode(issues)
		case strings.Contains(path, "/pulls/102") && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(&github.PullRequest{Number: github.Ptr(102)})
		case strings.Contains(path, "/pulls/103") && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).
				Encode(&github.PullRequest{Number: github.Ptr(103), Head: &github.PullRequestBranch{SHA: github.Ptr("sha103")}})
		case strings.Contains(path, "/issues/101") && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(&github.Issue{Number: github.Ptr(101)})
		case strings.Contains(path, "/issues/102") && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(&github.Issue{Number: github.Ptr(102)})
		case strings.Contains(path, "/issues/103") && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(&github.Issue{Number: github.Ptr(103)})
		case strings.Contains(path, "/labels") && (r.Method == http.MethodDelete || r.Method == http.MethodPost):
			w.WriteHeader(http.StatusOK)
		default:
			if strings.Contains(r.URL.Path, "/search/issues") {
				_ = json.NewEncoder(w).Encode(&github.IssuesSearchResult{Total: github.Ptr(0)})
			} else {
				_ = json.NewEncoder(w).Encode([]interface{}{})
			}
		}
	}

	p := setupMockPoller(t, mockHandler)
	p.Tick(ctx)
}

func TestPoller_DrainCommands(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	p := setupMockPoller(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	p.Commands <- poller.Command{Action: "takeover", IssueNum: 123}
	p.Commands <- poller.Command{Action: "rerun-ci", PRNum: 456}

	p.DrainCommands(ctx)
	assert.Empty(t, p.Commands)
}

func TestPoller_ProcessCodingIssue(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	tests := []struct {
		name   string
		hasPR  bool
		active bool
		nudge  bool
	}{
		{name: "pr found", hasPR: true},
		{name: "agent active", hasPR: false, active: true},
		{name: "request nudge", hasPR: false, active: false, nudge: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := setupMockPoller(t, func(w http.ResponseWriter, r *http.Request) {
				path := r.URL.Path
				switch {
				case strings.Contains(path, "/pulls") && r.Method == http.MethodGet:
					if tt.hasPR {
						num := 99
						_ = json.NewEncoder(w).
							Encode([]*github.PullRequest{{Number: &num, State: github.Ptr("open")}})
					} else {
						_ = json.NewEncoder(w).Encode([]*github.PullRequest{})
					}
				case strings.Contains(path, "/comments") && r.Method == http.MethodGet:
					_ = json.NewEncoder(w).Encode([]*github.IssueComment{})
				case strings.Contains(path, "/labels") && (r.Method == http.MethodDelete || r.Method == http.MethodPost):
					w.WriteHeader(http.StatusOK)
				default:
					w.WriteHeader(http.StatusOK)
				}
			})
			issue := &github.Issue{
				Number: github.Ptr(123),
				Labels: []*github.Label{{Name: github.Ptr("ai-coding")}},
			}
			displayInfo := make(map[int]*poller.IssueDisplayInfo)
			manager := &poller.AISessionManager{Slots: 1, Active: make(map[int]bool)}
			p.ProcessCodingIssue(ctx, issue, displayInfo, manager)
		})
	}
}

func TestPoller_ProcessOne_NoPR(t *testing.T) {
	t.Parallel()
	type args struct {
		issue       *github.Issue
		displayInfo map[int]*poller.IssueDisplayInfo
	}
	type wants struct {
		err bool
	}
	tests := []struct {
		name  string
		args  args
		wants wants
	}{
		{
			name: "no pr found",
			args: args{
				issue:       &github.Issue{Number: github.Ptr(123)},
				displayInfo: make(map[int]*poller.IssueDisplayInfo),
			},
			wants: wants{err: false},
		},
		{
			name: "pr merged manually",
			args: args{
				issue: &github.Issue{
					Number: github.Ptr(124),
					Labels: []*github.Label{{Name: github.Ptr("ai-review")}},
				},
				displayInfo: make(map[int]*poller.IssueDisplayInfo),
			},
			wants: wants{err: false},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			p := setupMockPoller(t, func(w http.ResponseWriter, r *http.Request) {
				path := r.URL.Path
				switch {
				case strings.Contains(path, "/pulls") && r.Method == http.MethodGet:
					_ = json.NewEncoder(w).Encode([]*github.PullRequest{})
				case strings.Contains(path, "/issues") && r.Method == http.MethodGet:
					if strings.Contains(path, "/124") {
						_ = json.NewEncoder(w).Encode(&github.Issue{Number: github.Ptr(124)})
					} else {
						_ = json.NewEncoder(w).Encode([]*github.Issue{})
					}
				case strings.Contains(path, "/search/issues") && r.Method == http.MethodGet:
					if strings.Contains(r.URL.Query().Get("q"), "124") {
						_ = json.NewEncoder(w).Encode(&github.IssuesSearchResult{
							Total: github.Ptr(1),
							Issues: []*github.Issue{
								{
									Number:           github.Ptr(456),
									State:            github.Ptr("closed"),
									PullRequestLinks: &github.PullRequestLinks{},
								},
							},
						})
					} else {
						_ = json.NewEncoder(w).Encode(&github.IssuesSearchResult{Total: github.Ptr(0)})
					}
				case (strings.Contains(path, "/labels") || strings.Contains(path, "/comments")) && r.Method == http.MethodPost:
					w.WriteHeader(http.StatusOK)
				case strings.Contains(path, "/issues") && r.Method == http.MethodPatch:
					w.WriteHeader(http.StatusOK)
				}
			})
			displayInfo := make(map[int]*poller.IssueDisplayInfo)
			manager := &poller.AISessionManager{Slots: 1, Active: make(map[int]bool)}
			err := p.ProcessOne(ctx, tt.args.issue, displayInfo, manager)
			if tt.wants.err {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestPoller_DeduplicateIssueLists(t *testing.T) {
	t.Parallel()
	makeIssue := func(n int) *github.Issue { return &github.Issue{Number: &n} }
	type args struct {
		queue     []*github.Issue
		coding    []*github.Issue
		reviewing []*github.Issue
	}
	type wants struct {
		newCodingLen    int
		newReviewingLen int
		newCodingFirst  int
	}
	tests := []struct {
		name  string
		args  args
		wants wants
	}{
		{
			name: "basic deduplicate",
			args: args{
				queue:     []*github.Issue{makeIssue(1), makeIssue(2)},
				coding:    []*github.Issue{makeIssue(2), makeIssue(3), makeIssue(4)},
				reviewing: []*github.Issue{makeIssue(4), makeIssue(5)},
			},
			wants: wants{
				newCodingLen:    1,
				newReviewingLen: 2,
				newCodingFirst:  3,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			newCoding, newReviewing := poller.DeduplicateIssueLists(
				tt.args.queue,
				tt.args.coding,
				tt.args.reviewing,
			)
			assert.Len(t, newReviewing, tt.wants.newReviewingLen)
			assert.Len(t, newCoding, tt.wants.newCodingLen)
			if len(newCoding) > 0 {
				assert.Equal(t, tt.wants.newCodingFirst, newCoding[0].GetNumber())
			}
		})
	}
}

func TestPoller_Snapshot(t *testing.T) {
	t.Parallel()
	type args struct {
		displayInfo map[int]*poller.IssueDisplayInfo
	}
	type wants struct {
		queueNotNil  bool
		codingNotNil bool
		reviewNotNil bool
	}
	tests := []struct {
		name  string
		args  args
		wants wants
	}{
		{
			name: "display info snapshot",
			args: args{
				displayInfo: map[int]*poller.IssueDisplayInfo{
					123: {Current: "working", AgentStatus: "pending"},
				},
			},
			wants: wants{queueNotNil: true, codingNotNil: true, reviewNotNil: true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			p := setupMockPoller(t, func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode([]*github.Issue{})
			})
			queue, coding, review := p.Snapshot(ctx, tt.args.displayInfo)
			if tt.wants.queueNotNil {
				assert.NotNil(t, queue)
			}
			if tt.wants.codingNotNil {
				assert.NotNil(t, coding)
			}
			if tt.wants.reviewNotNil {
				assert.NotNil(t, review)
			}
		})
	}
}

func TestPoller_Tick_Complex(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	p := setupMockPoller(t, func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case strings.Contains(path, "/issues") && r.Method == http.MethodGet:
			if strings.Contains(r.URL.Query().Get("labels"), "ai-coding") {
				_ = json.NewEncoder(w).Encode([]*github.Issue{{Number: github.Ptr(10)}})
			} else if strings.Contains(r.URL.Query().Get("labels"), "ai-review") {
				_ = json.NewEncoder(w).Encode([]*github.Issue{{Number: github.Ptr(20)}})
			} else if strings.Contains(r.URL.Query().Get("labels"), "ai-command") {
				_ = json.NewEncoder(w).Encode([]*github.Issue{{Number: github.Ptr(30)}})
			} else {
				_ = json.NewEncoder(w).Encode([]*github.Issue{})
			}
		case strings.Contains(path, "/pulls") && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode([]*github.PullRequest{})
		default:
			if strings.Contains(r.URL.Path, "/search/issues") {
				_ = json.NewEncoder(w).Encode(&github.IssuesSearchResult{Total: github.Ptr(0)})
			} else if strings.Contains(r.URL.Path, "/issues/10/comments") {
				_ = json.NewEncoder(w).Encode([]*github.IssueComment{})
			} else if strings.Contains(r.URL.Path, "/pulls") {
				_ = json.NewEncoder(w).Encode([]*github.PullRequest{})
			} else {
				_ = json.NewEncoder(w).Encode([]interface{}{})
			}
		}
	})

	p.Tick(ctx)
}
func TestPoller_ProcessCodingPR_TableDriven(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	type args struct {
		issue       *github.Issue
		mockHandler http.HandlerFunc
	}
	type wants struct {
		err bool
	}
	tests := []struct {
		name  string
		args  args
		wants wants
	}{
		{
			name: "agent active - no nudge",
			args: args{
				issue: &github.Issue{
					Number:    github.Ptr(123),
					Title:     github.Ptr("Fix bug"),
					CreatedAt: &github.Timestamp{Time: time.Now().Add(-1 * time.Hour)},
					Labels:    []*github.Label{{Name: github.Ptr("ai-coding")}},
				},
				mockHandler: func(w http.ResponseWriter, r *http.Request) {
					path := r.URL.Path
					switch {
					case strings.Contains(path, "/labels/ai-coding") && r.Method == http.MethodGet:
						_ = json.NewEncoder(w).Encode(&github.Label{
							Name: github.Ptr("ai-coding"),
						})
					case strings.Contains(path, "/events") && r.Method == http.MethodGet:
						// Coding label event 1 hour ago
						_ = json.NewEncoder(w).Encode([]*github.IssueEvent{
							{
								Event:     github.Ptr("labeled"),
								Label:     &github.Label{Name: github.Ptr("ai-coding")},
								CreatedAt: &github.Timestamp{Time: time.Now().Add(-1 * time.Hour)},
							},
						})
					case strings.Contains(path, "/comments") && r.Method == http.MethodGet:
						_ = json.NewEncoder(w).Encode([]*github.IssueComment{})
					case strings.Contains(path, "/copilot/jobs") && r.Method == http.MethodGet:
						_ = json.NewEncoder(w).Encode(&github.Jobs{Jobs: []*github.WorkflowJob{
							{ID: github.Ptr(int64(1)), Status: github.Ptr("in_progress")},
						}})
					case strings.Contains(path, "/pulls") && r.Method == http.MethodGet:
						_ = json.NewEncoder(w).Encode([]*github.PullRequest{})
					}
				},
			},
			wants: wants{err: false},
		},
		{
			name: "should nudge - timeout reached",
			args: args{
				issue: &github.Issue{
					Number:    github.Ptr(124),
					Title:     github.Ptr("Refactor"),
					CreatedAt: &github.Timestamp{Time: time.Now().Add(-1 * time.Hour)},
					Labels:    []*github.Label{{Name: github.Ptr("ai-coding")}},
				},
				mockHandler: func(w http.ResponseWriter, r *http.Request) {
					path := r.URL.Path
					switch {
					case strings.Contains(path, "/labels/ai-coding") && r.Method == http.MethodGet:
						_ = json.NewEncoder(w).Encode(&github.Label{
							Name: github.Ptr("ai-coding"),
						})
					case strings.Contains(path, "/events") && r.Method == http.MethodGet:
						_ = json.NewEncoder(w).Encode([]*github.IssueEvent{
							{
								Event:     github.Ptr("labeled"),
								Label:     &github.Label{Name: github.Ptr("ai-coding")},
								CreatedAt: &github.Timestamp{Time: time.Now().Add(-2 * time.Hour)},
							},
						})
					case strings.Contains(path, "/comments") && r.Method == http.MethodGet:
						_ = json.NewEncoder(w).Encode([]*github.IssueComment{})
					case strings.Contains(path, "/copilot/jobs") && r.Method == http.MethodGet:
						_ = json.NewEncoder(w).Encode(&github.Jobs{Jobs: []*github.WorkflowJob{}})
					case strings.Contains(path, "/pulls") && r.Method == http.MethodGet:
						_ = json.NewEncoder(w).Encode([]*github.PullRequest{})
					case strings.Contains(path, "/copilot/invoke") && r.Method == http.MethodPost:
						w.WriteHeader(http.StatusCreated)
						_ = json.NewEncoder(w).Encode(map[string]string{"id": "new-job"})
					}
				},
			},
			wants: wants{err: false},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := setupMockPoller(t, tt.args.mockHandler)
			displayInfo := make(map[int]*poller.IssueDisplayInfo)
			// Mock PR for coding path
			pr := &github.PullRequest{
				Number: tt.args.issue.Number,
				Head:   &github.PullRequestBranch{SHA: github.Ptr("sha")},
			}
			manager := &poller.AISessionManager{Slots: 1, Active: make(map[int]bool)}
			err := p.ProcessCodingPR(ctx, pr, tt.args.issue.GetNumber(), displayInfo, manager)
			if tt.wants.err {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestPoller_ProcessOne(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	type args struct {
		issue       *github.Issue
		mockHandler http.HandlerFunc
	}
	type wants struct {
		err bool
	}
	tests := []struct {
		name  string
		args  args
		wants wants
	}{
		{
			name: "Review issue with merged PR",
			args: args{
				issue: &github.Issue{
					Number: github.Ptr(126),
					Title:  github.Ptr("Feature"),
				},
				mockHandler: func(w http.ResponseWriter, r *http.Request) {
					path := r.URL.Path
					switch {
					case strings.Contains(path, "/pulls/126") && r.Method == http.MethodGet:
						_ = json.NewEncoder(w).Encode(&github.PullRequest{
							Number:    github.Ptr(126),
							Mergeable: github.Ptr(true),
							Head:      &github.PullRequestBranch{SHA: github.Ptr("sha126")},
						})
					case strings.Contains(path, "/check-runs") && r.Method == http.MethodGet:
						_ = json.NewEncoder(w).
							Encode(&github.ListCheckRunsResults{CheckRuns: []*github.CheckRun{}})
					case strings.Contains(path, "/reviews") && r.Method == http.MethodGet:
						_ = json.NewEncoder(w).Encode([]*github.PullRequestReview{})
					case strings.Contains(path, "/comments") && r.Method == http.MethodGet:
						_ = json.NewEncoder(w).Encode([]*github.IssueComment{})
					}
				},
			},
			wants: wants{err: false},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := setupMockPoller(t, tt.args.mockHandler)
			displayInfo := make(map[int]*poller.IssueDisplayInfo)
			manager := &poller.AISessionManager{Slots: 1, Active: make(map[int]bool)}
			err := p.ProcessOne(ctx, tt.args.issue, displayInfo, manager)
			if tt.wants.err {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestPoller_HandleCommand(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	type args struct {
		cmd         poller.Command
		mockHandler http.HandlerFunc
	}
	type wants struct{}

	tests := []struct {
		name  string
		args  args
		wants wants
	}{
		{
			name: "retry-merge",
			args: args{
				cmd: poller.Command{Action: "retry-merge", PRNum: 123},
				mockHandler: func(w http.ResponseWriter, r *http.Request) {
					if strings.Contains(r.URL.Path, "/issues/123/comments") &&
						r.Method == http.MethodGet {
						_ = json.NewEncoder(w).Encode([]*github.IssueComment{
							{
								ID:   github.Ptr(int64(456)),
								Body: github.Ptr("Merge resolution failed"),
							},
						})
					}
					if strings.Contains(r.URL.Path, "/issues/comments/456") &&
						r.Method == http.MethodDelete {
						w.WriteHeader(http.StatusNoContent)
					}
				},
			},
			wants: wants{},
		},
		{
			name: "retry-merge with success marker",
			args: args{
				cmd: poller.Command{Action: "retry-merge", PRNum: 123},
				mockHandler: func(w http.ResponseWriter, r *http.Request) {
					if strings.Contains(r.URL.Path, "/issues/123/comments") &&
						r.Method == http.MethodGet {
						_ = json.NewEncoder(w).Encode([]*github.IssueComment{
							{
								ID:   github.Ptr(int64(789)),
								Body: github.Ptr("copilot-autodev:local-resolution-success:sha123"),
							},
						})
					}
					if strings.Contains(r.URL.Path, "/issues/comments/789") &&
						r.Method == http.MethodDelete {
						w.WriteHeader(http.StatusNoContent)
					}
				},
			},
			wants: wants{},
		},
		{
			name: "takeover",
			args: args{
				cmd: poller.Command{Action: "takeover", IssueNum: 456},
				mockHandler: func(w http.ResponseWriter, r *http.Request) {
					if strings.Contains(r.URL.Path, "/issues/456/labels") &&
						r.Method == http.MethodPost {
						w.WriteHeader(http.StatusOK)
					}
					if strings.Contains(r.URL.Path, "/issues/456/labels") &&
						r.Method == http.MethodDelete {
						w.WriteHeader(http.StatusOK)
					}
				},
			},
			wants: wants{},
		},
		{
			name: "rerun-ci",
			args: args{
				cmd: poller.Command{Action: "rerun-ci", PRNum: 123},
				mockHandler: func(w http.ResponseWriter, r *http.Request) {
					if strings.Contains(r.URL.Path, "/pulls/123") && r.Method == http.MethodGet {
						_ = json.NewEncoder(w).
							Encode(&github.PullRequest{Head: &github.PullRequestBranch{SHA: github.Ptr("sha")}})
					}
					if strings.Contains(r.URL.Path, "/commits/sha/check-runs") {
						_ = json.NewEncoder(w).
							Encode(&github.ListCheckRunsResults{CheckRuns: []*github.CheckRun{{ID: github.Ptr(int64(789))}}})
					}
					if strings.Contains(r.URL.Path, "/check-runs/789/rerequest") &&
						r.Method == http.MethodPost {
						w.WriteHeader(http.StatusCreated)
					}
				},
			},
			wants: wants{},
		},
		{
			name: "priority-up",
			args: args{
				cmd:         poller.Command{Action: "priority-up", IssueNum: 111},
				mockHandler: func(w http.ResponseWriter, r *http.Request) {},
			},
			wants: wants{},
		},
		{
			name: "priority-down",
			args: args{
				cmd:         poller.Command{Action: "priority-down", IssueNum: 111},
				mockHandler: func(w http.ResponseWriter, r *http.Request) {},
			},
			wants: wants{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := setupMockPoller(t, tt.args.mockHandler)
			p.HandleCommand(ctx, tt.args.cmd)
		})
	}
}

func TestPoller_NudgeSingleCodingIssue(t *testing.T) {
	t.Parallel()

	type args struct {
		issue       *github.Issue
		mockHandler func(w http.ResponseWriter, r *http.Request)
	}
	type wants struct {
		expectError  bool
		expectStatus string
	}
	tests := []struct {
		name  string
		args  args
		wants wants
	}{
		{
			name: "no coding label yet",
			args: args{
				issue: &github.Issue{Number: github.Ptr(123)},
				mockHandler: func(w http.ResponseWriter, r *http.Request) {
					if strings.Contains(r.URL.Path, "/issues/123/events") {
						_ = json.NewEncoder(w).Encode([]*github.IssueEvent{})
					}
				},
			},
			wants: wants{
				expectStatus: "Agent assigned, awaiting PR",
				expectError:  false,
			},
		},
		{
			name: "waiting for timeout",
			args: args{
				issue: &github.Issue{Number: github.Ptr(123)},
				mockHandler: func(w http.ResponseWriter, r *http.Request) {
					if strings.Contains(r.URL.Path, "/issues/123/events") {
						_ = json.NewEncoder(w).Encode([]*github.IssueEvent{
							{
								Event:     github.Ptr("labeled"),
								Label:     &github.Label{Name: github.Ptr("ai-coding")},
								CreatedAt: &github.Timestamp{Time: time.Now()}, // Just labeled
							},
						})
					} else if strings.Contains(r.URL.Path, "/issues/123/comments") {
						_ = json.NewEncoder(w).Encode([]*github.IssueComment{})
					}
				},
			},
			wants: wants{
				expectStatus: "Agent assigned",
				expectError:  false,
			},
		},
		{
			name: "agent exhausted retries",
			args: args{
				issue: &github.Issue{Number: github.Ptr(123)},
				mockHandler: func(w http.ResponseWriter, r *http.Request) {
					if strings.Contains(r.URL.Path, "/issues/123/events") {
						_ = json.NewEncoder(w).Encode([]*github.IssueEvent{
							{
								Event:     github.Ptr("labeled"),
								Label:     &github.Label{Name: github.Ptr("ai-coding")},
								CreatedAt: &github.Timestamp{Time: time.Now().Add(-2 * time.Hour)},
							},
						})
					} else if strings.Contains(r.URL.Path, "/issues/123/comments") {
						if r.Method == http.MethodGet {
							// fake 5 API retries
							comments := make([]*github.IssueComment, 5)
							for i := 0; i < 5; i++ {
								comments[i] = &github.IssueComment{
									Body:      github.Ptr(ghclient.CopilotNudgeCommentMarker),
									CreatedAt: &github.Timestamp{Time: time.Now().Add(-1 * time.Hour)},
									User:      &github.User{Login: github.Ptr("bot")},
								}
							}
							_ = json.NewEncoder(w).Encode(comments)
						} else if r.Method == http.MethodPost {
							w.WriteHeader(http.StatusCreated)
						}
					} else if strings.Contains(r.URL.Path, "/issues/123") && r.Method == http.MethodPatch {
						w.WriteHeader(http.StatusOK)
					}
				},
			},
			wants: wants{
				expectStatus: "No response after 5 nudge(s) — returning to queue",
				expectError:  false,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := setupMockPoller(t, tt.args.mockHandler)
			displayInfo := make(map[int]*poller.IssueDisplayInfo)

			manager := &poller.AISessionManager{Slots: 1, Active: make(map[int]bool)}
			err := p.NudgeSingleCodingIssue(t.Context(), tt.args.issue, displayInfo, 10*time.Second, manager)
			if tt.wants.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				if tt.wants.expectStatus != "" {
					assert.Contains(t, displayInfo[123].Current, tt.wants.expectStatus)
				}
			}
		})
	}
}

func TestPoller_HandleAgentTimeout(t *testing.T) {
	t.Parallel()

	type args struct {
		pr          *github.PullRequest
		num         int
		lastAction  time.Time
		cfg         poller.AgentTimeoutCfg
		displayInfo map[int]*poller.IssueDisplayInfo
	}
	type wants struct {
		handled      bool
		expectStatus string
	}
	tests := []struct {
		name  string
		args  args
		wants wants
	}{
		{
			name: "timeout not reached",
			args: args{
				pr:         &github.PullRequest{Number: github.Ptr(123)},
				num:        123,
				lastAction: time.Now(),
				cfg: poller.AgentTimeoutCfg{
					CountFn: func(ctx context.Context, num int) (int, error) {
						return 3, nil
					},
					PromptKind:   "Test Prompt",
					NoticeFormat: "Warning %d",
					StatusVerb:   "failures",
				},
				displayInfo: make(map[int]*poller.IssueDisplayInfo),
			},
			wants: wants{handled: false},
		},
		{
			name: "timeout reached retries exhausted",
			args: args{
				pr:         &github.PullRequest{Number: github.Ptr(123)},
				num:        123,
				lastAction: time.Now().Add(-2 * time.Hour),
				cfg: poller.AgentTimeoutCfg{
					CountFn: func(ctx context.Context, num int) (int, error) {
						return 3, nil
					},
					PromptKind:   "Test Prompt",
					NoticeFormat: "Warning %d",
					StatusVerb:   "failures",
				},
				displayInfo: make(map[int]*poller.IssueDisplayInfo),
			},
			wants: wants{handled: true, expectStatus: "Agent unresponsive"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := setupMockPoller(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			})
			handled := p.HandleAgentTimeout(
				t.Context(),
				tt.args.pr,
				tt.args.num,
				tt.args.lastAction,
				tt.args.cfg,
				tt.args.displayInfo,
			)
			assert.Equal(t, tt.wants.handled, handled)
			if tt.wants.expectStatus != "" {
				assert.Contains(t, tt.args.displayInfo[123].Current, tt.wants.expectStatus)
			}
		})
	}
}

func TestPoller_HandleNudgeExhaustion(t *testing.T) {
	t.Parallel()
	type args struct {
		num         int
		retries     int
		displayInfo map[int]*poller.IssueDisplayInfo
	}
	type wants struct {
		err          bool
		expectStatus string
	}
	tests := []struct {
		name  string
		args  args
		wants wants
	}{
		{
			name:  "valid nudge exhaustion",
			args:  args{num: 123, retries: 5, displayInfo: make(map[int]*poller.IssueDisplayInfo)},
			wants: wants{err: false, expectStatus: "No response after 5 nudge(s)"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := setupMockPoller(
				t,
				func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) },
			)
			err := p.HandleNudgeExhaustion(
				t.Context(),
				tt.args.num,
				tt.args.retries,
				tt.args.displayInfo,
			)
			if tt.wants.err {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
			assert.Contains(t, tt.args.displayInfo[tt.args.num].Current, tt.wants.expectStatus)
		})
	}
}

func TestPoller_BuildCIFailureSection(t *testing.T) {
	t.Parallel()
	type args struct {
		workflowName string
		failedJobs   []ghclient.FailedJobInfo
	}
	type wants struct {
		contains    []string
		notContains []string
	}
	tests := []struct {
		name  string
		args  args
		wants wants
	}{
		{
			name: "valid job failure",
			args: args{
				workflowName: "test-workflow",
				failedJobs:   []ghclient.FailedJobInfo{{Name: "job1"}},
			},
			wants: wants{contains: []string{"test-workflow", "job1"}},
		},
		{
			name:  "no workflow name",
			args:  args{workflowName: "", failedJobs: nil},
			wants: wants{notContains: []string{"test-workflow"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			res := poller.BuildCIFailureSection(tt.args.workflowName, tt.args.failedJobs)
			for _, s := range tt.wants.contains {
				assert.Contains(t, res, s)
			}
			for _, s := range tt.wants.notContains {
				assert.NotContains(t, res, s)
			}
		})
	}
}

func TestPoller_ProcessOne_MissingPR(t *testing.T) {
	t.Parallel()

	type args struct {
		issue       *github.Issue
		displayInfo map[int]*poller.IssueDisplayInfo
	}
	type wants struct {
		err          bool
		expectStatus string
	}
	tests := []struct {
		name  string
		args  args
		wants wants
	}{
		{
			name: "missing pr",
			args: args{
				issue:       &github.Issue{Number: github.Ptr(123)},
				displayInfo: make(map[int]*poller.IssueDisplayInfo),
			},
			wants: wants{err: false, expectStatus: "No PR found"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := setupMockPoller(t, func(w http.ResponseWriter, r *http.Request) {
				// Mock OpenPRForIssue
				if strings.Contains(r.URL.Path, "/pulls") {
					_ = json.NewEncoder(w).Encode([]*github.PullRequest{}) // no PR
				} else if strings.Contains(r.URL.Path, "/events") {
					_ = json.NewEncoder(w).Encode([]*github.IssueEvent{}) // no merge events
				} else if strings.Contains(r.URL.Path, "/issues/123/labels") && r.Method == http.MethodDelete {
					w.WriteHeader(http.StatusOK)
				} else if strings.Contains(r.URL.Path, "/issues/123/labels") && r.Method == http.MethodPost {
					w.WriteHeader(http.StatusOK)
				}
			})
			manager := &poller.AISessionManager{Slots: 1, Active: make(map[int]bool)}
			err := p.ProcessOne(t.Context(), tt.args.issue, tt.args.displayInfo, manager)
			if tt.wants.err {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
			assert.Contains(
				t,
				tt.args.displayInfo[tt.args.issue.GetNumber()].Current,
				tt.wants.expectStatus,
			)
		})
	}
}
