package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

func runLearning(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: resleeve learning <append|list> [args]")
		return 2
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "append", "add":
		return runLearningAppend(ctx, rest)
	case "list", "ls":
		return runLearningList(ctx, rest)
	default:
		fmt.Fprintf(os.Stderr, "resleeve learning: unknown subcommand %q\n", sub)
		return 2
	}
}

func runLearningAppend(ctx context.Context, args []string) int {
	supersedes := ""
	var positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--supersedes":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "learning append: --supersedes needs an ID")
				return 2
			}
			supersedes = args[i+1]
			i++
		case strings.HasPrefix(a, "--supersedes="):
			supersedes = strings.TrimPrefix(a, "--supersedes=")
		default:
			positional = append(positional, a)
		}
	}
	if len(positional) == 0 {
		fmt.Fprintln(os.Stderr, "usage: resleeve learning append <scope> [\"<text>\" | -] [--supersedes <id>]")
		return 2
	}
	scope := positional[0]
	rest := positional[1:]

	var content string
	switch {
	case len(rest) == 0 || (len(rest) == 1 && rest[0] == "-"):
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			fmt.Fprintln(os.Stderr, "learning append:", err)
			return 1
		}
		content = string(b)
	default:
		content = strings.Join(rest, " ")
	}
	content = strings.TrimSpace(content)
	if content == "" {
		fmt.Fprintln(os.Stderr, "learning append: empty content")
		return 2
	}

	c, err := clientFromEndpoint()
	if err != nil {
		fmt.Fprintln(os.Stderr, "learning append:", err)
		return 1
	}
	l, err := c.AppendLearning(ctx, scope, content, supersedes)
	if err != nil {
		fmt.Fprintln(os.Stderr, "learning append:", err)
		return 1
	}
	fmt.Printf("learning appended: %s (id=%s)\n", scope, l.ID)
	return 0
}

func runLearningList(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("learning list", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	inherit := fs.Bool("inherit", false, "walk the scope chain")
	includeSuperseded := fs.Bool("include-superseded", false, "include superseded learnings")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: resleeve learning list <scope> [--inherit] [--include-superseded]")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	c, err := clientFromEndpoint()
	if err != nil {
		fmt.Fprintln(os.Stderr, "learning list:", err)
		return 1
	}
	ls, err := c.ListLearnings(ctx, fs.Arg(0), *inherit, *includeSuperseded)
	if err != nil {
		fmt.Fprintln(os.Stderr, "learning list:", err)
		return 1
	}
	if len(ls) == 0 {
		fmt.Println("(no learnings)")
		return 0
	}
	for _, l := range ls {
		stale := ""
		if l.SupersedesID != nil {
			stale = " (supersedes " + *l.SupersedesID + ")"
		}
		fmt.Printf("- [%s] %s%s\n", l.CreatedAt.Format("2006-01-02"), oneLine(l.Content), stale)
		fmt.Printf("  id=%s scope=%s\n", l.ID, l.Scope)
	}
	return 0
}

func oneLine(s string) string {
	return strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
}
