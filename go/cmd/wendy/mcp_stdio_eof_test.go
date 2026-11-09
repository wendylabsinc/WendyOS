package main

import "testing"

func TestMCPServeImmediateEOFFromRealProcess(t *testing.T) {
	// EOF cancels the serving context inside Read. Exercise both SDK scheduling
	// outcomes repeatedly: neither may turn that clean disconnect into stderr.
	for attempt := range 30 {
		code, stdout, stderr := runWendy(t, "mcp", "serve")
		if code != 0 || stdout != "" || stderr != "" {
			t.Fatalf("immediate EOF attempt %d: exit=%d stdout=%q stderr=%q", attempt, code, stdout, stderr)
		}
	}
}
