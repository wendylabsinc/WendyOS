package commands

import (
	"bytes"
	"strings"
	"testing"
)

func TestMCPCmd_HelpText(t *testing.T) {
	cmd := newMCPCmd()
	buf := new(bytes.Buffer)
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs([]string{"--help"})
	_ = cmd.Execute()
	out := buf.String()
	if !strings.Contains(out, "serve") {
		t.Fatalf("expected help to mention 'serve', got: %s", out)
	}
}
