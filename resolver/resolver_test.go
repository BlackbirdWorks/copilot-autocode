//nolint:gocritic,goimports
package resolver_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BlackbirdWorks/copilot-autodev/config"
	"github.com/BlackbirdWorks/copilot-autodev/resolver"
)

type mockRunner struct {
	runFunc    func(ctx context.Context, dir, token, name string, args ...string) error
	outputFunc func(ctx context.Context, dir, name string, args ...string) (string, error)
	calls      []string
}

func (m *mockRunner) Run(
	ctx context.Context,
	_ io.Writer,
	dir, token, name string,
	args ...string,
) error {
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

	type args struct {
		runFunc    func(ctx context.Context, dir, token, name string, args ...string) error
		outputFunc func(ctx context.Context, dir, name string, args ...string) (string, error)
	}
	type wants struct {
		wantErr       bool
		errContains   string
		expectedCalls []string
	}

	tests := []struct {
		name  string
		args  args
		wants wants
	}{
		{
			name: "success - ai stages only",
			args: args{
				outputFunc: func(_ context.Context, _, name string, args ...string) (string, error) {
					if name == "git" && args[0] == "diff" {
						return "file1.go\n", nil
					}
					if name == "git" && args[0] == "rev-parse" {
						return "abcd1234\n", nil
					}
					return "", nil
				},
			},
			wants: wants{
				expectedCalls: []string{
					"git clone --branch head --single-branch https://x-access-token:ghp_secure_token@github.com/owner/repo.git .",
					"git config user.email copilot-autodev@users.noreply.github.com",
					"git config user.name copilot-autodev",
					"git fetch origin base",
					"git merge --no-edit FETCH_HEAD",
					"git rev-parse HEAD", // pre-SHA
					"ai-resolve resolve conflicts",
					"git rev-parse HEAD", // post-SHA (matches pre)
					"git add --all",
					"git diff --cached --name-only",
					"git commit -m chore: resolve merge conflicts via AI",
					"git rev-parse HEAD", // final SHA
					"git push origin head",
				},
			},
		},
		{
			name: "success - ai commits directly",
			args: args{
				outputFunc: func(ctx context.Context, dir, name string, args ...string) (string, error) {
					if name == "git" && args[0] == "rev-parse" {
						return "", nil
					}
					return "", nil
				},
			},
			wants: wants{
				expectedCalls: []string{},
			},
		},
		{
			name: "clone failure redacts token",
			args: args{
				runFunc: func(_ context.Context, _, token, name string, args ...string) error {
					if name == "git" && args[0] == "clone" {
						return errors.New("failed to clone: " + token)
					}
					return nil
				},
			},
			wants: wants{
				wantErr:     true,
				errContains: "<redacted>",
			},
		},
		{
			name: "no changes from AI",
			args: args{
				outputFunc: func(_ context.Context, _, name string, args ...string) (string, error) {
					if name == "git" && args[0] == "diff" {
						return "", nil // No diff staged
					}
					if name == "git" && args[0] == "rev-parse" {
						return "abcd1234\n", nil // SHAs match, forces diff check
					}
					return "", nil
				},
			},
			wants: wants{
				wantErr:     true,
				errContains: "made no changes",
			},
		},
		{
			name: "git config failure",
			args: args{
				runFunc: func(_ context.Context, _, _, name string, args ...string) error {
					if name == "git" && args[0] == "config" {
						return errors.New("config failed")
					}
					return nil
				},
			},
			wants: wants{
				wantErr:     true,
				errContains: "git config",
			},
		},
		{
			name: "git fetch failure",
			args: args{
				runFunc: func(_ context.Context, _, _, name string, args ...string) error {
					if name == "git" && args[0] == "fetch" {
						return errors.New("fetch failed")
					}
					return nil
				},
			},
			wants: wants{
				wantErr:     true,
				errContains: "git fetch",
			},
		},
		{
			name: "ai resolver failure",
			args: args{
				runFunc: func(_ context.Context, _, _, name string, _ ...string) error {
					if name == "ai-resolve" {
						return errors.New("ai failed")
					}
					return nil
				},
				outputFunc: func(_ context.Context, _, name string, args ...string) (string, error) {
					if name == "git" && args[0] == "rev-parse" {
						return "abcd1234\n", nil
					}
					return "", nil
				},
			},
			wants: wants{
				wantErr:     true,
				errContains: "AI resolver",
			},
		},
		{
			name: "git add failure",
			args: args{
				runFunc: func(_ context.Context, _, _, name string, args ...string) error {
					if name == "git" && args[0] == "add" {
						return errors.New("add failed")
					}
					return nil
				},
				outputFunc: func(_ context.Context, _, name string, args ...string) (string, error) {
					if name == "git" && args[0] == "rev-parse" {
						return "abcd1234\n", nil
					}
					return "", nil
				},
			},
			wants: wants{
				wantErr:     true,
				errContains: "git add",
			},
		},
		{
			name: "git commit failure",
			args: args{
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
					if name == "git" && args[0] == "rev-parse" {
						return "abcd1234\n", nil // forces diff check and commit
					}
					return "", nil
				},
			},
			wants: wants{
				wantErr:     true,
				errContains: "git commit",
			},
		},
		{
			name: "failure - git push",
			args: args{
				runFunc: func(_ context.Context, _, _, name string, args ...string) error {
					if name == "git" && args[0] == "push" {
						return errors.New("push failed")
					}
					return nil
				},
				outputFunc: func(_ context.Context, _, name string, args ...string) (string, error) {
					if name == "git" && args[0] == "rev-parse" {
						return "different-sha\n", nil
					}
					return "", nil
				},
			},
			wants: wants{
				wantErr:     true,
				errContains: "git push",
			},
		},
		{
			name: "success - merge works cleanly",
			args: args{
				runFunc: func(_ context.Context, _, _, name string, args ...string) error {
					return nil
				},
				outputFunc: func(_ context.Context, _, name string, args ...string) (string, error) {
					if name == "git" && args[0] == "rev-parse" {
						return "abcd1234\n", nil
					}
					return "", nil
				},
			},
			wants: wants{
				expectedCalls: []string{
					"git clone --branch head --single-branch https://x-access-token:ghp_secure_token@github.com/owner/repo.git .",
					"git config user.email copilot-autodev@users.noreply.github.com",
					"git config user.name copilot-autodev",
					"git fetch origin base",
					"git merge --no-edit FETCH_HEAD",
					"git rev-parse HEAD",
					"ai-resolve resolve conflicts",
					"git rev-parse HEAD",
					"git add --all",
					"git diff --cached --name-only",
					"git commit -m chore: resolve merge conflicts via AI",
					"git rev-parse HEAD",
					"git push origin head",
				},
			},
		},
		{
			name: "get head sha failure",
			args: args{
				outputFunc: func(_ context.Context, _, name string, args ...string) (string, error) {
					if name == "git" && args[0] == "rev-parse" {
						return "", errors.New("rev-parse failed")
					}
					return "", nil
				},
			},
			wants: wants{
				wantErr:     true,
				errContains: "git rev-parse HEAD",
			},
		},
		{
			name: "prompt inject via placeholder",
			args: args{
				outputFunc: func(_ context.Context, _, name string, args ...string) (string, error) {
					if name == "git" && args[0] == "rev-parse" {
						return "abcd1234\n", nil
					}
					return "", nil
				},
			},
			wants: wants{
				wantErr: true, // fails later because we don't mock diff
				expectedCalls: []string{
					"git clone --branch head --single-branch https://x-access-token:ghp_secure_token@github.com/owner/repo.git .",
					"git config user.email copilot-autodev@users.noreply.github.com",
					"git config user.name copilot-autodev",
					"git fetch origin base",
					"git merge --no-edit FETCH_HEAD",
					"git rev-parse HEAD",
					"ai-resolve resolve-this: resolve conflicts", // Injected
					"git rev-parse HEAD",
					"git add --all",
					"git diff --cached --name-only",
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()

			// Custom mock for tests that need stateful SHAs or diffs
			if tt.name == "success - ai commits directly" || tt.name == "failure - git push" ||
				tt.name == "success - merge works cleanly" {
				revParseCalls := 0
				originalOutputFunc := tt.args.outputFunc
				tt.args.outputFunc = func(ctx context.Context, dir, name string, args ...string) (string, error) {
					if name == "git" && args[0] == "rev-parse" {
						revParseCalls++
						if revParseCalls == 1 {
							return "pre-sha\n", nil
						}
						return "post-sha\n", nil
					}
					if name == "git" && args[0] == "diff" &&
						tt.name == "success - merge works cleanly" {
						return "clean-merge-fix.go\n", nil
					}
					if originalOutputFunc != nil {
						return originalOutputFunc(ctx, dir, name, args...)
					}
					return "", nil
				}
				if tt.name == "success - merge works cleanly" {
					tt.args.outputFunc = func(ctx context.Context, dir, name string, args ...string) (string, error) {
						if name == "git" && args[0] == "rev-parse" {
							revParseCalls++
							if revParseCalls <= 2 {
								return "pre-sha\n", nil
							}
							return "final-sha\n", nil
						}
						if name == "git" && args[0] == "diff" {
							return "clean-merge-fix.go\n", nil
						}
						return "", nil
					}
				}
			}

			if tt.name == "success - ai stages only" {
				revParseCalls := 0
				tt.args.outputFunc = func(ctx context.Context, dir, name string, args ...string) (string, error) {
					if name == "git" && args[0] == "diff" {
						return "file1.go\n", nil
					}
					if name == "git" && args[0] == "rev-parse" {
						revParseCalls++
						if revParseCalls <= 2 {
							return "pre-sha\n", nil
						}
						return "final-sha\n", nil
					}
					return "", nil
				}
				tt.wants.expectedCalls = []string{
					"git clone --branch head --single-branch https://x-access-token:ghp_secure_token@github.com/owner/repo.git .",
					"git config user.email copilot-autodev@users.noreply.github.com",
					"git config user.name copilot-autodev",
					"git fetch origin base",
					"git merge --no-edit FETCH_HEAD",
					"git rev-parse HEAD", // pre-SHA
					"ai-resolve resolve conflicts",
					"git rev-parse HEAD", // post-SHA (same)
					"git add --all",
					"git diff --cached --name-only",
					"git commit -m chore: resolve merge conflicts via AI",
					"git rev-parse HEAD", // final SHA
					"git push origin head",
				}
			}

			m := &mockRunner{
				runFunc:    tt.args.runFunc,
				outputFunc: tt.args.outputFunc,
			}
			r := resolver.NewWithRunner(m)

			// Setup cfg for prompt inject test
			testCfg := cfg
			if tt.name == "prompt inject via placeholder" {
				testCfg = &config.Config{
					AIMergeResolverCmd:    "ai-resolve",
					AIMergeResolverArgs:   []string{"resolve-this: {prompt}"},
					AIMergeResolverPrompt: "resolve conflicts",
				}
			}

			newSha, err := r.RunLocalResolution(ctx, token, prd, testCfg, 123)

			if tt.wants.wantErr {
				require.Error(t, err)
				if tt.wants.errContains != "" {
					assert.Contains(t, err.Error(), tt.wants.errContains)
				}
			} else {
				require.NoError(t, err)
				if tt.name == "success - ai commits directly" {
					assert.Equal(t, "post-sha", newSha)
				} else {
					assert.Equal(t, "final-sha", newSha)
				}
			}

			if len(tt.wants.expectedCalls) > 0 {
				assert.Equal(t, tt.wants.expectedCalls, m.calls)
			}
		})
	}
}

