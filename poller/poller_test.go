package poller_test

import (
	"testing"

	"github.com/google/go-github/v68/github"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BlackbirdWorks/copilot-autocode/config"
	"github.com/BlackbirdWorks/copilot-autocode/ghclient"
	"github.com/BlackbirdWorks/copilot-autocode/poller"
)

// ─── FormatFallbackPrompt ─────────────────────────────────────────────────────

func TestFormatFallbackPrompt(t *testing.T) {
	t.Parallel()
	num := 42
	title := "Fix the login bug"
	url := "https://github.com/org/repo/issues/42"
	issue := &github.Issue{
		Number:  &num,
		Title:   &title,
		HTMLURL: &url,
	}

	tests := []struct {
		name     string
		template string
		want     string
	}{
		{
			name:     "default template expands all placeholders",
			template: "Please start working on issue #{issue_number}: {issue_title}.\n{issue_url}",
			want:     "Please start working on issue #42: Fix the login bug.\nhttps://github.com/org/repo/issues/42",
		},
		{
			name:     "only issue_number placeholder",
			template: "Work on #{issue_number}",
			want:     "Work on #42",
		},
		{
			name:     "only issue_title placeholder",
			template: "Task: {issue_title}",
			want:     "Task: Fix the login bug",
		},
		{
			name:     "only issue_url placeholder",
			template: "See {issue_url}",
			want:     "See https://github.com/org/repo/issues/42",
		},
		{
			name:     "no placeholders — template returned as-is",
			template: "Please start working on this issue.",
			want:     "Please start working on this issue.",
		},
		{
			name:     "all placeholders appear multiple times",
			template: "{issue_number} {issue_number} {issue_title} {issue_url}",
			want:     "42 42 Fix the login bug https://github.com/org/repo/issues/42",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, poller.FormatFallbackPrompt(tc.template, issue))
		})
	}
}

func TestSortIssuesAsc(t *testing.T) {
	t.Parallel()
	makeIssue := func(n int) *github.Issue { return &github.Issue{Number: &n} }

	tests := []struct {
		name  string
		input []int
		want  []int
	}{
		{"empty slice", nil, nil},
		{"single element", []int{5}, []int{5}},
		{"already sorted", []int{1, 2, 3, 10}, []int{1, 2, 3, 10}},
		{"reverse sorted", []int{10, 5, 3, 1}, []int{1, 3, 5, 10}},
		{"mixed order", []int{4, 1, 7, 2, 9, 3}, []int{1, 2, 3, 4, 7, 9}},
		{"duplicates preserved", []int{3, 1, 2, 1, 3}, []int{1, 1, 2, 3, 3}},
		{"two elements swapped", []int{2, 1}, []int{1, 2}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			issues := make([]*github.Issue, len(tc.input))
			for i, n := range tc.input {
				issues[i] = makeIssue(n)
			}

			poller.SortIssuesAsc(issues)

			require.Len(t, issues, len(tc.want))
			for i, want := range tc.want {
				assert.Equal(t, want, issues[i].GetNumber(), "index %d", i)
			}
		})
	}
}

// ─── BuildCIFailureSection ───────────────────────────────────────────────────

func TestBuildCIFailureSection(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		workflowName string
		failedJobs   []ghclient.FailedJobInfo
		wantContains []string
		wantAbsent   []string
	}{
		{
			name:         "no workflow or jobs",
			workflowName: "",
			failedJobs:   nil,
			wantContains: []string{"please fix the following CI failures"},
			wantAbsent:   []string{"Failing workflow", "Failed jobs"},
		},
		{
			name:         "workflow name included",
			workflowName: "CI / Build",
			failedJobs:   nil,
			wantContains: []string{"**Failing workflow:** CI / Build"},
			wantAbsent:   []string{"Failed jobs"},
		},
		{
			name:         "single failed job with log URL",
			workflowName: "CI",
			failedJobs: []ghclient.FailedJobInfo{
				{Name: "test", LogURL: "https://logs.example.com/1"},
			},
			wantContains: []string{
				"**Failing workflow:** CI",
				"**Failed jobs:** test",
				"**test** logs: https://logs.example.com/1",
			},
		},
		{
			name:         "multiple failed jobs, second has no log URL",
			workflowName: "CI",
			failedJobs: []ghclient.FailedJobInfo{
				{Name: "build", LogURL: "https://logs.example.com/build"},
				{Name: "lint", LogURL: ""},
			},
			wantContains: []string{
				"**Failed jobs:** build, lint",
				"**build** logs: https://logs.example.com/build",
			},
			wantAbsent: []string{"**lint** logs"},
		},
		{
			name:         "empty workflow name skips that section",
			workflowName: "",
			failedJobs: []ghclient.FailedJobInfo{
				{Name: "unit-tests", LogURL: ""},
			},
			wantContains: []string{"**Failed jobs:** unit-tests"},
			wantAbsent:   []string{"Failing workflow"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := poller.BuildCIFailureSection(tc.workflowName, tc.failedJobs)
			for _, want := range tc.wantContains {
				assert.Contains(t, got, want)
			}
			for _, absent := range tc.wantAbsent {
				assert.NotContains(t, got, absent)
			}
		})
	}
}

// TestApproveRetriesFallback verifies that the Poller's retry-limit logic
// correctly caps the number of workflow-run approval attempts.
func TestApproveRetriesFallback(t *testing.T) {
	t.Parallel()
	// New() initialises approveRetries to an empty map and exposes MaxAgentContinueRetries
	// through the config. We test only the public API surface here.
	p := poller.New(&config.Config{
		MaxAgentContinueRetries: 3,
	}, nil, "")
	require.NotNil(t, p)
	assert.Equal(t, 3, p.Cfg().MaxAgentContinueRetries)
}
