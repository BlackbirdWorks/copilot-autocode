//nolint:gocritic,goimports
package resolver

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRedactingWriter(t *testing.T) {
	t.Parallel()

	type args struct {
		input []byte
		w     *redactingWriter
	}
	type wants struct {
		n   int
		out string
	}
	tests := []struct {
		name  string
		args  args
		wants wants
	}{
		{
			name: "with token",
			args: args{
				input: []byte("hello secret world"),
				w: &redactingWriter{
					token: "secret",
				},
			},
			wants: wants{
				n:   18,
				out: "hello <redacted> world",
			},
		},
		{
			name: "without token",
			args: args{
				input: []byte("hello secret world"),
				w:     &redactingWriter{},
			},
			wants: wants{
				n:   18,
				out: "hello secret world",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			tt.args.w.w = &buf
			n, err := tt.args.w.Write(tt.args.input)
			require.NoError(t, err)
			assert.Equal(t, tt.wants.n, n)
			assert.Equal(t, tt.wants.out, buf.String())
		})
	}
}
