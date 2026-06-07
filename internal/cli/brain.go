package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/mattkorwel/resleeve/internal/agent"
	"github.com/mattkorwel/resleeve/internal/serve"
)

// runBrain dispatches the `brain` verb (round-11b — SHARED-BRAIN
// management + brain selection):
//
//	resleeve brain create <name>          create a shared brain (you own it)
//	resleeve brain list                   list brains you're a member of
//	resleeve brain member list <brain>    list a brain's members
//	resleeve brain member add <brain> <user>   add a member (owner only)
//	resleeve brain member rm  <brain> <user>   remove a member (owner only)
//	resleeve brain use <brain-id>         set the machine's active brain
//	resleeve brain use --clear            revert to your personal brain
//
// The management verbs talk to the upstream `resleeve serve` with the
// device bearer (same path as `pair`/`login`); `brain use` is local-only
// (it writes the active-brain client config the daemon reads).
func runBrain(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: resleeve brain <create|list|member|use> [args]")
		return 2
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "create":
		return runBrainCreate(ctx, rest)
	case "list":
		return runBrainList(ctx, rest)
	case "member":
		return runBrainMember(ctx, rest)
	case "use":
		return runBrainUse(ctx, rest)
	default:
		fmt.Fprintf(os.Stderr, "resleeve brain: unknown subcommand %q\n", sub)
		return 2
	}
}

// upstreamAuth resolves (upstream, deviceToken) for an authenticated
// brain-management call, mirroring how `pair invite` finds the bearer:
// the upstream is the flag/env, the token is the keychain entry under
// (upstream, email). Returns a non-zero exit code via the bool when it
// fails (after printing the reason).
func upstreamAuth(upstreamFlag, emailFlag string) (upstream, token string, ok bool) {
	upstream = pickUpstream(upstreamFlag)
	if upstream == "" {
		fmt.Fprintln(os.Stderr, "brain: --upstream required (or set $RESLEEVE_UPSTREAM)")
		return "", "", false
	}
	email := strings.ToLower(strings.TrimSpace(emailFlag))
	if email == "" {
		got, err := promptLine("email: ")
		if err != nil {
			return "", "", false
		}
		email = strings.ToLower(strings.TrimSpace(got))
	}
	kc, err := defaultKeychain()
	if err != nil {
		fmt.Fprintln(os.Stderr, "brain: keychain:", err)
		return "", "", false
	}
	token, err = kc.Get(upstream, email)
	if err != nil {
		fmt.Fprintln(os.Stderr, "brain: keychain get:", err)
		return "", "", false
	}
	if token == "" {
		fmt.Fprintln(os.Stderr, "brain: no device token on file — run `resleeve login` first")
		return "", "", false
	}
	return upstream, token, true
}

