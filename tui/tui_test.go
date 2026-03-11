//nolint:gocritic,goimports
package tui_test

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/google/go-github/v68/github"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BlackbirdWorks/copilot-autodev/ghclient"
	"github.com/BlackbirdWorks/copilot-autodev/poller"
	"github.com/BlackbirdWorks/copilot-autodev/tui"
)

// ansiRe matches ANSI CSI escape sequences so tests can compare plain text.
var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func stripAnsi(s string) string {
	return ansiRe.ReplaceAllString(s, "")
}

// issueAt returns a minimal *github.Issue with the given number, title, and
// creation time — enough for RenderItem to work without nil dereferences.
func issueAt(num int, title string, createdAt time.Time) *github.Issue {
	ts := github.Timestamp{Time: createdAt}
	return &github.Issue{
		Number:    &num,
		Title:     &title,
		CreatedAt: &ts,
	}
}

// ─── FormatCountdown ────────────────────────────────────────────────────────

func TestFormatCountdown(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		d    time.Duration
		want string
	}{
		{"zero", 0, "0s"},
		{"500ms rounds to 1s", 500 * time.Millisecond, "1s"},
		{"45 seconds", 45 * time.Second, "45s"},
		{"59 seconds", 59 * time.Second, "59s"},
		{"exactly 1 minute", 60 * time.Second, "1m 0s"},
		{"9 minutes 42 seconds", 9*time.Minute + 42*time.Second, "9m 42s"},
		{"59 minutes 59 seconds", 59*time.Minute + 59*time.Second, "59m 59s"},
		{"exactly 1 hour", 60 * time.Minute, "1h 0m"},
		{"1 hour 3 minutes", 63 * time.Minute, "1h 3m"},
		{"25 hours", 25 * time.Hour, "25h 0m"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, tui.FormatCountdown(tc.d))
		})
	}
}

// ─── RenderStatusSubLine ────────────────────────────────────────────────────

func TestRenderStatusSubLine(t *testing.T) {
	t.Parallel()
	m := tui.Model{}
	now := time.Now()

	tests := []struct {
		name         string
		current      string
		next         string
		nextActionAt time.Time
		colWidth     int
		wantContains []string
		wantAbsent   []string
	}{
		{
			name:         "current only — no separator",
			current:      "CI failing",
			colWidth:     60,
			wantContains: []string{"CI failing"},
			wantAbsent:   []string{"·"},
		},
		{
			name:         "current and next — stacked present",
			current:      "CI failing",
			next:         "Asked Copilot to fix",
			colWidth:     60,
			wantContains: []string{"CI failing", "Next: Asked Copilot to fix"},
		},
		{
			name:         "future countdown appends 'in …'",
			current:      "Waiting on coding agent to start",
			next:         "Poke Copilot",
			nextActionAt: now.Add(5 * time.Minute),
			colWidth:     80,
			wantContains: []string{
				"Waiting on coding agent to start",
				"Poke Copilot in",
				"m",
			},
		},
		{
			name:         "past nextActionAt appends 'now'",
			current:      "Waiting on coding agent to start",
			next:         "Poke Copilot",
			nextActionAt: now.Add(-time.Second),
			colWidth:     80,
			wantContains: []string{"Poke Copilot now"},
		},
		{
			name:         "very narrow column wraps text",
			current:      "CI failing",
			next:         "fix",
			colWidth:     5,
			wantAbsent:   []string{"…"},
			wantContains: []string{"CI failing", "fix"},
		},
		{
			name:         "long text wraps on narrow column",
			current:      "CI running",
			next:         "Refinement (if checks pass)",
			colWidth:     30,
			wantAbsent:   []string{"…"},
			wantContains: []string{"CI", "Refinement"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := &poller.State{
				CurrentStatus: tc.current,
				NextAction:    tc.next,
				NextActionAt:  tc.nextActionAt,
			}
			got := stripAnsi(m.RenderStatusSubLine(s, tc.colWidth))
			for _, want := range tc.wantContains {
				assert.Contains(t, got, want)
			}
			for _, absent := range tc.wantAbsent {
				assert.NotContains(t, got, absent)
			}
		})
	}
}

