package jobops_test

import (
	"context"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/jobops"
)

func TestCheckpointArgumentsFailBeforeHostAccess(t *testing.T) {
	base := jobops.CheckpointOptions{Path: "/tmp/model.pt", Python: "/tmp/venv/bin/python", SHA256: strings.Repeat("a", 64)}
	for _, field := range []string{"path", "python", "hash", "source"} {
		bad := base
		switch field {
		case "path":
			bad.Path = "../model.pt"
		case "python":
			bad.Python = "python3"
		case "hash":
			bad.SHA256 = "wrong"
		case "source":
			bad.SourceDirectory = "/"
		}
		if _, err := jobops.InspectCheckpoint(context.Background(), nil, bad); err == nil || strings.Contains(err.Error(), "connect to a device") {
			t.Fatalf("invalid checkpoint arguments reached host: %s %v", field, err)
		}
	}
}

func TestCheckpointInspectionRetainsHostAuthenticationGate(t *testing.T) {
	c := connection(t, &host{})
	c.IsMTLS = false
	_, err := jobops.InspectCheckpoint(context.Background(), c, jobops.CheckpointOptions{Path: "/tmp/model.pt", Python: "/tmp/venv/bin/python", SHA256: strings.Repeat("a", 64)})
	if err == nil || !strings.Contains(err.Error(), "authenticated mTLS") {
		t.Fatalf("checkpoint inspector bypassed host gate: %v", err)
	}
}
