package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/BlackbirdWorks/copilot-autocode/ghclient"
	"github.com/BlackbirdWorks/copilot-autocode/poller"
	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// PollEvent wraps a poller.Event for delivery into the Bubble Tea message bus.
type PollEvent struct{ poller.Event }

// secondTickMsg is fired every second so live countdown timers in item
// status sub-lines stay up-to-date between poll events.
type secondTickMsg time.Time

func secondTick() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg {
		return secondTickMsg(t)
	})
}

// Model is the root Bubble Tea model for the dashboard.
type Model struct {
	spinner spinner.Model
	width   int
	height  int

	queue    []*poller.State
	coding   []*poller.State
	review   []*poller.State
	lastRun  time.Time
	lastErr  error
	lastWarn string // most recent non-fatal warning (e.g. Copilot assignment failure)

	owner string
	repo  string
}

// New creates a fresh Model.
func New(owner, repo string) Model {
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("#00ff87"))
	return Model{
		spinner: sp,
		owner:   owner,
		repo:    repo,
	}
}

// Init starts the spinner and the per-second countdown tick.
func (m Model) Init() tea.Cmd {
	return tea.Batch(m.spinner.Tick, secondTick())
}

// Update handles all messages.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.KeyMsg:
		if msg.String() == "ctrl+c" || msg.String() == "q" {
			return m, tea.Quit
		}

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height

	case PollEvent:
		m.queue = msg.Queue
		m.coding = msg.Coding
		m.review = msg.Review
		m.lastRun = msg.LastRun
		m.lastErr = msg.Err
		if len(msg.Warnings) > 0 {
			// Keep only the most recent warning for display.
			m.lastWarn = msg.Warnings[len(msg.Warnings)-1]
		}

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd

	case secondTickMsg:
		// Re-render every second so countdown timers stay accurate.
		return m, secondTick()
	}

	return m, nil
}

// View renders the full dashboard.
func (m Model) View() string {
	if m.width == 0 {
		return "Initializing…"
	}

	// Reserve space for title (3 lines) + status bar (1 line) + borders (2).
	colHeight := m.height - 6
	if colHeight < 5 {
		colHeight = 5
	}
	colWidth := (m.width - 8) / 3
	if colWidth < 20 {
		colWidth = 20
	}

	title := titleStyle.Width(m.width).Render(
		fmt.Sprintf(" 🤖  Copilot Orchestrator  ·  %s/%s ", m.owner, m.repo),
	)

	queueCol := m.renderColumn("📋  Queue", headerQueue, badgeQueue,
		m.queue, colWidth, colHeight)
	codingCol := m.renderColumn("⚙️   Active (Coding)", headerCoding, badgeCoding,
		m.coding, colWidth, colHeight)
	reviewCol := m.renderColumn("🔍  In Review (CI/Fix)", headerReview, badgeReview,
		m.review, colWidth, colHeight)

	columns := lipgloss.JoinHorizontal(lipgloss.Top,
		queueCol, "  ", codingCol, "  ", reviewCol)

	statusLine := m.renderStatus()

	return lipgloss.JoinVertical(lipgloss.Left,
		title,
		"",
		columns,
		statusLine,
	)
}

func (m Model) renderColumn(
	header string,
	headerSt lipgloss.Style,
	_ lipgloss.Style,
	states []*poller.State,
	width, height int,
) string {
	var sb strings.Builder
	sb.WriteString(headerSt.Render(header))
	sb.WriteString(fmt.Sprintf("  (%d)\n", len(states)))

	linesUsed := 2
	itemsRendered := 0
	for _, s := range states {
		item, itemLines := m.renderItem(s, width)
		sb.WriteString(item)
		sb.WriteString("\n")
		linesUsed += itemLines
		itemsRendered++
		if linesUsed >= height-2 {
			remaining := len(states) - itemsRendered
			if remaining > 0 {
				sb.WriteString(dimItemStyle.Render(fmt.Sprintf("  … %d more", remaining)))
				sb.WriteString("\n")
			}
			break
		}
	}

	// Pad to fill the column height.
	for linesUsed < height-2 {
		sb.WriteString("\n")
		linesUsed++
	}

	return columnStyle.Width(width).Height(height).Render(sb.String())
}