func runBrainCreate(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("brain create", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	upstream := fs.String("upstream", "", "v2 sync upstream URL (default: $RESLEEVE_UPSTREAM)")
	email := fs.String("email", "", "account email (for keychain lookup; default: prompt)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) != 1 {
		fmt.Fprintln(os.Stderr, "usage: resleeve brain create [--upstream URL] [--email E] <name>")
		return 2
	}
	name := rest[0]

	up, token, ok := upstreamAuth(*upstream, *email)
	if !ok {
		return 1
	}
	var resp serve.CreateBrainResp
	if err := postJSON(ctx, up+"/v1/brains", token, serve.CreateBrainReq{Name: name}, &resp); err != nil {
		fmt.Fprintln(os.Stderr, "brain create:", err)
		return 1
	}
	fmt.Println(resp.BrainID)
	return 0
}

func runBrainList(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("brain list", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	upstream := fs.String("upstream", "", "v2 sync upstream URL (default: $RESLEEVE_UPSTREAM)")
	email := fs.String("email", "", "account email (for keychain lookup; default: prompt)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	up, token, ok := upstreamAuth(*upstream, *email)
	if !ok {
		return 1
	}
	var resp serve.ListBrainsResp
	if err := getJSON(ctx, up+"/v1/brains", token, &resp); err != nil {
		fmt.Fprintln(os.Stderr, "brain list:", err)
		return 1
	}
	if len(resp.Brains) == 0 {
		fmt.Println("(no brains)")
		return 0
	}
	active, _ := agent.LoadActiveBrain()
	fmt.Printf("%-34s  %-20s  %-8s  %-6s  %s\n", "ID", "NAME", "KIND", "ROLE", "ACTIVE")
	for _, b := range resp.Brains {
		marker := ""
		if b.ID == active {
			marker = "*"
		}
		fmt.Printf("%-34s  %-20s  %-8s  %-6s  %s\n", b.ID, b.Name, b.Kind, b.Role, marker)
	}
	return 0
}

func runBrainMember(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: resleeve brain member <add|rm|list> [args]")
		return 2
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "add":
		return runBrainMemberAddRm(ctx, rest, true)
	case "rm", "remove":
		return runBrainMemberAddRm(ctx, rest, false)
	case "list":
		return runBrainMemberList(ctx, rest)
	default:
		fmt.Fprintf(os.Stderr, "resleeve brain member: unknown subcommand %q\n", sub)
		return 2
	}
}

func runBrainMemberAddRm(ctx context.Context, args []string, add bool) int {
	name := "brain member rm"
	if add {
		name = "brain member add"
	}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	upstream := fs.String("upstream", "", "v2 sync upstream URL (default: $RESLEEVE_UPSTREAM)")
	email := fs.String("email", "", "account email (for keychain lookup; default: prompt)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) != 2 {
		fmt.Fprintf(os.Stderr, "usage: resleeve %s [--upstream URL] [--email E] <brain-id> <user-id>\n", name)
		return 2
	}
	brainID, userID := rest[0], rest[1]

	up, token, ok := upstreamAuth(*upstream, *email)
	if !ok {
		return 1
	}
	if add {
		if err := postJSON(ctx, up+"/v1/brains/"+brainID+"/members", token,
			serve.AddMemberReq{UserID: userID}, nil); err != nil {
			fmt.Fprintln(os.Stderr, name+":", err)
			return 1
		}
		fmt.Printf("added %s to brain %s\n", userID, brainID)
		return 0
	}
	if err := deleteJSON(ctx, up+"/v1/brains/"+brainID+"/members/"+userID, token); err != nil {
		fmt.Fprintln(os.Stderr, name+":", err)
		return 1
	}
	fmt.Printf("removed %s from brain %s\n", userID, brainID)
	return 0
}

func runBrainMemberList(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("brain member list", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	upstream := fs.String("upstream", "", "v2 sync upstream URL (default: $RESLEEVE_UPSTREAM)")
	email := fs.String("email", "", "account email (for keychain lookup; default: prompt)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) != 1 {
		fmt.Fprintln(os.Stderr, "usage: resleeve brain member list [--upstream URL] [--email E] <brain-id>")
		return 2
	}
	brainID := rest[0]

	up, token, ok := upstreamAuth(*upstream, *email)
	if !ok {
		return 1
	}
	var resp serve.ListMembersResp
	if err := getJSON(ctx, up+"/v1/brains/"+brainID+"/members", token, &resp); err != nil {
		fmt.Fprintln(os.Stderr, "brain member list:", err)
		return 1
	}
	for _, m := range resp.Members {
		fmt.Println(m)
	}
	return 0
}

// runBrainUse sets (or clears) the machine's GLOBAL active brain. This is
// a local-only operation: it writes the active-brain client config that
// the daemon's sync client reads to append ?brain=<id> upstream. A
// running daemon picks up the change on its next sync cycle if it re-reads
// the config; today it reads at construction, so the user should restart
// the daemon (`resleeve down && resleeve up`) for an already-running
// daemon to switch — printed as a hint.
func runBrainUse(_ context.Context, args []string) int {
	fs := flag.NewFlagSet("brain use", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	clear := fs.Bool("clear", false, "clear the active brain (revert to your personal brain)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	if *clear {
		if len(rest) != 0 {
			fmt.Fprintln(os.Stderr, "usage: resleeve brain use --clear")
			return 2
		}
		if err := agent.WriteActiveBrain(""); err != nil {
			fmt.Fprintln(os.Stderr, "brain use:", err)
			return 1
		}
		fmt.Println("active brain cleared — syncing your personal brain")
		fmt.Println("restart the daemon to apply: resleeve down && resleeve up")
		return 0
	}
	if len(rest) != 1 {
		fmt.Fprintln(os.Stderr, "usage: resleeve brain use <brain-id>  (or --clear)")
		return 2
	}
	brainID := strings.TrimSpace(rest[0])
	if brainID == "" {
		fmt.Fprintln(os.Stderr, "brain use: empty brain id")
		return 2
	}
	if err := agent.WriteActiveBrain(brainID); err != nil {
		fmt.Fprintln(os.Stderr, "brain use:", err)
		return 1
	}
	fmt.Printf("active brain set to %s\n", brainID)
	fmt.Println("restart the daemon to apply: resleeve down && resleeve up")
	return 0
}

// --- small HTTP helpers (GET/DELETE counterparts of postJSON) ---

// getJSON issues an authenticated GET and decodes the JSON body into out.
// Error bodies are surfaced via the same envelope-aware path as postJSON.
func getJSON(ctx context.Context, endpoint, bearer string, out any) error {
	return requestJSON(ctx, http.MethodGet, endpoint, bearer, nil, out)
}

// deleteJSON issues an authenticated DELETE; the response body (if any) is
// drained. Used by `brain member rm`.
func deleteJSON(ctx context.Context, endpoint, bearer string) error {
	return requestJSON(ctx, http.MethodDelete, endpoint, bearer, nil, nil)
}

// requestJSON is the shared GET/DELETE driver. POST keeps using postJSON
// (which sets Content-Type); this one carries no request body.
func requestJSON(ctx context.Context, method, endpoint, bearer string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal: %w", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, rdr)
	if err != nil {
		return err
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("http %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return decodeHTTPError(resp)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// decodeHTTPError renders a 4xx/5xx response into a Go error, preferring
// the structured {"error":{"code","message"}} envelope and falling back
// to the legacy flat shape — mirrors postJSON's error handling so the
// server's 403/404 messages surface cleanly.
func decodeHTTPError(resp *http.Response) error {
	errBody, _ := io.ReadAll(resp.Body)
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(errBody, &env) == nil && env.Error.Message != "" {
		return fmt.Errorf("%s: %s", resp.Status, env.Error.Message)
	}
	var legacy struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(errBody, &legacy) == nil && legacy.Error != "" {
		return fmt.Errorf("%s: %s", resp.Status, legacy.Error)
	}
	return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(errBody)))
}