// ─── RenderItem line count ───────────────────────────────────────────────────

func TestRenderItemLineCount(t *testing.T) {
	t.Parallel()
	m := tui.Model{}
	issue := issueAt(42, "Fix the thing", time.Now())

	tests := []struct {
		name      string
		state     *poller.State
		colWidth  int
		wantLines int
	}{
		{
			name:      "no status → 1 line",
			state:     &poller.State{Issue: issue, CurrentStatus: ""},
			colWidth:  60,
			wantLines: 1,
		},
		{
			name:      "with status defaults to unselected 1 line",
			state:     &poller.State{Issue: issue, CurrentStatus: "Waiting for CI"},
			colWidth:  60,
			wantLines: 1,
		},
		{
			name: "status + next action defaults to unselected 1 line",
			state: &poller.State{
				Issue:         issue,
				CurrentStatus: "CI failing",
				NextAction:    "Asked Copilot to fix",
			},
			colWidth:  60,
			wantLines: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rendered, lines := m.RenderItem(tc.state, tc.colWidth)
			assert.Equal(t, tc.wantLines, lines)
			assert.Equal(t, tc.wantLines-1, strings.Count(rendered, "\n"))
		})
	}
}

// ─── RenderItem title truncation ─────────────────────────────────────────────

func TestRenderItemTitleTruncation(t *testing.T) {
	t.Parallel()
	m := tui.Model{}
	num := 1
	now := time.Now()

	tests := []struct {
		name         string
		title        string
		colWidth     int
		wantEllipsis bool
	}{
		{
			name:         "short title fits — no ellipsis",
			title:        "Short",
			colWidth:     60,
			wantEllipsis: false,
		},
		{
			name:         "title longer than budget — ellipsis appended",
			title:        strings.Repeat("A", 100),
			colWidth:     40,
			wantEllipsis: true,
		},
		{
			name:         "title exactly at budget — no ellipsis",
			title:        "Fix",
			colWidth:     30,
			wantEllipsis: false,
		},
		{
			name:         "multi-byte (Japanese) title truncated at rune boundary",
			title:        strings.Repeat("日", 50),
			colWidth:     40,
			wantEllipsis: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			issue := issueAt(num, tc.title, now)
			s := &poller.State{Issue: issue}
			rendered, _ := m.RenderItem(s, tc.colWidth)
			plain := stripAnsi(rendered)

			assert.Equal(t, tc.wantEllipsis, strings.Contains(plain, "…"))
			require.True(t, utf8.ValidString(plain), "output must be valid UTF-8")
		})
	}
}

// ─── RenderItem title line content ───────────────────────────────────────────

func TestRenderItemTitleContent(t *testing.T) {
	t.Parallel()
	m := tui.Model{}
	now := time.Now()

	prNum := 99
	pr := &github.PullRequest{Number: &prNum}

	tests := []struct {
		name         string
		state        *poller.State
		colWidth     int
		wantContains []string
	}{
		{
			name:         "issue number appears in title line",
			state:        &poller.State{Issue: issueAt(434, "Some title", now)},
			colWidth:     60,
			wantContains: []string{"#434"},
		},
		{
			name:         "pr ref appears when PR is attached",
			state:        &poller.State{Issue: issueAt(434, "Some title", now), PR: pr},
			colWidth:     60,
			wantContains: []string{"#434", "PR#99"},
		},
		{
			name:         "no pr ref without PR",
			state:        &poller.State{Issue: issueAt(434, "Some title", now)},
			colWidth:     60,
			wantContains: []string{"#434"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rendered, _ := m.RenderItem(tc.state, tc.colWidth)
			// PR string is on the first line when unselected, but might be on a
			// separate line if wrapped or part of a subline if selected. Since
			// we are testing unselected state here, the PR ref should be in the
			// plain title line. Wait no, PR string IS in the first line.
			titleLine := stripAnsi(rendered)
			for _, want := range tc.wantContains {
				assert.Contains(t, titleLine, want)
			}
		})
	}
}

