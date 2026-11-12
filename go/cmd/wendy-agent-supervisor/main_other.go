//go:build !linux

package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "wendy-agent-supervisor runs only on Linux")
	os.Exit(1)
}
