// logger_test.go
//
//nolint:gocritic
package logger_test

import (
	"log/slog"
	"testing"

	"github.com/BlackbirdWorks/copilot-autodev/pkgs/logger"
	"github.com/stretchr/testify/assert"
)

func TestSaveAndLoad(t *testing.T) {
	t.Parallel()
	type args struct {
		l *slog.Logger
	}
	type wants struct {
		isEqual bool
	}
	tests := []struct {
		name  string
		args  args
		wants wants
	}{
		{
			name: "save and load",
			args: args{
				l: slog.Default(),
			},
			wants: wants{
				isEqual: true,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			ctx = logger.Save(ctx, tt.args.l)
			l2 := logger.Load(ctx)
			if tt.wants.isEqual {
				assert.Equal(t, tt.args.l, l2)
			}
		})
	}
}

func TestLoadEmpty(t *testing.T) {
	t.Parallel()
	type args struct{}
	type wants struct {
		notNil bool
	}
	tests := []struct {
		name  string
		args  args
		wants wants
	}{
		{
			name:  "load empty context returns default",
			args:  args{},
			wants: wants{notNil: true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			l2 := logger.Load(ctx)
			if tt.wants.notNil {
				assert.NotNil(t, l2)
			}
		})
	}
}
