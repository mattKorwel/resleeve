package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mattkorwel/resleeve/internal/agent"
	"github.com/mattkorwel/resleeve/internal/memory"
)

// tool is a registered MCP tool.
type tool struct {
	name        string
	description string
	inputSchema map[string]any
	handler     func(ctx context.Context, c *agent.Client, defaultScope string, raw json.RawMessage) (toolCallResult, error)
}

func (s *Server) register(t *tool) {
	s.tools[t.name] = t
}

// registerTools wires the resleeve memory-curation tool surface. Every
// tool maps 1:1 to a daemon HTTP endpoint via agent.Client.
func (s *Server) registerTools() {
	// ----- scope CRUD -----

	s.register(&tool{
		name:        "resleeve_scope_set",
		description: "Create or update a scope (a node in the hierarchical memory tree). Only call when the user explicitly asks to create/update a scope.",
		inputSchema: schemaObject(map[string]any{
			"path":           schemaString("Scope path (slash-separated, e.g. 'resleeve' or 'resleeve/mcp-server'). Required."),
			"kind":           schemaString("portfolio | program | project | dispatch | agent | other"),
			"title":          schemaString("Human-readable title."),
			"description":    schemaString("Longer narrative."),
			"cwd":            schemaString("Optional working directory the scope is associated with."),
			"do_not_inherit": schemaBool("If true, this scope is a boundary reset — children won't inherit from ancestors."),
		}, []string{"path"}),
		handler: func(ctx context.Context, c *agent.Client, _ string, raw json.RawMessage) (toolCallResult, error) {
			a := decodeArgs(raw)
			path := a.string("path")
			if path == "" {
				return toolCallResult{}, fmt.Errorf("path is required")
			}
			kind := a.string("kind")
			if kind != "" && !memory.ScopeKind(kind).Valid() {
				return toolCallResult{}, fmt.Errorf("invalid kind %q", kind)
			}
			sc := &memory.Scope{
				Path:         path,
				Kind:         memory.ScopeKind(kind),
				Title:        a.string("title"),
				Description:  a.string("description"),
				Cwd:          a.string("cwd"),
				DoNotInherit: a.bool("do_not_inherit"),
			}
			got, err := c.PutScope(ctx, sc)
			if err != nil {
				return toolCallResult{}, err
			}
			return toolCallResult{Content: textBlock(fmt.Sprintf("ok: scope %s set (kind=%s)", got.Path, got.Kind))}, nil
		},
	})

	s.register(&tool{
		name:        "resleeve_scope_get",
		description: "Fetch one scope's metadata.",
		inputSchema: schemaObject(map[string]any{
			"path": schemaString("Scope path. Optional if $RESLEEVE_SCOPE / marker resolves a default."),
		}, nil),
		handler: func(ctx context.Context, c *agent.Client, defaultScope string, raw json.RawMessage) (toolCallResult, error) {
			a := decodeArgs(raw)
			path := a.stringOr("path", defaultScope)
			if path == "" {
				return toolCallResult{}, fmt.Errorf("no path: pass {\"path\":\"...\"} or set $RESLEEVE_SCOPE")
			}
			s, err := c.GetScope(ctx, path)
			if err != nil {
				return toolCallResult{}, err
			}
			var b strings.Builder
			fmt.Fprintf(&b, "path:        %s\n", s.Path)
			fmt.Fprintf(&b, "kind:        %s\n", s.Kind)
			fmt.Fprintf(&b, "title:       %s\n", s.Title)
			if s.Description != "" {
				fmt.Fprintf(&b, "description: %s\n", s.Description)
			}
			if s.Cwd != "" {
				fmt.Fprintf(&b, "cwd:         %s\n", s.Cwd)
			}
			if s.DoNotInherit {
				fmt.Fprintf(&b, "do_not_inherit: true\n")
			}
			fmt.Fprintf(&b, "created_at:  %s\n", s.CreatedAt.Format("2006-01-02T15:04:05Z"))
			fmt.Fprintf(&b, "updated_at:  %s\n", s.UpdatedAt.Format("2006-01-02T15:04:05Z"))
			return toolCallResult{Content: textBlock(b.String())}, nil
		},
	})

	s.register(&tool{
		name:        "resleeve_scope_list",
		description: "List all scopes in the memory tree.",
		inputSchema: schemaObject(map[string]any{}, nil),
		handler: func(ctx context.Context, c *agent.Client, _ string, _ json.RawMessage) (toolCallResult, error) {
			scopes, err := c.ListScopes(ctx)
			if err != nil {
				return toolCallResult{}, err
			}
			if len(scopes) == 0 {
				return toolCallResult{Content: textBlock("(no scopes)")}, nil
			}
			var b strings.Builder
			fmt.Fprintf(&b, "%-50s  %-12s  %s\n", "PATH", "KIND", "TITLE")
			for _, s := range scopes {
				marker := ""
				if s.DoNotInherit {
					marker = "  [do_not_inherit]"
				}
				fmt.Fprintf(&b, "%-50s  %-12s  %s%s\n", s.Path, s.Kind, s.Title, marker)
			}
			return toolCallResult{Content: textBlock(b.String())}, nil
		},
	})

	s.register(&tool{
		name:        "resleeve_scope_delete",
		description: "Delete a scope. Refuses (409) if the scope has child scopes — delete those first. Only call on explicit user request.",
		inputSchema: schemaObject(map[string]any{
			"path": schemaString("Scope path to delete."),
		}, []string{"path"}),
		handler: func(ctx context.Context, c *agent.Client, _ string, raw json.RawMessage) (toolCallResult, error) {
			a := decodeArgs(raw)
			path := a.string("path")
			if path == "" {
				return toolCallResult{}, fmt.Errorf("path is required")
			}
			if err := c.DeleteScope(ctx, path); err != nil {
				return toolCallResult{}, err
			}
			return toolCallResult{Content: textBlock("ok: scope " + path + " deleted")}, nil
		},
	})

	// ----- plan slots -----

	s.register(&tool{
		name:        "resleeve_plan_write",
		description: "Overwrite a plan slot on a scope. Slot defaults to '_default'. Use named slots for sibling drafts.",
		inputSchema: schemaObject(map[string]any{
			"scope":   schemaString("Scope path. Optional if $RESLEEVE_SCOPE / marker resolves a default."),
			"slot":    schemaString("Plan slot name; default '_default'."),
			"content": schemaString("Markdown plan content."),
		}, []string{"content"}),
		handler: func(ctx context.Context, c *agent.Client, defaultScope string, raw json.RawMessage) (toolCallResult, error) {
			a := decodeArgs(raw)
			scope, err := requireScope(a, defaultScope)
			if err != nil {
				return toolCallResult{}, err
			}
			slot := a.stringOr("slot", memory.DefaultPlanSlot)
			content := a.string("content")
			p, err := c.PutPlan(ctx, scope, slot, content)
			if err != nil {
				return toolCallResult{}, err
			}
			return toolCallResult{Content: textBlock(fmt.Sprintf("ok: plan %s/%s written (%d bytes)", scope, p.Name, len(content)))}, nil
		},
	})

	s.register(&tool{
		name:        "resleeve_plan_read",
		description: "Read one plan slot. With inherit=true, returns the slot from each ancestor (shallow→deep), truncated at do_not_inherit boundaries.",
		inputSchema: schemaObject(map[string]any{
			"scope":   schemaString("Scope path. Optional if $RESLEEVE_SCOPE / marker resolves a default."),
			"slot":    schemaString("Plan slot name; default '_default'."),
			"inherit": schemaBool("Walk ancestor scopes and concatenate."),
		}, nil),
		handler: func(ctx context.Context, c *agent.Client, defaultScope string, raw json.RawMessage) (toolCallResult, error) {
			a := decodeArgs(raw)
			scope, err := requireScope(a, defaultScope)
			if err != nil {
				return toolCallResult{}, err
			}
			slot := a.stringOr("slot", memory.DefaultPlanSlot)
			if a.bool("inherit") {
				plans, err := c.GetPlanInherited(ctx, scope, slot)
				if err != nil {
					return toolCallResult{}, err
				}
				if len(plans) == 0 {
					return toolCallResult{Content: textBlock(fmt.Sprintf("(no plan at %s/%s or any ancestor)", scope, slot))}, nil
				}
				var b strings.Builder
				for _, p := range plans {
					fmt.Fprintf(&b, "<!-- inherit: %s -->\n", p.Scope)
					b.WriteString(p.Content)
					if !strings.HasSuffix(p.Content, "\n") {
						b.WriteByte('\n')
					}
				}
				return toolCallResult{Content: textBlock(safeText(b.String(), "(empty inherited plan chain)"))}, nil
			}
			p, err := c.GetPlan(ctx, scope, slot)
			if err != nil {
				return toolCallResult{}, err
			}
			return toolCallResult{Content: textBlock(safeText(p.Content, fmt.Sprintf("(empty plan at %s/%s)", scope, slot)))}, nil
		},
	})

	s.register(&tool{
		name:        "resleeve_plan_list",
		description: "List all plan slots at a scope.",
		inputSchema: schemaObject(map[string]any{
			"scope": schemaString("Scope path. Optional if $RESLEEVE_SCOPE / marker resolves a default."),
		}, nil),
		handler: func(ctx context.Context, c *agent.Client, defaultScope string, raw json.RawMessage) (toolCallResult, error) {
			a := decodeArgs(raw)
			scope, err := requireScope(a, defaultScope)
			if err != nil {
				return toolCallResult{}, err
			}
			plans, err := c.ListPlans(ctx, scope)
			if err != nil {
				return toolCallResult{}, err
			}
			if len(plans) == 0 {
				return toolCallResult{Content: textBlock("(no plans at " + scope + ")")}, nil
			}
			var b strings.Builder
			fmt.Fprintf(&b, "%-30s  %-10s  %s\n", "SLOT", "BYTES", "UPDATED")
			for _, p := range plans {
				fmt.Fprintf(&b, "%-30s  %-10d  %s\n", p.Name, len(p.Content), p.UpdatedAt.Format("2006-01-02T15:04:05Z"))
			}
			return toolCallResult{Content: textBlock(b.String())}, nil
		},
	})

	// ----- learnings -----

	s.register(&tool{
		name:        "resleeve_learning_append",
		description: "Append a learning to a scope. Append-only — never edits prior entries. Use supersedes_id to mark a prior entry as wrong (it stays in storage as audit). Only call on explicit user ask (e.g. 'save this as a learning').",
		inputSchema: schemaObject(map[string]any{
			"scope":         schemaString("Scope path. Optional if $RESLEEVE_SCOPE / marker resolves a default."),
			"content":       schemaString("The learning — concise, durable, one finding per call."),
			"supersedes_id": schemaString("Optional id of a prior learning this one corrects."),
		}, []string{"content"}),
		handler: func(ctx context.Context, c *agent.Client, defaultScope string, raw json.RawMessage) (toolCallResult, error) {
			a := decodeArgs(raw)
			scope, err := requireScope(a, defaultScope)
			if err != nil {
				return toolCallResult{}, err
			}
			content := strings.TrimSpace(a.string("content"))
			if content == "" {
				return toolCallResult{}, fmt.Errorf("content is empty")
			}
			l, err := c.AppendLearning(ctx, scope, content, a.string("supersedes_id"))
			if err != nil {
				return toolCallResult{}, err
			}
			return toolCallResult{Content: textBlock(fmt.Sprintf("ok: learning appended to %s (id=%s)\n  preview: %s", l.Scope, l.ID, shorten(l.Content, 80)))}, nil
		},
	})

	s.register(&tool{
		name:        "resleeve_learning_list",
		description: "List learnings for a scope (most recent first). Optionally walk ancestors, optionally include superseded entries.",
		inputSchema: schemaObject(map[string]any{
			"scope":              schemaString("Scope path. Optional if $RESLEEVE_SCOPE / marker resolves a default."),
			"inherit":            schemaBool("Walk ancestor scopes and concatenate."),
			"include_superseded": schemaBool("Include superseded entries (default false)."),
		}, nil),
		handler: func(ctx context.Context, c *agent.Client, defaultScope string, raw json.RawMessage) (toolCallResult, error) {
			a := decodeArgs(raw)
			scope, err := requireScope(a, defaultScope)
			if err != nil {
				return toolCallResult{}, err
			}
			ls, err := c.ListLearnings(ctx, scope, a.bool("inherit"), a.bool("include_superseded"))
			if err != nil {
				return toolCallResult{}, err
			}
			if len(ls) == 0 {
				return toolCallResult{Content: textBlock("(no learnings at " + scope + ")")}, nil
			}
			var b strings.Builder
			for _, l := range ls {
				stale := ""
				if l.SupersedesID != nil {
					stale = "  (supersedes " + *l.SupersedesID + ")"
				}
				fmt.Fprintf(&b, "- [%s] id=%s scope=%s%s\n", l.CreatedAt.Format("2006-01-02"), l.ID, l.Scope, stale)
				fmt.Fprintf(&b, "  %s\n", strings.ReplaceAll(strings.TrimSpace(l.Content), "\n", "\n  "))
			}
			return toolCallResult{Content: textBlock(b.String())}, nil
		},
	})

	// ----- rolled-up context (the bridge's view) -----

	s.register(&tool{
		name:        "resleeve_context",
		description: "Return the rolled-up markdown context for a scope: parent plans + learnings walked shallow→deep with do_not_inherit boundaries respected. Same content the SessionStart hook injects.",
		inputSchema: schemaObject(map[string]any{
			"scope": schemaString("Scope path. Optional if $RESLEEVE_SCOPE / marker resolves a default."),
		}, nil),
		handler: func(ctx context.Context, c *agent.Client, defaultScope string, raw json.RawMessage) (toolCallResult, error) {
			a := decodeArgs(raw)
			scope, err := requireScope(a, defaultScope)
			if err != nil {
				return toolCallResult{}, err
			}
			out, err := c.GetContext(ctx, scope)
			if err != nil {
				return toolCallResult{}, err
			}
			return toolCallResult{Content: textBlock(safeText(out, "(no inherited plans or learnings for "+scope+")"))}, nil
		},
	})
}
