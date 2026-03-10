package tui

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/atotto/clipboard"
	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"

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
	tuiChromeRows    = 9  // rows reserved for title, status bar, borders, margins, and copy button
	tuiLogBoxHeight  = 5  // fixed rows for logs
	logHistorySize   = 50 // simple ring buffer of recent logs
	tuiColMinHeight  = 5  // minimum column height in rows
	tuiColSidePad    = 6  // overhead: 3 cols × 2 border chars (no spacers)
	tuiColMinWidth   = 20 // minimum column width in characters
	tuiItemLPad      = 2  // left-indent for item content inside a column
	tuiItemLineCount = 2  // lines used per item when status sub-line is present
	tuiSubLinePad    = 4  // sub-line indent: Padding(0,1)(=2) + 2-space indent
	tuiSubLineMinW   = 5  // minimum useful sub-line content width
	tuiSecsPerMin    = 60 // seconds per minute for % modulo in formatCountdown
	tuiStatusMaxLen  = 80 // max characters shown for error/warning messages

	tuiDoublePadding = 2 // multiplier for symmetric horizontal padding
	tuiColorSuccess  = "10"
	tuiColorFailure  = "9"
	tuiColorSpinner  = "#00ff87"

	tuiCopyFeedbackDuration = time.Second * 2

	// Layout padding and border constants.
	tuiBorderRows       = 2  // rows for top/bottom borders
	tuiLogBoxPadding    = 2  // horizontal padding for log box
	tuiCopyWidgetMargin = 4  // horizontal margin for copy button
	tuiMinInnerWidth    = 10 // minimum width for truncated content

	// Selection indicator prefix (plain char, styled at render time).
	tuiSelectPrefix    = "▸ "
	tuiNonSelectPrefix = "  "
)

// secondTickMsg is fired every second so live countdown timers in item
// status sub-lines stay up-to-date between poll events.
type secondTickMsg time.Time

func secondTick() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg {
		return secondTickMsg(t)
	})
}

// clearCopyMsg is fired after logs are copied to reset the "[Copied!]" widget.
type clearCopyMsg struct{}

// clearActionMsg is fired after a brief delay to clear the action feedback.
type clearActionMsg struct{}

// mergeLogReloadMsg is fired every 500 ms to re-read the merge log file.
type mergeLogReloadMsg struct{}