func TestNew(t *testing.T) {
	t.Parallel()
	type args struct{}
	type wants struct {
		isNotNil bool
	}
	tests := []struct {
		name  string
		args  args
		wants wants
	}{
		{"new instance", args{}, wants{isNotNil: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := resolver.New()
			if tt.wants.isNotNil {
				assert.NotNil(t, r)
			}
		})
	}
}

func TestLogRunner(t *testing.T) {
	t.Parallel()

	t.Run("Run logs and redacts", func(t *testing.T) {
		var logBuf strings.Builder
		mock := &mockRunner{
			runFunc: func(ctx context.Context, dir, token, name string, args ...string) error {
				return errors.New("error with " + token)
			},
		}
		lr := resolver.NewLogRunner(mock, &logBuf)

		err := lr.Run(t.Context(), nil, ".", "secret-token", "git", "push")
		require.Error(t, err)
		assert.Contains(t, logBuf.String(), "$ git push")
		assert.Contains(t, logBuf.String(), "ERROR: error with <redacted>")
	})

	t.Run("Output logs", func(t *testing.T) {
		var logBuf strings.Builder
		mock := &mockRunner{
			outputFunc: func(ctx context.Context, dir, name string, args ...string) (string, error) {
				return "some output", nil
			},
		}
		lr := resolver.NewLogRunner(mock, &logBuf)

		out, err := lr.Output(t.Context(), ".", "git", "rev-parse", "HEAD")
		require.NoError(t, err)
		assert.Equal(t, "some output", out)
		assert.Contains(t, logBuf.String(), "$ git rev-parse HEAD")
		assert.Contains(t, logBuf.String(), "some output")
	})
}

