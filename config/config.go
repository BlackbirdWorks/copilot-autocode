// Package config provides configuration loading from config.yaml.
package config

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Default values and validation bounds for built-in configuration.
const (
	defaultMaxConcurrentIssues        = 3
	defaultPollIntervalSeconds        = 45
	defaultMaxRefinementRounds        = 3
	defaultMaxMergeConflictRetries    = 3
	defaultLocalMergeAttempts         = 2
	defaultLocalMergeDelayMinutes     = 10
	defaultCopilotInvokeTimeoutSecs   = 600
	defaultCopilotInvokeMaxRetries    = 3
	defaultAgentTimeoutRetryDelaySecs = 1800
	defaultMaxAgentContinueRetries    = 3
	defaultMaxCIFixRounds             = 6
	defaultLogMaxSizeMB               = 5
	defaultLogMaxFiles                = 3
	defaultMergeLogRetentionMinutes   = 60
	defaultCloudAgent                 = "codex"
	defaultLocalClaudeCmd             = "claude"
	defaultLocalClaudeInitialModel    = "opus-4.7"
	defaultLocalClaudeFollowupModel   = "sonnet"
	promptPlaceholder                 = "{prompt}"
	modelPlaceholder                  = "{model}"

	minPollIntervalSecs         = 10
	minCopilotInvokeTimeoutSecs = 30
	minAgentRetryDelaySecs      = 60
)

