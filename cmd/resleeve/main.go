// Command resleeve is the resleeve CLI entry point.
// See docs/design/round-3/00-v1-cut.md for v1 scope.
package main

import (
	"os"

	"github.com/mattkorwel/resleeve/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:]))
}
