//go:build windows

package commands

import "os"

// openPromptTTY finds no fallback terminal on Windows: prompts need stdout or
// stderr to be the console.
func openPromptTTY() (*os.File, func(), bool) {
	return nil, nil, false
}
