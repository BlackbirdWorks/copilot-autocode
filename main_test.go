//nolint:gocritic,goimports
package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLogWriter(t *testing.T) {
	t.Parallel()

	type args struct {
		input []byte
	}
	type wants struct {
		n   int
		msg string
	}
	tests := []struct {
		name  string
		args  args
		wants wants
	}{
		{
			name: "with newline",
			args: args{
				input: []byte("hello world\n"),
			},
			wants: wants{
				n:   12,
				msg: "hello world",
			},
		},
		{
			name: "without newline",
			args: args{
				input: []byte("no newline"),
			},
			wants: wants{
				n:   10,
				msg: "no newline",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			msgCh := make(chan string, 100)
			lw := &logWriter{
				msgCh: msgCh,
			}
			lw.start() // start goroutine

			n, err := lw.Write(tt.args.input)
			require.NoError(t, err)
			assert.Equal(t, tt.wants.n, n)

			msg := <-msgCh
			assert.Equal(t, tt.wants.msg, msg)
		})
	}
}

func TestMain_MissingTokenExits(t *testing.T) {
	t.Parallel()

	if os.Getenv("COPILOT_AUTODEV_TEST_MAIN_MISSING_TOKEN") == "1" {
		os.Args = []string{"copilot-autodev", "-config", "config.yaml"}
		_ = os.Unsetenv("GITHUB_TOKEN")
		main()
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestMain_MissingTokenExits")
	cmd.Env = append(os.Environ(), "COPILOT_AUTODEV_TEST_MAIN_MISSING_TOKEN=1")
	out, err := cmd.CombinedOutput()
	require.Error(t, err)
	assert.Contains(t, string(out), "GITHUB_TOKEN environment variable is required")
}

func TestMain_ConfigLoadErrorExits(t *testing.T) {
	t.Parallel()

	if os.Getenv("COPILOT_AUTODEV_TEST_MAIN_BAD_CONFIG") == "1" {
		os.Args = []string{"copilot-autodev", "-config", "does-not-exist.yaml"}
		require.NoError(t, os.Setenv("GITHUB_TOKEN", "test-token"))
		main()
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestMain_ConfigLoadErrorExits")
	cmd.Env = append(os.Environ(),
		"COPILOT_AUTODEV_TEST_MAIN_BAD_CONFIG=1",
		"GITHUB_TOKEN=test-token",
	)
	out, err := cmd.CombinedOutput()
	require.Error(t, err)
	assert.True(
		t,
		strings.Contains(string(out), "failed to load config") ||
			strings.Contains(string(out), "read config file"),
	)
}

func TestLogWriter_DropsWhenBufferFull(t *testing.T) {
	t.Parallel()

	msgCh := make(chan string, 1)
	lw := &logWriter{msgCh: msgCh}

	_, err := lw.Write([]byte("first\n"))
	require.NoError(t, err)
	_, err = lw.Write([]byte("second\n"))
	require.NoError(t, err)

	assert.Equal(t, 1, len(msgCh))
	assert.Equal(t, "first", <-msgCh)
}