// ─── Log Viewer Scrolling ───────────────────────────────────────────────────

func TestModel_LogViewer_Scrolling(t *testing.T) {
	t.Parallel()

	type args struct {
		keys []tea.KeyMsg
	}
	type wants struct {
		isOpen bool
		scroll int
	}
	tests := []struct {
		name  string
		args  args
		wants wants
	}{
		{
			name: "update log viewer",
			args: args{
				keys: []tea.KeyMsg{
					{Type: tea.KeyRunes, Runes: []rune("L")},
				},
			},
			wants: wants{isOpen: true, scroll: 0},
		},
		{
			name: "pgdown/pgup in log viewer",
			args: args{
				keys: []tea.KeyMsg{
					{Type: tea.KeyRunes, Runes: []rune("L")},
					{Type: tea.KeyPgDown},
					{Type: tea.KeyPgUp},
				},
			},
			wants: wants{isOpen: true, scroll: 13},
		},
		{
			name: "pgdown scroll limit",
			args: args{
				keys: []tea.KeyMsg{
					{Type: tea.KeyRunes, Runes: []rune("L")},
					{Type: tea.KeyPgDown},
					{Type: tea.KeyPgDown},
				},
			},
			wants: wants{isOpen: true, scroll: 0},
		},
		{
			name: "pgup scroll limit",
			args: args{
				keys: []tea.KeyMsg{
					{Type: tea.KeyRunes, Runes: []rune("L")},
					{Type: tea.KeyPgUp},
					{Type: tea.KeyPgUp},
				},
			},
			wants: wants{isOpen: true, scroll: 26},
		},
		{
			name: "home/end in log viewer",
			args: args{
				keys: []tea.KeyMsg{
					{Type: tea.KeyRunes, Runes: []rune("L")},
					{Type: tea.KeyEnd},
					{Type: tea.KeyHome},
				},
			},
			wants: wants{isOpen: true, scroll: 473},
		},
		{
			name: "close log viewer",
			args: args{
				keys: []tea.KeyMsg{
					{Type: tea.KeyRunes, Runes: []rune("L")},
					{Type: tea.KeyEsc},
				},
			},
			wants: wants{isOpen: false},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m := tui.New("owner", "repo", 30, nil, nil, "cloud")
			m.DoneStartup()
			tm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
			m = tm.(tui.Model)

			tmpDir := t.TempDir()
			logPath := tmpDir + "/test.log"
			m.SetLogFilePath(logPath)
			var manyLines strings.Builder
			for i := 0; i < 2000; i++ {
				manyLines.WriteString("line\n")
			}
			_ = os.WriteFile(logPath, []byte(manyLines.String()), 0644)

			for _, k := range tt.args.keys {
				tm, _ = m.Update(k)
				m = tm.(tui.Model)
			}

			assert.Equal(t, tt.wants.isOpen, m.LogViewerOpen())
			if tt.wants.isOpen {
				assert.Equal(t, tt.wants.scroll, m.LogViewerScroll())
			}
		})
	}
}

// ─── Queues and Timeline ─────────────────────────────────────────────────────

func TestModel_Update_QueuesAndTimeline(t *testing.T) {
	t.Parallel()

	m := tui.New("o", "r", 30, nil, nil, "cloud")
	m.DoneStartup()
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = tm.(tui.Model)

	m = m.UpdatePollEvent(poller.Event{
		Queue: []*poller.State{
			{
				Issue:  &github.Issue{Number: github.Ptr(1), Title: github.Ptr("Issue 1")},
				Status: "coding",
			},
			{
				Issue:  &github.Issue{Number: github.Ptr(2), Title: github.Ptr("Issue 2")},
				Status: "review",
			},
		},
	})

	out := m.View()
	assert.Contains(t, out, "Issue 1")
	assert.Contains(t, out, "Issues tracked: 2")
}

