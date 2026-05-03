package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"log/slog"

	"github.com/google/go-github/v68/github"

	"github.com/BlackbirdWorks/copilot-autodev/config"
	"github.com/BlackbirdWorks/copilot-autodev/ghclient"
	"github.com/BlackbirdWorks/copilot-autodev/pkgs/logger"
)

// Runner abstracts command execution for testability.
type Runner interface {
	Run(ctx context.Context, dir, token, name string, args ...string) error
	Output(ctx context.Context, dir, name string, args ...string) (string, error)
}

// RealRunner is the production implementation using os/exec.
type RealRunner struct{}

func (r *RealRunner) Run(ctx context.Context, dir, token, name string, args ...string) error {
	return runCmd(ctx, dir, token, name, args...)
}

func (r *RealRunner) Output(ctx context.Context, dir, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	return string(out), err
}

// CLIAgent implements CodingAgent by running a local CLI tool (e.g. copilot,
// claude, aider) in a clone of the repository.  It creates branches, runs the
// CLI with the task prompt, commits the result, and pushes.  The orchestrator's
// normal PR-discovery logic then picks up the PR created by the push.
type CLIAgent struct {
	gh    *ghclient.Client
	cfg   *config.Config
	token string

	mu     sync.Mutex
	active map[int]*taskHandle // issueNum → running task
	runner Runner
}

type taskHandle struct {
	cancel context.CancelFunc
	done   chan struct{}
	err    error
}

// NewCLIAgent creates a CLIAgent backed by the given GitHub client, config, and token.
func NewCLIAgent(gh *ghclient.Client, cfg *config.Config, token string) *CLIAgent {
	return &CLIAgent{
		gh:     gh,
		cfg:    cfg,
		token:  token,
		active: make(map[int]*taskHandle),
		runner: &RealRunner{},
	}
}

// NewCLIAgentWithRunner creates a CLIAgent with a custom runner (for testing).
func NewCLIAgentWithRunner(
	gh *ghclient.Client,
	cfg *config.Config,
	token string,
	runner Runner,
) *CLIAgent {
	return &CLIAgent{
		gh:     gh,
		cfg:    cfg,
		token:  token,
		active: make(map[int]*taskHandle),
		runner: runner,
	}
}

func (a *CLIAgent) InvokeTask(
	ctx context.Context, prompt, issueTitle string, issueNum int, issueURL string,
) (string, error) {
	branch := fmt.Sprintf("copilot-autodev/issue-%d", issueNum)

	// Fetch the full issue body so the CLI agent has the complete context.
	issue, _, err := a.gh.GH().Issues.Get(ctx, a.cfg.GitHubOwner, a.cfg.GitHubRepo, issueNum)
	var issueBody string
	if err == nil && issue.GetBody() != "" {
		issueBody = issue.GetBody()
	}

	var fullPrompt string
	if issueBody != "" {
		fullPrompt = fmt.Sprintf("Issue #%d: %s\n\n%s\n\n%s\n\nFixes #%d\n%s",
			issueNum, issueTitle, issueBody, prompt, issueNum, issueURL)
	} else {
		fullPrompt = fmt.Sprintf("%s\n\nFixes #%d\n%s", prompt, issueNum, issueURL)
	}

	taskCtx, cancel := context.WithCancel(ctx)
	handle := &taskHandle{cancel: cancel, done: make(chan struct{})}

	a.mu.Lock()
	// Cancel any existing task for this issue.
	if prev, ok := a.active[issueNum]; ok {
		prev.cancel()
	}
	a.active[issueNum] = handle
	a.mu.Unlock()

	go func() {
		defer close(handle.done)
		defer cancel()
		handle.err = a.runCLITask(taskCtx, fullPrompt, issueTitle, issueNum, branch)

		a.mu.Lock()
		// Only clean up if we're still the active task.
		if a.active[issueNum] == handle {
			delete(a.active, issueNum)
		}
		a.mu.Unlock()

		if handle.err != nil {
			logger.Load(ctx).ErrorContext(ctx, "CLI agent task failed",
				slog.Int("issue", issueNum), slog.Any("err", handle.err))
		}
	}()

	// Return a synthetic job ID based on the branch name.
	return branch, nil
}

