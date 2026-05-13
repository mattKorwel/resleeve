package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/mattkorwel/resleeve/internal/event"
	rsql "github.com/mattkorwel/resleeve/internal/storage/sql"
)

// Client is a small HTTP client for talking to the local daemon.
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
		HTTP:    &http.Client{Timeout: 30 * time.Second},
	}
}

// AppendEvents POSTs a batch of events for the given session.
func (c *Client) AppendEvents(ctx context.Context, sessionID string, events []event.Event) error {
	body, err := json.Marshal(map[string]any{"events": events})
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	endpoint := fmt.Sprintf("%s/v1/sessions/%s/events", c.BaseURL, url.PathEscape(sessionID))
	return c.doNoBody(ctx, http.MethodPost, endpoint, bytes.NewReader(body), "application/json")
}

// ListSessions returns sessions matching the filter.
func (c *Client) ListSessions(ctx context.Context, f rsql.SessionFilter) ([]*rsql.Session, error) {
	q := url.Values{}
	if f.Scope != "" {
		q.Set("scope", f.Scope)
	}
	if f.AgentName != "" {
		q.Set("agent_name", f.AgentName)
	}
	if f.Status != "" {
		q.Set("status", string(f.Status))
	}
	if f.Limit > 0 {
		q.Set("limit", strconv.Itoa(f.Limit))
	}
	if f.Since != nil {
		q.Set("since", f.Since.UTC().Format(time.RFC3339Nano))
	}
	endpoint := c.BaseURL + "/v1/sessions"
	if len(q) > 0 {
		endpoint += "?" + q.Encode()
	}
	var resp struct {
		Sessions []*rsql.Session `json:"sessions"`
	}
	if err := c.doJSON(ctx, http.MethodGet, endpoint, nil, &resp); err != nil {
		return nil, err
	}
	return resp.Sessions, nil
}

// GetSession fetches one session's metadata.
func (c *Client) GetSession(ctx context.Context, id string) (*rsql.Session, error) {
	endpoint := fmt.Sprintf("%s/v1/sessions/%s", c.BaseURL, url.PathEscape(id))
	var ses rsql.Session
	if err := c.doJSON(ctx, http.MethodGet, endpoint, nil, &ses); err != nil {
		return nil, err
	}
	return &ses, nil
}

// ListEvents fetches events for a session with seq > sinceSeq.
func (c *Client) ListEvents(ctx context.Context, sessionID string, sinceSeq int64, limit int) ([]event.Event, error) {
	q := url.Values{}
	if sinceSeq > 0 {
		q.Set("since", strconv.FormatInt(sinceSeq, 10))
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	endpoint := fmt.Sprintf("%s/v1/sessions/%s/events", c.BaseURL, url.PathEscape(sessionID))
	if len(q) > 0 {
		endpoint += "?" + q.Encode()
	}
	var resp struct {
		Events []event.Event `json:"events"`
	}
	if err := c.doJSON(ctx, http.MethodGet, endpoint, nil, &resp); err != nil {
		return nil, err
	}
	return resp.Events, nil
}

// Search runs a cross-session content search.
func (c *Client) Search(ctx context.Context, query string, limit int) ([]rsql.EventSearchHit, error) {
	q := url.Values{}
	q.Set("q", query)
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	endpoint := c.BaseURL + "/v1/search?" + q.Encode()
	var resp struct {
		Hits []rsql.EventSearchHit `json:"hits"`
	}
	if err := c.doJSON(ctx, http.MethodGet, endpoint, nil, &resp); err != nil {
		return nil, err
	}
	return resp.Hits, nil
}

func (c *Client) doJSON(ctx context.Context, method, endpoint string, body io.Reader, into any) error {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return err
	}
	if c.Secret != "" {
		req.Header.Set("Authorization", "Bearer "+c.Secret)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, endpoint, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("daemon %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	if into == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(into)
}

func (c *Client) doNoBody(ctx context.Context, method, endpoint string, body io.Reader, contentType string) error {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
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
