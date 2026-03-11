//nolint:gocritic,goimports
package agent_test

import (
	"net/http"
)

type fakeRoundTripper struct {
	handler func(*http.Request) (*http.Response, error)
}

func (f *fakeRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	return f.handler(r)
}