// Config holds the application configuration.
// All fields have built-in defaults; only github_owner and github_repo are required.
type Config struct {
	// Required: the target repository.
	GitHubOwner string `yaml:"github_owner"`
	GitHubRepo  string `yaml:"github_repo"`

	// Labels applied to issues to track their state through the workflow.
	// Defaults: ai-queue, ai-coding, ai-review, manual-takeover.
	LabelQueue    string `yaml:"label_queue"`
	LabelCoding   string `yaml:"label_coding"`
	LabelReview   string `yaml:"label_review"`
	LabelTakeover string `yaml:"label_takeover"`

	// MaxConcurrentIssues is the maximum number of issues that may be in the
	// ai-coding state simultaneously.  Default: 3.
	MaxConcurrentIssues int `yaml:"max_concurrent_issues"`

	// PollIntervalSeconds controls how often the orchestrator polls GitHub.
	// Minimum: 10.  Default: 45.
	PollIntervalSeconds int `yaml:"poll_interval_seconds"`

	// MaxRefinementRounds is the number of times @copilot is asked to review
	// its implementation against the original issue requirements after CI
	// passes.  The PR is approved and merged only after all rounds are done.
	// Default: 3.
	MaxRefinementRounds int `yaml:"max_refinement_rounds"`

	// RefinementPrompt is the instructional body sent to @copilot for each
	// refinement round.  The orchestrator prefixes it with the round counter
	// and appends the machine-readable marker automatically.
	// Default: a sensible built-in prompt.
	RefinementPrompt string `yaml:"refinement_prompt"`

	// MergeMethod is the strategy used when auto-merging a PR.
	// Accepted values: squash, merge, rebase.  Default: "squash".
	MergeMethod string `yaml:"merge_method"`

	// MergeCommitMessage is the commit message written when the PR is merged.
	// Default: "Auto-merged by copilot-autodev".
	MergeCommitMessage string `yaml:"merge_commit_message"`

	// MergeConflictPrompt is the comment posted on a PR that is behind or has
	// merge conflicts, asking @copilot to rebase/resolve.
	MergeConflictPrompt string `yaml:"merge_conflict_prompt"`

	// MaxMergeConflictRetries is the number of times the orchestrator asks
	// @copilot to fix merge conflicts before falling back to local AI-powered
	// resolution.  Set to 0 to disable @copilot retries and always use local.
	// Default: 3.
	MaxMergeConflictRetries int `yaml:"max_merge_conflict_retries"`

	// LocalMergeAttempts is the number of times local AI resolution is retried
	// if it fails. Default: 2.
	LocalMergeAttempts int `yaml:"local_merge_attempts"`

	// LocalMergeDelayMinutes is the delay between local AI resolution attempts.
	// Default: 10.
	LocalMergeDelayMinutes int `yaml:"local_merge_delay_minutes"`

	// AIMergeResolverCmd is the executable invoked for local merge-conflict
	// resolution after @copilot retries are exhausted.  Default: "gemini".
	// Example: "gh" with AIMergeResolverArgs: ["copilot", "suggest", "-t", "shell"]
	AIMergeResolverCmd string `yaml:"ai_merge_resolver_cmd"`

	// AIMergeResolverArgs are extra arguments inserted between AIMergeResolverCmd
	// and the prompt.  Useful for multi-word CLI tools like `gh copilot suggest`.
	// If the placeholder "{prompt}" appears in any argument, it is replaced with
	// the actual prompt. Otherwise, the prompt is appended at the end.
	// Example: ["-p", "{prompt}", "--yolo"] results in: <cmd> -p <prompt> --yolo.
	// Default: empty (for gemini-style single-arg CLIs).
	AIMergeResolverArgs []string `yaml:"ai_merge_resolver_args"`

	// AIMergeResolverPrompt is the text passed to AIMergeResolverCmd.
	AIMergeResolverPrompt string `yaml:"ai_merge_resolver_prompt"`

	// CopilotInvokeTimeoutSeconds is how long (in seconds) the orchestrator
	// waits for the Copilot coding agent to start working on an issue (i.e.
	// open a PR) before it sends a nudge comment and re-assigns the agent to
	// re-trigger it.  Minimum: 30.  Default: 600 (10 minutes).
	CopilotInvokeTimeoutSeconds int `yaml:"copilot_invoke_timeout_seconds"`

	// CopilotInvokeMaxRetries is how many nudge attempts the orchestrator will
	// make before giving up and returning the issue to the queue.  Default: 3.
	CopilotInvokeMaxRetries int `yaml:"copilot_invoke_max_retries"`

	// FallbackIssueInvokePrompt is the task prompt sent directly to the Copilot
	// API when the coding agent has not started within the timeout.  It is passed
	// to the same backend that powers the GitHub Agents tab and the
	// `gh agent-task create` command.  The following placeholders are expanded at
	// runtime:
	//
	//	{issue_number} — the issue number (e.g. 42)
	//	{issue_title}  — the issue title
	//	{issue_url}    — the URL of the issue on GitHub
	//
	// Default: a built-in prompt that references all three fields.
	FallbackIssueInvokePrompt string `yaml:"fallback_issue_invoke_prompt"`

	// AgentTimeoutRetryDelaySeconds is how long (in seconds) the orchestrator
	// waits before posting "@copilot continue" when the Copilot agent's
	// workflow run concluded with "timed_out".  For non-timeout failures the
	// continue comment is posted immediately.  Minimum: 60.  Default: 1800
	// (30 minutes).
	AgentTimeoutRetryDelaySeconds int `yaml:"agent_timeout_retry_delay_seconds"`

	// AgentContinuePrompt is the comment posted on a PR when the Copilot
	// agent's workflow run fails or times out, telling it to resume.
	// Default: "@copilot continue".
	AgentContinuePrompt string `yaml:"agent_continue_prompt"`

	// MaxAgentContinueRetries is how many "@copilot continue" comments the
	// orchestrator will post on a single PR before falling back to the normal
	// CI-fix flow.  Default: 3.
	MaxAgentContinueRetries int `yaml:"max_agent_continue_retries"`

	// MaxCIFixRounds is the number of CI-fix-only prompts posted after
	// refinement rounds are exhausted but CI is still failing.  These prompts
	// contain only the failing workflow/job details (no review requirements).
	// Default: 6 (double the refinement rounds).
	MaxCIFixRounds int `yaml:"max_ci_fix_rounds"`

	// SkipCIChecks bypasses the CI wait loop when true, merging PRs after
	// refinement even if tests fail or no workflows exist.  Default: false.
	SkipCIChecks bool `yaml:"skip_ci_checks"`

	// LogMaxSizeMB is the maximum size (in megabytes) of a single log file
	// before it is rotated.  Default: 5.
	LogMaxSizeMB int `yaml:"log_max_size_mb"`

	// LogMaxFiles is the maximum number of rotated log files to keep.
	// Older files are deleted when the limit is exceeded.  Default: 3.
	LogMaxFiles int `yaml:"log_max_files"`

	// MergeLogRetentionMinutes is how long merge resolution log files are
	// kept after the associated PR is merged.  Set to 0 to keep forever.
	// Default: 60 (1 hour).
	MergeLogRetentionMinutes int `yaml:"merge_log_retention_minutes"`

	// CIMaxRetries is the number of times @copilot is asked to fix CI failures
	// before the orchestrator stops. Default: 6.
	CIMaxRetries int `yaml:"ci_max_retries"`

	// CopilotOAuthToken is an OAuth token (gho_... or ghu_...) used for calls
	// to the Copilot individual API (api.individual.githubcopilot.com).
	// PATs are rejected by that API; only OAuth tokens work.
	// If left empty, the daemon auto-discovers the token by running
	// `gh auth token`.  Set this explicitly when the daemon cannot access the
	// gh CLI or the macOS keychain (e.g. in headless / launchd environments).
	// Obtain via: gh auth token
	CopilotOAuthToken string `yaml:"copilot_oauth_token"`

	// AssignInsteadOfInvoke, if true, skips the Copilot Jobs API GraphQL call
	// and invokes the agent by posting an @copilot comment directly. Use this
	// when the GraphQL invoke mutation returns errors. Default: false.
	AssignInsteadOfInvoke bool `yaml:"assign_instead_of_invoke"`

	// AgentType selects the coding agent backend.
	// "cloud" (default) uses the GitHub cloud coding-agent API.
	// "claude" is a shortcut for the GitHub Claude coding agent.
	// "cli" uses a local CLI agent (e.g. copilot, claude, aider).
	// "codex" uses the local Codex CLI to submit Codex Cloud tasks.
	AgentType string `yaml:"agent_type"`

	// CloudAgent selects which GitHub Agent HQ agent starts new background tasks.
	// Accepted values: codex, claude, copilot. Default: codex.
	CloudAgent string `yaml:"cloud_agent"`

	// CloudAgentLogin is the GitHub login for the cloud coding agent to assign.
	// Leave empty to derive it from cloud_agent.
	CloudAgentLogin string `yaml:"cloud_agent_login"`

	// CloudAgentMention is the @-mention used when falling back to comment-based
	// invocation or follow-up prompts. Leave empty to derive it from cloud_agent.
	CloudAgentMention string `yaml:"cloud_agent_mention"`

	// CloudAgentSessionID is an optional numeric agent_id for the private
	// follow-up session API. Leave 0 to derive it from cloud_agent.
	CloudAgentSessionID int64 `yaml:"cloud_agent_session_id"`

	// CLIAgentCmd is the executable invoked by the "cli" agent backend.
	// Default: "copilot".
	// Examples: "claude", "aider", "gh copilot".
	CLIAgentCmd string `yaml:"cli_agent_cmd"`

	// CLIAgentArgs are extra arguments passed to CLIAgentCmd.
	// If the placeholder "{prompt}" appears in any argument, it is replaced
	// with the task prompt.  Otherwise the prompt is appended at the end.
	// If the placeholder "{model}" appears in any argument, it is replaced with
	// the model selected for the task phase. If no model placeholder exists,
	// generic CLI agents do not receive a model argument automatically.
	// Default: ["-p", "{prompt}"].
	CLIAgentArgs []string `yaml:"cli_agent_args"`

	// CLIAgentInitialModel is used for first-pass local CLI coding tasks.
	CLIAgentInitialModel string `yaml:"cli_agent_initial_model"`

	// CLIAgentRefinementModel is used for local CLI refinement prompts.
	CLIAgentRefinementModel string `yaml:"cli_agent_refinement_model"`

	// CLIAgentCIFixModel is used for local CLI CI-fix prompts.
	CLIAgentCIFixModel string `yaml:"cli_agent_ci_fix_model"`

	// CLIAgentFollowupModel is used for local CLI follow-up prompt types that
	// do not have a more specific model configured.
	CLIAgentFollowupModel string `yaml:"cli_agent_followup_model"`

	// CodexCloudCmd is the executable used for Codex Cloud mode.
	// Default: "codex".
	CodexCloudCmd string `yaml:"codex_cloud_cmd"`

	// CodexCloudEnvID is the Codex Cloud environment identifier passed to
	// `codex cloud exec --env`. If empty, Codex Cloud mode attempts to infer it
	// from `codex cloud list --json`.
	CodexCloudEnvID string `yaml:"codex_cloud_env_id"`

	// CodexCloudBranch optionally selects the branch passed to Codex Cloud.
	// If empty, Codex Cloud uses its default for the selected environment.
	CodexCloudBranch string `yaml:"codex_cloud_branch"`

	// CodexCloudAttempts is the number of assistant attempts requested for
	// each Codex Cloud task. Default: 1.
	CodexCloudAttempts int `yaml:"codex_cloud_attempts"`

	// NotificationsEnabled controls whether desktop notifications are sent
	// for key events (PR merged, agent timeout, manual fix needed).
	// Default: true.
	NotificationsEnabled bool   `yaml:"notifications_enabled"`
	Model                string `yaml:"model"`
}