func (a *CLIAgent) runCLITask(
	ctx context.Context, prompt, issueTitle string, issueNum int, branch string,
) error {
	tmpDir, err := os.MkdirTemp("", "copilot-autodev-cli-*")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	cloneURL := fmt.Sprintf("https://x-access-token:%s@github.com/%s/%s.git",
		a.token, a.cfg.GitHubOwner, a.cfg.GitHubRepo)

	// Clone the default branch.
	if err := a.runGit(ctx, tmpDir, "clone", "--single-branch", cloneURL, "."); err != nil {
		return fmt.Errorf("git clone: %w", err)
	}

	// Configure git identity.
	if err := a.runGit(ctx, tmpDir, "config", "user.email", "copilot-autodev@users.noreply.github.com"); err != nil {
		return err
	}
	if err := a.runGit(ctx, tmpDir, "config", "user.name", "copilot-autodev"); err != nil {
		return err
	}

	// Create a new branch for this issue.
	if err := a.runGit(ctx, tmpDir, "checkout", "-b", branch); err != nil {
		return fmt.Errorf("git checkout -b: %w", err)
	}

	// Record pre-run SHA.
	preSha, err := a.gitOutput(ctx, tmpDir, "rev-parse", "HEAD")
	if err != nil {
		return err
	}

	// Run the CLI agent.
	if err := a.runCLI(ctx, tmpDir, "initial", prompt); err != nil {
		return fmt.Errorf("CLI agent %q: %w", a.cfg.CLIAgentCmd, err)
	}

	// Check if the CLI committed directly.
	postSha, err := a.gitOutput(ctx, tmpDir, "rev-parse", "HEAD")
	if err != nil {
		return err
	}

	if strings.TrimSpace(preSha) == strings.TrimSpace(postSha) {
		// Stage and commit any unstaged changes.
		if err := a.runGit(ctx, tmpDir, "add", "--all"); err != nil {
			return err
		}
		status, _ := a.gitOutput(ctx, tmpDir, "diff", "--cached", "--name-only")
		if strings.TrimSpace(status) == "" {
			return fmt.Errorf("CLI agent %q made no changes", a.cfg.CLIAgentCmd)
		}
		commitMsg := fmt.Sprintf("fix: address issue #%d — %s", issueNum, issueTitle)
		if err := a.runGit(ctx, tmpDir, "commit", "-m", commitMsg); err != nil {
			return err
		}
	}

	// Push the branch.
	if err := a.runGit(ctx, tmpDir, "push", "-u", "origin", branch); err != nil {
		return fmt.Errorf("git push: %w", err)
	}

	// Create a PR via the GitHub API.
	title := fmt.Sprintf("[copilot-autodev] #%d: %s", issueNum, issueTitle)
	body := fmt.Sprintf(
		"Fixes #%d\n\nGenerated by copilot-autodev CLI agent (`%s`).",
		issueNum,
		a.cfg.CLIAgentCmd,
	)
	if err := a.createPR(ctx, title, body, branch); err != nil {
		return fmt.Errorf("create PR: %w", err)
	}

	return nil
}

func (a *CLIAgent) GetTaskStatus(ctx context.Context, jobID string) (*TaskStatus, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	// jobID is the branch name; find the task by scanning active handles.
	for _, handle := range a.active {
		select {
		case <-handle.done:
			if handle.err != nil {
				return &TaskStatus{JobID: jobID, Status: "failed"}, nil
			}
			return &TaskStatus{JobID: jobID, Status: "completed"}, nil
		default:
			return &TaskStatus{JobID: jobID, Status: "in_progress"}, nil
		}
	}
	return &TaskStatus{JobID: jobID, Status: "completed"}, nil
}

func (a *CLIAgent) IsActive(_ context.Context, issueNum int, _ string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	handle, ok := a.active[issueNum]
	if !ok {
		return false
	}
	select {
	case <-handle.done:
		return false
	default:
		return true
	}
}

