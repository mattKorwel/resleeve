package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/mattkorwel/resleeve/internal/agent"
	"github.com/mattkorwel/resleeve/internal/memory"
)

func runPlan(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: resleeve plan <write|read|list> [args]")
		return 2
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "write":
		return runPlanWrite(ctx, rest)
	case "read":
		return runPlanRead(ctx, rest)
	case "list", "ls":
		return runPlanList(ctx, rest)
	default:
		fmt.Fprintf(os.Stderr, "resleeve plan: unknown subcommand %q\n", sub)
		return 2
	}
}

func runPlanWrite(ctx context.Context, args []string) int {
	// Hand-rolled flag parsing so the order is flexible and trailing
	// positional (the scope path) doesn't get eaten by Go's flag pkg.
	scope, slot, content, file, editFlag, force, ok := parsePlanWriteArgs(args)
	if !ok {
		fmt.Fprintln(os.Stderr, "usage: resleeve plan write <scope> [--slot N] [--content S | --file F | --edit] [--force]")
		fmt.Fprintln(os.Stderr, "       default: read content from stdin (the agent-driven case)")
		return 2
	}
	c, err := clientFromEndpoint()
	if err != nil {
		fmt.Fprintln(os.Stderr, "plan write:", err)
		return 1
	}

	body, err := resolvePlanWriteContent(ctx, scope, slot, content, file, editFlag)
	if err != nil {
		fmt.Fprintln(os.Stderr, "plan write:", err)
		return 1
	}

	// Read the current HEAD version and write against it (optimistic
	// concurrency). A missing plan -> base_version 0 (expect-new).
	// --force bypasses the check. round-12B.
	base := memory.NewPlanBaseVersion
	if !force {
		if p, err := c.GetPlan(ctx, scope, slot); err == nil && p != nil {
			base = p.Version
		}
	}
	p, err := c.PutPlan(ctx, scope, slot, body, base, force)
	if err != nil {
		var conflict *agent.PlanConflict
		if errors.As(err, &conflict) {
			headV := int64(0)
			if conflict.Head != nil {
				headV = conflict.Head.Version
			}
			fmt.Fprintf(os.Stderr,
				"plan write: conflict — %s [%s] was advanced to version %d since you last read it.\n"+
					"Re-read with `resleeve plan read %s%s`, merge, and retry (or pass --force to overwrite).\n",
				scope, slotOrDefault(slot), headV, scope, slotFlagSuffix(slot))
			return 1
		}
		fmt.Fprintln(os.Stderr, "plan write:", err)
		return 1
	}
	fmt.Printf("plan write: %s [%s] version %d (%d bytes)\n", scope, slotOrDefault(slot), p.Version, len(body))
	return 0
}

// slotFlagSuffix renders the " --slot N" suffix for help hints, empty for
// the default slot.
func slotFlagSuffix(slot string) string {
	if slot == "" {
		return ""
	}
	return " --slot " + slot
}

func parsePlanWriteArgs(args []string) (scope, slot, content, file string, editFlag, force, ok bool) {
	var positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--force":
			force = true
		case a == "--slot":
			if i+1 >= len(args) {
				return
			}
			slot = args[i+1]
			i++
		case strings.HasPrefix(a, "--slot="):
			slot = strings.TrimPrefix(a, "--slot=")
		case a == "--content":
			if i+1 >= len(args) {
				return
			}
			content = args[i+1]
			i++
		case strings.HasPrefix(a, "--content="):
			content = strings.TrimPrefix(a, "--content=")
		case a == "--file":
			if i+1 >= len(args) {
				return
			}
			file = args[i+1]
			i++
		case strings.HasPrefix(a, "--file="):
			file = strings.TrimPrefix(a, "--file=")
		case a == "--edit":
			editFlag = true
		default:
			positional = append(positional, a)
		}
	}
	if len(positional) != 1 {
		return
	}
	scope = positional[0]
	ok = true
	return
}

func resolvePlanWriteContent(ctx context.Context, scope, slot, content, file string, editFlag bool) (string, error) {
	switch {
	case content != "":
		return content, nil
	case file != "":
		b, err := os.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("read --file: %w", err)
		}
		return string(b), nil
	case editFlag:
		return editFromEditor(ctx, scope, slot)
	default:
		// stdin (default — the agent-driven case)
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", fmt.Errorf("read stdin: %w", err)
		}
		return string(b), nil
	}
}

func editFromEditor(ctx context.Context, scope, slot string) (string, error) {
	// Pull the current content (if any) so the user edits in place.
	c, err := clientFromEndpoint()
	if err != nil {
		return "", err
	}
	initial := ""
	if p, err := c.GetPlan(ctx, scope, slot); err == nil && p != nil {
		initial = p.Content
	}
	tmp, err := os.CreateTemp("", "resleeve-plan-*.md")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if _, err := io.WriteString(tmp, initial); err != nil {
		_ = tmp.Close()
		return "", err
	}
	_ = tmp.Close()
	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = "vi"
	}
	cmd := exec.Command(editor, tmpPath)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("run %s: %w", filepath.Base(editor), err)
	}
	b, err := os.ReadFile(tmpPath)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func runPlanRead(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("plan read", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	slot := fs.String("slot", "", "plan slot (default _default)")
	inherit := fs.Bool("inherit", false, "walk the scope chain")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: resleeve plan read <scope> [--slot N] [--inherit]")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	scope := fs.Arg(0)
	c, err := clientFromEndpoint()
	if err != nil {
		fmt.Fprintln(os.Stderr, "plan read:", err)
		return 1
	}
	if *inherit {
		plans, err := c.GetPlanInherited(ctx, scope, *slot)
		if err != nil {
			fmt.Fprintln(os.Stderr, "plan read:", err)
			return 1
		}
		if len(plans) == 0 {
			fmt.Println("(no plans on the inherited chain)")
			return 0
		}
		for _, p := range plans {
			fmt.Printf("## Plan (%s)\n\n%s\n\n", p.Scope, strings.TrimRight(p.Content, "\n"))
		}
		return 0
	}
	p, err := c.GetPlan(ctx, scope, *slot)
	if err != nil {
		fmt.Fprintln(os.Stderr, "plan read:", err)
		return 1
	}
	// Version goes to stderr so stdout stays pure content (pipe-friendly,
	// and re-feedable to `plan write`). round-12B: the printed version is
	// what a caller passes back as --base / base_version on the next write.
	fmt.Fprintf(os.Stderr, "# plan %s [%s] version %d\n", scope, slotOrDefault(*slot), p.Version)
	fmt.Print(p.Content)
	if !strings.HasSuffix(p.Content, "\n") {
		fmt.Println()
	}
	return 0
}

func runPlanList(ctx context.Context, args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: resleeve plan list <scope>")
		return 2
	}
	c, err := clientFromEndpoint()
	if err != nil {
		fmt.Fprintln(os.Stderr, "plan list:", err)
		return 1
	}
	plans, err := c.ListPlans(ctx, args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "plan list:", err)
		return 1
	}
	if len(plans) == 0 {
		fmt.Println("(no plans)")
		return 0
	}
	fmt.Printf("%-32s  %-22s  %s\n", "SLOT", "UPDATED", "BYTES")
	for _, p := range plans {
		fmt.Printf("%-32s  %-22s  %d\n", p.Name, p.UpdatedAt.Format("2006-01-02T15:04:05Z"), len(p.Content))
	}
	return 0
}

func slotOrDefault(slot string) string {
	if slot == "" {
		return "_default"
	}
	return slot
}