// Load reads a YAML config file from path and returns a populated Config with
// defaults applied.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config file %q: %w", path, err)
	}

	cfg := DefaultConfig()
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config file %q: %w", path, err)
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// DefaultConfig returns a Config populated with all built-in default values.
func DefaultConfig() *Config {
	return &Config{
		LabelQueue:                    "ai-queue",
		LabelCoding:                   "ai-coding",
		LabelReview:                   "ai-review",
		LabelTakeover:                 "manual-takeover",
		MaxConcurrentIssues:           defaultMaxConcurrentIssues,
		PollIntervalSeconds:           defaultPollIntervalSeconds,
		MaxRefinementRounds:           defaultMaxRefinementRounds,
		RefinementPrompt:              "Please review your implementation against all requirements in the original issue and refine anything that is missing or incomplete. Please commit and push often so you don't lose work.",
		MergeMethod:                   "squash",
		MergeCommitMessage:            "Auto-merged by copilot-autodev",
		MergeConflictPrompt:           "@codex Please merge from main and address any merge conflicts. Please commit and push often so you don't lose work.",
		MaxMergeConflictRetries:       defaultMaxMergeConflictRetries,
		LocalMergeAttempts:            defaultLocalMergeAttempts,
		LocalMergeDelayMinutes:        defaultLocalMergeDelayMinutes,
		AIMergeResolverCmd:            "copilot",
		AIMergeResolverArgs:           []string{"-p", promptPlaceholder, "--yolo"},
		AIMergeResolverPrompt:         "Please resolve all git merge conflicts in this repository. Make minimal changes to resolve the conflicts while preserving the intent of both sides.",
		CopilotInvokeTimeoutSeconds:   defaultCopilotInvokeTimeoutSecs,
		CopilotInvokeMaxRetries:       defaultCopilotInvokeMaxRetries,
		FallbackIssueInvokePrompt:     "Please start working on issue #{issue_number}: {issue_title}.\n{issue_url}\n\nPlease make a pull request with your changes. Please commit and push often so you don't lose work.",
		AgentTimeoutRetryDelaySeconds: defaultAgentTimeoutRetryDelaySecs,
		AgentContinuePrompt:           "@codex continue. Please commit and push often so you don't lose work.",
		MaxAgentContinueRetries:       defaultMaxAgentContinueRetries,
		MaxCIFixRounds:                defaultMaxCIFixRounds,
		LogMaxSizeMB:                  defaultLogMaxSizeMB,
		LogMaxFiles:                   defaultLogMaxFiles,
		MergeLogRetentionMinutes:      defaultMergeLogRetentionMinutes,
		AgentType:                     "cloud",
		CloudAgent:                    defaultCloudAgent,
		CLIAgentCmd:                   "copilot",
		CLIAgentArgs:                  []string{"-p", promptPlaceholder},
		CodexCloudCmd:                 "codex",
		CodexCloudAttempts:            1,
		NotificationsEnabled:          true,
		Model:                         "auto",
	}
}

