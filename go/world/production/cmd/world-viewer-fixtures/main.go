// world-viewer-fixtures writes deterministic synthetic viewer wire examples.
// It performs no signing, networking, device operations or model inference.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
)

func main() {
	out := flag.String("out", "", "output JSON file (stdout when omitted)")
	flag.Parse()
	fixture, err := generate()
	if err == nil {
		var raw []byte
		raw, err = json.MarshalIndent(fixture, "", "  ")
		if err == nil {
			raw = append(raw, '\n')
			if *out == "" {
				_, err = os.Stdout.Write(raw)
			} else {
				err = os.WriteFile(*out, raw, 0644)
			}
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