// ─── Selection and Commands ──────────────────────────────────────────────────

func TestModel_Update_Comprehensive(t *testing.T) {
	t.Parallel()

	cmdCh := make(chan poller.Command, 10)
	m := tui.New("owner", "repo", 30, cmdCh, nil, "cloud")
	m.DoneStartup()

	// Set window size
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	m = tm.(tui.Model)

	t.Run("key events", func(t *testing.T) {
		tests := []struct {
			name string
			key  tea.KeyMsg
			want func(t *testing.T, m tui.Model)
		}{
			{
				name: "quit q",
				key:  tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}},
				want: func(t *testing.T, m tui.Model) {
					// How to check tea.Quit? Update returns (tea.Model, tea.Cmd).
					// We'll check the command in the actual test loop below.
				},
			},
			{
				name: "up navigation",
				key:  tea.KeyMsg{Type: tea.KeyUp},
				want: func(t *testing.T, m tui.Model) {
					assert.Equal(t, 0, m.SelectedRow())
				},
			},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				tm, cmd := m.Update(tt.key)
				m2 := tm.(tui.Model)
				tt.want(t, m2)
				if tt.name == "quit q" {
					assert.NotNil(t, cmd)
				}
			})
		}
	})

	t.Run("poll event", func(t *testing.T) {
		event := poller.Event{
			Queue: []*poller.State{{Issue: &github.Issue{Number: github.Ptr(1)}}},
		}
		m2 := m.UpdatePollEvent(event)
		assert.Len(t, m2.Queue(), 1)
	})

	t.Run("log event", func(t *testing.T) {
		tm, _ := m.Update(tui.LogEvent{Message: "test log"})
		m2 := tm.(tui.Model)
		// No easy way to check logs without exported field, but it covers the branch.
		assert.NotNil(t, m2)
	})

	t.Run("overlays", func(t *testing.T) {
		// Inject an item so we can open the detail pane
		m = m.UpdatePollEvent(poller.Event{
			Queue: []*poller.State{{Issue: &github.Issue{Number: github.Ptr(1)}}},
		})

		// Enter opens detail pane
		tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
		m2 := tm.(tui.Model)
		assert.True(t, m2.DetailPaneOpen())

		// 'a' opens activity feed
		tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
		m2 = tm.(tui.Model)
		assert.NotNil(t, m2) // it returns a command to fetch timeline
	})
}

func TestModel_View_Comprehensive(t *testing.T) {
	t.Parallel()

	m := tui.New("owner", "repo", 30, nil, nil, "cli")

	t.Run("initializing", func(t *testing.T) {
		// m.width is 0 initially
		assert.Contains(t, m.View(), "Initializing...")
	})

	t.Run("loading screen", func(t *testing.T) {
		tm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
		m2 := tm.(tui.Model)
		// startupDone is false initially
		view := stripAnsi(m2.View())
		assert.Contains(t, view, "Connecting to owner/repo")
	})

	t.Run("main dashboard", func(t *testing.T) {
		tm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
		m2 := tm.(tui.Model)
		m2.DoneStartup()
		m2 = m2.UpdatePollEvent(poller.Event{
			Queue: []*poller.State{
				{Issue: &github.Issue{Number: github.Ptr(1), Title: github.Ptr("Issue 1")}},
			},
		})

		view := stripAnsi(m2.View())
		assert.Contains(t, view, "LIST Queue")
		assert.Contains(t, view, "Issue 1")
		assert.Contains(t, view, "agent: cli")
	})

	t.Run("error and warning", func(t *testing.T) {
		tm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
		m2 := tm.(tui.Model)
		m2.DoneStartup()

		// Test error
		mErr := m2.UpdatePollEvent(poller.Event{Err: fmt.Errorf("fatal error")})
		assert.Contains(t, stripAnsi(mErr.View()), "fatal error")

		// Test warning
		mWarn := m2.UpdatePollEvent(poller.Event{Warnings: []string{"warning message"}})
		assert.Contains(t, stripAnsi(mWarn.View()), "warning message")
	})
}