func (c *Config) validate() error {
	if c.GitHubOwner == "" {
		return errors.New("config: github_owner is required")
	}
	if c.GitHubRepo == "" {
		return errors.New("config: github_repo is required")
	}
	if c.MaxConcurrentIssues < 1 {
		c.MaxConcurrentIssues = 1
	}
	if c.PollIntervalSeconds < minPollIntervalSecs {
		c.PollIntervalSeconds = minPollIntervalSecs
	}
	if c.MaxRefinementRounds < 0 {
		c.MaxRefinementRounds = 0
	}
	switch c.MergeMethod {
	case "squash", "merge", "rebase":
	// valid
	default:
		return fmt.Errorf(
			"config: merge_method must be squash, merge, or rebase (got %q)",
			c.MergeMethod,
		)
	}
	if c.MaxMergeConflictRetries < 0 {
		c.MaxMergeConflictRetries = 0
	}
	if c.LocalMergeAttempts < 1 {
		c.LocalMergeAttempts = 1
	}
	if c.LocalMergeDelayMinutes < 1 {
		c.LocalMergeDelayMinutes = 1
	}
	// copilot-cli requires the prompt via -p/--prompt, not as a bare positional arg.
	// Auto-set args when the user pointed at "copilot" but left args empty.
	if c.AIMergeResolverCmd == "copilot" && len(c.AIMergeResolverArgs) == 0 {
		c.AIMergeResolverArgs = []string{"-p"}
	}
	if c.CopilotInvokeTimeoutSeconds < minCopilotInvokeTimeoutSecs {
		c.CopilotInvokeTimeoutSeconds = minCopilotInvokeTimeoutSecs
	}
	if c.CopilotInvokeMaxRetries < 1 {
		c.CopilotInvokeMaxRetries = 1
	}
	if c.AgentTimeoutRetryDelaySeconds < minAgentRetryDelaySecs {
		c.AgentTimeoutRetryDelaySeconds = minAgentRetryDelaySecs
	}
	if c.MaxAgentContinueRetries < 1 {
		c.MaxAgentContinueRetries = 1
	}
	if c.MaxCIFixRounds < 0 {
		c.MaxCIFixRounds = 0
	}
	if c.LogMaxSizeMB < 1 {
		c.LogMaxSizeMB = defaultLogMaxSizeMB
	}
	if c.LogMaxFiles < 1 {
		c.LogMaxFiles = defaultLogMaxFiles
	}
	c.applyAgentTypePreset()
	if err := c.applyCloudAgentPreset(); err != nil {
		return err
	}
	if c.CodexCloudCmd == "" {
		c.CodexCloudCmd = "codex"
	}
	if c.CodexCloudAttempts < 1 {
		c.CodexCloudAttempts = 1
	}
	return nil
}

