package devcover

import (
	"os"
	"os/exec"
	"testing"
)

// requireRoot skips the calling test unless it can create mount namespaces.
// With WENDY_DEVCOVER_REQUIRED=1 it fails instead, so CI cannot pass by
// skipping everything.
func requireRoot(t *testing.T) {
	t.Helper()
	reason := ""
	if os.Geteuid() != 0 {
		reason = "needs root"
	} else if _, err := exec.LookPath("unshare"); err != nil {
		reason = "needs the unshare command"
	}
	if reason == "" {
		return
	}
	if os.Getenv("WENDY_DEVCOVER_REQUIRED") == "1" {
		t.Fatalf("WENDY_DEVCOVER_REQUIRED=1 but %s", reason)
	}
	t.Skip(reason)
}
