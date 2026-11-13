package jobops

import (
	"context"
	_ "embed"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
)

//go:embed compute.py
var computeProgram string

// ComputeStatus samples current GPU ownership, capacity and host headroom.
// An idle GPU is not proof of training or application readiness.
func ComputeStatus(ctx context.Context, conn *grpcclient.AgentConnection) (map[string]any, error) {
	return execute(ctx, conn, computeProgram, []byte(`{}`), 16384, 12*time.Second)
}
