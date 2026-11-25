// world-agent-pair-fixture prepares disposable test trust; it is not a CA service.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/wendylabsinc/wendy/go/world/production/internal/agentvmfixture"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "paired VM fixture refused:", err)
		os.Exit(1)
	}
}

func run(args []string, out io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: world-agent-pair-fixture init|peers [flags]")
	}
	f := flag.NewFlagSet(args[0], flag.ContinueOnError)
	directory := f.String("directory", "", "fresh protected pair directory containing fixed a/b issuers")
	var states [2]*string
	if args[0] == "init" {
		states[0] = f.String("state-directory-a", "", "separate empty World state directory for a")
		states[1] = f.String("state-directory-b", "", "separate empty World state directory for b")
	} else if args[0] != "peers" {
		return fmt.Errorf("unknown pair fixture command")
	}
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	if f.NArg() != 0 || *directory == "" {
		return fmt.Errorf("explicit directory and no positional arguments required")
	}
	var value any
	var err error
	if args[0] == "init" {
		value, err = agentvmfixture.InitializePairEnrollment(*directory, [2]string{*states[0], *states[1]})
	} else {
		value, err = agentvmfixture.ReadPairPeers(*directory)
	}
	if err != nil {
		return err
	}
	raw, err := agentvmfixture.JSONReceipt(value)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(out, string(raw))
	return err
}