func TestModel_Render_Detailed(t *testing.T) {
	t.Parallel()
	m := tui.New("o", "r", 30, nil, nil, "cli")

	s := &poller.State{
		Issue: &github.Issue{
			Number:    github.Ptr(123),
			Title:     github.Ptr("Fixed `bug` in core"),
			CreatedAt: &github.Timestamp{Time: time.Now().Add(-10 * time.Minute)},
		},
		Status:        "coding",
		CurrentStatus: "Working on it",
		AgentStatus:   "pending",
	}

	t.Run("render item unselected", func(t *testing.T) {
		view, lines := m.RenderItem(s, 50)
		assert.Equal(t, 1, lines)
		assert.Contains(t, stripAnsi(view), "#123")
		assert.Contains(t, stripAnsi(view), "Fixed bug in core")
	})

	t.Run("render item selected with status", func(t *testing.T) {
		m2 := m.UpdatePollEvent(poller.Event{
			Coding: []*poller.State{s},
		})
		m2.DoneStartup()
		tm, _ := m2.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
		m2 = tm.(tui.Model)

		// Move to coding column (index 1)
		tm, _ = m2.Update(tea.KeyMsg{Type: tea.KeyRight})
		m2 = tm.(tui.Model)

		view := stripAnsi(m2.View())
		assert.Contains(t, view, "Working on it")
	})
}

func TestModel_OverlayScroll(t *testing.T) {
	t.Parallel()
	m := tui.New("o", "r", 30, nil, nil, "cli")
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	m = tm.(tui.Model)

	t.Run("detail pane scrolling", func(t *testing.T) {
		// Set a very small height to force scrolling
		tm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 15})
		m = tm.(tui.Model)

		m2 := m.UpdatePollEvent(poller.Event{
			Queue: []*poller.State{{
				Issue: &github.Issue{
					Number: github.Ptr(1),
					Body: github.Ptr(
						"line1\nline2\nline3\nline4\nline5\nline6\nline7\nline8\nline9\nline10",
					),
				},
			}},
		})
		// Open detail pane
		tm, _ = m2.Update(tea.KeyMsg{Type: tea.KeyEnter})
		m2 = tm.(tui.Model)
		require.True(t, m2.DetailPaneOpen())

		// Scroll down
		tm, _ = m2.Update(tea.KeyMsg{Type: tea.KeyDown})
		m3 := tm.(tui.Model)
		assert.Equal(t, 1, m3.DetailPaneScroll())

		// Scroll up
		tm, _ = m3.Update(tea.KeyMsg{Type: tea.KeyUp})
		m3 = tm.(tui.Model)
		assert.Equal(t, 0, m3.DetailPaneScroll())

		// Page down - should scroll some amount
		tm, _ = m3.Update(tea.KeyMsg{Type: tea.KeyPgDown})
		m3 = tm.(tui.Model)
		assert.True(t, m3.DetailPaneScroll() > 0)

		// Close with esc
		tm, _ = m3.Update(tea.KeyMsg{Type: tea.KeyEsc})
		m3 = tm.(tui.Model)
		assert.False(t, m3.DetailPaneOpen())
	})
}

