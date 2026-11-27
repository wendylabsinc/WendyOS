package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/wendylabsinc/wendy/go/world/internal/scenario"
)

func run(out io.Writer) error {
	report, err := scenario.Run()
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(report)
}

func main() {
	if len(os.Args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: world-demo (writes a synthetic in-process scenario as JSON)")
		os.Exit(2)
	}
	if err := run(os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
