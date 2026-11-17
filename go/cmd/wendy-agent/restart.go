package main

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/go/internal/agent/services"
)

// newAgentRestart is how the agent restarts itself after an update or a
// renewed certificate. Nothing asks the agent to stop then: it exits on its
// own and systemd starts it again, so the shutdown at the end of main never
// runs (see agentShutdown for that one).
//
// Wendy Net is the one thing stopped first. Its link, TUNs, policy rules and
// routes outlive the process. The next agent reclaims them only when it is a
// build that knows them; an older agent installed as a rollback, or one
// side-loaded from another branch, is left with kernel state it cannot
// explain. Everything else either goes with the process or is meant to outlive
// it: apps keep running across an agent restart, so nothing here stops them.
func newAgentRestart(logger *zap.Logger, mesh *meshLifetime) *services.AgentRestart {
	return &services.AgentRestart{
		Logger: logger,
		Stop: func(ctx context.Context) error {
			// Stop even a mesh that never started, so that an enrollment
			// finishing right now cannot start one behind the exit.
			started := mesh.Started()
			start := time.Now()
			if err := mesh.Stop(ctx); err != nil {
				return fmt.Errorf("stopping Wendy Net: %w", err)
			}
			if started {
				logger.Info("Stopped Wendy Net before the agent restart", zap.Duration("took", time.Since(start)))
			}
			return nil
		},
	}
}
