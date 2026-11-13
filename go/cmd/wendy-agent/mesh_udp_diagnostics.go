package main

import (
	"context"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/udpmessage"
	"go.uber.org/zap"
)

// Normal journal diagnostics expose only fixed aggregate fields. They provide
// hardware evidence without logging peers, app addresses or message contents.
func logUDPMessageState(ctx context.Context, logger *zap.Logger, manager *udpmessage.Manager) {
	epoch := time.Now().UTC().Format(time.RFC3339Nano)
	logger.Info("wendy net: UDP message limits", zap.String("manager_epoch", epoch), zap.Int("protocol_version", 2), zap.Int("message_max", udpmessage.MaxMessageSize), zap.Int("chunk_max", udpmessage.MaxChunkSize), zap.Int("chunk_min", udpmessage.MinChunkSize), zap.Duration("message_lifetime", udpmessage.Lifetime), zap.Int("rx_node_bytes", udpmessage.NodeBytes), zap.Int("tx_node_bytes", udpmessage.NodeBytes), zap.Int("rx_node_messages", udpmessage.NodeRXMessages), zap.Int("tx_node_messages", udpmessage.NodeTXMessages), zap.Int("node_flows", udpmessage.NodeFlows), zap.Int("node_socket_bytes", udpmessage.NodeSocketBytes))
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	previous := udpmessage.Stats{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			snapshot := manager.Snapshot()
			if snapshot != previous {
				logger.Info("wendy net: UDP message state", zap.String("manager_epoch", epoch), zap.Any("stats", snapshot))
				previous = snapshot
			}
		}
	}
}