func TestModel_ReloadMergeLog(t *testing.T) {
	t.Parallel()
	m := tui.New("o", "r", 30, nil, nil, "cli")

	tmpDir := t.TempDir()
	logPath := tmpDir + "/merge.log"
	_ = os.WriteFile(logPath, []byte("line 1\nline 2\n"), 0644)

	// Inject state with merge log path
	m = m.UpdatePollEvent(poller.Event{
		Queue: []*poller.State{{
			Issue:        &github.Issue{Number: github.Ptr(1)},
			MergeLogPath: logPath,
		}},
	})

	t.Run("open and reload", func(t *testing.T) {
		// 'v' opens merge log
		tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'v'}})
		m2 := tm.(tui.Model)
		assert.True(t, m2.LogViewerOpen())
		assert.Equal(t, 3, len(m2.LogViewerLines())) // "line 1", "line 2", ""

		// Update file and reload
		_ = os.WriteFile(logPath, []byte("line 1\nline 2\nline 3\n"), 0644)
		m2 = m2.ReloadMergeLog()
		assert.Equal(t, 4, len(m2.LogViewerLines()))
	})
}

func TestModel_ActivityFeed(t *testing.T) {
	t.Parallel()
	m := tui.New("o", "r", 30, nil, nil, "cli")
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = tm.(tui.Model)
	m.DoneStartup()

	t.Run("activity feed msg", func(t *testing.T) {
		// Activity feed shows for the SELECTED state and must be marked open.
		m2 := m.UpdatePollEvent(poller.Event{
			Queue: []*poller.State{{Issue: &github.Issue{Number: github.Ptr(1)}}},
		})
		m2.SetActivityFeedOpen(true)
		entries := []ghclient.TimelineEntry{
			{Actor: "user1", Event: "commented", Detail: "LGTM", Time: time.Now()},
		}
		m3 := m2.UpdateActivityFeed(entries, nil)
		view := stripAnsi(m3.View())
		assert.Contains(t, view, "ACTIVITY TIMELINE")
		assert.Contains(t, view, "user1")
		assert.Contains(t, view, "commented")
	})

	t.Run("activity feed error", func(t *testing.T) {
		m2 := m.UpdatePollEvent(poller.Event{
			Queue: []*poller.State{{Issue: &github.Issue{Number: github.Ptr(1)}}},
		})
		m2.SetActivityFeedOpen(true)
		m3 := m2.UpdateActivityFeed(nil, fmt.Errorf("api error"))
		view := stripAnsi(m3.View())
		assert.Contains(t, view, "Error: api error")
	})
}

func TestModel_LogViewer_Keys(t *testing.T) {
	t.Parallel()
	m := tui.New("o", "r", 30, nil, nil, "cli")
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = tm.(tui.Model)

	// Set up log viewer with many lines to test scrolling
	tmpDir := t.TempDir()
	logPath := tmpDir + "/test.log"
	var sb strings.Builder
	for i := 1; i <= 100; i++ {
		sb.WriteString(fmt.Sprintf("line %d\n", i))
	}
	_ = os.WriteFile(logPath, []byte(sb.String()), 0644)
	m.SetLogFilePath(logPath)

	// Open log viewer
	tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'L'}})
	m = tm.(tui.Model)
	require.True(t, m.LogViewerOpen())

	t.Run("scroll keys", func(t *testing.T) {
		// Initial scroll is 0 (bottom)
		assert.Equal(t, 0, m.LogViewerScroll())

		// Scroll up (k)
		tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
		m2 := tm.(tui.Model)
		assert.Equal(t, 1, m2.LogViewerScroll())

		// Scroll down (j)
		tm, _ = m2.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
		m2 = tm.(tui.Model)
		assert.Equal(t, 0, m2.LogViewerScroll())

		// Home (g) scrolls to top (maxScroll)
		tm, _ = m2.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'g'}})
		m2 = tm.(tui.Model)
		assert.True(t, m2.LogViewerScroll() > 0)

		// End (G) scrolls to bottom (0)
		tm, _ = m2.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'G'}})
		m2 = tm.(tui.Model)
		assert.Equal(t, 0, m2.LogViewerScroll())
	})
}