// renderItem renders a single issue as a title line plus an optional status
// sub-line.  It returns the rendered string and the number of lines it occupies
// (1 if no status is known, 2 when a status sub-line is present).
func (m Model) renderItem(s *poller.State, colWidth int) (string, int) {
	issue := s.Issue
	numStr := fmt.Sprintf("#%d", issue.GetNumber())
	title := issue.GetTitle()

	age := ghclient.TimeAgo(issue.GetCreatedAt().Time)
	agePart := fmt.Sprintf("  [%s]", age)

	prStr := ""
	if s.PR != nil {
		prStr = fmt.Sprintf(" → PR#%d", s.PR.GetNumber())
	}

	// Effective wrap width inside the column: Width(colWidth) with Padding(0,1)
	// causes lipgloss to wrap at colWidth-2 (subtracts left and right padding).
	// Line format: "  <numStr> <title><prStr><agePart>"
	// Fixed overhead: 2 (indent) + visual width of numStr + 1 (space) + visual widths of prStr and agePart.
	// Use lipgloss.Width for visual-width measurement so that multi-byte characters
	// (e.g. the → arrow in prStr) are counted as display columns, not bytes.
	effectiveWidth := colWidth - 2
	fixed := 2 + lipgloss.Width(numStr) + 1 + lipgloss.Width(prStr) + lipgloss.Width(agePart)
	available := effectiveWidth - fixed
	if available < 1 {
		available = 1
	}

	// Truncate using rune iteration for multi-byte character safety.
	// The ellipsis "…" is a single display column (despite being 3 UTF-8 bytes).
	runes := []rune(title)
	if len(runes) > available {
		if available > 1 {
			title = string(runes[:available-1]) + "…"
		} else {
			title = "…"
		}
	}

	num := issueNumStyle.Render(numStr)
	line := fmt.Sprintf("  %s %s", num, itemStyle.Render(title))
	if s.PR != nil {
		line += prNumStyle.Render(prStr)
	}
	line += dimItemStyle.Render(agePart)

	if s.CurrentStatus == "" {
		return line, 1
	}
	return line + "\n" + m.renderStatusSubLine(s, colWidth), 2
}

// renderStatusSubLine builds the dim secondary line shown beneath a title line,
// containing the current phase and (if applicable) the next action with a live
// countdown derived from State.NextActionAt.
func (m Model) renderStatusSubLine(s *poller.State, colWidth int) string {
	current := s.CurrentStatus
	next := s.NextAction

	if !s.NextActionAt.IsZero() {
		until := time.Until(s.NextActionAt)
		if until <= 0 {
			next += " now"
		} else {
			next += " in " + formatCountdown(until)
		}
	}

	var text string
	if next != "" {
		text = fmt.Sprintf("%s  ·  %s", current, next)
	} else {
		text = current
	}

	// Truncate to the column's effective content width minus the 2-space indent.
	// effectiveWidth = colWidth - 2 (Padding(0,1)) - 2 (sub-line indent).
	maxWidth := colWidth - 4
	if maxWidth < 1 {
		maxWidth = 1
	}
	runes := []rune(text)
	if len(runes) > maxWidth {
		if maxWidth > 1 {
			text = string(runes[:maxWidth-1]) + "…"
		} else {
			text = "…"
		}
	}

	return statusLineStyle.Render("  " + text)
}

// formatCountdown formats a duration as a short human-readable string,
// e.g. "9m 42s", "1h 3m", "58s".
func formatCountdown(d time.Duration) string {
	d = d.Round(time.Second)
	h := int(d.Hours())
	mins := int(d.Minutes()) % 60
	secs := int(d.Seconds()) % 60
	if h > 0 {
		return fmt.Sprintf("%dh %dm", h, mins)
	}
	if mins > 0 {
		return fmt.Sprintf("%dm %ds", mins, secs)
	}
	return fmt.Sprintf("%ds", secs)
}

func (m Model) renderStatus() string {
	spin := m.spinner.View()
	var parts []string

	if m.lastRun.IsZero() {
		parts = append(parts, spin+" Waiting for first poll…")
	} else {
		parts = append(parts, spin+fmt.Sprintf(" Last poll: %s", ghclient.TimeAgo(m.lastRun)))
	}

	total := len(m.queue) + len(m.coding) + len(m.review)
	parts = append(parts, fmt.Sprintf("Issues tracked: %d", total))
	parts = append(parts, "Press q / Ctrl-C to quit")

	status := statusBarStyle.Render(strings.Join(parts, "  ·  "))

	if m.lastErr != nil {
		errStr := m.lastErr.Error()
		if len(errStr) > 80 {
			errStr = errStr[:77] + "…"
		}
		status = lipgloss.JoinVertical(lipgloss.Left,
			status,
			errorStyle.Render("⚠  "+errStr),
		)
	} else if m.lastWarn != "" {
		warnStr := m.lastWarn
		if len(warnStr) > 80 {
			warnStr = warnStr[:77] + "…"
		}
		status = lipgloss.JoinVertical(lipgloss.Left,
			status,
			warnStyle.Render("⚠  "+warnStr),
		)
	}
	return status
}
