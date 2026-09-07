// Command sqlshape checks Go packages (`sqlshape ./...`, or `go vet -vettool=$(which
// sqlshape) ./...`), and manages the schema: `sqlshape diff`, `apply`, `verify-schema`.
// See `sqlshape help`.
package main

import (
	"context"
	"os"

	"golang.org/x/tools/go/analysis/singlechecker"

	"github.com/kr9ly/sqlshape/internal/cli"
	"github.com/kr9ly/sqlshape/internal/vet"
)

func main() {
	// a bare invocation, or one whose first argument is a flag or a package pattern, is the
	// checker (that is how go vet drives a vettool); a subcommand name selects the rest
	if len(os.Args) > 1 && cli.IsSubcommand(os.Args[1]) && os.Args[1] != "vet" {
		os.Exit(cli.Run(context.Background(), os.Args[1], os.Args[2:], os.Stdout, os.Stderr))
	}
	if len(os.Args) > 1 && os.Args[1] == "vet" {
		os.Args = append(os.Args[:1], os.Args[2:]...)
	}
	singlechecker.Main(vet.Analyzer)
}
