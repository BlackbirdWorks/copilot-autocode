//nolint:gocritic,goimports
package tui

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/BlackbirdWorks/copilot-autodev/config"
	"github.com/BlackbirdWorks/copilot-autodev/ghclient"
	"github.com/BlackbirdWorks/copilot-autodev/poller"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/google/go-github/v68/github"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type tuiFakeRoundTripper struct {
	handler func(*http.Request) (*http.Response, error)
}

func (f *tuiFakeRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	return f.handler(r)
}

func testTimelineClient(t *testing.T, handler http.HandlerFunc) *ghclient.Client {
	t.Helper()
	rt := &tuiFakeRoundTripper{
		handler: func(r *http.Request) (*http.Response, error) {
			rec := httptest.NewRecorder()
			handler(rec, r)
			return rec.Result(), nil
		},
	}
	cfg := config.DefaultConfig()
	cfg.GitHubOwner = "owner"
	cfg.GitHubRepo = "repo"
	return ghclient.NewWithTransport("token", cfg, rt)
}

func TestOpenActivityFeedAndHandleMessage(t *testing.T) {
	t.Parallel()

	client := testTimelineClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/issues/99/events"):
			_ = json.NewEncoder(w).Encode([]*github.IssueEvent{{
				Event:     github.Ptr("labeled"),
				CreatedAt: &github.Timestamp{Time: time.Now().Add(-time.Minute)},
				Actor:     &github.User{Login: github.Ptr("bot")},
				Label:     &github.Label{Name: github.Ptr("ai-coding")},
			}})
		case strings.Contains(r.URL.Path, "/issues/99/comments"):
			_ = json.NewEncoder(w).Encode([]*github.IssueComment{{
				Body:      github.Ptr("plain comment body"),
				CreatedAt: &github.Timestamp{Time: time.Now()},
				User:      &github.User{Login: github.Ptr("dev")},
			}})
		default:
			w.WriteHeader(http.StatusOK)
		}
	})

	m := New("owner", "repo", 30, nil, client, "cloud")
	m.queue = []*poller.State{
		{Issue: &github.Issue{Number: github.Ptr(99), Title: github.Ptr("Issue")}},
	}
	m.selectedCol = 0
	m.selectedRow = 0

	tm, cmd := m.openActivityFeed()
	require.NotNil(t, cmd)
	opened := tm.(Model)
	assert.True(t, opened.activityFeedOpen)
	assert.Contains(t, strings.Join(opened.activityFeedLines, "\n"), "Loading timeline")

	msg := cmd()
	feed, ok := msg.(activityFeedMsg)
	require.True(t, ok)
	require.NoError(t, feed.err)
	require.NotEmpty(t, feed.entries)

	updated, _ := opened.handleActivityFeedMsg(feed)
	withLines := updated.(Model)
	assert.True(t, len(withLines.activityFeedLines) >= 2)
}

func TestUpdateOverlayScrollClosesAndMoves(t *testing.T) {
	t.Parallel()

	m := New("owner", "repo", 30, nil, nil, "cloud")
	m.height = 4
	m.detailPaneOpen = true
	m.detailPaneLines = []string{"line0", "line1", "line2", "line3", "line4", "line5"}

	tm, _ := m.updateOverlayScroll(tea.KeyMsg{Type: tea.KeyDown}, "detail")
	m = tm.(Model)
	assert.Equal(t, 1, m.detailPaneScroll)

	tm, _ = m.updateOverlayScroll(tea.KeyMsg{Type: tea.KeyEnd}, "detail")
	m = tm.(Model)
	assert.True(t, m.detailPaneScroll >= 0)

	tm, _ = m.updateOverlayScroll(tea.KeyMsg{Type: tea.KeyEsc}, "detail")
	m = tm.(Model)
	assert.False(t, m.detailPaneOpen)
	assert.Nil(t, m.detailPaneLines)
	assert.Equal(t, 0, m.detailPaneScroll)
}

