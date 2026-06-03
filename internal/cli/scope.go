package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/mattkorwel/resleeve/internal/memory"
)

func runScope(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: resleeve scope <set|get|list|delete> [args]")
		return 2
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "set":
		return runScopeSet(ctx, rest)
	case "get":
		return runScopeGet(ctx, rest)
	case "list", "ls":
		return runScopeList(ctx, rest)
	case "delete", "rm":
		return runScopeDelete(ctx, rest)
	default:
		fmt.Fprintf(os.Stderr, "resleeve scope: unknown subcommand %q\n", sub)
		return 2
	}
}

func runScopeSet(ctx context.Context, args []string) int {
	// Hand-rolled parsing so the user can put flags before or after the
	// positional path (flag.FlagSet stops at the first positional).
	var kind, title, description, cwd string
	var doNotInherit bool
	var positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--kind":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "scope set: --kind needs a value")
				return 2
			}
			kind = args[i+1]
			i++
		case strings.HasPrefix(a, "--kind="):
			kind = strings.TrimPrefix(a, "--kind=")
		case a == "--title":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "scope set: --title needs a value")
				return 2
			}
			title = args[i+1]
			i++
		case strings.HasPrefix(a, "--title="):
			title = strings.TrimPrefix(a, "--title=")
		case a == "--description":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "scope set: --description needs a value")
				return 2
			}
			description = args[i+1]
			i++
		case strings.HasPrefix(a, "--description="):
			description = strings.TrimPrefix(a, "--description=")
		case a == "--cwd":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "scope set: --cwd needs a value")
				return 2
			}
			cwd = args[i+1]
			i++
		case strings.HasPrefix(a, "--cwd="):
			cwd = strings.TrimPrefix(a, "--cwd=")
		case a == "--do-not-inherit":
			doNotInherit = true
		default:
			positional = append(positional, a)
		}
	}
	if len(positional) != 1 {
		fmt.Fprintln(os.Stderr, "usage: resleeve scope set <path> [--kind X] [--title T] [--description D] [--cwd C] [--do-not-inherit]")
		return 2
	}
	path := positional[0]
	if kind != "" && !memory.ScopeKind(kind).Valid() {
		fmt.Fprintf(os.Stderr, "scope set: invalid kind %q\n", kind)
		return 2
	}

	c, err := clientFromEndpoint()
	if err != nil {
		fmt.Fprintln(os.Stderr, "scope set:", err)
		return 1
	}
	s := &memory.Scope{
		Path:         path,
		Kind:         memory.ScopeKind(kind),
		Title:        title,
		Description:  description,
		Cwd:          cwd,
		DoNotInherit: doNotInherit,
	}
	if _, err := c.PutScope(ctx, s); err != nil {
		fmt.Fprintln(os.Stderr, "scope set:", err)
		return 1
	}
	fmt.Println("scope set:", path)
	return 0
}

func runScopeGet(ctx context.Context, args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: resleeve scope get <path>")
		return 2
	}
	c, err := clientFromEndpoint()
	if err != nil {
		fmt.Fprintln(os.Stderr, "scope get:", err)
		return 1
	}
	s, err := c.GetScope(ctx, args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "scope get:", err)
		return 1
	}
	fmt.Printf("path:           %s\n", s.Path)
	fmt.Printf("kind:           %s\n", s.Kind)
	fmt.Printf("title:          %s\n", s.Title)
	if s.Description != "" {
		fmt.Printf("description:    %s\n", s.Description)
	}
	if s.Cwd != "" {
		fmt.Printf("cwd:            %s\n", s.Cwd)
	}
	if s.DoNotInherit {
		fmt.Println("do_not_inherit: true")
	}
	fmt.Printf("created_at:     %s\n", s.CreatedAt.Format("2006-01-02T15:04:05Z"))
	fmt.Printf("updated_at:     %s\n", s.UpdatedAt.Format("2006-01-02T15:04:05Z"))
	return 0
}

func runScopeList(ctx context.Context, args []string) int {
	_ = args // no flags in v1
	c, err := clientFromEndpoint()
	if err != nil {
		fmt.Fprintln(os.Stderr, "scope list:", err)
		return 1
	}
	scopes, err := c.ListScopes(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "scope list:", err)
		return 1
	}
	if len(scopes) == 0 {
		fmt.Println("(no scopes)")
		return 0
	}
	fmt.Printf("%-50s  %-12s  %s\n", "PATH", "KIND", "TITLE")
	for _, s := range scopes {
		marker := ""
		if s.DoNotInherit {
			marker = " ⛔"
		}
		fmt.Printf("%-50s  %-12s  %s%s\n", truncate(s.Path, 50), s.Kind, s.Title, marker)
	}
	return 0
}

func runScopeDelete(ctx context.Context, args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: resleeve scope delete <path>")
		return 2
	}
	c, err := clientFromEndpoint()
	if err != nil {
		fmt.Fprintln(os.Stderr, "scope delete:", err)
		return 1
	}
	if err := c.DeleteScope(ctx, args[0]); err != nil {
		if strings.Contains(err.Error(), "has children") || errors.Is(err, memory.ErrScopeHasChildren) {
			fmt.Fprintln(os.Stderr, "scope delete: refused — scope has children. Delete them first.")
			return 1
		}
		fmt.Fprintln(os.Stderr, "scope delete:", err)
		return 1
	}
	fmt.Println("scope deleted:", args[0])
	return 0
}
