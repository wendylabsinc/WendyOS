// Command docgen generates CLI reference documentation from the Cobra command tree.
//
// Usage:
//
//	go run ./cmd/docgen -out ./docs/cli                         # markdown (default)
//	go run ./cmd/docgen -out ./docs/cli -format yaml            # yaml with front matter
//	go run ./cmd/docgen -out ./docs/cli -format markdown        # explicit markdown
package main

import (
	"fmt"
	"log"
	"os"

	"github.com/spf13/cobra/doc"
	"github.com/wendylabsinc/wendy/internal/cli/commands"
)

func main() {
	outDir := "./docs/cli"
	format := "markdown"

	// Simple flag parsing without pulling in pflag/cobra for a tiny tool.
	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-out":
			i++
			if i < len(args) {
				outDir = args[i]
			}
		case "-format":
			i++
			if i < len(args) {
				format = args[i]
			}
		case "-help", "--help", "-h":
			fmt.Println("Usage: docgen [-out DIR] [-format markdown|yaml|man]")
			fmt.Println()
			fmt.Println("Generates CLI reference docs from the Cobra command tree.")
			fmt.Println()
			fmt.Println("Flags:")
			fmt.Println("  -out DIR       Output directory (default: ./docs/cli)")
			fmt.Println("  -format FMT    Output format: markdown, yaml, man (default: markdown)")
			os.Exit(0)
		}
	}

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		log.Fatalf("creating output directory: %v", err)
	}

	root := commands.NewRootCmd()

	// Disable the auto-generated timestamp so the output is reproducible.
	root.DisableAutoGenTag = true

	switch format {
	case "markdown", "md":
		if err := doc.GenMarkdownTree(root, outDir); err != nil {
			log.Fatalf("generating markdown docs: %v", err)
		}
	case "yaml":
		if err := doc.GenYamlTree(root, outDir); err != nil {
			log.Fatalf("generating yaml docs: %v", err)
		}
	case "man":
		header := &doc.GenManHeader{
			Title:   "WENDY",
			Section: "1",
		}
		if err := doc.GenManTree(root, header, outDir); err != nil {
			log.Fatalf("generating man pages: %v", err)
		}
	default:
		log.Fatalf("unknown format %q: expected markdown, yaml, or man", format)
	}

	fmt.Printf("Generated %s docs in %s\n", format, outDir)
}
