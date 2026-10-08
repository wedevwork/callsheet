// Command mcpfixture-export is the decoder-enrollment maintainer command
// (design decoder-enrollment B2, FP-18): it exports one pinned, complete
// capture bundle into a new staging directory outside every Git work tree
// as a metadata-sanitised enrollment candidate with its sanitization.json
// receipt:
//
//	go run ./cmd/mcpfixture-export --source ABSOLUTE_BUNDLE --out ABSOLUTE_NEW_STAGING_DIRECTORY
//
// It makes no model call, network access or decoder invocation, writes no
// oracle, attestation, index or registry entry and commits nothing. The
// policy and its accepted sources are compiled in
// (mcpqual.ProductionFixturePolicy); no flag, plan or variable changes them.
// Exit 2 is an invalid invocation, 1 any validation, policy, safety or I/O
// failure, 0 a completed candidate export.
package main

import (
	"io"
	"os"

	"github.com/wedevwork/callsheet/internal/mcpqual"
)

// exit ends the process; tests replace it to run main in process.
var exit = os.Exit

func main() {
	exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run forwards the arguments to the command runner with the production
// policy, the only policy this entry point ever uses.
func run(args []string, stdout, stderr io.Writer) int {
	return mcpqual.RunFixtureExport(args, stdout, stderr, mcpqual.ProductionFixturePolicy())
}
