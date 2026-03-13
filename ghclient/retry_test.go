package ghclient_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BlackbirdWorks/copilot-autodev/config"
	"github.com/BlackbirdWorks/copilot-autodev/ghclient"
)

func TestRetryRoundTripper(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		failures   int
		status     int
		maxRetries int
		wantError  bool
		wantCalls  int
	}{
		{
			name:       "success after one 503",
			failures:   1,
			status:     http.StatusServiceUnavailable,
			maxRetries: 2,
			wantError:  false,
			wantCalls:  2,
		},
		{
			name:       "success after two 503s",
			failures:   2,
			status:     http.StatusServiceUnavailable,
			maxRetries: 2,
			wantError:  false,
			wantCalls:  3,
		},
		{
			name:       "exhausted retries after 503s",
			failures:   5,
			status:     http.StatusServiceUnavailable,
			maxRetries: 3,
			wantError:  true,
			wantCalls:  4,
		},
		{
			name:       "no retry on 404",
			failures:   1,
			status:     http.StatusNotFound,
			maxRetries: 2,
			wantError:  true,
			wantCalls:  1,
		},
		{
			name:       "success after one 429",
			failures:   1,
			status:     http.StatusTooManyRequests,
			maxRetries: 2,
			wantError:  false,
			wantCalls:  2,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if calls <= tc.failures {
					w.WriteHeader(tc.status)
					return
				}
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"ok":true}`))
			}))
			defer srv.Close()

			cfg := &config.Config{GitHubOwner: "owner", GitHubRepo: "repo"}
			client := ghclient.NewWithTransport("token", cfg, srv.Client().Transport)
			
			gh := client.GH()
			gh.BaseURL = parseURL(srv.URL + "/")

			_, _, err := gh.Repositories.Get(t.Context(), "owner", "repo")
			
			if tc.wantError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tc.wantCalls, calls)
		})
	}
}

func parseURL(s string) *url.URL {
	u, _ := url.Parse(s)
	return u
}
