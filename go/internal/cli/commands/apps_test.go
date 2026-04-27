package commands

import (
	"bytes"
	"strings"
	"testing"
)

func TestDeviceAppsListCommand_HelpDescribesDeployedApps(t *testing.T) {
	cmd := newDeviceCmd()
	buf := new(bytes.Buffer)
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs([]string{"apps", "list", "--help"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "List deployed applications") {
		t.Fatalf("expected help output to contain %q, got %q", "List deployed applications", output)
	}
	if strings.Contains(output, "List running applications") {
		t.Fatalf("expected help output to avoid stale wording, got %q", output)
	}
}