func (c *Config) applyAgentTypePreset() {
	switch strings.ToLower(strings.TrimSpace(c.AgentType)) {
	case "local_claude", "claude_local", "claude-code":
		c.AgentType = "cli"
		c.CLIAgentCmd = defaultLocalClaudeCmd
		c.CLIAgentArgs = ensureLocalClaudeArgs(c.CLIAgentArgs)
		if c.CLIAgentInitialModel == "" {
			c.CLIAgentInitialModel = defaultLocalClaudeInitialModel
		}
		if c.CLIAgentFollowupModel == "" {
			c.CLIAgentFollowupModel = defaultLocalClaudeFollowupModel
		}
		if c.CLIAgentRefinementModel == "" {
			c.CLIAgentRefinementModel = c.CLIAgentFollowupModel
		}
		if c.CLIAgentCIFixModel == "" {
			c.CLIAgentCIFixModel = c.CLIAgentFollowupModel
		}
	case defaultLocalClaudeCmd:
		c.AgentType = defaultLocalClaudeCmd
		c.CloudAgent = defaultLocalClaudeCmd
		c.CloudAgentLogin = ""
		c.CloudAgentMention = ""
		c.CloudAgentSessionID = 0
	}
}

func ensureLocalClaudeArgs(args []string) []string {
	if len(args) == 0 {
		return []string{"-p", promptPlaceholder, "--model", modelPlaceholder}
	}
	hasPrompt := false
	hasModel := false
	for _, arg := range args {
		if strings.Contains(arg, promptPlaceholder) {
			hasPrompt = true
		}
		if strings.Contains(arg, modelPlaceholder) {
			hasModel = true
		}
	}
	if !hasPrompt {
		args = append(args, promptPlaceholder)
	}
	if !hasModel {
		args = append(args, "--model", modelPlaceholder)
	}
	return args
}

func (c *Config) applyCloudAgentPreset() error {
	agent := strings.ToLower(strings.TrimSpace(c.CloudAgent))
	if agent == "" {
		agent = defaultCloudAgent
	}
	var login, mention string
	var sessionID int64
	switch agent {
	case "codex":
		login, mention = "codex", "@codex"
	case defaultLocalClaudeCmd:
		login, mention = defaultLocalClaudeCmd, "@claude"
	case "copilot":
		login, mention, sessionID = "copilot-swe-agent", "@copilot", 1143301
	default:
		return fmt.Errorf("config: cloud_agent must be codex, claude, or copilot (got %q)", c.CloudAgent)
	}

	c.CloudAgent = agent
	if c.CloudAgentLogin == "" {
		c.CloudAgentLogin = login
	}
	if c.CloudAgentMention == "" {
		c.CloudAgentMention = mention
	}
	if c.CloudAgentSessionID == 0 {
		c.CloudAgentSessionID = sessionID
	}
	if !strings.HasPrefix(c.CloudAgentMention, "@") {
		c.CloudAgentMention = "@" + c.CloudAgentMention
	}
	return nil
}
