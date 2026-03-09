package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/BlackbirdWorks/copilot-autocode/ghclient"
	"github.com/BlackbirdWorks/copilot-autocode/poller"
)

// PollEvent wraps a poller.Event for delivery into the Bubble Tea message bus.
type PollEvent struct{ poller.Event }

// LogEvent is emitted by main.go's logWriter to pass standard log messages to the UI.
type LogEvent struct {
	Message string
}

// Layout and animation constants for the dashboard.
const (
	tuiNumCols       = 3  // number of kanban columns
	tuiSpinnerFPS    = 10 // spinner frames per second
	tuiChromeRows    = 6  // rows reserved for title, status bar, and borders
	tuiColMinHeight  = 5  // minimum column height in rows
	tuiColSidePad    = 8  // total horizontal padding (borders + gutters)
	tuiColMinWidth   = 20 // minimum column width in characters
	tuiItemLPad      = 2  // left-indent for item content inside a column
	tuiItemLineCount = 2  // lines used per item when status sub-line is present
	tuiSubLinePad    = 4  // sub-line indent: Padding(0,1)(=2) + 2-space indent
	tuiSubLineMinW   = 5  // minimum useful sub-line content width
	tuiSecsPerMin    = 60 // seconds per minute for % modulo in formatCountdown
	tuiStatusMaxLen  = 80 // max characters shown for error/warning messages
)

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

	logs    []string // simple ring buffer of recent logs
	logHead int      // index for next log insertion

	owner    string
	repo     string
	interval int
}

// New creates a fresh Model.
func New(owner, repo string, interval int) Model {
	sp := spinner.New()
	sp.Spinner = spinner.Spinner{
		Frames: []string{"|", "/", "-", "\\"},
		FPS:    time.Second / tuiSpinnerFPS,
	}
	sp.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("#00ff87"))
	return Model{
		spinner:  sp,
		logs:     make([]string, 50), // keep last 50 logs
		owner:    owner,
		repo:     repo,
		interval: interval,
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

	case LogEvent:
		m.logs[m.logHead] = msg.Message
		m.logHead = (m.logHead + 1) % len(m.logs)
		return m, nil

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
		return "Initializing..."
	}

	// Reserve space for title (3 lines) + status bar (1 line) + borders (2).
	colHeight := max(m.height-tuiChromeRows, tuiColMinHeight)
	colWidth := max((m.width-tuiColSidePad)/tuiNumCols, tuiColMinWidth)

	title := titleStyle.Width(m.width).Render(
		fmt.Sprintf(" [BOT] Copilot Orchestrator - %s/%s ", m.owner, m.repo),
	)

	queueCol := m.renderColumn("LIST Queue", headerQueue, badgeQueue,
		m.queue, colWidth, colHeight)
	codingCol := m.renderColumn("RUN  Active (Coding)", headerCoding, badgeCoding,
		m.coding, colWidth, colHeight)
	reviewCol := m.renderColumn("TEST In Review (CI/Fix)", headerReview, badgeReview,
		m.review, colWidth, colHeight)

	columns := lipgloss.JoinHorizontal(lipgloss.Top,
		queueCol, "  ", codingCol, "  ", reviewCol)

	logBoxHeight := 5 // fixed 5 rows for logs
	logBoxWidth := m.width - 2
	logContent := m.renderLogs(logBoxHeight)
	logBox := logBoxStyle.Width(logBoxWidth).Height(logBoxHeight).Render(logContent)

	statusLine := m.renderStatus()

	return lipgloss.JoinVertical(lipgloss.Left,
		title,
		"",
		columns,
		"",
		logBox,
		"",
		statusLine,
	)
}

