//go:build linux

package main

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/nanprovider"
	"go.uber.org/zap"
)

// runConfiguredMeshCarriers runs one Babel node for every enabled carrier.
// Its observer is cleared only after the radio provider has drained its links.
func runConfiguredMeshCarriers(ctx context.Context, configDir string, id localmesh.TCPIdentity, logger *zap.Logger, observe func(func() localmesh.NodeSnapshot)) error {
	var stopNAN context.CancelFunc
	var nanDone sync.WaitGroup
	return localmesh.RunConfiguredWithNode(ctx, configDir, id, func(node *localmesh.Node, cfg *localmesh.TCPConfig) {
		if node == nil {
			if stopNAN != nil {
				stopNAN()
				nanDone.Wait()
			}
			observe(nil)
			return
		}
		observe(node.Snapshot)
		if !cfg.NAN {
			return
		}
		nanCtx, cancel := context.WithCancel(ctx)
		stopNAN = cancel
		nanDone.Add(1)
		go func() {
			defer nanDone.Done()
			provider := nanprovider.Provider{Credentials: node.Credentials, Node: node, Logger: logger}
			for nanCtx.Err() == nil {
				if err := provider.Run(nanCtx); err != nil && !errors.Is(err, context.Canceled) && nanCtx.Err() == nil {
					logger.Warn("NAN carrier stopped", zap.Error(err))
				}
				select {
				case <-nanCtx.Done():
					return
				case <-time.After(5 * time.Second):
				}
			}
		}()
	})
}