func mergeLogReloadTick() tea.Cmd {
	return tea.Tick(500*time.Millisecond, func(_ time.Time) tea.Msg {
		return mergeLogReloadMsg{}
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

	logs    []string // simple ring buffer of recent logs: size logHistorySize
	logHead int      // index for next log insertion

	logsCopied bool // true if logs were recently copied to clipboard

	// Cursor selection state.
	selectedCol int // 0=queue, 1=coding, 2=review
	selectedRow int // 0-indexed within the selected column

	// Command channel for sending actions back to the poller.
	commandCh chan<- poller.Command

	// Action feedback shown briefly in the status bar.
	actionFeedback string

	// Log viewer overlay state.
	logViewerOpen    bool
	logViewerLines   []string // lines loaded from the log file
	logViewerScroll  int      // scroll offset (0 = bottom/most recent)
	logViewerTitle   string   // title shown in the viewer header
	logFilePath      string   // path to the main poller log file
	mergeLogTailing  bool     // true when the merge log viewer is live-tailing
	mergeLogFilePath string   // path being tailed (so reload can re-read it)

	owner    string
	repo     string
	interval int
}

// New creates a fresh Model.  commandCh is used to send actions back to the
// poller (e.g. retry merge resolution).  It may be nil if no commands are
// needed.
func New(owner, repo string, interval int, commandCh chan<- poller.Command) Model {
	sp := spinner.New()
	sp.Spinner = spinner.Spinner{
		Frames: []string{"-", "\\", "|", "/"},
		FPS:    time.Second / tuiSpinnerFPS,
	}
	sp.Style = lipgloss.NewStyle().Foreground(lipgloss.Color(tuiColorSpinner))
	return Model{
		spinner:     sp,
		logs:        make([]string, logHistorySize), // keep last N logs
		logFilePath: "copilot-autocode.log",
		owner:       owner,
		repo:        repo,
		interval:    interval,
		commandCh:   commandCh,
	}
}

// Init starts the spinner and the per-second countdown tick.
func (m Model) Init() tea.Cmd {
	return tea.Batch(m.spinner.Tick, secondTick())
}

// columnItems returns the items slice for the given column index.
func (m Model) columnItems(col int) []*poller.State {
	switch col {
	case 0:
		return m.queue
	case 1:
		return m.coding
	case 2:
		return m.review
	default:
		return nil
	}
}

// clampSelection ensures selectedRow is within bounds for the current column.
func (m *Model) clampSelection() {
	items := m.columnItems(m.selectedCol)
	if len(items) == 0 {
		m.selectedRow = 0
		return
	}
	if m.selectedRow >= len(items) {
		m.selectedRow = len(items) - 1
	}
	if m.selectedRow < 0 {
		m.selectedRow = 0
	}
}

// selectedState returns the currently selected State, or nil if the column is empty.
func (m Model) selectedState() *poller.State {
	items := m.columnItems(m.selectedCol)
	if len(items) == 0 || m.selectedRow >= len(items) {
		return nil
	}
	return items[m.selectedRow]
}

// needsManualMergeFix returns true if the item's current status indicates a
// failed local merge resolution that can be retried.
const mergeFixStatus = "Merge conflicts unresolved \u2014 needs manual fix"

// Update handles all messages.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// When log viewer is open, it consumes all key input.
	if m.logViewerOpen {
		if keyMsg, ok := msg.(tea.KeyMsg); ok {
			return m.updateLogViewer(keyMsg)
		}
		// Still handle window resize and spinner ticks.
		switch msg := msg.(type) {
		case tea.WindowSizeMsg:
			m.width = msg.Width
			m.height = msg.Height
			return m, nil
		case spinner.TickMsg:
			var cmd tea.Cmd
			m.spinner, cmd = m.spinner.Update(msg)
			return m, cmd
		case LogEvent:
			m.logs[m.logHead] = msg.Message
			m.logHead = (m.logHead + 1) % len(m.logs)
			return m, nil
		}
		return m, nil
	}

	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q":
			return m, tea.Quit

		case "c":
			var sb strings.Builder
			size := len(m.logs)
			for i := range size {
				idx := (m.logHead + i) % size
				if m.logs[idx] != "" {
					sb.WriteString(m.logs[idx])
					sb.WriteString("\n")
				}
			}
			_ = clipboard.WriteAll(sb.String())
			m.logsCopied = true
			return m, tea.Tick(tuiCopyFeedbackDuration, func(_ time.Time) tea.Msg { return clearCopyMsg{} })

		// Cursor navigation.
		case "up", "k":
			if m.selectedRow > 0 {
				m.selectedRow--
			}
			return m, nil
		case "down", "j":
			items := m.columnItems(m.selectedCol)
			if m.selectedRow < len(items)-1 {
				m.selectedRow++
			}
			return m, nil
		case "left", "h":
			if m.selectedCol > 0 {
				m.selectedCol--
				m.clampSelection()
			}
			return m, nil
		case "right", "l":
			if m.selectedCol < tuiNumCols-1 {
				m.selectedCol++
				m.clampSelection()
			}
			return m, nil

		// Actions.
		case "r":
			return m, m.handleRetryMerge()

		// Log viewers.
		case "L":
			m.mergeLogFilePath = m.logFilePath
			m.mergeLogTailing = true
			m.openLogFile(m.logFilePath)
			return m, mergeLogReloadTick()
		case "v":
			return m.openMergeLogCmd()
		}

	case clearCopyMsg:
		m.logsCopied = false
		return m, nil

	case clearActionMsg:
		m.actionFeedback = ""
		return m, nil

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
		m.clampSelection()

	case LogEvent:
		m.logs[m.logHead] = msg.Message
		m.logHead = (m.logHead + 1) % len(m.logs)
		return m, nil

	case mergeLogReloadMsg:
		if m.logViewerOpen && m.mergeLogTailing {
			// Re-read and only auto-scroll if the user is already at the bottom.
			atBottom := m.logViewerScroll == 0
			m.reloadMergeLog()
			if atBottom {
				m.logViewerScroll = 0
			}
			return m, mergeLogReloadTick()
		}
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

// handleRetryMerge sends a retry-merge command for the selected item if applicable.
func (m *Model) handleRetryMerge() tea.Cmd {
	s := m.selectedState()
	if s == nil || s.CurrentStatus != mergeFixStatus || s.PR == nil {
		return nil
	}
	if m.commandCh == nil {
		return nil
	}

	m.commandCh <- poller.Command{
		Action: "retry-merge",
		PRNum:  s.PR.GetNumber(),
	}
	m.actionFeedback = fmt.Sprintf("Retry merge resolution queued for PR#%d", s.PR.GetNumber())
	return tea.Tick(tuiCopyFeedbackDuration, func(_ time.Time) tea.Msg { return clearActionMsg{} })
}

// View renders the full dashboard.
func (m Model) View() string {
	if m.width == 0 {
		return "Initializing..."
	}

	if m.logViewerOpen {
		return m.renderLogViewer()
	}

	// Roughly tuiChromeRows + (tuiLogBoxHeight + tuiBorderRows) total reserved lines.
	reservedRows := tuiChromeRows + tuiLogBoxHeight + tuiBorderRows
	colHeight := max(m.height-reservedRows, tuiColMinHeight)

	// Ensure colWidth takes internal padding/borders into account.
	// Each col's outer rendered width = colWidth + 2 (left+right borders).
	// 3 cols with no spacers → total = 3*(colWidth+2) = 3*colWidth+6.
	colWidth := max((m.width-tuiColSidePad)/tuiNumCols, tuiColMinWidth)
	// Give leftover chars to the last column so columns fill the full terminal.
	lastColWidth := max(m.width-tuiColSidePad-colWidth*(tuiNumCols-1), colWidth)

	title := titleStyle.Width(m.width).Render(
		fmt.Sprintf(" [BOT] Copilot Orchestrator - %s/%s ", m.owner, m.repo),
	)

	queueCol := m.renderColumn("LIST Queue", headerQueue, badgeQueue,
		m.queue, colWidth, colHeight, 0)
	codingCol := m.renderColumn("RUN  Active (Coding)", headerCoding, badgeCoding,
		m.coding, colWidth, colHeight, 1)
	reviewCol := m.renderColumn("TEST In Review (CI/Fix)", headerReview, badgeReview,
		m.review, lastColWidth, colHeight, 2)

	columns := lipgloss.JoinHorizontal(lipgloss.Top, queueCol, codingCol, reviewCol)

	logBoxWidth := m.width - tuiLogBoxPadding // horizontal border padding
	logContent := m.renderLogs(tuiLogBoxHeight)
	logBox := logBoxStyle.Width(logBoxWidth).Height(tuiLogBoxHeight).Render(logContent)

	copyText := dimItemStyle.Render("[c] copy  [r] retry merge  [v] merge log  [L] all logs  [arrows/hjkl] navigate")
	if m.logsCopied {
		copyText = lipgloss.NewStyle().Foreground(lipgloss.Color(tuiColorSuccess)).Render("[Copied!]")
	} else if m.actionFeedback != "" {
		copyText = lipgloss.NewStyle().Foreground(lipgloss.Color(tuiColorSuccess)).Render(m.actionFeedback)
	}
	copyWidget := lipgloss.NewStyle().Width(m.width - tuiCopyWidgetMargin).Align(lipgloss.Right).Render(copyText)

	statusLine := m.renderStatus()
	if m.lastErr != nil {
		statusLine = errorStyle.Render(statusLine)
	} else if m.lastWarn != "" {
		statusLine = warnStyle.Render(statusLine)
	}

	return lipgloss.JoinVertical(lipgloss.Left,
		title,
		"",
		columns,
		"",
		copyWidget,
		logBox,
		"",
		statusLine,
	)
}

func colorLogLine(line string) string {
	styled := line
	switch {
	case strings.Contains(line, "INFO"):
		styled = strings.Replace(line, "INFO", logInfoStyle.Render("INFO"), 1)
	case strings.Contains(line, "WARN"):
		styled = strings.Replace(line, "WARN", logWarnStyle.Render("WARN"), 1)
	case strings.Contains(line, "ERROR"):
		styled = strings.Replace(line, "ERROR", logErrorStyle.Render("ERROR"), 1)
	case strings.Contains(line, "DEBUG"):
		styled = strings.Replace(line, "DEBUG", logDebugStyle.Render("DEBUG"), 1)
	}
	return logLineStyle.Render(styled)
}

func (m Model) renderLogs(height int) string {
	// Calculate an effective inner width to truncate logs
	// Box padding is left/right 1 + border left/right 1 = 4 total subtracted
	innerW := m.width - (tuiLogBoxPadding * tuiDoublePadding) // padding + borders
	innerW = max(innerW, tuiMinInnerWidth)

	// Read out logs from the ring buffer in chronological order
	var ordered []string
	size := len(m.logs)
	for i := range size {
		idx := (m.logHead + i) % size
		if m.logs[idx] != "" {
			// truncate log to prevent terminal wrapping from ruining box
			line := m.logs[idx]
			if lipgloss.Width(line) > innerW {
				// use runes for safe truncation
				runes := []rune(line)
				if len(runes) > innerW {
					line = string(runes[:innerW-1]) + "…"
				}
			}
			ordered = append(ordered, line)
		}
	}

	// Keep only the last 'height' logs
	if len(ordered) > height {
		ordered = ordered[len(ordered)-height:]
	}

	var sb strings.Builder
	for i, l := range ordered {
		sb.WriteString(colorLogLine(l))
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
	colIndex int,
) string {
	var sb strings.Builder
	sb.WriteString(headerSt.Render(header))
	fmt.Fprintf(&sb, "  (%d)\n", len(states))

	linesUsed := 2
	itemsRendered := 0
	for i, s := range states {
		selected := colIndex == m.selectedCol && i == m.selectedRow
		item, itemLines := m.renderItem(s, width, selected)
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

	cStyle := columnStyle
	if colIndex == m.selectedCol {
		cStyle = selectedColumnStyle
	}
	return cStyle.Width(width).Height(height).Render(sb.String())
}

// renderItem renders a single issue as a title line plus an optional status
// sub-line.  It returns the rendered string and the number of lines it occupies
// (1 if no status is known, 2 when a status sub-line is present).
func (m Model) renderItem(s *poller.State, colWidth int, selected bool) (string, int) {
	issue := s.Issue
	numStr := fmt.Sprintf("#%d", issue.GetNumber())
	title := issue.GetTitle()

	age := ghclient.TimeAgo(issue.GetCreatedAt().Time)
	agePart := ""
	if s.CurrentStatus == "" {
		agePart = fmt.Sprintf("  [%s]", age)
	}

	prStr := ""
	if s.PR != nil {
		prStr = fmt.Sprintf(" -> PR#%d", s.PR.GetNumber())
	}

	// Determine the line prefix based on selection state.
	prefix := tuiNonSelectPrefix
	if selected {
		prefix = selectedIndicator.Render(tuiSelectPrefix)
	}

	// Line format: "<prefix><numStr> <title><prStr><agePart><spinner>"
	// colWidth is passed to lipgloss .Width() which includes padding (0,1).
	// Item content fits in colWidth - 2 (1 padding each side), minus 1 safety buffer.
	effectiveWidth := colWidth - 3
	overhead := tuiItemLPad + lipgloss.Width(numStr) + 1 + lipgloss.Width(prStr) + lipgloss.Width(agePart)

	spinnerStr := ""
	if s.AgentStatus == "pending" {
		spinnerStr = "  " + m.spinner.View()
		overhead += lipgloss.Width(spinnerStr)
	}

	available := max(effectiveWidth-overhead, 1)

	// Truncate title using physical display width.
	if runewidth.StringWidth(title) > available {
		if available > 1 {
			title = runewidth.Truncate(title, available, "…")
		} else {
			title = "…"
		}
	}

	issueHTML := issue.GetHTMLURL()

	// Colorize text in backticks
	parts := strings.Split(title, "`")
	var styledTitleBuilder strings.Builder
	for i, part := range parts {
		if i%2 == 1 && i < len(parts)-1 {
			styledTitleBuilder.WriteString(codeSpanStyle.Render(part))
		} else {
			styledTitleBuilder.WriteString(itemStyle.Render(part))
		}
	}
	renderedTitle := styledTitleBuilder.String()

	// Make Issue Number and Title clickable links (OSC-8) and visually underline them.
	if issueHTML != "" {
		numStr = issueNumStyle.Underline(true).Render(numStr)
		numStr = fmt.Sprintf("\x1b]8;;%s\x1b\\%s\x1b]8;;\x1b\\", issueHTML, numStr)

		var underlineBuilder strings.Builder
		for i, part := range parts {
			if i%2 == 1 && i < len(parts)-1 {
				underlineBuilder.WriteString(codeSpanStyle.Underline(true).Render(part))
			} else {
				underlineBuilder.WriteString(itemStyle.Underline(true).Render(part))
			}
		}
		renderedTitle = underlineBuilder.String()
		renderedTitle = fmt.Sprintf("\x1b]8;;%s\x1b\\%s\x1b]8;;\x1b\\", issueHTML, renderedTitle)
	} else {
		numStr = issueNumStyle.Render(numStr)
	}

	if s.PR != nil {
		prNum := fmt.Sprintf("PR#%d", s.PR.GetNumber())
		renderedPR := prNumStyle.Render(prNum)
		if prHTML := s.PR.GetHTMLURL(); prHTML != "" {
			renderedPR = prNumStyle.Underline(true).Render(prNum)
			renderedPR = fmt.Sprintf("\x1b]8;;%s\x1b\\%s\x1b]8;;\x1b\\", prHTML, renderedPR)
		}
		prStr = " -> " + renderedPR
	}

	line := fmt.Sprintf("%s%s %s", prefix, numStr, renderedTitle)
	line += prStr
	line += dimItemStyle.Render(agePart)
	line += spinnerStr

	if s.CurrentStatus == "" || !selected {
		return line, lipgloss.Height(line)
	}

	// Calculate and render the full expanded sub-line for selected items without truncating it.
	subLineText := m.RenderStatusSubLine(s, colWidth)
	fullText := line + "\n" + subLineText
	return fullText, lipgloss.Height(fullText)
}

// RenderItem is the exported version for tests (delegates to renderItem without selection).
func (m Model) RenderItem(s *poller.State, colWidth int) (string, int) {
	return m.renderItem(s, colWidth, false)
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
	case "success":
		agentIcon = " " + lipgloss.NewStyle().Foreground(lipgloss.Color(tuiColorSuccess)).Render("[OK]")
	case "failed":
		agentIcon = " " + lipgloss.NewStyle().Foreground(lipgloss.Color(tuiColorFailure)).Render("[X]")
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

	if agentIcon != "" {
		parts = append(parts, "copilot"+agentIcon)
	}

	// Append next action if applicable.
	if next != "" {
		parts = append(parts, fmt.Sprintf("Next: %s", next))
	}

	text = strings.Join(parts, "\n")

	// Allow the sub-line to wrap freely. Apply left padding of 7 to align neatly under title.
	// We use the full available column width and do not truncate.
	contentStyle := statusLineStyle.Width(colWidth - 7).PaddingLeft(7)
	return contentStyle.Render(text)
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

// ── Log viewer overlay ──────────────────────────────────────────────────

const logViewerMaxLines = 500 // max lines to load from the log file

// openLogFile reads the given file and opens the fullscreen viewer.
func (m *Model) openLogFile(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		m.actionFeedback = fmt.Sprintf("Cannot open log: %v", err)
		return
	}
	lines := strings.Split(string(data), "\n")
	// Keep only the last logViewerMaxLines lines.
	if len(lines) > logViewerMaxLines {
		lines = lines[len(lines)-logViewerMaxLines:]
	}
	m.logViewerLines = lines
	m.logViewerScroll = 0 // 0 = viewing the bottom (most recent)
	m.logViewerOpen = true
	m.logViewerTitle = path
}

// openMergeLog opens the merge resolution log for the currently selected item (static, no tail).
func (m *Model) openMergeLog() {
	s := m.selectedState()
	if s == nil || s.MergeLogPath == "" {
		m.actionFeedback = "No merge log available for selected item"
		return
	}
	m.openLogFile(s.MergeLogPath)
}

// openMergeLogCmd starts the live-tail merge log viewer for the selected item.
func (m *Model) openMergeLogCmd() (tea.Model, tea.Cmd) {
	s := m.selectedState()
	if s == nil || s.MergeLogPath == "" {
		m.actionFeedback = "No merge log available for selected item"
		return m, nil
	}
	m.mergeLogFilePath = s.MergeLogPath
	m.mergeLogTailing = true
	m.openLogFile(s.MergeLogPath)
	return m, mergeLogReloadTick()
}

// reloadMergeLog re-reads the merge log file in place (called by the tail ticker).
func (m *Model) reloadMergeLog() {
	if m.mergeLogFilePath == "" {
		return
	}
	data, err := os.ReadFile(m.mergeLogFilePath)
	if err != nil {
		return // file may not exist yet — silently skip
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) > logViewerMaxLines {
		lines = lines[len(lines)-logViewerMaxLines:]
	}
	m.logViewerLines = lines
}

// updateLogViewer handles key input while the log viewer overlay is open.
func (m Model) updateLogViewer(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	viewHeight := m.logViewerHeight()
	maxScroll := max(len(m.logViewerLines)-viewHeight, 0)

	switch msg.String() {
	case "L", "v", "esc", "q":
		m.logViewerOpen = false
		m.logViewerLines = nil
		m.mergeLogTailing = false
		m.mergeLogFilePath = ""
		return m, nil
	case "up", "k":
		if m.logViewerScroll < maxScroll {
			m.logViewerScroll++
		}
		return m, nil
	case "down", "j":
		if m.logViewerScroll > 0 {
			m.logViewerScroll--
		}
		return m, nil
	case "pgup", "ctrl+u":
		m.logViewerScroll = min(m.logViewerScroll+viewHeight/2, maxScroll)
		return m, nil
	case "pgdown", "ctrl+d":
		m.logViewerScroll = max(m.logViewerScroll-viewHeight/2, 0)
		return m, nil
	case "c":
		var sb strings.Builder
		for i, line := range m.logViewerLines {
			sb.WriteString(line)
			if i < len(m.logViewerLines)-1 {
				sb.WriteString("\n")
			}
		}
		_ = clipboard.WriteAll(sb.String())
		m.logsCopied = true
		return m, tea.Tick(tuiCopyFeedbackDuration, func(_ time.Time) tea.Msg { return clearCopyMsg{} })
	case "home", "g":
		m.logViewerScroll = maxScroll
		return m, nil
	case "end", "G":
		m.logViewerScroll = 0
		return m, nil
	}
	return m, nil
}

// logViewerHeight returns the number of visible log lines (screen minus chrome).
func (m Model) logViewerHeight() int {
	// 3 rows: title bar, blank, status bar
	return max(m.height-3, 1)
}

// renderLogViewer draws the fullscreen log viewer overlay.
func (m Model) renderLogViewer() string {
	viewH := m.logViewerHeight()
	total := len(m.logViewerLines)

	// Calculate the window: scroll=0 means bottom, scroll=N means N lines up.
	endIdx := total - m.logViewerScroll
	startIdx := max(endIdx-viewH, 0)
	if endIdx < 0 {
		endIdx = 0
	}

	visible := m.logViewerLines[startIdx:endIdx]

	var sb strings.Builder

	// Display scroll position as [ %d/%d ]
	scrollPos := ""
	if total > viewH {
		scrollPos = fmt.Sprintf("  [ %d/%d ]", startIdx+1, total)
	}

	headerText := fmt.Sprintf(" LOG VIEWER%s — %s%s  (scroll: up/down/pgup/pgdn  home/end: g/G  copy: c  close: L/v/esc/q) ",
		map[bool]string{true: " [LIVE]", false: ""}[m.mergeLogTailing],
		m.logViewerTitle,
		scrollPos,
	)
	if m.logsCopied {
		headerText = fmt.Sprintf(" LOG VIEWER%s — %s%s  [Copied!] ",
			map[bool]string{true: " [LIVE]", false: ""}[m.mergeLogTailing],
			m.logViewerTitle,
			scrollPos,
		)
	}
	sb.WriteString(titleStyle.Width(m.width).Render(headerText))
	sb.WriteString("\n")

	innerW := max(m.width-2, tuiMinInnerWidth)
	for _, line := range visible {
		// Truncate long lines to avoid terminal wrapping.
		runes := []rune(line)
		if len(runes) > innerW {
			line = string(runes[:innerW-1]) + "…"
		}
		sb.WriteString(colorLogLine(line))
		sb.WriteString("\n")
	}
	// Pad remaining lines.
	for i := len(visible); i < viewH; i++ {
		sb.WriteString("\n")
	}

	posInfo := fmt.Sprintf("Line %d-%d of %d", startIdx+1, endIdx, total)
	sb.WriteString(statusBarStyle.Render(posInfo))

	return sb.String()
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
