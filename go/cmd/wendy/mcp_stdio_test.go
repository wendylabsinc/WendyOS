package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMCPServeImmediateEOFFromRealProcess(t *testing.T) {
	client, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runWendyWithStdin(t, client, "mcp", "serve")
	if code != 0 || stdout != "" || stderr != "" {
		t.Fatalf("immediate EOF: exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestMCPServeReadFailureFromRealProcess(t *testing.T) {
	// Inherit a valid file descriptor that rejects reads at the OS boundary.
	input, err := os.OpenFile(filepath.Join(t.TempDir(), "stdin"), os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()

	code, stdout, stderr := runWendyWithStdin(t, input, "mcp", "serve", "--json")
	if code != 1 || stdout != "" {
		t.Fatalf("stdin failure: exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	// The final stderr line is the CLI error, even if the transport logs first.
	lines := strings.Split(strings.TrimSpace(stderr), "\n")
	var failure errorEnvelope
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &failure); err != nil {
		t.Fatalf("invalid CLI error: %v, stderr=%q", err, stderr)
	}
	if failure.Error.Code != "error" || failure.Error.Exit != code || !strings.Contains(failure.Error.Message, "read ") {
		t.Fatalf("stdin read error was lost: %+v", failure.Error)
	}
}
