package resolver_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BlackbirdWorks/copilot-autocode/config"
	"github.com/BlackbirdWorks/copilot-autocode/resolver"
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

func TestRunLocalResolution(t *testing.T) {
	t.Parallel()

	prd := resolver.PRDetails{
		Owner:      "owner",
		Repo:       "repo",
		HeadBranch: "head",
		BaseBranch: "base",
	}
	cfg := &config.Config{
		AIMergeResolverCmd:    "ai-resolve",
		AIMergeResolverPrompt: "resolve conflicts",
	}
	token := "ghp_secure_token"

	tests := []struct {
		name          string
		runFunc       func(ctx context.Context, dir, token, name string, args ...string) error
		outputFunc    func(ctx context.Context, dir, name string, args ...string) (string, error)
		wantErr       bool
		errContains   string
		expectedCalls []string
	}{
		{
			name: "success",
			outputFunc: func(_ context.Context, _, name string, args ...string) (string, error) {
				if name == "git" && args[0] == "diff" {
					return "file1.go\n", nil
				}
				return "", nil
			},
			expectedCalls: []string{
				"git clone --branch head --single-branch https://x-access-token:ghp_secure_token@github.com/owner/repo.git .",
				"git config user.email copilot-autocode@users.noreply.github.com",
				"git config user.name copilot-autocode",
				"git fetch origin base",
				"git merge --no-edit origin/base",
				"ai-resolve resolve conflicts",
				"git add --all",
				"git diff --cached --name-only",
				"git commit -m chore: resolve merge conflicts via AI",
				"git push origin head",
			},
		},
		{
			name: "clone failure redacts token",
			runFunc: func(_ context.Context, _, token, name string, args ...string) error {
				if name == "git" && args[0] == "clone" {
					return errors.New("failed to clone: " + token)
				}
				return nil
			},
			wantErr:     true,
			errContains: "<redacted>",
		},
		{
			name: "no changes from AI",
			outputFunc: func(_ context.Context, _, name string, args ...string) (string, error) {
				if name == "git" && args[0] == "diff" {
					return "", nil
				}
				return "", nil
			},
			wantErr:     true,
			errContains: "made no changes",
		},
		{
			name: "git config failure",
			runFunc: func(_ context.Context, _, _, name string, args ...string) error {
				if name == "git" && args[0] == "config" {
					return errors.New("config failed")
				}
				return nil
			},
			wantErr:     true,
			errContains: "git config",
		},
		{
			name: "git fetch failure",
			runFunc: func(_ context.Context, _, _, name string, args ...string) error {
				if name == "git" && args[0] == "fetch" {
					return errors.New("fetch failed")
				}
				return nil
			},
			wantErr:     true,
			errContains: "git fetch",
		},
		{
			name: "ai resolver failure",
			runFunc: func(_ context.Context, _, _, name string, _ ...string) error {
				if name == "ai-resolve" {
					return errors.New("ai failed")
				}
				return nil
			},
			wantErr:     true,
			errContains: "AI resolver",
		},
		{
			name: "git add failure",
			runFunc: func(_ context.Context, _, _, name string, args ...string) error {
				if name == "git" && args[0] == "add" {
					return errors.New("add failed")
				}
				return nil
			},
			wantErr:     true,
			errContains: "git add",
		},
		{
			name: "git commit failure",
			runFunc: func(_ context.Context, _, _, name string, args ...string) error {
				if name == "git" && args[0] == "commit" {
					return errors.New("commit failed")
				}
				return nil
			},
			outputFunc: func(_ context.Context, _, name string, args ...string) (string, error) {
				if name == "git" && args[0] == "diff" {
					return "file.go", nil
				}
				return "", nil
			},
			wantErr:     true,
			errContains: "git commit",
		},
		{
			name: "git push failure",
			runFunc: func(_ context.Context, _, _, name string, args ...string) error {
				if name == "git" && args[0] == "push" {
					return errors.New("push failed")
				}
				return nil
			},
			outputFunc: func(_ context.Context, _, name string, args ...string) (string, error) {
				if name == "git" && args[0] == "diff" {
					return "file.go", nil
				}
				return "", nil
			},
			wantErr:     true,
			errContains: "git push",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m := &mockRunner{
				runFunc:    tt.runFunc,
				outputFunc: tt.outputFunc,
			}
			r := resolver.NewWithRunner(m)
			err := r.RunLocalResolution(context.Background(), token, prd, cfg)

			if tt.wantErr {
				require.Error(t, err)
				if tt.errContains != "" {
					assert.Contains(t, err.Error(), tt.errContains)
				}
			} else {
				require.NoError(t, err)
			}

			if len(tt.expectedCalls) > 0 {
				assert.Equal(t, tt.expectedCalls, m.calls)
			}
		})
	}
}

func TestNew(t *testing.T) {
	t.Parallel()
	r := resolver.New()
	assert.NotNil(t, r)
}

func TestRealRunner(t *testing.T) {
	t.Parallel()
	rr := &resolver.RealRunner{}
	ctx := context.Background()

	// We can't easily run real commands in CI without side effects,
	// but we can test that they return an error for non-existent commands.
	err := rr.Run(ctx, ".", "", "non-existent-command-12345")
	require.Error(t, err)

	_, err = rr.Output(ctx, ".", "non-existent-command-12345")
	require.Error(t, err)
}
