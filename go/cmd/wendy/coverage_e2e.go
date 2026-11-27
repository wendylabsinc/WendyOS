//go:build e2e_coverage

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// A start receipt lets collection detect a killed invocation even when other
// invocations in the same test successfully wrote counters. No arguments or
// environment values are recorded. This file is absent from ordinary builds.
func init() {
	if dir := os.Getenv("GOCOVERDIR"); dir != "" {
		name := fmt.Sprintf("invocation.%d.%d", os.Getpid(), time.Now().UnixNano())
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0600); err != nil {
			fmt.Fprintln(os.Stderr, "E2E coverage receipt:", err)
			os.Exit(1)
		}
	}
}
