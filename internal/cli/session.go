package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/mattkorwel/resleeve/internal/agent"
	"github.com/mattkorwel/resleeve/internal/event"
	rsql "github.com/mattkorwel/resleeve/internal/storage/sql"
)

func runSession(ctx context.Context, args []string) int {
	if len(args) == 0 {
		printSessionUsage(os.Stderr)
		return 2
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list":
		return runSessionList(ctx, rest)
	case "show":
		return runSessionShow(ctx, rest)
	case "search":
		return runSessionSearch(ctx, rest)
	case "tail":
		return runSessionTail(ctx, rest)
	case "help", "--help", "-h":
		printSessionUsage(os.Stdout)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "resleeve session: unknown subcommand %q\n\n", sub)
		printSessionUsage(os.Stderr)
		return 2
	}
}

func printSessionUsage(w *os.File) {
	fmt.Fprintln(w, "usage: resleeve session <subcommand> [args]")
	fmt.Fprintln(w, "subcommands:")
	fmt.Fprintln(w, "  list [--scope X] [--limit N] [--status active|ended|...]")
	fmt.Fprintln(w, "  show <session-id>")
	fmt.Fprintln(w, "  search <query> [--limit N]")
	fmt.Fprintln(w, "  tail <session-id> [--from <seq>]")
}

func clientFromEndpoint() (*agent.Client, error) {
	url, secret, err := agent.LoadEndpoint()
	if err != nil {
		return nil, fmt.Errorf("no resleeve daemon endpoint: %w", err)
	}
	return agent.NewClient(url, secret), nil
}

// --- session list ---

