package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
	failed map[int]*taskHandle // issueNum → last failed task (for retry)
	runner Runner
}

type taskHandle struct {
	cancel context.CancelFunc
	done   chan struct{}
	err    error
	dir    string // persistent working directory
}

// NewCLIAgent creates a CLIAgent backed by the given GitHub client, config, and token.
func NewCLIAgent(gh *ghclient.Client, cfg *config.Config, token string) *CLIAgent {
	return &CLIAgent{
		gh:     gh,
		cfg:    cfg,
		token:  token,
		active: make(map[int]*taskHandle),
		failed: make(map[int]*taskHandle),
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
		failed: make(map[int]*taskHandle),
		runner: runner,
	}
}

// workDir returns the deterministic persistent working directory for an issue.
// The path is ~/.copilot-autodev/workdirs/<owner>-<repo>/issue-<num>/.
func (a *CLIAgent) workDir(issueNum int) string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.TempDir()
	}
	return filepath.Join(home, ".copilot-autodev", "workdirs",
		a.cfg.GitHubOwner+"-"+a.cfg.GitHubRepo,
		fmt.Sprintf("issue-%d", issueNum))
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

	dir := a.workDir(issueNum)
	taskCtx, cancel := context.WithCancel(ctx)
	handle := &taskHandle{cancel: cancel, done: make(chan struct{}), dir: dir}

	a.mu.Lock()
	// Cancel any existing task for this issue.
	if prev, ok := a.active[issueNum]; ok {
		prev.cancel()
	}
	delete(a.failed, issueNum)
	a.active[issueNum] = handle
	a.mu.Unlock()

	go func() {
		defer close(handle.done)
		defer cancel()
		handle.err = a.runCLITask(taskCtx, fullPrompt, issueTitle, issueNum, branch, dir)

		a.mu.Lock()
		if a.active[issueNum] == handle {
			delete(a.active, issueNum)
			if handle.err != nil {
				a.failed[issueNum] = handle
			}
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
	ctx context.Context, prompt, issueTitle string, issueNum int, branch, dir string,
) error {
	cloneURL := fmt.Sprintf("https://x-access-token:%s@github.com/%s/%s.git",
		a.token, a.cfg.GitHubOwner, a.cfg.GitHubRepo)

	// Check if we can reuse an existing working directory.
	if a.isValidGitDir(ctx, dir) {
		// Reuse existing clone: fetch latest and reset to default branch.
		if err := a.runGit(ctx, dir, "fetch", "origin"); err != nil {
			// Fetch failed — fall through to fresh clone.
			_ = os.RemoveAll(dir)
		} else {
			// Reset to the default branch HEAD for a clean start.
			_ = a.runGit(ctx, dir, "checkout", "-f", "HEAD")
			_ = a.runGit(ctx, dir, "clean", "-fd")
			// Try to check out the branch if it already exists on remote.
			if err := a.runGit(ctx, dir, "checkout", branch); err != nil {
				// Branch doesn't exist yet — create from default branch.
				_ = a.runGit(ctx, dir, "checkout", "-B", branch, "origin/HEAD")
			} else {
				// Branch exists — pull latest.
				_ = a.runGit(ctx, dir, "pull", "--rebase", "origin", branch)
			}
			return a.runCLITaskInDir(ctx, prompt, issueTitle, issueNum, branch, dir)
		}
	}

	// Fresh clone into the persistent directory.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create workdir: %w", err)
	}

	if err := a.runGit(ctx, dir, "clone", "--single-branch", cloneURL, "."); err != nil {
		return fmt.Errorf("git clone: %w", err)
	}

	// Configure git identity.
	if err := a.runGit(ctx, dir, "config", "user.email", "copilot-autodev@users.noreply.github.com"); err != nil {
		return err
	}
	if err := a.runGit(ctx, dir, "config", "user.name", "copilot-autodev"); err != nil {
		return err
	}

	// Create a new branch for this issue.
	if err := a.runGit(ctx, dir, "checkout", "-b", branch); err != nil {
		return fmt.Errorf("git checkout -b: %w", err)
	}

	return a.runCLITaskInDir(ctx, prompt, issueTitle, issueNum, branch, dir)
}

