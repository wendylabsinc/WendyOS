//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/bleprovider"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/nanprovider"
	"go.uber.org/zap"
)

// runConfiguredMeshCarriers runs one Babel node for every enabled carrier.
// Its observer is cleared only after the radio provider has drained its links.
func runConfiguredMeshCarriers(ctx context.Context, configDir string, id localmesh.TCPIdentity, logger *zap.Logger, observe func(func() localmesh.NodeSnapshot)) error {
	var stopProviders context.CancelFunc
	var providersDone sync.WaitGroup
	return localmesh.RunConfiguredWithNode(ctx, configDir, id, func(node *localmesh.Node, cfg *localmesh.TCPConfig) {
		if node == nil {
			if stopProviders != nil {
				stopProviders()
				providersDone.Wait()
			}
			observe(nil)
			return
		}
		observe(node.Snapshot)
		if !cfg.NAN && !cfg.BLE {
			return
		}
		providerCtx, cancel := context.WithCancel(ctx)
		stopProviders = cancel
		start := func(name string, run func(context.Context) error) {
			providersDone.Add(1)
			go func() {
				defer providersDone.Done()
				for providerCtx.Err() == nil {
					if err := run(providerCtx); err != nil && !errors.Is(err, context.Canceled) && providerCtx.Err() == nil {
						logger.Warn(name+" carrier stopped", zap.Error(err))
					}
					select {
					case <-providerCtx.Done():
						return
					case <-time.After(5 * time.Second):
					}
				}
			}()
		}
		if cfg.NAN {
			provider := nanprovider.Provider{Credentials: node.Credentials, Node: node, Logger: logger}
			start("NAN", provider.Run)
		}
		if cfg.BLE {
			ble := bleprovider.Config{Credentials: node.Credentials, Node: node, MeshName: fmt.Sprintf("org:%d:default", id.Org), Logger: logger}
			start("BLE", func(ctx context.Context) error { return bleprovider.Run(ctx, ble) })
		}
	})
}