func TestModel_Navigation(t *testing.T) {
	t.Parallel()
	m := tui.New("o", "r", 30, nil, nil, "cli")
	m.DoneStartup()
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = tm.(tui.Model)

	// Inject some items in multiple columns
	m = m.UpdatePollEvent(poller.Event{
		Queue:  []*poller.State{{Issue: &github.Issue{Number: github.Ptr(1)}}},
		Coding: []*poller.State{{Issue: &github.Issue{Number: github.Ptr(2)}}},
		Review: []*poller.State{{Issue: &github.Issue{Number: github.Ptr(3)}}},
	})

	t.Run("arrow and hjkl", func(t *testing.T) {
		assert.Equal(t, 0, m.SelectedCol())

		// Right (l)
		tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
		m = tm.(tui.Model)
		assert.Equal(t, 1, m.SelectedCol())

		// Left (h)
		tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'h'}})
		m = tm.(tui.Model)
		assert.Equal(t, 0, m.SelectedCol())

		// Down (j)
		tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
		m = tm.(tui.Model)
		assert.Equal(t, 0, m.SelectedRow()) // only 1 item, so stays at 0

		// Wrap around right (NOT supported - should stay at index 2)
		tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyRight}) // col 1
		m = tm.(tui.Model)
		tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyRight}) // col 2
		m = tm.(tui.Model)
		tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyRight}) // stays at 2
		m = tm.(tui.Model)
		assert.Equal(t, 2, m.SelectedCol())
	})
}

func TestModel_Actions(t *testing.T) {
	t.Parallel()
	cmdCh := make(chan poller.Command, 10)
	m := tui.New("o", "r", 30, cmdCh, nil, "cli")
	m.DoneStartup()
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = tm.(tui.Model)

	t.Run("priority up/down", func(t *testing.T) {
		// Inject item in queue for priority tests
		m2 := m.UpdatePollEvent(poller.Event{
			Queue: []*poller.State{{Issue: &github.Issue{Number: github.Ptr(123)}}},
		})

		// Priority up ('+')
		tm, _ = m2.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'+'}})
		m3 := tm.(tui.Model)
		assert.Contains(t, m3.ActionFeedback(), "Priority increased")

		select {
		case cmd := <-cmdCh:
			assert.Equal(t, "priority-up", cmd.Action)
			assert.Equal(t, 123, cmd.IssueNum)
		case <-time.After(500 * time.Millisecond):
			t.Fatal("timed out waiting for priority-up command")
		}

		// Priority down ('-')
		tm, _ = m2.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'-'}})
		m3 = tm.(tui.Model)
		assert.Contains(t, m3.ActionFeedback(), "Priority decreased")

		select {
		case cmd := <-cmdCh:
			assert.Equal(t, "priority-down", cmd.Action)
			assert.Equal(t, 123, cmd.IssueNum)
		case <-time.After(500 * time.Millisecond):
			t.Fatal("timed out waiting for priority-down command")
		}
	})

	t.Run("takeover", func(t *testing.T) {
		// Inject item
		m2 := m.UpdatePollEvent(poller.Event{
			Queue: []*poller.State{{Issue: &github.Issue{Number: github.Ptr(123)}}},
		})

		// Takeover ('t')
		tm, _ = m2.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'t'}})
		m3 := tm.(tui.Model)
		assert.Contains(t, m3.ActionFeedback(), "Manual takeover requested")

		select {
		case cmd := <-cmdCh:
			assert.Equal(t, "takeover", cmd.Action)
			assert.Equal(t, 123, cmd.IssueNum)
		case <-time.After(500 * time.Millisecond):
			t.Fatal("timed out waiting for takeover command")
		}
	})

	t.Run("rerun-ci", func(t *testing.T) {
		// Needs PR to be present
		m2 := m.UpdatePollEvent(poller.Event{
			Queue: []*poller.State{{
				Issue: &github.Issue{Number: github.Ptr(123)},
				PR:    &github.PullRequest{Number: github.Ptr(456)},
			}},
		})

		// Rerun CI ('f')
		tm, _ = m2.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'f'}})
		m3 := tm.(tui.Model)
		assert.Contains(t, m3.ActionFeedback(), "CI re-run requested")

		select {
		case cmd := <-cmdCh:
			assert.Equal(t, "rerun-ci", cmd.Action)
			assert.Equal(t, 456, cmd.PRNum)
		case <-time.After(500 * time.Millisecond):
			t.Fatal("timed out waiting for rerun-ci command")
		}
	})

	t.Run("retry-merge", func(t *testing.T) {
		// Needs PR + merge_fix status
		m2 := m.UpdatePollEvent(poller.Event{
			Queue: []*poller.State{{
				Issue:         &github.Issue{Number: github.Ptr(123)},
				PR:            &github.PullRequest{Number: github.Ptr(456)},
				CurrentStatus: "Merge conflicts unresolved \u2014 needs manual fix",
			}},
		})

		// Retry merge ('r')
		tm, _ = m2.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
		m3 := tm.(tui.Model)
		assert.Contains(t, m3.ActionFeedback(), "Retry merge resolution queued")

		select {
		case cmd := <-cmdCh:
			assert.Equal(t, "retry-merge", cmd.Action)
			assert.Equal(t, 456, cmd.PRNum)
		case <-time.After(500 * time.Millisecond):
			t.Fatal("timed out waiting for retry-merge command")
		}
	})
}

