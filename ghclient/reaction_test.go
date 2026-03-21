//nolint:gocritic,goimports
package ghclient_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/google/go-github/v68/github"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCopilotEngagementStatus(t *testing.T) {
	t.Parallel()
	now := time.Now()

	tests := []struct {
		name          string
		comments      []*github.IssueComment
		reactions     map[int64][]*github.Reaction
		wantEngaged   bool
		wantReactedAt time.Time
	}{
		{
			name:        "no comments",
			comments:    nil,
			wantEngaged: false,
		},
		{
			name: "agent already replied",
			comments: []*github.IssueComment{
				{
					Body: github.Ptr("@copilot please do X"),
					User: &github.User{Login: github.Ptr("user1")},
				},
				{
					Body: github.Ptr("I have completed X"),
					User: &github.User{Login: github.Ptr("copilot-swe-agent[bot]")},
				},
			},
			wantEngaged: false,
		},
		{
			name: "agent reacted but hasn't replied",
			comments: []*github.IssueComment{
				{
					ID:        github.Ptr(int64(10)),
					Body:      github.Ptr("@copilot please do X"),
					User:      &github.User{Login: github.Ptr("copilot-autodev")},
					CreatedAt: &github.Timestamp{Time: now},
					Reactions: &github.Reactions{TotalCount: github.Ptr(1)},
				},
			},
			reactions: map[int64][]*github.Reaction{
				10: {
					{User: &github.User{Login: github.Ptr("copilot-swe-agent[bot]")}},
				},
			},
			wantEngaged:   true,
			wantReactedAt: now,
		},
		{
			name: "another user reacted, not agent",
			comments: []*github.IssueComment{
				{
					ID:        github.Ptr(int64(20)),
					Body:      github.Ptr("@copilot please do X"),
					User:      &github.User{Login: github.Ptr("copilot-autodev")},
					CreatedAt: &github.Timestamp{Time: now},
					Reactions: &github.Reactions{TotalCount: github.Ptr(1)},
				},
			},
			reactions: map[int64][]*github.Reaction{
				20: {
					{User: &github.User{Login: github.Ptr("other-user")}},
				},
			},
			wantEngaged: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := setupMockGitHubAPI(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/repos/test-owner/test-repo/issues/1/comments" {
					_ = json.NewEncoder(w).Encode(tc.comments)
					return
				}
				var commentID int64
				_, _ = fmt.Sscanf(r.URL.Path, "/repos/test-owner/test-repo/issues/comments/%d/reactions", &commentID)
				if commentID > 0 {
					reacts := tc.reactions[commentID]
					if reacts == nil {
						reacts = []*github.Reaction{}
					}
					_ = json.NewEncoder(w).Encode(reacts)
					return
				}
				w.WriteHeader(http.StatusNotFound)
			})

			engaged, reactedAt, err := c.CopilotEngagementStatus(t.Context(), 1)
			require.NoError(t, err)
			assert.Equal(t, tc.wantEngaged, engaged)
			if tc.wantEngaged {
				assert.Equal(t, tc.wantReactedAt.Unix(), reactedAt.Unix())
			} else {
				assert.True(t, reactedAt.IsZero())
			}
		})
	}
}
