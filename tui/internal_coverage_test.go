//nolint:gocritic,goimports
package tui

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/google/go-github/v68/github"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BlackbirdWorks/copilot-autodev/poller"
)

func TestInitAndGetterHelpers(t *testing.T) {
	t.Parallel()

	m := New("owner", "repo", 30, nil, nil, "cloud")
	cmd := m.Init()
	require.NotNil(t, cmd)

	// The cmd returned by tea.Batch should produce a message when invoked.
	msg := cmd()
	require.NotNil(t, msg)

	assert.Empty(t, m.Queue())
	assert.Empty(t, m.Coding())
	assert.Empty(t, m.Review())
	assert.Equal(t, 0, m.SelectedCol())
	assert.Equal(t, 0, m.SelectedRow())
	assert.Equal(t, 0, m.Width())
	assert.Equal(t, 0, m.Height())
	assert.Equal(t, [3]int{}, m.ColOffsets())
	assert.False(t, m.LogViewerOpen())
	assert.Equal(t, "", m.LogViewerTitle())
	assert.False(t, m.ActivityFeedOpen())
	assert.Equal(t, 0, m.ActivityFeedScroll())
}

func TestTickCommandFactories(t *testing.T) {
	t.Parallel()

	cmds := []tea.Cmd{secondTick(), loadingPunTick(), mergeLogReloadTick()}
	for _, cmd := range cmds {
		require.NotNil(t, cmd)
		msg := cmd()
		require.NotNil(t, msg)
	}
}

func TestUpdatePollEventHelper(t *testing.T) {
	t.Parallel()

	now := time.Now()
	issueNum := 17
	title := "Investigate flaky test"
	issue := &github.Issue{Number: github.Ptr(issueNum), Title: github.Ptr(title)}

	evt := poller.Event{
		Queue:   []*poller.State{{Issue: issue, Status: "queue", CurrentStatus: "Waiting"}},
		LastRun: now,
	}

	m := New("owner", "repo", 30, nil, nil, "cli")
	m.DoneStartup()
	m2 := m.UpdatePollEvent(evt)

	require.Len(t, m2.Queue(), 1)
	assert.Equal(t, issueNum, m2.Queue()[0].Issue.GetNumber())
}