func (a *CLIAgent) SendPrompt(ctx context.Context, req PromptRequest) error {
	if req.PRNum == 0 {
		return errors.New("CLI agent SendPrompt requires a PR number")
	}

	// Get the PR to find the head branch.
	pr, _, err := a.gh.GH().PullRequests.Get(ctx, a.cfg.GitHubOwner, a.cfg.GitHubRepo, req.PRNum)
	if err != nil {
		return fmt.Errorf("get PR #%d: %w", req.PRNum, err)
	}
	branch := pr.GetHead().GetRef()

	tmpDir, err := os.MkdirTemp("", "copilot-autodev-cli-*")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	cloneURL := fmt.Sprintf("https://x-access-token:%s@github.com/%s/%s.git",
		a.token, a.cfg.GitHubOwner, a.cfg.GitHubRepo)

	// Clone the PR branch.
	if err := a.runGit(ctx, tmpDir, "clone", "--branch", branch, "--single-branch", cloneURL, "."); err != nil {
		return fmt.Errorf("git clone: %w", err)
	}
	if err := a.runGit(ctx, tmpDir, "config", "user.email", "copilot-autodev@users.noreply.github.com"); err != nil {
		return err
	}
	if err := a.runGit(ctx, tmpDir, "config", "user.name", "copilot-autodev"); err != nil {
		return err
	}

	// For merge conflicts, fetch and merge the base branch first.
	if req.PromptType == "merge-conflict" {
		baseBranch := pr.GetBase().GetRef()
		_ = a.runGit(ctx, tmpDir, "fetch", "origin", baseBranch)
		_ = a.runGit(ctx, tmpDir, "merge", "--no-edit", "FETCH_HEAD")
	}

	preSha, _ := a.gitOutput(ctx, tmpDir, "rev-parse", "HEAD")

	// Run the CLI with the prompt.
	if err := a.runCLI(ctx, tmpDir, req.PromptType, req.Body); err != nil {
		return fmt.Errorf("CLI agent %q: %w", a.cfg.CLIAgentCmd, err)
	}

	postSha, _ := a.gitOutput(ctx, tmpDir, "rev-parse", "HEAD")
	if strings.TrimSpace(preSha) == strings.TrimSpace(postSha) {
		if err := a.runGit(ctx, tmpDir, "add", "--all"); err != nil {
			return err
		}
		status, _ := a.gitOutput(ctx, tmpDir, "diff", "--cached", "--name-only")
		if strings.TrimSpace(status) == "" {
			// No changes — that's OK for some prompt types.
			return nil
		}
		commitMsg := fmt.Sprintf("chore: %s via CLI agent", req.PromptType)
		if err := a.runGit(ctx, tmpDir, "commit", "-m", commitMsg); err != nil {
			return err
		}
	}

	return a.runGit(ctx, tmpDir, "push", "origin", branch)
}

func (a *CLIAgent) HasRespondedSince(
	ctx context.Context,
	prNum int,
	since time.Time,
) (bool, error) {
	// Check for new commits on the PR since the given time.
	commits, _, err := a.gh.GH().PullRequests.ListCommits(
		ctx, a.cfg.GitHubOwner, a.cfg.GitHubRepo, prNum,
		&github.ListOptions{PerPage: 10},
	)
	if err != nil {
		return false, err
	}
	for _, c := range commits {
		if c.GetCommit().GetCommitter().GetDate().Time.After(since) {
			return true, nil
		}
	}
	return false, nil
}

func (a *CLIAgent) DiscoverPR(_ context.Context, _ int) (*github.PullRequest, error) {
	// CLI agent PRs are discovered via the normal OpenPRForIssue branch-matching logic.
	return nil, nil
}

// CleanupWorkdir is a no-op for CLIAgent — it does not use persistent workdirs.
func (a *CLIAgent) CleanupWorkdir(_ context.Context, _ int) {}

// CleanupAllWorkdirs is a no-op for CLIAgent — it does not use persistent workdirs.
func (a *CLIAgent) CleanupAllWorkdirs(_ context.Context) int { return 0 }

// RetryTask re-invokes the CLI agent for a previously failed issue.
// For CLIAgent, this is equivalent to InvokeTask with no prompt override.
func (a *CLIAgent) RetryTask(ctx context.Context, issueNum int) error {
	_, err := a.InvokeTask(ctx, "", "", issueNum, "")
	return err
}

// createPR creates a pull request via the GitHub API.
func (a *CLIAgent) createPR(ctx context.Context, title, body, branch string) error {
	base := "main" // TODO: could read default branch from repo settings
	_, _, err := a.gh.GH().PullRequests.Create(
		ctx,
		a.cfg.GitHubOwner,
		a.cfg.GitHubRepo,
		&github.NewPullRequest{
			Title: &title,
			Body:  &body,
			Head:  &branch,
			Base:  &base,
		},
	)
	return err
}

