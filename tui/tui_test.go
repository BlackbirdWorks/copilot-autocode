package tui

import (
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/BlackbirdWorks/copilot-autocode/poller"
	"github.com/google/go-github/v68/github"
)

// ansiRe matches ANSI CSI escape sequences so tests can compare plain text.
var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func stripAnsi(s string) string {
	return ansiRe.ReplaceAllString(s, "")
}

// issueAt returns a minimal *github.Issue with the given number, title, and
// creation time — enough for renderItem to work without nil dereferences.
func issueAt(num int, title string, createdAt time.Time) *github.Issue {
	ts := github.Timestamp{Time: createdAt}
	return &github.Issue{
		Number:    &num,
		Title:     &title,
		CreatedAt: &ts,
	}
}

// ─── formatCountdown ────────────────────────────────────────────────────────

func TestFormatCountdown(t *testing.T) {
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
			got := formatCountdown(tc.d)
			if got != tc.want {
				t.Errorf("formatCountdown(%v) = %q; want %q", tc.d, got, tc.want)
			}
		})
	}
}

// ─── renderStatusSubLine ────────────────────────────────────────────────────

func TestRenderStatusSubLine(t *testing.T) {
	m := Model{}
	now := time.Now()

	tests := []struct {
		name         string
		current      string
		next         string
		nextActionAt time.Time
		colWidth     int
		wantContains []string // all must appear in the stripped output
		wantAbsent   []string // none must appear
	}{
		{
			name:         "current only — no separator",
			current:      "CI failing",
			colWidth:     60,
			wantContains: []string{"CI failing"},
			wantAbsent:   []string{"·"},
		},
		{
			name:         "current and next — separator present",
			current:      "CI failing",
			next:         "Asked Copilot to fix",
			colWidth:     60,
			wantContains: []string{"CI failing", "·", "Asked Copilot to fix"},
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
				"m", // at least minutes in the countdown
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
			name:     "long text truncated on narrow column",
			current:  "Waiting on coding agent to start",
			next:     "Poke Copilot",
			colWidth: 20,
			// maxWidth = 20 - 4 = 16; full text is ~50 chars → truncated
			wantContains: []string{"…"},
		},
		{
			name:     "very narrow column produces single ellipsis",
			current:  "CI failing",
			next:     "fix",
			colWidth: 5,
			// maxWidth = 5 - 4 = 1; only "…" fits
			wantContains: []string{"…"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := &poller.State{
				CurrentStatus: tc.current,
				NextAction:    tc.next,
				NextActionAt:  tc.nextActionAt,
			}
			got := stripAnsi(m.renderStatusSubLine(s, tc.colWidth))

			for _, want := range tc.wantContains {
				if !strings.Contains(got, want) {
					t.Errorf("renderStatusSubLine() = %q; want it to contain %q", got, want)
				}
			}
			for _, absent := range tc.wantAbsent {
				if strings.Contains(got, absent) {
					t.Errorf("renderStatusSubLine() = %q; want it NOT to contain %q", got, absent)
				}
			}
		})
	}
}

// ─── renderItem line count ───────────────────────────────────────────────────

func TestRenderItemLineCount(t *testing.T) {
	m := Model{}
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
			name:      "with status → 2 lines",
			state:     &poller.State{Issue: issue, CurrentStatus: "Waiting for CI"},
			colWidth:  60,
			wantLines: 2,
		},
		{
			name:      "status + next action → 2 lines",
			state:     &poller.State{Issue: issue, CurrentStatus: "CI failing", NextAction: "Asked Copilot to fix"},
			colWidth:  60,
			wantLines: 2,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rendered, lines := m.renderItem(tc.state, tc.colWidth)
			if lines != tc.wantLines {
				t.Errorf("renderItem() lines = %d; want %d", lines, tc.wantLines)
			}
			// Verify that the embedded newline count matches the reported line count.
			actualNewlines := strings.Count(rendered, "\n")
			if actualNewlines != tc.wantLines-1 {
				t.Errorf("renderItem() newline count = %d; want %d (rendered: %q)",
					actualNewlines, tc.wantLines-1, rendered)
			}
		})
	}
}

// ─── renderItem title truncation ─────────────────────────────────────────────

func TestRenderItemTitleTruncation(t *testing.T) {
	m := Model{}
	num := 1
	now := time.Now() // "just now" age → predictable fixed-part width

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
			title:        "Fix", // very short, always fits
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
			issue := issueAt(num, tc.title, now)
			s := &poller.State{Issue: issue}
			rendered, _ := m.renderItem(s, tc.colWidth)
			plain := stripAnsi(rendered)

			hasEllipsis := strings.Contains(plain, "…")
			if hasEllipsis != tc.wantEllipsis {
				t.Errorf("renderItem() ellipsis=%v; want %v (plain: %q)",
					hasEllipsis, tc.wantEllipsis, plain)
			}

			// Output must always be valid UTF-8 regardless of title content.
			if !utf8.ValidString(plain) {
				t.Errorf("renderItem() produced invalid UTF-8: %q", plain)
			}
		})
	}
}

// ─── renderItem title line content ───────────────────────────────────────────

func TestRenderItemTitleContent(t *testing.T) {
	m := Model{}
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
			rendered, _ := m.renderItem(tc.state, tc.colWidth)
			// Title line is always the first line.
			titleLine := stripAnsi(strings.SplitN(rendered, "\n", 2)[0])
			for _, want := range tc.wantContains {
				if !strings.Contains(titleLine, want) {
					t.Errorf("renderItem() title line = %q; want it to contain %q", titleLine, want)
				}
			}
		})
	}
}
