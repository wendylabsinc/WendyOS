//go:build windows

package commands

import (
	"fmt"
	"os"
	"os/exec"
)

// launchAssistantWithPrompt writes the full prompt to a temp file and asks the
// assistant to read it. On Windows the assistant binaries (claude, codex) are
// shipped as `.cmd` shims whose `%*` forwarding re-parses arguments through
// cmd.exe, which mangles multi-line prompts containing quotes or `%`. The short
// "Read the file at %q ..." instruction contains no `%` and no embedded
// newlines, so it survives that re-parsing intact regardless of prompt content.
func launchAssistantWithPrompt(choice, prompt string) error {
	tmpPath, cleanup, err := writePromptForAssistant(prompt)
	if err != nil {
		return err
	}
	defer cleanup()

	short := fmt.Sprintf("Read the file at %q for project context, then help me get started building this project.", tmpPath)

	cmd := exec.Command(choice, short)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// writePromptForAssistant writes prompt to a temp .md file and returns its path
// plus a cleanup func. Split out so the temp-file lifecycle is unit-testable
// without invoking the real assistant binary.
func writePromptForAssistant(prompt string) (string, func(), error) {
	f, err := os.CreateTemp("", "wendy-init-prompt-*.md")
	if err != nil {
		return "", nil, fmt.Errorf("creating prompt temp file: %w", err)
	}
	path := f.Name()

	if _, err := f.WriteString(prompt); err != nil {
		f.Close()
		os.Remove(path)
		return "", nil, fmt.Errorf("writing prompt temp file: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return "", nil, fmt.Errorf("closing prompt temp file: %w", err)
	}

	return path, func() { os.Remove(path) }, nil
}
