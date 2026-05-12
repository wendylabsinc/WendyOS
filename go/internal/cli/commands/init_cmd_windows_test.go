//go:build windows

package commands

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWritePromptForAssistant_CreatesFileWithPromptAndCleansUp(t *testing.T) {
	prompt := "multi-line prompt\nwith \"quotes\" and 100% percent signs"

	path, cleanup, err := writePromptForAssistant(prompt)
	if err != nil {
		t.Fatalf("writePromptForAssistant: %v", err)
	}
	if path == "" {
		t.Fatal("returned path is empty")
	}
	if filepath.Ext(path) != ".md" {
		t.Fatalf("path %q does not end in .md", path)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading temp file: %v", err)
	}
	if string(content) != prompt {
		t.Fatalf("file content = %q, want %q", string(content), prompt)
	}

	cleanup()

	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("expected temp file to be removed after cleanup, got err=%v", err)
	}
}

func TestWritePromptForAssistant_CleanupIsIdempotent(t *testing.T) {
	path, cleanup, err := writePromptForAssistant("anything")
	if err != nil {
		t.Fatalf("writePromptForAssistant: %v", err)
	}

	cleanup()
	// Second call must not panic or surface an error to the caller — it is
	// invoked via defer in launchAssistantWithPrompt and we may have already
	// hit an error path that removed the file.
	cleanup()

	if strings.TrimSpace(path) == "" {
		t.Fatal("path should not be empty")
	}
}