func TestOpenDetailPaneIncludesSections(t *testing.T) {
	t.Parallel()

	now := time.Now()
	issue := &github.Issue{
		Number:    github.Ptr(7),
		Title:     github.Ptr("Fix parser"),
		HTMLURL:   github.Ptr("https://example/issue/7"),
		Body:      github.Ptr("line A\nline B"),
		CreatedAt: &github.Timestamp{Time: now.Add(-2 * time.Hour)},
	}
	pr := &github.PullRequest{
		Number:         github.Ptr(70),
		HTMLURL:        github.Ptr("https://example/pr/70"),
		Head:           &github.PullRequestBranch{SHA: github.Ptr("abc123")},
		MergeableState: github.Ptr("clean"),
		Draft:          github.Ptr(false),
	}

	m := New("owner", "repo", 30, nil, nil, "cloud")
	m.review = []*poller.State{{
		Issue:         issue,
		PR:            pr,
		Status:        "review",
		CurrentStatus: "CI running",
		NextAction:    "Wait for checks",
		AgentStatus:   "pending",
	}}
	m.selectedCol = 2
	m.selectedRow = 0

	m = m.openDetailPane()
	assert.True(t, m.detailPaneOpen)
	joined := strings.Join(m.detailPaneLines, "\n")
	assert.Contains(t, joined, "Issue #7")
	assert.Contains(t, joined, "PR #70")
	assert.Contains(t, joined, "Orchestrator Status")
	assert.Contains(t, joined, "Issue Body")
}

func TestHandleActivityFeedMsgError(t *testing.T) {
	t.Parallel()
	m := New("owner", "repo", 30, nil, nil, "cloud")
	updated, _ := m.handleActivityFeedMsg(activityFeedMsg{err: fmt.Errorf("boom")})
	m2 := updated.(Model)
	require.NotEmpty(t, m2.activityFeedLines)
	assert.Contains(t, m2.activityFeedLines[0], "Error: boom")
}

func TestOpenActivityFeedNoSelectionOrClient(t *testing.T) {
	t.Parallel()
	m := New("owner", "repo", 30, nil, nil, "cloud")
	tm, cmd := m.openActivityFeed()
	assert.Equal(t, m, tm)
	assert.Nil(t, cmd)
}

func TestUpdate_ActionAndOverlayPaths(t *testing.T) {
	t.Parallel()

	client := testTimelineClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/issues/1/events"):
			_ = json.NewEncoder(w).Encode([]*github.IssueEvent{})
		case strings.Contains(r.URL.Path, "/issues/1/comments"):
			_ = json.NewEncoder(w).Encode([]*github.IssueComment{})
		default:
			w.WriteHeader(http.StatusOK)
		}
	})

	cmdCh := make(chan poller.Command, 10)
	tmp := t.TempDir()
	mainLog := tmp + "/main.log"
	mergeLog := tmp + "/merge.log"
	require.NoError(t, os.WriteFile(mainLog, []byte("INFO hello\nWARN warning\n"), 0o644))
	require.NoError(t, os.WriteFile(mergeLog, []byte("ERROR merge issue\n"), 0o644))

	m := New("owner", "repo", 30, cmdCh, client, "cloud")
	m.DoneStartup()
	m.SetLogFilePath(mainLog)
	m.width = 100
	m.height = 30
	m.queue = []*poller.State{
		{Issue: &github.Issue{Number: github.Ptr(1), Title: github.Ptr("Q")}, Status: "queue"},
	}
	m.coding = []*poller.State{{
		Issue:         &github.Issue{Number: github.Ptr(2), Title: github.Ptr("C")},
		PR:            &github.PullRequest{Number: github.Ptr(22)},
		Status:        "coding",
		CurrentStatus: mergeFixStatus,
	}}
	m.review = []*poller.State{{
		Issue:        &github.Issue{Number: github.Ptr(3), Title: github.Ptr("R")},
		PR:           &github.PullRequest{Number: github.Ptr(33)},
		Status:       "review",
		MergeLogPath: mergeLog,
	}}

	m.selectedCol = 1
	tm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	m = tm.(Model)
	require.NotNil(t, cmd)
	assert.Equal(t, "retry-merge", (<-cmdCh).Action)

	tm, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("t")})
	m = tm.(Model)
	require.NotNil(t, cmd)
	assert.Equal(t, "takeover", (<-cmdCh).Action)

	tm, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("f")})
	m = tm.(Model)
	require.NotNil(t, cmd)
	assert.Equal(t, "rerun-ci", (<-cmdCh).Action)

	m.selectedCol = 0
	m.selectedRow = 0
	tm, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("+")})
	m = tm.(Model)
	require.NotNil(t, cmd)
	assert.Equal(t, "priority-up", (<-cmdCh).Action)

	tm, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("-")})
	m = tm.(Model)
	require.NotNil(t, cmd)
	assert.Equal(t, "priority-down", (<-cmdCh).Action)

	// Open activity feed then close it through overlay key handling.
	tm, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	m = tm.(Model)
	require.NotNil(t, cmd)
	msg := cmd()
	tm, _ = m.Update(msg)
	m = tm.(Model)
	assert.True(t, m.activityFeedOpen)
	tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = tm.(Model)
	assert.False(t, m.activityFeedOpen)

	// Open and close detail pane.
	tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = tm.(Model)
	assert.True(t, m.detailPaneOpen)
	tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = tm.(Model)
	assert.False(t, m.detailPaneOpen)

	// Open default log viewer and close it.
	tm, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("L")})
	m = tm.(Model)
	require.NotNil(t, cmd)
	assert.True(t, m.logViewerOpen)
	tm, _ = m.Update(mergeLogReloadMsg{})
	m = tm.(Model)
	tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = tm.(Model)
	assert.False(t, m.logViewerOpen)

	// Open selected merge log with live-tail command path.
	m.selectedCol = 2
	m.selectedRow = 0
	tm, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("v")})
	m = tm.(Model)
	require.NotNil(t, cmd)
	assert.True(t, m.logViewerOpen)
	assert.True(t, m.mergeLogTailing)
	tm, _ = m.Update(mergeLogReloadMsg{})
	m = tm.(Model)
	assert.NotEmpty(t, m.logViewerLines)
}

