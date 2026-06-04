package serve

import (
	"bytes"
	"io"
	"net/http"
	"testing"
	"time"
)

// newHTTPClient returns the test HTTP client (short timeout so a hung
// server fails the test instead of the whole package).
func newHTTPClient() *http.Client { return &http.Client{Timeout: 30 * time.Second} }

func newHTTPReq(t *testing.T, url, bearer string, body []byte) (*http.Request, error) {
	t.Helper()
	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	return req, nil
}

func readAll(t *testing.T, r io.Reader) string {
	t.Helper()
	b, err := io.ReadAll(r)
	if err != nil {
		return "<read error>"
	}
	return string(b)
}
