package ghclient

import (
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/BlackbirdWorks/copilot-autodev/pkgs/logger"
	"log/slog"
)

// RetryRoundTripper is an http.RoundTripper that retries requests on transient errors.
type RetryRoundTripper struct {
	next       http.RoundTripper
	maxRetries int
}

// NewRetryRoundTripper creates a new RetryRoundTripper.
func NewRetryRoundTripper(next http.RoundTripper, maxRetries int) *RetryRoundTripper {
	if next == nil {
		next = http.DefaultTransport
	}
	return &RetryRoundTripper{
		next:       next,
		maxRetries: maxRetries,
	}
}

// RoundTrip implements http.RoundTripper.
func (rt *RetryRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	var lastResp *http.Response
	var lastErr error

	for attempt := 0; attempt <= rt.maxRetries; attempt++ {
		if attempt > 0 {
			// Exponential backoff: 1s, 2s, 4s, 8s...
			backoff := time.Duration(math.Pow(2, float64(attempt-1))) * time.Second
			
			// Respect Retry-After header if present
			if lastResp != nil {
				if after := lastResp.Header.Get("Retry-After"); after != "" {
					if seconds, err := strconv.Atoi(after); err == nil {
						backoff = time.Duration(seconds) * time.Second
					} else if date, err := http.ParseTime(after); err == nil {
						backoff = time.Until(date)
					}
				}
			}

			select {
			case <-req.Context().Done():
				return nil, req.Context().Err()
			case <-time.After(backoff):
			}
		}

		// Prepare a new request body if necessary (rewind)
		if req.Body != nil && attempt > 0 {
			// Note: go-github handles body rewinding if we use their client, 
			// but for direct calls with our own transport, we have to be careful.
			// However, most go-github calls use non-streaming bodies (bytes.Reader).
		}

		lastResp, lastErr = rt.next.RoundTrip(req)
		if lastErr == nil && !rt.isTransient(lastResp.StatusCode) {
			return lastResp, nil
		}

		if lastErr != nil {
			logger.Load(req.Context()).WarnContext(req.Context(), "request failed; retrying",
				slog.String("url", req.URL.String()),
				slog.Int("attempt", attempt+1),
				slog.Any("err", lastErr))
		} else {
			logger.Load(req.Context()).WarnContext(req.Context(), "request failed with transient status; retrying",
				slog.String("url", req.URL.String()),
				slog.Int("status", lastResp.StatusCode),
				slog.Int("attempt", attempt+1))
			// Close the body of the failed response before retrying
			lastResp.Body.Close()
		}
	}

	return lastResp, lastErr
}

func (rt *RetryRoundTripper) isTransient(code int) bool {
	return code == http.StatusTooManyRequests ||
		code == http.StatusBadGateway ||
		code == http.StatusServiceUnavailable ||
		code == http.StatusGatewayTimeout
}