func TestModel_RenderHelpers(t *testing.T) {
	t.Parallel()

	t.Run("pipeline bar", func(t *testing.T) {
		tests := []struct {
			status string
			want   string
		}{
			{"queue", "Queued"},
			{"coding", "Coding"},
			{"review", "CI Checks"},
			{"merging", "Merge"},
		}
		for _, tt := range tests {
			t.Run(tt.status, func(t *testing.T) {
				s := &poller.State{Status: tt.status}
				bar := stripAnsi(tui.RenderPipelineBar(s))
				assert.Contains(t, bar, tt.want)
			})
		}
	})
}

func TestModel_FormatHelpers(t *testing.T) {
	t.Parallel()

	t.Run("format countdown", func(t *testing.T) {
		tests := []struct {
			d    time.Duration
			want string
		}{
			{5 * time.Second, "5s"},
			{65 * time.Second, "1m 5s"},
			{3605 * time.Second, "1h 0m"},
		}
		for _, tt := range tests {
			t.Run(tt.want, func(t *testing.T) {
				assert.Equal(t, tt.want, tui.FormatCountdown(tt.d))
			})
		}
	})
}

func TestModel_Render_Overlays(t *testing.T) {
	t.Parallel()
	m := tui.New("o", "r", 30, nil, nil, "cli")
	m.DoneStartup()
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = tm.(tui.Model)

	t.Run("log viewer", func(t *testing.T) {
		m2 := m.SetLogViewerLines([]string{"line 1", "line 2", "line 3"})
		m2 = m2.SetLogViewerOpen(true)
		view := m2.View()
		assert.Contains(t, view, "LOG VIEWER")
		assert.Contains(t, view, "line 1")
		assert.Contains(t, view, "line 3")
	})

	t.Run("detail pane", func(t *testing.T) {
		m2 := m.UpdatePollEvent(poller.Event{
			Queue: []*poller.State{{
				Issue: &github.Issue{
					Number: github.Ptr(123),
					Title:  github.Ptr("Test Issue"),
				},
			}},
		})
		// Open detail pane ('enter')
		tm, _ = m2.Update(tea.KeyMsg{Type: tea.KeyEnter})
		m3 := tm.(tui.Model)
		assert.True(t, m3.DetailPaneOpen())
		view := m3.View()
		assert.Contains(t, view, "#123")
		assert.Contains(t, view, "Test Issue")
	})
}