func runSessionList(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("session list", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	scope := fs.String("scope", "", "filter by scope")
	status := fs.String("status", "", "filter by status (active|ended|failed|partial)")
	limit := fs.Int("limit", 50, "max rows")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	c, err := clientFromEndpoint()
	if err != nil {
		fmt.Fprintln(os.Stderr, "session list:", err)
		return 1
	}
	f := rsql.SessionFilter{Scope: *scope, Limit: *limit}
	if *status != "" {
		f.Status = rsql.SessionStatus(*status)
	}
	sessions, err := c.ListSessions(ctx, f)
	if err != nil {
		fmt.Fprintln(os.Stderr, "session list:", err)
		return 1
	}
	if len(sessions) == 0 {
		fmt.Println("(no sessions)")
		return 0
	}
	fmt.Printf("%-38s  %-22s  %-10s  %-12s  %-8s  %s\n", "ID", "SCOPE/AGENT", "CLI", "STATUS", "EVENTS", "STARTED")
	for _, s := range sessions {
		slot := s.Slot.Scope + "/" + s.Slot.AgentName
		fmt.Printf("%-38s  %-22s  %-10s  %-12s  %-8d  %s\n",
			truncate(s.ID, 38), truncate(slot, 22), truncate(s.CLI, 10),
			truncate(string(s.Status), 12), s.EventCount, humanTime(s.StartedAt),
		)
	}
	return 0
}

// --- session show ---

func runSessionShow(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("session show", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	showThinking := fs.Bool("thinking", false, "include thinking blocks")
	showSystem := fs.Bool("system", false, "include system events")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) != 1 {
		fmt.Fprintln(os.Stderr, "usage: resleeve session show <session-id>")
		return 2
	}
	sessionID := rest[0]

	c, err := clientFromEndpoint()
	if err != nil {
		fmt.Fprintln(os.Stderr, "session show:", err)
		return 1
	}
	ses, err := c.GetSession(ctx, sessionID)
	if err != nil {
		fmt.Fprintln(os.Stderr, "session show:", err)
		return 1
	}

	fmt.Printf("session %s\n", ses.ID)
	fmt.Printf("  slot     %s/%s\n", ses.Slot.Scope, ses.Slot.AgentName)
	fmt.Printf("  cli      %s %s\n", ses.CLI, ses.CLIVersion)
	if ses.Cwd != "" {
		fmt.Printf("  cwd      %s\n", ses.Cwd)
	}
	fmt.Printf("  started  %s\n", ses.StartedAt.Format(time.RFC3339))
	if ses.EndedAt != nil {
		fmt.Printf("  ended    %s\n", ses.EndedAt.Format(time.RFC3339))
	}
	fmt.Printf("  status   %s\n", ses.Status)
	fmt.Println()

	// Stream events in pages.
	var since int64
	skippedKinds := 0
	for {
		events, err := c.ListEvents(ctx, sessionID, since, 200)
		if err != nil {
			fmt.Fprintln(os.Stderr, "session show:", err)
			return 1
		}
		if len(events) == 0 {
			break
		}
		for _, e := range events {
			if e.Seq > since {
				since = e.Seq
			}
			if e.Kind == event.KindThinking && !*showThinking {
				skippedKinds++
				continue
			}
			if e.Kind == event.KindSystem && !*showSystem {
				skippedKinds++
				continue
			}
			fmt.Println(renderEvent(e))
		}
		if len(events) < 200 {
			break
		}
	}
	if skippedKinds > 0 {
		fmt.Fprintf(os.Stderr, "\n(%d event(s) hidden — pass --thinking and/or --system to show)\n", skippedKinds)
	}
	return 0
}

// --- session search ---

func runSessionSearch(ctx context.Context, args []string) int {
	// flag.FlagSet stops at the first positional, so flags-after-query
	// gets eaten by the query. Parse flags by hand to allow intermixing.
	limit := 20
	var positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--limit":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "session search: --limit needs a value")
				return 2
			}
			n, err := strconv.Atoi(args[i+1])
			if err != nil {
				fmt.Fprintln(os.Stderr, "session search: --limit must be an integer")
				return 2
			}
			limit = n
			i++
		case strings.HasPrefix(a, "--limit="):
			n, err := strconv.Atoi(strings.TrimPrefix(a, "--limit="))
			if err != nil {
				fmt.Fprintln(os.Stderr, "session search: --limit must be an integer")
				return 2
			}
			limit = n
		default:
			positional = append(positional, a)
		}
	}
	if len(positional) == 0 {
		fmt.Fprintln(os.Stderr, "usage: resleeve session search <query> [--limit N]")
		return 2
	}
	query := strings.Join(positional, " ")

	c, err := clientFromEndpoint()
	if err != nil {
		fmt.Fprintln(os.Stderr, "session search:", err)
		return 1
	}
	hits, err := c.Search(ctx, query, limit)
	if err != nil {
		fmt.Fprintln(os.Stderr, "session search:", err)
		return 1
	}
	if len(hits) == 0 {
		fmt.Println("(no matches)")
		return 0
	}
	for _, h := range hits {
		fmt.Printf("%s seq=%d [%s] %s\n",
			truncate(h.Event.SessionID, 38), h.Event.Seq, h.Event.Kind, h.Snippet)
	}
	return 0
}

// --- session tail ---