func TestRealRunner(t *testing.T) {
	t.Parallel()
	type args struct {
		cmd string
	}
	type wants struct {
		wantErr bool
	}
	tests := []struct {
		name  string
		args  args
		wants wants
	}{
		{"non-existent command", args{cmd: "non-existent-command-12345"}, wants{wantErr: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rr := &resolver.RealRunner{}
			ctx := context.Background()

			err := rr.Run(ctx, nil, ".", "", tt.args.cmd)
			if tt.wants.wantErr {
				require.Error(t, err)
			}

			_, err = rr.Output(ctx, ".", tt.args.cmd)
			if tt.wants.wantErr {
				require.Error(t, err)
			}
		})
	}
}

func TestRealRunner_RunSuccessAndFailure(t *testing.T) {
	t.Parallel()
	rr := &resolver.RealRunner{}
	ctx := context.Background()

	var out strings.Builder
	err := rr.Run(ctx, &out, ".", "ghp_secret_token", "echo", "hello")
	require.NoError(t, err)
	assert.Contains(t, out.String(), "hello")

	out.Reset()
	err = rr.Run(ctx, &out, ".", "ghp_secret_token", "sh", "-c", "echo ghp_secret_token && exit 1")
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "ghp_secret_token")
	assert.Contains(t, err.Error(), "<redacted>")
}
