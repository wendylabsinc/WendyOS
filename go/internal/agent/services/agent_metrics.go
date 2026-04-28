package services

import (
	"context"
	"time"
)

// CollectAgentMetrics periodically samples the wendy-agent process's CPU and
// memory and publishes them as OTel metrics using process.* semconv names.
func CollectAgentMetrics(ctx context.Context, broadcaster *TelemetryBroadcaster) {
	resource := newAgentResource()
	startTime := time.Now()
	ticker := time.NewTicker(metricsCollectionInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case t := <-ticker.C:
			user, sys := agentCPUNanos()
			mem := agentMemBytes()
			publishProcessMetrics(broadcaster, resource, "wendy.agent",
				"process.cpu.time", "process.memory.usage",
				user, sys, mem, startTime, t)
		}
	}
}
