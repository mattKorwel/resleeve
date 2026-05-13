// Command resleeve is the resleeve CLI entry point.
// See docs/design/round-3/00-v1-cut.md for v1 scope.
package main

import (
	"fmt"
	"os"
)

const version = "0.0.0-dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println("resleeve", version)
		return
	}
	fmt.Println("resleeve", version)
	fmt.Println("usage: resleeve <command>")
	fmt.Println("(commands not yet implemented — see docs/design/round-3/00-v1-cut.md)")
	os.Exit(2)
}