func TestRenderStatusAndLogsBranches(t *testing.T) {
	t.Parallel()

	m := New("owner", "repo", 30, nil, nil, "cloud")
	m.width = 80
	m.height = 20
	m.logs = make([]string, logHistorySize)
	m.logs[0] = "INFO all good"
	m.logs[1] = "WARN maybe"
	m.logs[2] = "ERROR broken"
	m.logs[3] = "DEBUG trace"
	m.logHead = 4

	renderedLogs := m.renderLogs(3)
	assert.Contains(t, renderedLogs, "WARN")
	assert.Contains(t, renderedLogs, "ERROR")
	assert.Contains(t, renderedLogs, "DEBUG")

	status := m.renderStatus()
	assert.Contains(t, status, "Waiting for first poll")

	m.lastRun = time.Now().Add(-10 * time.Second)
	m.interval = 1
	m.lastErr = errors.New(strings.Repeat("x", 120))
	status = m.renderStatus()
	assert.Contains(t, status, "Polling now")
	assert.Contains(t, status, "!")

	m.lastErr = nil
	m.lastWarn = strings.Repeat("w", 120)
	status = m.renderStatus()
	assert.Contains(t, status, "!")
}

func TestUpdateLogViewerExtraKeys(t *testing.T) {
	t.Parallel()

	m := New("owner", "repo", 30, nil, nil, "cloud")
	m.height = 6
	m.logViewerOpen = true
	m.logViewerLines = []string{"a", "b", "c", "d", "e", "f", "g"}

	tm, _ := m.updateLogViewer(tea.KeyMsg{Type: tea.KeyPgUp})
	m = tm.(Model)
	assert.True(t, m.logViewerScroll > 0)

	tm, _ = m.updateLogViewer(tea.KeyMsg{Type: tea.KeyPgDown})
	m = tm.(Model)
	tm, _ = m.updateLogViewer(tea.KeyMsg{Type: tea.KeyHome})
	m = tm.(Model)
	tm, _ = m.updateLogViewer(tea.KeyMsg{Type: tea.KeyEnd})
	m = tm.(Model)

	tm, cmd := m.updateLogViewer(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("c")})
	m = tm.(Model)
	require.NotNil(t, cmd)
	assert.True(t, m.logsCopied)
}
