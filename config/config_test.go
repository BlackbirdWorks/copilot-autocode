//nolint:gocritic,goimports
package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BlackbirdWorks/copilot-autodev/config"
)

// writeConfig writes content to a temp file and returns its path.
func writeConfig(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "config*.yaml")
	require.NoError(t, err, "create temp config")
	_, err = f.WriteString(content)
	require.NoError(t, err, "write temp config")
	f.Close()
	return f.Name()
}

func TestConfigLoad(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		yaml        string
		nonexistent bool
		wantErr     bool
		errContains string
		check       func(t *testing.T, c *config.Config)
	}{
		{
			name: "minimal config uses all defaults",
			yaml: "github_owner: myorg\ngithub_repo: myrepo\n",
			check: func(t *testing.T, c *config.Config) {
				assert.Equal(t, "myorg", c.GitHubOwner)
				assert.Equal(t, "myrepo", c.GitHubRepo)
				assert.Equal(t, "ai-queue", c.LabelQueue)
				assert.Equal(t, "ai-coding", c.LabelCoding)
				assert.Equal(t, "ai-review", c.LabelReview)
				assert.Equal(t, "squash", c.MergeMethod)
				assert.Equal(t, 3, c.MaxConcurrentIssues)
				assert.Equal(t, 45, c.PollIntervalSeconds)
				assert.Equal(t, 3, c.MaxRefinementRounds)
				assert.Equal(t, 3, c.MaxMergeConflictRetries)
				assert.Equal(t, 2, c.LocalMergeAttempts)
				assert.Equal(t, 10, c.LocalMergeDelayMinutes)
				assert.Equal(t, "copilot", c.AIMergeResolverCmd)
				assert.Equal(t, []string{"-p", "{prompt}", "--yolo"}, c.AIMergeResolverArgs)
				assert.Equal(t, 600, c.CopilotInvokeTimeoutSeconds)
				assert.Equal(t, 3, c.CopilotInvokeMaxRetries)
				assert.Equal(t, "codex", c.CloudAgent)
				assert.Equal(t, "codex", c.CloudAgentLogin)
				assert.Equal(t, "@codex", c.CloudAgentMention)
				assert.Equal(t, int64(0), c.CloudAgentSessionID)
				assert.Equal(t, "codex", c.CodexCloudCmd)
				assert.Equal(t, 1, c.CodexCloudAttempts)
				assert.NotEmpty(t, c.FallbackIssueInvokePrompt)
				assert.Contains(t, c.FallbackIssueInvokePrompt, "{issue_number}")
			},
		},
		{
			name: "custom values override defaults",
			yaml: `
github_owner: corp
github_repo: platform
merge_method: rebase
max_concurrent_issues: 5
poll_interval_seconds: 120
copilot_invoke_timeout_seconds: 300
copilot_invoke_max_retries: 2
cloud_agent: copilot
label_queue: custom-queue
`,
			check: func(t *testing.T, c *config.Config) {
				assert.Equal(t, "rebase", c.MergeMethod)
				assert.Equal(t, 5, c.MaxConcurrentIssues)
				assert.Equal(t, 120, c.PollIntervalSeconds)
				assert.Equal(t, 300, c.CopilotInvokeTimeoutSeconds)
				assert.Equal(t, 2, c.CopilotInvokeMaxRetries)
				assert.Equal(t, "copilot", c.CloudAgent)
				assert.Equal(t, "copilot-swe-agent", c.CloudAgentLogin)
				assert.Equal(t, "@copilot", c.CloudAgentMention)
				assert.Equal(t, int64(1143301), c.CloudAgentSessionID)
				assert.Equal(t, "custom-queue", c.LabelQueue)
			},
		},
		{
			name: "claude cloud agent preset",
			yaml: "github_owner: o\ngithub_repo: r\ncloud_agent: claude\n",
			check: func(t *testing.T, c *config.Config) {
				assert.Equal(t, "claude", c.CloudAgent)
				assert.Equal(t, "claude", c.CloudAgentLogin)
				assert.Equal(t, "@claude", c.CloudAgentMention)
				assert.Equal(t, int64(0), c.CloudAgentSessionID)
			},
		},
		{
			name: "claude agent type uses github claude",
			yaml: "github_owner: o\ngithub_repo: r\nagent_type: claude\ncloud_agent_login: codex\ncloud_agent_mention: '@codex'\ncloud_agent_session_id: 1143301\n",
			check: func(t *testing.T, c *config.Config) {
				assert.Equal(t, "claude", c.AgentType)
				assert.Equal(t, "claude", c.CloudAgent)
				assert.Equal(t, "claude", c.CloudAgentLogin)
				assert.Equal(t, "@claude", c.CloudAgentMention)
				assert.Equal(t, int64(0), c.CloudAgentSessionID)
			},
		},
		{
			name: "local claude shortcut uses claude code CLI with phased models",
			yaml: "github_owner: o\ngithub_repo: r\nagent_type: local_claude\n",
			check: func(t *testing.T, c *config.Config) {
				assert.Equal(t, "cli", c.AgentType)
				assert.Equal(t, "claude", c.CLIAgentCmd)
				assert.Equal(t, []string{"-p", "{prompt}", "--model", "{model}"}, c.CLIAgentArgs)
				assert.Equal(t, "opus-4.7", c.CLIAgentInitialModel)
				assert.Equal(t, "sonnet", c.CLIAgentFollowupModel)
				assert.Equal(t, "sonnet", c.CLIAgentRefinementModel)
				assert.Equal(t, "sonnet", c.CLIAgentCIFixModel)
			},
		},
		{
			name: "local claude shortcut preserves custom model choices",
			yaml: `
github_owner: o
github_repo: r
agent_type: claude_local
cli_agent_args: ["--dangerously-skip-permissions", "-p", "{prompt}"]
cli_agent_initial_model: "opus-4.7"
cli_agent_refinement_model: "sonnet"
cli_agent_ci_fix_model: "sonnet"
`,
			check: func(t *testing.T, c *config.Config) {
				assert.Equal(t, "cli", c.AgentType)
				assert.Equal(t, "claude", c.CLIAgentCmd)
				assert.Equal(
					t,
					[]string{"--dangerously-skip-permissions", "-p", "{prompt}", "--model", "{model}"},
					c.CLIAgentArgs,
				)
				assert.Equal(t, "opus-4.7", c.CLIAgentInitialModel)
				assert.Equal(t, "sonnet", c.CLIAgentRefinementModel)
				assert.Equal(t, "sonnet", c.CLIAgentCIFixModel)
			},
		},
		{
			name: "codex backend allows env auto-discovery",
			yaml: "github_owner: o\ngithub_repo: r\nagent_type: codex\n",
			check: func(t *testing.T, c *config.Config) {
				assert.Equal(t, "codex", c.AgentType)
				assert.Empty(t, c.CodexCloudEnvID)
			},
		},
		{
			name: "codex backend config",
			yaml: "github_owner: o\ngithub_repo: r\nagent_type: codex\ncodex_cloud_env_id: env_123\ncodex_cloud_attempts: 2\n",
			check: func(t *testing.T, c *config.Config) {
				assert.Equal(t, "codex", c.AgentType)
				assert.Equal(t, "env_123", c.CodexCloudEnvID)
				assert.Equal(t, 2, c.CodexCloudAttempts)
			},
		},
		{
			name: "merge method: all valid variants accepted",
			yaml: "github_owner: o\ngithub_repo: r\nmerge_method: merge\n",
			check: func(t *testing.T, c *config.Config) {
				assert.Equal(t, "merge", c.MergeMethod)
			},
		},
		{
			name:        "missing github_owner → error",
			yaml:        "github_repo: myrepo\n",
			wantErr:     true,
			errContains: "github_owner is required",
		},
		{
			name:        "missing github_repo → error",
			yaml:        "github_owner: myorg\n",
			wantErr:     true,
			errContains: "github_repo is required",
		},
		{
			name:        "invalid merge_method → error",
			yaml:        "github_owner: o\ngithub_repo: r\nmerge_method: cherry-pick\n",
			wantErr:     true,
			errContains: "merge_method must be squash",
		},
		{
			name: "below-minimum values are clamped to their floors",
			yaml: `
github_owner: o
github_repo: r
max_concurrent_issues: 0
poll_interval_seconds: 5
max_refinement_rounds: -1
max_merge_conflict_retries: -1
local_merge_attempts: 0
local_merge_delay_minutes: 0
copilot_invoke_timeout_seconds: 10
copilot_invoke_max_retries: 0
`,
			check: func(t *testing.T, c *config.Config) {
				assert.Equal(t, 1, c.MaxConcurrentIssues, "clamped from 0")
				assert.Equal(t, 10, c.PollIntervalSeconds, "clamped from 5")
				assert.Equal(t, 0, c.MaxRefinementRounds, "clamped from -1")
				assert.Equal(t, 0, c.MaxMergeConflictRetries, "clamped from -1")
				assert.Equal(t, 1, c.LocalMergeAttempts, "clamped from 0")
				assert.Equal(t, 1, c.LocalMergeDelayMinutes, "clamped from 0")
				assert.Equal(t, 30, c.CopilotInvokeTimeoutSeconds, "clamped from 10")
				assert.Equal(t, 1, c.CopilotInvokeMaxRetries, "clamped from 0")
			},
		},
		{
			name:        "file not found → error",
			nonexistent: true,
			wantErr:     true,
			errContains: "read config file",
		},
		{
			name:        "malformed YAML → error",
			yaml:        "github_owner: [not a string\n",
			wantErr:     true,
			errContains: "parse config file",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var path string
			if tc.nonexistent {
				path = filepath.Join(t.TempDir(), "nonexistent.yaml")
			} else {
				path = writeConfig(t, tc.yaml)
			}

			cfg, err := config.Load(path)

			if tc.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.errContains)
				return
			}
			require.NoError(t, err)
			if tc.check != nil {
				tc.check(t, cfg)
			}
		})
	}
}
