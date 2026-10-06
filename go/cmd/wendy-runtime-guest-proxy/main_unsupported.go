//go:build !linux

package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "wendy-runtime-guest-proxy runs only inside the Linux runtime guest")
	os.Exit(1)
}
