//nolint:gocritic,goimports
package agent_test

import (
	"context"
	"encoding/json"
	"errors"
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

type mockRunner struct {
	runFunc    func(ctx context.Context, dir, token, name string, args ...string) error
	outputFunc func(ctx context.Context, dir, name string, args ...string) (string, error)
	calls      []string
}

func (m *mockRunner) Run(ctx context.Context, dir, token, name string, args ...string) error {
	m.calls = append(m.calls, name+" "+strings.Join(args, " "))
	if m.runFunc != nil {
		return m.runFunc(ctx, dir, token, name, args...)
	}
	return nil
}

func (m *mockRunner) Output(ctx context.Context, dir, name string, args ...string) (string, error) {
	m.calls = append(m.calls, name+" "+strings.Join(args, " "))
	if m.outputFunc != nil {
		return m.outputFunc(ctx, dir, name, args...)
	}
	return "", nil
}

func TestCLIAgent_IsActive(t *testing.T) {
	t.Parallel()
	ag := agent.NewCLIAgent(nil, nil, "token")
	assert.False(t, ag.IsActive(context.Background(), 123))
}

func TestCLIAgent_InvokeTask_Simple(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	cfg := config.DefaultConfig()
	cfg.GitHubOwner = "owner"
	cfg.GitHubRepo = "repo"
	cfg.CLIAgentCmd = "echo"

	// Mock ghclient to return an issue
	rt := &fakeRoundTripper{
		handler: func(r *http.Request) (*http.Response, error) {
			rec := httptest.NewRecorder()
			_ = json.NewEncoder(rec).
				Encode(&github.Issue{Body: github.Ptr("issue body"), Number: github.Ptr(123)})
			return rec.Result(), nil
		},
	}
	client := ghclient.NewWithTransport("token", cfg, rt)

	runner := &mockRunner{}
	ag := agent.NewCLIAgentWithRunner(client, cfg, "token", runner)

	id, err := ag.InvokeTask(ctx, "prompt", "title", 123, "url")
	require.NoError(t, err)
	assert.Equal(t, "copilot-autodev/issue-123", id)
}

func TestCLIAgent_InvokeTask_Async(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	cfg := config.DefaultConfig()
	cfg.GitHubOwner = "owner"
	cfg.GitHubRepo = "repo"
	cfg.CLIAgentCmd = "echo"

	rt := &fakeRoundTripper{
		handler: func(r *http.Request) (*http.Response, error) {
			rec := httptest.NewRecorder()
			_ = json.NewEncoder(rec).Encode(&github.Issue{
				Number: github.Ptr(123),
				Title:  github.Ptr("Title"),
				Body:   github.Ptr("Body"),
			})
			return rec.Result(), nil
		},
	}
	client := ghclient.NewWithTransport("token", cfg, rt)

	runner := &mockRunner{
		outputFunc: func(ctx context.Context, dir, name string, args ...string) (string, error) {
			if name == "git" && args[0] == "rev-parse" {
				return "sha123", nil
			}
			return "", nil
		},
	}
	ag := agent.NewCLIAgentWithRunner(client, cfg, "token", runner)

	id, err := ag.InvokeTask(ctx, "prompt", "title", 123, "url")
	require.NoError(t, err)
	assert.Equal(t, "copilot-autodev/issue-123", id)

	// Poll IsActive until it's false (goroutine finished)
	assert.Eventually(t, func() bool {
		return !ag.IsActive(ctx, 123)
	}, 2*time.Second, 10*time.Millisecond)

	// Check status
	ts, err := ag.GetTaskStatus(ctx, "copilot-autodev/issue-123")
	require.NoError(t, err)
	assert.Equal(t, "completed", ts.Status)
}

func TestCLIAgent_InvokeTask_Failures(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	tests := []struct {
		name    string
		runFunc func(ctx context.Context, dir, token, name string, args ...string) error
	}{
		{
			name: "clone failure",
			runFunc: func(ctx context.Context, dir, token, name string, args ...string) error {
				if name == "git" && args[0] == "clone" {
					return errors.New("clone failed")
				}
				return nil
			},
		},
		{
			name: "checkout failure",
			runFunc: func(ctx context.Context, dir, token, name string, args ...string) error {
				if name == "git" && args[0] == "checkout" {
					return errors.New("checkout failed")
				}
				return nil
			},
		},
		{
			name: "add failure",
			runFunc: func(ctx context.Context, dir, token, name string, args ...string) error {
				if name == "git" && args[0] == "add" {
					return errors.New("add failed")
				}
				return nil
			},
		},
		{
			name: "commit failure",
			runFunc: func(ctx context.Context, dir, token, name string, args ...string) error {
				if name == "git" && args[0] == "commit" {
					return errors.New("commit failed")
				}
				return nil
			},
		},
		{
			name: "pr creation failure",
			runFunc: func(ctx context.Context, dir, token, name string, args ...string) error {
				return nil
			},
		},
		{
			name: "placeholder injection",
			runFunc: func(ctx context.Context, dir, token, name string, args ...string) error {
				// We just want to see it run
				return nil
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			localCfg := config.DefaultConfig()
			if tt.name == "placeholder injection" {
				localCfg.CLIAgentArgs = []string{"--prompt", "{prompt}"}
			}
			rt := &fakeRoundTripper{
				handler: func(r *http.Request) (*http.Response, error) {
					rec := httptest.NewRecorder()
					if tt.name == "pr creation failure" && strings.Contains(r.URL.Path, "/pulls") &&
						r.Method == http.MethodPost {
						rec.WriteHeader(http.StatusUnprocessableEntity)
						_ = json.NewEncoder(rec).
							Encode(map[string]string{"message": "Validation Failed"})
					} else {
						_ = json.NewEncoder(rec).Encode(&github.Issue{Number: github.Ptr(123)})
					}
					return rec.Result(), nil
				},
			}
			client := ghclient.NewWithTransport("token", localCfg, rt)
			runner := &mockRunner{
				runFunc: tt.runFunc,
				outputFunc: func(ctx context.Context, dir, name string, args ...string) (string, error) {
					if name == "git" && args[0] == "rev-parse" {
						return "sha123", nil
					}
					if name == "git" && args[0] == "diff" {
						if tt.name == "pr creation failure" {
							return "file.go", nil
						}
						return "", nil
					}
					return "", nil
				},
			}
			ag := agent.NewCLIAgentWithRunner(client, localCfg, "token", runner)

			_, err := ag.InvokeTask(ctx, "prompt", "title", 123, "url")
			require.NoError(t, err)

			assert.Eventually(t, func() bool {
				return !ag.IsActive(ctx, 123)
			}, 1*time.Second, 10*time.Millisecond)
		})
	}
}

func TestCLIAgent_HasRespondedSince(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	cfg := config.DefaultConfig()

	// Mock PR commits
	rt := &fakeRoundTripper{
		handler: func(r *http.Request) (*http.Response, error) {
			rec := httptest.NewRecorder()
			_ = json.NewEncoder(rec).Encode([]*github.RepositoryCommit{
				{
					Commit: &github.Commit{
						Committer: &github.CommitAuthor{Date: &github.Timestamp{Time: time.Now()}},
					},
				},
			})
			return rec.Result(), nil
		},
	}
	client := ghclient.NewWithTransport("token", cfg, rt)
	ag := agent.NewCLIAgent(client, cfg, "token")

	has, err := ag.HasRespondedSince(ctx, 123, time.Now().Add(-1*time.Hour))
	require.NoError(t, err)
	assert.True(t, has)
}

func TestCLIAgent_SendPrompt_Error(t *testing.T) {
	t.Parallel()
	ag := agent.NewCLIAgent(nil, nil, "token")
	err := ag.SendPrompt(context.Background(), agent.PromptRequest{PRNum: 0})
	assert.Error(t, err)
}

func TestCLIAgent_SendPrompt_SuccessBranches(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	tests := []struct {
		name         string
		promptType   string
		diffOut      string
		wantContains []string
		wantAbsent   []string
	}{
		{
			name:       "merge conflict prompt includes fetch/merge",
			promptType: "merge-conflict",
			diffOut:    "changed.go",
			wantContains: []string{
				"git fetch origin main",
				"git merge --no-edit FETCH_HEAD",
				"git commit -m chore: merge-conflict via CLI agent",
				"git push origin feature-branch",
			},
		},
		{
			name:       "no staged changes returns without commit",
			promptType: "continue",
			diffOut:    "",
			wantAbsent: []string{
				"git commit -m",
				"git push origin feature-branch",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := config.DefaultConfig()
			cfg.GitHubOwner = "owner"
			cfg.GitHubRepo = "repo"

			rt := &fakeRoundTripper{
				handler: func(r *http.Request) (*http.Response, error) {
					rec := httptest.NewRecorder()
					if strings.Contains(r.URL.Path, "/pulls/456") && r.Method == http.MethodGet {
						_ = json.NewEncoder(rec).Encode(&github.PullRequest{
							Number: github.Ptr(456),
							Head:   &github.PullRequestBranch{Ref: github.Ptr("feature-branch")},
							Base:   &github.PullRequestBranch{Ref: github.Ptr("main")},
						})
						return rec.Result(), nil
					}
					rec.WriteHeader(http.StatusOK)
					return rec.Result(), nil
				},
			}

			client := ghclient.NewWithTransport("token", cfg, rt)
			runner := &mockRunner{
				runFunc: func(_ context.Context, _, _, _ string, _ ...string) error {
					return nil
				},
				outputFunc: func(_ context.Context, _, name string, args ...string) (string, error) {
					if name == "git" && args[0] == "rev-parse" {
						return "same-sha", nil
					}
					if name == "git" && args[0] == "diff" {
						return tt.diffOut, nil
					}
					return "", nil
				},
			}

			ag := agent.NewCLIAgentWithRunner(client, cfg, "token", runner)
			err := ag.SendPrompt(
				ctx,
				agent.PromptRequest{PRNum: 456, PromptType: tt.promptType, Body: "please fix"},
			)
			require.NoError(t, err)

			joined := strings.Join(runner.calls, "\n")
			for _, want := range tt.wantContains {
				assert.Contains(t, joined, want)
			}
			for _, absent := range tt.wantAbsent {
				assert.NotContains(t, joined, absent)
			}
		})
	}
}

func TestRealRunner(t *testing.T) {
	t.Parallel()
	rr := &agent.RealRunner{}
	ctx := t.Context()

	// Success case
	err := rr.Run(ctx, ".", "", "echo", "hello")
	require.NoError(t, err)

	// Fallback case (command not in path)
	err = rr.Run(ctx, ".", "", "non-existent-command-123", "args")
	require.Error(t, err)

	// Output
	out, err := rr.Output(ctx, ".", "echo", "hello")
	require.NoError(t, err)
	assert.Contains(t, out, "hello")
}

func TestCLIAgent_DiscoverPR(t *testing.T) {
	t.Parallel()
	ag := agent.NewCLIAgent(nil, nil, "token")
	pr, err := ag.DiscoverPR(t.Context(), 123)
	require.NoError(t, err)
	assert.Nil(t, pr)
}
