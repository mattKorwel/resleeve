package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/mattkorwel/resleeve/internal/memory"
)

// PutScope upserts a scope.
func (c *Client) PutScope(ctx context.Context, s *memory.Scope) (*memory.Scope, error) {
	body, _ := json.Marshal(s)
	endpoint := c.BaseURL + "/v1/scope?path=" + url.QueryEscape(s.Path)
	var got memory.Scope
	if err := c.doJSON(ctx, http.MethodPut, endpoint, bytes.NewReader(body), &got); err != nil {
		return nil, err
	}
	return &got, nil
}

// GetScope fetches a single scope.
func (c *Client) GetScope(ctx context.Context, path string) (*memory.Scope, error) {
	endpoint := c.BaseURL + "/v1/scope?path=" + url.QueryEscape(path)
	var got memory.Scope
	if err := c.doJSON(ctx, http.MethodGet, endpoint, nil, &got); err != nil {
		return nil, err
	}
	return &got, nil
}

// DeleteScope removes a scope (refuses with 409 if it has children).
func (c *Client) DeleteScope(ctx context.Context, path string) error {
	endpoint := c.BaseURL + "/v1/scope?path=" + url.QueryEscape(path)
	return c.doNoBody(ctx, http.MethodDelete, endpoint, nil, "")
}

// ListScopes returns all scopes.
func (c *Client) ListScopes(ctx context.Context) ([]*memory.Scope, error) {
	endpoint := c.BaseURL + "/v1/scopes"
	var resp struct {
		Scopes []*memory.Scope `json:"scopes"`
	}
	if err := c.doJSON(ctx, http.MethodGet, endpoint, nil, &resp); err != nil {
		return nil, err
	}
	return resp.Scopes, nil
}

// PutPlan upserts a plan into a (scope, slot).
func (c *Client) PutPlan(ctx context.Context, scope, slot, content string) (*memory.Plan, error) {
	body, _ := json.Marshal(map[string]string{"content": content})
	endpoint := fmt.Sprintf("%s/v1/plan?scope=%s&slot=%s",
		c.BaseURL, url.QueryEscape(scope), url.QueryEscape(slot))
	var got memory.Plan
	if err := c.doJSON(ctx, http.MethodPut, endpoint, bytes.NewReader(body), &got); err != nil {
		return nil, err
	}
	return &got, nil
}

// GetPlan returns one plan from one slot.
func (c *Client) GetPlan(ctx context.Context, scope, slot string) (*memory.Plan, error) {
	endpoint := fmt.Sprintf("%s/v1/plan?scope=%s&slot=%s",
		c.BaseURL, url.QueryEscape(scope), url.QueryEscape(slot))
	var got memory.Plan
	if err := c.doJSON(ctx, http.MethodGet, endpoint, nil, &got); err != nil {
		return nil, err
	}
	return &got, nil
}

// GetPlanInherited returns the default-slot plans from each ancestor
// scope (after applying the .donotinherit boundary).
func (c *Client) GetPlanInherited(ctx context.Context, scope, slot string) ([]*memory.Plan, error) {
	endpoint := fmt.Sprintf("%s/v1/plan?scope=%s&slot=%s&inherit=true",
		c.BaseURL, url.QueryEscape(scope), url.QueryEscape(slot))
	var resp struct {
		Plans []*memory.Plan `json:"plans"`
	}
	if err := c.doJSON(ctx, http.MethodGet, endpoint, nil, &resp); err != nil {
		return nil, err
	}
	return resp.Plans, nil
}

// ListPlans returns all slots for a scope.
func (c *Client) ListPlans(ctx context.Context, scope string) ([]*memory.Plan, error) {
	endpoint := c.BaseURL + "/v1/plans?scope=" + url.QueryEscape(scope)
	var resp struct {
		Plans []*memory.Plan `json:"plans"`
	}
	if err := c.doJSON(ctx, http.MethodGet, endpoint, nil, &resp); err != nil {
		return nil, err
	}
	return resp.Plans, nil
}

// AppendLearning posts a new learning, optionally superseding a prior one.
func (c *Client) AppendLearning(ctx context.Context, scope, content, supersedesID string) (*memory.Learning, error) {
	body, _ := json.Marshal(map[string]string{"content": content})
	endpoint := c.BaseURL + "/v1/learnings?scope=" + url.QueryEscape(scope)
	if supersedesID != "" {
		endpoint += "&supersedes=" + url.QueryEscape(supersedesID)
	}
	var got memory.Learning
	if err := c.doJSON(ctx, http.MethodPost, endpoint, bytes.NewReader(body), &got); err != nil {
		return nil, err
	}
	return &got, nil
}

// ListLearnings returns learnings for a scope (or its ancestor chain
// with inherit=true), optionally including superseded entries.
func (c *Client) ListLearnings(ctx context.Context, scope string, inherit, includeSuperseded bool) ([]*memory.Learning, error) {
	endpoint := c.BaseURL + "/v1/learnings?scope=" + url.QueryEscape(scope)
	if inherit {
		endpoint += "&inherit=true"
	}
	if includeSuperseded {
		endpoint += "&include=superseded"
	}
	var resp struct {
		Learnings []*memory.Learning `json:"learnings"`
	}
	if err := c.doJSON(ctx, http.MethodGet, endpoint, nil, &resp); err != nil {
		return nil, err
	}
	return resp.Learnings, nil
}

// GetContext returns the rolled-up markdown context for a scope (used
// by the bridge's SessionStart injection).
func (c *Client) GetContext(ctx context.Context, scope string) (string, error) {
	endpoint := c.BaseURL + "/v1/context?scope=" + url.QueryEscape(scope)
	var resp struct {
		Scope   string `json:"scope"`
		Context string `json:"context"`
	}
	if err := c.doJSON(ctx, http.MethodGet, endpoint, nil, &resp); err != nil {
		return "", err
	}
	return resp.Context, nil
}
