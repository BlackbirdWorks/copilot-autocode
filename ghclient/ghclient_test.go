package ghclient

import (
	"testing"
	"time"
)

// TestTimeAgo verifies that TimeAgo produces the correct relative-time label
// for representative durations across all four branches of the function.
func TestTimeAgo(t *testing.T) {
	now := time.Now()

	tests := []struct {
		name string
		t    time.Time
		want string
	}{
		// < 1 minute → "just now"
		{"1 second ago", now.Add(-1 * time.Second), "just now"},
		{"30 seconds ago", now.Add(-30 * time.Second), "just now"},
		{"59 seconds ago", now.Add(-59 * time.Second), "just now"},

		// >= 1 minute, < 1 hour → "Nm ago"
		{"exactly 1 minute ago", now.Add(-1 * time.Minute), "1m ago"},
		{"5 minutes ago", now.Add(-5 * time.Minute), "5m ago"},
		{"59 minutes ago", now.Add(-59 * time.Minute), "59m ago"},

		// >= 1 hour, < 24 hours → "Nh ago"
		{"exactly 1 hour ago", now.Add(-1 * time.Hour), "1h ago"},
		{"3 hours ago", now.Add(-3 * time.Hour), "3h ago"},
		{"23 hours ago", now.Add(-23 * time.Hour), "23h ago"},

		// >= 24 hours → "Nd ago"
		{"exactly 1 day ago", now.Add(-24 * time.Hour), "1d ago"},
		{"2 days ago", now.Add(-48 * time.Hour), "2d ago"},
		{"10 days ago", now.Add(-240 * time.Hour), "10d ago"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := TimeAgo(tc.t)
			if got != tc.want {
				t.Errorf("TimeAgo(%v) = %q; want %q", tc.t, got, tc.want)
			}
		})
	}
}