// runCLITaskInDir runs the CLI agent in an already-prepared directory.
func (a *CLIAgent) runCLITaskInDir(
	ctx context.Context, prompt, issueTitle string, issueNum int, branch, dir string,
) error {
	// Record pre-run SHA.
	preSha, err := a.gitOutput(ctx, dir, "rev-parse", "HEAD")
	if err != nil {
		return err
	}

	// Run the CLI agent.
	if err := a.runCLI(ctx, dir, prompt); err != nil {
		return fmt.Errorf("CLI agent %q: %w", a.cfg.CLIAgentCmd, err)
	}

	// Check if the CLI committed directly.
	postSha, err := a.gitOutput(ctx, dir, "rev-parse", "HEAD")
	if err != nil {
		return err
	}

	if strings.TrimSpace(preSha) == strings.TrimSpace(postSha) {
		// Stage and commit any unstaged changes.
		if err := a.runGit(ctx, dir, "add", "--all"); err != nil {
			return err
		}
		status, _ := a.gitOutput(ctx, dir, "diff", "--cached", "--name-only")
		if strings.TrimSpace(status) == "" {
			return fmt.Errorf("CLI agent %q made no changes", a.cfg.CLIAgentCmd)
		}
		commitMsg := fmt.Sprintf("fix: address issue #%d — %s", issueNum, issueTitle)
		if err := a.runGit(ctx, dir, "commit", "-m", commitMsg); err != nil {
			return err
		}
	}

	// Push the branch.
	if err := a.runGit(ctx, dir, "push", "-u", "origin", branch); err != nil {
		return fmt.Errorf("git push: %w", err)
	}

	// Create a draft PR via the GitHub API.
	title := fmt.Sprintf("[copilot-autodev] #%d: %s", issueNum, issueTitle)
	body := fmt.Sprintf(
		"Fixes #%d\n\nGenerated by copilot-autodev CLI agent (`%s`).",
		issueNum,
		a.cfg.CLIAgentCmd,
	)
	if err := a.createPR(ctx, title, body, branch); err != nil {
		// PR may already exist from a previous run — that's OK.
		if !strings.Contains(err.Error(), "already exists") &&
			!strings.Contains(err.Error(), "A pull request already exists") {
			return fmt.Errorf("create PR: %w", err)
		}
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

func (a *CLIAgent) IsActive(_ context.Context, issueNum int) bool {
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

	// Get the PR to find the head branch and issue number.
	pr, _, err := a.gh.GH().PullRequests.Get(ctx, a.cfg.GitHubOwner, a.cfg.GitHubRepo, req.PRNum)
	if err != nil {
		return fmt.Errorf("get PR #%d: %w", req.PRNum, err)
	}
	branch := pr.GetHead().GetRef()

	// Try to reuse the persistent workdir for this issue.
	dir := a.workDir(req.IssueNum)
	reuseDir := req.IssueNum > 0 && a.isValidGitDir(ctx, dir)

	if !reuseDir {
		// Fall back to a temp directory.
		dir, err = os.MkdirTemp("", "copilot-autodev-cli-*")
		if err != nil {
			return fmt.Errorf("create temp dir: %w", err)
		}
		defer os.RemoveAll(dir)
	}

	cloneURL := fmt.Sprintf("https://x-access-token:%s@github.com/%s/%s.git",
		a.token, a.cfg.GitHubOwner, a.cfg.GitHubRepo)

	if reuseDir {
		// Update the existing clone to the PR branch.
		_ = a.runGit(ctx, dir, "fetch", "origin")
		_ = a.runGit(ctx, dir, "checkout", branch)
		_ = a.runGit(ctx, dir, "pull", "--rebase", "origin", branch)
	} else {
		// Clone the PR branch fresh.
		if err := a.runGit(ctx, dir, "clone", "--branch", branch, "--single-branch", cloneURL, "."); err != nil {
			return fmt.Errorf("git clone: %w", err)
		}
		if err := a.runGit(ctx, dir, "config", "user.email", "copilot-autodev@users.noreply.github.com"); err != nil {
			return err
		}
		if err := a.runGit(ctx, dir, "config", "user.name", "copilot-autodev"); err != nil {
			return err
		}
	}

	// For merge conflicts, fetch and merge the base branch first.
	if req.PromptType == "merge-conflict" {
		baseBranch := pr.GetBase().GetRef()
		_ = a.runGit(ctx, dir, "fetch", "origin", baseBranch)
		_ = a.runGit(ctx, dir, "merge", "--no-edit", "FETCH_HEAD")
	}

	preSha, _ := a.gitOutput(ctx, dir, "rev-parse", "HEAD")

	// Run the CLI with the prompt.
	if err := a.runCLI(ctx, dir, req.Body); err != nil {
		return fmt.Errorf("CLI agent %q: %w", a.cfg.CLIAgentCmd, err)
	}

	postSha, _ := a.gitOutput(ctx, dir, "rev-parse", "HEAD")
	if strings.TrimSpace(preSha) == strings.TrimSpace(postSha) {
		if err := a.runGit(ctx, dir, "add", "--all"); err != nil {
			return err
		}
		status, _ := a.gitOutput(ctx, dir, "diff", "--cached", "--name-only")
		if strings.TrimSpace(status) == "" {
			// No changes — that's OK for some prompt types.
			return nil
		}
		commitMsg := fmt.Sprintf("chore: %s via CLI agent", req.PromptType)
		if err := a.runGit(ctx, dir, "commit", "-m", commitMsg); err != nil {
			return err
		}
	}

	return a.runGit(ctx, dir, "push", "origin", branch)
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

func (a *CLIAgent) RetryTask(ctx context.Context, issueNum int) error {
	a.mu.Lock()
	// Cancel any existing active task.
	if prev, ok := a.active[issueNum]; ok {
		prev.cancel()
	}
	delete(a.failed, issueNum)
	a.mu.Unlock()

	// Fetch the issue details for a fresh prompt.
	issue, _, err := a.gh.GH().Issues.Get(ctx, a.cfg.GitHubOwner, a.cfg.GitHubRepo, issueNum)
	if err != nil {
		return fmt.Errorf("get issue #%d: %w", issueNum, err)
	}

	prompt := fmt.Sprintf("Issue #%d: %s\n\n%s\n\nFixes #%d\n%s",
		issueNum, issue.GetTitle(), issue.GetBody(), issueNum, issue.GetHTMLURL())
	_, retryErr := a.InvokeTask(ctx, prompt, issue.GetTitle(), issueNum, issue.GetHTMLURL())
	return retryErr
}

func (a *CLIAgent) CleanupWorkdir(_ context.Context, issueNum int) {
	dir := a.workDir(issueNum)
	_ = os.RemoveAll(dir)

	a.mu.Lock()
	delete(a.failed, issueNum)
	a.mu.Unlock()
}

func (a *CLIAgent) CleanupAllWorkdirs(_ context.Context) int {
	home, err := os.UserHomeDir()
	if err != nil {
		return 0
	}
	repoDir := filepath.Join(home, ".copilot-autodev", "workdirs",
		a.cfg.GitHubOwner+"-"+a.cfg.GitHubRepo)
	entries, err := os.ReadDir(repoDir)
	if err != nil {
		return 0
	}
	removed := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		_ = os.RemoveAll(filepath.Join(repoDir, e.Name()))
		removed++
	}

	a.mu.Lock()
	a.failed = make(map[int]*taskHandle)
	a.mu.Unlock()

	return removed
}

// IsFailed returns true if a task for the given issue completed with an error.
func (a *CLIAgent) IsFailed(issueNum int) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, ok := a.failed[issueNum]
	return ok
}

// isValidGitDir returns true if dir contains a valid git repository.
func (a *CLIAgent) isValidGitDir(ctx context.Context, dir string) bool {
	info, err := os.Stat(filepath.Join(dir, ".git"))
	if err != nil || !info.IsDir() {
		return false
	}
	// Verify git can read it.
	_, err = a.gitOutput(ctx, dir, "rev-parse", "--git-dir")
	return err == nil
}

// createPR creates a draft pull request via the GitHub API.
func (a *CLIAgent) createPR(ctx context.Context, title, body, branch string) error {
	base := "main" // TODO: could read default branch from repo settings
	draft := true
	_, _, err := a.gh.GH().PullRequests.Create(
		ctx,
		a.cfg.GitHubOwner,
		a.cfg.GitHubRepo,
		&github.NewPullRequest{
			Title: &title,
			Body:  &body,
			Head:  &branch,
			Base:  &base,
			Draft: &draft,
		},
	)
	return err
}

// runCLI executes the configured CLI agent command with the given prompt.
func (a *CLIAgent) runCLI(ctx context.Context, dir, prompt string) error {
	args := buildCLIArgs(a.cfg.CLIAgentArgs, prompt)
	return a.runner.Run(ctx, dir, a.token, a.cfg.CLIAgentCmd, args...)
}

// buildCLIArgs builds the argument list, injecting the prompt via {prompt} placeholder.
func buildCLIArgs(templateArgs []string, prompt string) []string {
	args := make([]string, 0, len(templateArgs)+1)
	injected := false
	for _, arg := range templateArgs {
		if strings.Contains(arg, "{prompt}") {
			args = append(args, strings.ReplaceAll(arg, "{prompt}", prompt))
			injected = true
		} else {
			args = append(args, arg)
		}
	}
	if !injected {
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
		//nolint:gosec // subprocess is intentionally built from safe components
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
