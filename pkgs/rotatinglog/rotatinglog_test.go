// rotatinglog_test.go
//
//nolint:gocritic
package rotatinglog_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/BlackbirdWorks/copilot-autodev/pkgs/rotatinglog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRotatingLog(t *testing.T) {
	t.Parallel()

	type args struct{}
	type wants struct {
		minEntries int
	}
	tests := []struct {
		name  string
		args  args
		wants wants
	}{
		{
			name:  "rotate log file",
			args:  args{},
			wants: wants{minEntries: 2},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			logPath := filepath.Join(dir, "test.log")

			rl, err := rotatinglog.New(logPath, 0, 3)
			require.NoError(t, err)

			_, err = rl.Write([]byte("1234567890")) // 10 bytes
			require.NoError(t, err)

			_, err = rl.Write([]byte("1234567890")) // 10 bytes -> hits limit
			require.NoError(t, err)

			_, err = rl.Write([]byte("1234567890")) // 10 bytes -> should rotate here
			require.NoError(t, err)

			err = rl.Close()
			require.NoError(t, err)

			entries, err := os.ReadDir(dir)
			require.NoError(t, err)
			assert.GreaterOrEqual(t, len(entries), tt.wants.minEntries)
		})
	}
}

func TestRotatingLog_MultipleRotations(t *testing.T) {
	t.Parallel()

	type args struct{}
	type wants struct {
		maxEntries int
	}
	tests := []struct {
		name  string
		args  args
		wants wants
	}{
		{
			name:  "rotate log file multiple times",
			args:  args{},
			wants: wants{maxEntries: 3},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			logPath := filepath.Join(dir, "test.log")

			rl, err := rotatinglog.New(logPath, 0, 2)
			require.NoError(t, err)

			for i := 0; i < 10; i++ {
				_, err = rl.Write([]byte("12345"))
				require.NoError(t, err)
			}

			err = rl.Close()
			require.NoError(t, err)

			entries, err := os.ReadDir(dir)
			require.NoError(t, err)
			assert.LessOrEqual(t, len(entries), tt.wants.maxEntries) // max backups 2 + main log = 3
		})
	}
}
