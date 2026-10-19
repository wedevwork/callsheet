// Command greeting prints a greeting.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
)

// run is the testable command; it returns the exit status.
func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("greeting", flag.ContinueOnError)
	fs.SetOutput(stderr)
	name := fs.String("name", "world", "the name to greet")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintln(stderr, "greeting: unexpected argument", fs.Arg(0))
		return 2
	}
	if *name == "" {
		fmt.Fprintln(stderr, "greeting: --name must not be empty")
		return 2
	}
	fmt.Fprintf(stdout, "Hello, %s!\n", *name)
	return 0
}

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }
