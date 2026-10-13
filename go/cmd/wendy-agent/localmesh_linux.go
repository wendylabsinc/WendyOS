//go:build linux

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/bleprovider"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/nanprovider"
	"go.uber.org/zap"
)

// runConfiguredMeshCarriers runs one Babel node for every enabled carrier.
// Its observer is cleared only after the radio provider has drained its links.
func runConfiguredMeshCarriers(ctx context.Context, configDir string, id localmesh.TCPIdentity, logger *zap.Logger, catalog *meshCatalogManager, sharing *meshSharingManager, observe func(func() localmesh.NodeSnapshot)) error {
	path := filepath.Join(configDir, "local-mesh.json")
	initial, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				current, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(current, initial) {
					cancelRun()
					return
				}
			}
		}
	}()
	defer func() { cancelRun(); <-watchDone }()
	var stopProviders context.CancelFunc
	var providersDone sync.WaitGroup
	return localmesh.RunConfiguredWithNode(runCtx, configDir, id, func(node *localmesh.Node, cfg *localmesh.TCPConfig) {
		if node == nil {
			sharing.Deactivate()
			catalog.Deactivate()
			if stopProviders != nil {
				stopProviders()
				providersDone.Wait()
			}
			observe(nil)
			return
		}
		observe(node.Snapshot)
		if err := node.SetGatewayOffers(runCtx, func() []int32 {
			offers := catalog.GatewayOffers()
			assets := make([]int32, 0, len(offers))
			for _, offer := range offers {
				assets = append(assets, offer.Key.Asset)
			}
			return assets
		}); err != nil {
			logger.Error("mesh route authorizer unavailable", zap.Error(err))
			return
		}
		if err := catalog.Activate(runCtx, node.Credentials, node.Snapshot, node.ReauthorizeRoutes); err != nil {
			logger.Error("mesh service catalog unavailable", zap.Error(err))
		}
		sharing.Activate(runCtx, node)
		if !cfg.NAN && !cfg.BLE && !cfg.Ethernet && !cfg.InfrastructureWiFi {
			return
		}
		providerCtx, cancel := context.WithCancel(runCtx)
		stopProviders = cancel
		selection := localmesh.NewPeerSelection(node.Snapshot)
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
			provider := nanprovider.Provider{Credentials: node.Credentials, Node: node, Selection: selection, Logger: logger}
			start("NAN", provider.Run)
		}
		if cfg.BLE {
			ble := bleprovider.Config{Credentials: node.Credentials, Node: node, Selection: selection, MeshName: fmt.Sprintf("org:%d:default", id.Org), Logger: logger}
			start("BLE", func(ctx context.Context) error { return bleprovider.Run(ctx, ble) })
		}
		if cfg.Ethernet || cfg.InfrastructureWiFi {
			lan := localmesh.LANConfig{Credentials: node.Credentials, Node: node, Ethernet: cfg.Ethernet, InfrastructureWiFi: cfg.InfrastructureWiFi, Selection: selection, Logger: logger}
			start("LAN", func(ctx context.Context) error { return localmesh.RunLAN(ctx, lan) })
		}
	})
}
