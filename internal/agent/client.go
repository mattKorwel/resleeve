package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/mattkorwel/resleeve/internal/event"
)

// Client is a small HTTP client for talking to the local daemon from
// the bridge subcommand (`resleeve hook`).
type Client struct {
	BaseURL string
	Secret  string
	HTTP    *http.Client
}

// NewClient builds a Client with sensible defaults.
func NewClient(baseURL, secret string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Secret:  secret,
		HTTP:    &http.Client{Timeout: 5 * time.Second},
	}
}

// AppendEvents POSTs a batch of events for the given session.
func (c *Client) AppendEvents(ctx context.Context, sessionID string, events []event.Event) error {
	body, err := json.Marshal(map[string]any{"events": events})
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	url := fmt.Sprintf("%s/v1/sessions/%s/events", c.BaseURL, sessionID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.Secret != "" {
		req.Header.Set("Authorization", "Bearer "+c.Secret)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("post: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("daemon: %s", resp.Status)
	}
	return nil
}