func (m Model) renderLogs(height int) string {
	// Read out logs from the ring buffer in chronological order
	var ordered []string
	size := len(m.logs)
	for i := 0; i < size; i++ {
		idx := (m.logHead + i) % size
		if m.logs[idx] != "" {
			ordered = append(ordered, m.logs[idx])
		}
	}

	// Keep only the last 'height' logs
	if len(ordered) > height {
		ordered = ordered[len(ordered)-height:]
	}

	var sb strings.Builder
	for i, l := range ordered {
		sb.WriteString(logLineStyle.Render(l))
		if i < len(ordered)-1 {
			sb.WriteString("\n")
		}
	}

	// Pad with newlines if fewer logs than height
	for i := len(ordered); i < height; i++ {
		sb.WriteString("\n")
	}
	return sb.String()
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
	fmt.Fprintf(&sb, "  (%d)\n", len(states))

	linesUsed := 2
	itemsRendered := 0
	for _, s := range states {
		item, itemLines := m.RenderItem(s, width)
		sb.WriteString(item)
		sb.WriteString("\n")
		linesUsed += itemLines
		itemsRendered++
		if linesUsed >= height-2 {
			remaining := len(states) - itemsRendered
			if remaining > 0 {
				sb.WriteString(dimItemStyle.Render(fmt.Sprintf("  ... %d more", remaining)))
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

// RenderItem renders a single issue as a title line plus an optional status
// sub-line.  It returns the rendered string and the number of lines it occupies
// (1 if no status is known, 2 when a status sub-line is present).
func (m Model) RenderItem(s *poller.State, colWidth int) (string, int) {
	issue := s.Issue
	numStr := fmt.Sprintf("#%d", issue.GetNumber())
	title := issue.GetTitle()

	age := ghclient.TimeAgo(issue.GetCreatedAt().Time)
	agePart := fmt.Sprintf("  [%s]", age)

	prStr := ""
	if s.PR != nil {
		prStr = fmt.Sprintf(" -> PR#%d", s.PR.GetNumber())
	}

	// Line format: "  <numStr> <title><prStr><agePart>"
	// Use exactly colWidth-tuiItemLPad for the total content to avoid wrapping.
	effectiveWidth := colWidth - tuiItemLPad
	overhead := tuiItemLPad + lipgloss.Width(numStr) + 1 + lipgloss.Width(prStr) + lipgloss.Width(agePart)
	available := max(effectiveWidth-overhead, 1)

	// Truncate title using runes.
	runes := []rune(title)
	if len(runes) > available {
		if available > 1 {
			title = string(runes[:available-1]) + "…"
		} else {
			title = "…"
		}
	}

	issueHTML := issue.GetHTMLURL()
	renderedTitle := itemStyle.Render(title)

	// Make Issue Number and Title clickable links (OSC-8).
	if issueHTML != "" {
		numStr = issueNumStyle.Render(numStr)
		numStr = fmt.Sprintf("\x1b]8;;%s\x1b\\%s\x1b]8;;\x1b\\", issueHTML, numStr)
		renderedTitle = fmt.Sprintf("\x1b]8;;%s\x1b\\%s\x1b]8;;\x1b\\", issueHTML, renderedTitle)
	} else {
		numStr = issueNumStyle.Render(numStr)
	}

	if s.PR != nil {
		prNum := fmt.Sprintf("PR#%d", s.PR.GetNumber())
		renderedPR := prNumStyle.Render(prNum)
		if prHTML := s.PR.GetHTMLURL(); prHTML != "" {
			renderedPR = fmt.Sprintf("\x1b]8;;%s\x1b\\%s\x1b]8;;\x1b\\", prHTML, renderedPR)
		}
		prStr = " -> " + renderedPR
	}

	line := fmt.Sprintf("  %s %s", numStr, renderedTitle)
	line += prStr
	line += dimItemStyle.Render(agePart)

	if s.CurrentStatus == "" {
		return line, 1
	}
	// Cap the sub-line height and avoid wrapping too.
	return line + "\n" + m.RenderStatusSubLine(s, colWidth), tuiItemLineCount
}

// RenderStatusSubLine builds the dim secondary line shown beneath a title line,
// containing the current phase and (if applicable) the next action with a live
// countdown derived from State.NextActionAt.
func (m Model) RenderStatusSubLine(s *poller.State, colWidth int) string {
	current := s.CurrentStatus
	next := s.NextAction

	if !s.NextActionAt.IsZero() {
		until := time.Until(s.NextActionAt)
		if until <= 0 {
			next += " now"
		} else {
			next += " in " + FormatCountdown(until)
		}
	}

	var text string
	agentIcon := ""
	switch s.AgentStatus {
	case "pending":
		agentIcon = " " + m.spinner.View()
	case "success":
		agentIcon = " " + lipgloss.NewStyle().Foreground(lipgloss.Color("10")).Render("[OK]")
	case "failed":
		agentIcon = " " + lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Render("[X]")
	}

	parts := []string{}
	// Prefix with refinement if applicable.
	if s.RefinementMax > 0 {
		parts = append(parts, fmt.Sprintf("refinement[%d/%d]", s.RefinementCount, s.RefinementMax))
	}

	// Add current phase status (CI failing, Waiting for CI, etc).
	if current != "" {
		parts = append(parts, current)
	}

	// Combine all parts with copilot icon.
	base := strings.Join(parts, " - ")
	if agentIcon != "" {
		text = fmt.Sprintf("%s - copilot %s", base, agentIcon)
	} else {
		text = base
	}

	// Append next action if applicable.
	if next != "" {
		text += fmt.Sprintf("  ·  %s", next)
	}

	// Truncate to the column's effective content width minus the 2-space indent.
	// effectiveWidth = colWidth - 2 (Padding(0,1)) - 2 (sub-line indent).
	maxWidth := max(colWidth-tuiSubLinePad, tuiSubLineMinW)

	// If too long, try shortening "refinement" to "ref"
	if lipgloss.Width(text) > maxWidth {
		text = strings.Replace(text, "refinement[", "ref[", 1)
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

// FormatCountdown formats a duration as a short human-readable string,
// e.g. "9m 42s", "1h 3m", "58s".
func FormatCountdown(d time.Duration) string {
	d = d.Round(time.Second)
	h := int(d.Hours())
	mins := int(d.Minutes()) % tuiSecsPerMin
	secs := int(d.Seconds()) % tuiSecsPerMin
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
		parts = append(parts, spin+" Waiting for first poll...")
	} else {
		nextPollRun := m.lastRun.Add(time.Duration(m.interval) * time.Second)
		until := time.Until(nextPollRun)
		if until <= 0 {
			parts = append(parts, spin+" Polling now...")
		} else {
			parts = append(parts, spin+fmt.Sprintf(" Next poll in %s", FormatCountdown(until)))
		}
	}

	total := len(m.queue) + len(m.coding) + len(m.review)
	parts = append(parts, fmt.Sprintf("Issues tracked: %d", total))
	parts = append(parts, "Press q / Ctrl-C to quit")

	status := statusBarStyle.Render(strings.Join(parts, "  -  "))

	if m.lastErr != nil {
		errStr := m.lastErr.Error()
		if len(errStr) > tuiStatusMaxLen {
			errStr = errStr[:tuiStatusMaxLen-3] + "..."
		}
		status = lipgloss.JoinVertical(lipgloss.Left,
			status,
			errorStyle.Render("!  "+errStr),
		)
	} else if m.lastWarn != "" {
		warnStr := m.lastWarn
		if len(warnStr) > tuiStatusMaxLen {
			warnStr = warnStr[:tuiStatusMaxLen-3] + "..."
		}
		status = lipgloss.JoinVertical(lipgloss.Left,
			status,
			warnStyle.Render("!  "+warnStr),
		)
	}
	return status
}