func runSessionTail(ctx context.Context, args []string) int {
	// Hand-rolled parsing so flags can appear before OR after the positional
	// session id (flag.FlagSet stops at the first positional).
	const usage = "usage: resleeve session tail <session-id> [--limit N] [--from SEQ] [--poll-ms MS]"
	limit := 50
	var fromSeq int64
	pollMS := 500
	var positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--limit":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "session tail: --limit needs a value")
				return 2
			}
			n, err := strconv.Atoi(args[i+1])
			if err != nil {
				fmt.Fprintln(os.Stderr, "session tail: --limit must be an integer")
				return 2
			}
			limit = n
			i++
		case strings.HasPrefix(a, "--limit="):
			n, err := strconv.Atoi(strings.TrimPrefix(a, "--limit="))
			if err != nil {
				fmt.Fprintln(os.Stderr, "session tail: --limit must be an integer")
				return 2
			}
			limit = n
		case a == "--from":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "session tail: --from needs a value")
				return 2
			}
			n, err := strconv.ParseInt(args[i+1], 10, 64)
			if err != nil {
				fmt.Fprintln(os.Stderr, "session tail: --from must be an integer")
				return 2
			}
			fromSeq = n
			i++
		case strings.HasPrefix(a, "--from="):
			n, err := strconv.ParseInt(strings.TrimPrefix(a, "--from="), 10, 64)
			if err != nil {
				fmt.Fprintln(os.Stderr, "session tail: --from must be an integer")
				return 2
			}
			fromSeq = n
		case a == "--poll-ms":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "session tail: --poll-ms needs a value")
				return 2
			}
			n, err := strconv.Atoi(args[i+1])
			if err != nil {
				fmt.Fprintln(os.Stderr, "session tail: --poll-ms must be an integer")
				return 2
			}
			pollMS = n
			i++
		case strings.HasPrefix(a, "--poll-ms="):
			n, err := strconv.Atoi(strings.TrimPrefix(a, "--poll-ms="))
			if err != nil {
				fmt.Fprintln(os.Stderr, "session tail: --poll-ms must be an integer")
				return 2
			}
			pollMS = n
		case strings.HasPrefix(a, "--"):
			fmt.Fprintf(os.Stderr, "session tail: unknown flag %q\n", a)
			fmt.Fprintln(os.Stderr, usage)
			return 2
		default:
			positional = append(positional, a)
		}
	}
	if len(positional) != 1 {
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
	sessionID := positional[0]

	c, err := clientFromEndpoint()
	if err != nil {
		fmt.Fprintln(os.Stderr, "session tail:", err)
		return 1
	}
	since := fromSeq
	interval := time.Duration(pollMS) * time.Millisecond
	for {
		if ctx.Err() != nil {
			return 0
		}
		events, err := c.ListEvents(ctx, sessionID, since, limit)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return 0
			}
			fmt.Fprintln(os.Stderr, "session tail:", err)
			return 1
		}
		for _, e := range events {
			if e.Seq > since {
				since = e.Seq
			}
			fmt.Println(renderEvent(e))
		}
		// Wait, then poll again.
		t := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			t.Stop()
			return 0
		case <-t.C:
		}
	}
}

// --- rendering ---

func renderEvent(e event.Event) string {
	ts := e.Timestamp.Format("15:04:05")
	prefix := fmt.Sprintf("[%s] %-18s", ts, e.Kind)
	body := summarizeContent(e)
	return prefix + " " + body
}

func summarizeContent(e event.Event) string {
	if len(e.Content) == 0 {
		return ""
	}
	var raw map[string]any
	if err := json.Unmarshal(e.Content, &raw); err != nil {
		return string(e.Content)
	}
	switch e.Kind {
	case event.KindUserMessage, event.KindAssistantMessage, event.KindThinking:
		if t, _ := raw["text"].(string); t != "" {
			return truncate(strings.ReplaceAll(t, "\n", " "), 200)
		}
	case event.KindToolCall:
		tool, _ := raw["tool"].(map[string]any)
		if tool != nil {
			name, _ := tool["name"].(string)
			args, _ := json.Marshal(tool["args"])
			return fmt.Sprintf("%s %s", name, truncate(string(args), 160))
		}
	case event.KindToolResult:
		tool, _ := raw["tool"].(map[string]any)
		if tool != nil {
			status, _ := tool["status"].(string)
			res, _ := json.Marshal(tool["result"])
			return fmt.Sprintf("(%s) %s", status, truncate(string(res), 160))
		}
	case event.KindSessionStart:
		cli, _ := raw["cli"].(string)
		cwd, _ := raw["cwd"].(string)
		return fmt.Sprintf("cli=%s cwd=%s", cli, cwd)
	case event.KindSessionEnd:
		reason, _ := raw["reason"].(string)
		return fmt.Sprintf("reason=%s", reason)
	}
	b, _ := json.Marshal(raw)
	return truncate(string(b), 200)
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	if max <= 1 {
		return s[:max]
	}
	return s[:max-1] + "…"
}

func humanTime(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return t.Format("2006-01-02")
	}
}