// runCLI executes the configured CLI agent command with the given prompt.
func (a *CLIAgent) runCLI(ctx context.Context, dir, promptType, prompt string) error {
	args := buildCLIArgs(a.cfg.CLIAgentArgs, prompt, a.cliModelForPromptType(promptType))
	return a.runner.Run(ctx, dir, a.token, a.cfg.CLIAgentCmd, args...)
}

func (a *CLIAgent) cliModelForPromptType(promptType string) string {
	switch promptType {
	case "initial":
		if a.cfg.CLIAgentInitialModel != "" {
			return a.cfg.CLIAgentInitialModel
		}
	case "refinement":
		if a.cfg.CLIAgentRefinementModel != "" {
			return a.cfg.CLIAgentRefinementModel
		}
	case "ci-fix":
		if a.cfg.CLIAgentCIFixModel != "" {
			return a.cfg.CLIAgentCIFixModel
		}
	}
	if promptType != "initial" && a.cfg.CLIAgentFollowupModel != "" {
		return a.cfg.CLIAgentFollowupModel
	}
	if a.cfg.Model != "" && a.cfg.Model != "auto" {
		return a.cfg.Model
	}
	return ""
}

// buildCLIArgs builds the argument list, injecting prompt/model placeholders.
func buildCLIArgs(templateArgs []string, prompt, model string) []string {
	args := make([]string, 0, len(templateArgs)+1)
	promptInjected := false
	for _, arg := range templateArgs {
		switch {
		case strings.Contains(arg, "{prompt}"):
			args = append(args, strings.ReplaceAll(arg, "{prompt}", prompt))
			promptInjected = true
		case strings.Contains(arg, "{model}"):
			if model != "" {
				args = append(args, strings.ReplaceAll(arg, "{model}", model))
			}
		default:
			args = append(args, arg)
		}
	}
	if !promptInjected {
		args = append(args, prompt)
	}
	return args
}

// runGit runs a git command in the given directory.
func (a *CLIAgent) runGit(ctx context.Context, dir string, args ...string) error {
	return a.runner.Run(ctx, dir, a.token, "git", args...)
}

// gitOutput runs a git command and returns stdout.
func (a *CLIAgent) gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	return a.runner.Output(ctx, dir, "git", args...)
}

// runCmd executes a command in dir with GitHub token injected into the environment.
// If the binary is not found in PATH, it falls back to invoking via `sh -l -c`
// to pick up the user's full PATH from their shell profile.
func runCmd(ctx context.Context, dir, token, name string, args ...string) error {
	var buf strings.Builder

	if _, err := exec.LookPath(name); err != nil {
		// Binary not on PATH — try via login shell.
		parts := append([]string{name}, args...)
		quoted := make([]string, len(parts))
		for i, p := range parts {
			quoted[i] = "'" + strings.ReplaceAll(p, "'", "'\\''") + "'"
		}
		prefix := fmt.Sprintf("GITHUB_TOKEN=%s GH_TOKEN=%s COPILOT_GITHUB_TOKEN=%s ",
			token, token, token)
		cmd := exec.CommandContext(ctx, "sh", "-l", "-c", prefix+strings.Join(quoted, " "))
		cmd.Dir = dir
		cmd.Stdout = &buf
		cmd.Stderr = &buf
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("%w\n%s", err, redact(buf.String(), token))
		}
		return nil
	}

	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	// Inject token environment variables.
	tokenKeys := map[string]bool{
		"GITHUB_TOKEN": true, "GH_TOKEN": true, "COPILOT_GITHUB_TOKEN": true,
	}
	env := make([]string, 0, len(os.Environ())+3)
	for _, kv := range os.Environ() {
		if before, _, ok := strings.Cut(kv, "="); ok && tokenKeys[before] {
			continue
		}
		env = append(env, kv)
	}
	env = append(env,
		"GITHUB_TOKEN="+token,
		"GH_TOKEN="+token,
		"COPILOT_GITHUB_TOKEN="+token,
	)
	cmd.Env = env

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w\n%s", err, redact(buf.String(), token))
	}
	return nil
}

// redact replaces all occurrences of token in s with "<redacted>".
func redact(s, token string) string {
	if token == "" {
		return s
	}
	return strings.ReplaceAll(s, token, "<redacted>")
}
