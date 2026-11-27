//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/bleprovider"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/mesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/meshsharing"
	"github.com/wendylabsinc/wendy/go/internal/agent/nanprovider"
	"github.com/wendylabsinc/wendy/go/internal/agent/services"
	"go.uber.org/zap"
)

// Recover kernel state before admitting a new carrier lifetime. Keep this
// boundary separate so lifecycle/status tests do not inspect the host firewall.
var recoverNetCarrierState = func(ctx context.Context) error {
	return errors.Join(meshsharing.CleanupStale(ctx), localmesh.CleanupStaleResolver(ctx))
}

// runConfiguredNetCarriers runs one Babel node for every enabled carrier.
// Its observer is cleared only after the radio provider has drained its links.
func runConfiguredNetCarriers(ctx context.Context, configDir string, id localmesh.TCPIdentity, logger *zap.Logger, catalog *meshCatalogManager, vips *mesh.VIPTable, sharing *meshSharingManager, observe func(func() localmesh.NodeSnapshot), agent ...func(context.Context, net.Conn) error) (runErr error) {
	if err := sharing.Deactivate(); err != nil {
		return err
	}
	if err := recoverNetCarrierState(ctx); err != nil {
		err = fmt.Errorf("recover Wendy Net state: %w", err)
		sharing.carriers.begin("", nil)
		sharing.setState(services.LocalMeshRuntimeStatus{Available: true, State: "failed", Detail: err.Error()})
		return err
	}
	participating, err := runWhileMeshParticipating(ctx, filepath.Join(configDir, "nan-mesh.json"), 2*time.Second, func(active context.Context) error {
		return runEnabledMeshCarriers(active, configDir, id, logger, catalog, vips, sharing, observe, agent...)
	})
	if !participating {
		// The active runner has drained providers, sharing, catalog and Babel
		// before this reports disabled. Keep saved carrier choices for re-enable.
		sharing.carriers.begin("", nil)
		state := services.LocalMeshRuntimeStatus{Available: true, State: "disabled", Detail: "Wendy Net participation is disabled."}
		if err != nil {
			state.State, state.Detail = "failed", err.Error()
		}
		sharing.setState(state)
	}
	return err
}

func runEnabledMeshCarriers(ctx context.Context, configDir string, id localmesh.TCPIdentity, logger *zap.Logger, catalog *meshCatalogManager, vips *mesh.VIPTable, sharing *meshSharingManager, observe func(func() localmesh.NodeSnapshot), agent ...func(context.Context, net.Conn) error) (runErr error) {
	path := filepath.Join(configDir, "local-mesh.json")
	initial, err := localmesh.ReadTCPConfigSnapshot(path)
	if err != nil {
		epoch := sharing.carriers.begin("unreadable configuration", []string{"local-net"})
		sharing.carriers.fail(epoch, err)
		return err
	}
	if initial == nil {
		sharing.carriers.begin("", nil)
		return nil
	}
	var cfg localmesh.TCPConfig
	err = json.Unmarshal(initial, &cfg)
	var names []string
	if err == nil {
		if cfg.Listen != "" || len(cfg.Peers) != 0 {
			names = append(names, "tcp")
		}
		if cfg.TLSListen != "" {
			names = append(names, "tls")
		}
		if cfg.NAN {
			names = append(names, "nan")
		}

		if cfg.BLE {
			names = append(names, "ble")
		}
		names = append(names, lanCarrierNames(&cfg)...)
	}
	if len(names) == 0 {
		// A present document without enabled carriers still needs a status
		// slot: identity or strict config validation can fail below.
		names = []string{"local-net"}
	}
	epoch := sharing.carriers.begin(string(initial), names)
	if err != nil {
		sharing.carriers.fail(epoch, err)
		return err
	}
	credentials, err := id.MeshCredentials()
	if err != nil {
		sharing.carriers.fail(epoch, err)
		return err
	}
	parsed, err := localmesh.ParseTCPConfig(initial, credentials.Device)
	if err != nil {
		sharing.carriers.fail(epoch, err)
		return err
	}
	if parsed == nil {
		sharing.carriers.end(epoch)
		return nil
	}
	id.PreparedCredentials = credentials
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	defer func() {
		if ctx.Err() != nil || errors.Is(runErr, context.Canceled) || runErr == nil {
			sharing.carriers.end(epoch)
		} else {
			sharing.carriers.fail(epoch, runErr)
		}
	}()
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
				current, err := localmesh.ReadTCPConfigSnapshot(path)
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
	defer func() { runErr = errors.Join(runErr, sharing.Deactivate()) }()
	return localmesh.RunTCPConfigWithNode(runCtx, configDir, id, parsed, func(node *localmesh.Node, cfg *localmesh.TCPConfig) {
		if node == nil {
			// The final defer retries and propagates any retained sharing
			// cleanup failure after all carriers and the node have drained.
			_ = sharing.Deactivate()
			catalog.Deactivate()
			if stopProviders != nil {
				stopProviders()
				providersDone.Wait()
			}
			observe(nil)
			return
		}
		if len(agent) != 0 {
			node.SetOperatorAgent(agent[0])
		}
		observe(node.Snapshot)
		if cfg.Listen != "" || len(cfg.Peers) != 0 {
			sharing.carriers.report(epoch, "tcp", localmesh.CarrierStatus{Ready: true})
		}
		if cfg.TLSListen != "" {
			sharing.carriers.report(epoch, "tls", localmesh.CarrierStatus{Ready: true})
		}
		if err := catalog.Activate(runCtx, node.Credentials, node.Snapshot, vips, cfg.Ethernet, cfg.InfrastructureWiFi, node.ControlHub()); err != nil {
			logger.Error("net service catalog unavailable", zap.Error(err))
		}
		sharing.Activate(runCtx, node)
		if !cfg.NAN && !cfg.BLE && !cfg.Ethernet && !cfg.InfrastructureWiFi {
			return
		}
		providerCtx, cancel := context.WithCancel(runCtx)
		stopProviders = cancel
		selection := localmesh.NewPeerSelection(node.Snapshot)
		start := func(names []string, run func(context.Context, func(localmesh.CarrierStatus)) error) {
			providersDone.Add(1)
			go func() {
				defer providersDone.Done()
				runMeshCarrier(providerCtx, 5*time.Second, func(status localmesh.CarrierStatus) {
					for _, name := range names {
						sharing.carriers.report(epoch, name, status)
					}
					if status.Err != nil {
						logger.Warn("Wendy Net carrier unavailable", zap.String("carrier", strings.Join(names, ", ")), zap.Error(status.Err))
					}
				}, run)
			}()
		}
		if cfg.NAN {
			provider := nanprovider.Provider{Credentials: node.Credentials, Node: node, Selection: selection, Logger: logger}
			start([]string{"nan"}, func(ctx context.Context, status func(localmesh.CarrierStatus)) error {
				provider.Status = status
				return provider.Run(ctx)
			})
		}
		// The BLE runtime owns the physical adapter setup and connection limits.
		blePolicy := localmesh.NewConnectionPolicy(node.Credentials.Device, false, localmesh.ConnectionLimits{Outgoing: localmesh.BLEMaxOutgoing, Attempts: localmesh.BLEMaxOutgoingAttempts, Total: localmesh.BLEMaxConnections})
		bleCandidates := &localmesh.CandidateQueue{}
		blePacer := &bleprovider.DialPacer{}

		if cfg.BLE {
			ble := bleprovider.Config{Pacer: blePacer, Policy: blePolicy, Candidates: bleCandidates, Credentials: node.Credentials, Node: node, Selection: selection, NetworkName: fmt.Sprintf("tenant:%s:default", node.Credentials.Device.Tenant()), Logger: logger}
			start([]string{"ble"}, func(ctx context.Context, status func(localmesh.CarrierStatus)) error {
				ble.Status = status
				return bleprovider.Run(ctx, ble)
			})
		}
		if names := lanCarrierNames(cfg); len(names) != 0 {
			lan := localmesh.LANConfig{Credentials: node.Credentials, Node: node, Ethernet: cfg.Ethernet, InfrastructureWiFi: cfg.InfrastructureWiFi, Selection: selection, Logger: logger}
			lan.TransportStatus = func(name string, status localmesh.CarrierStatus) {
				sharing.carriers.report(epoch, name, status)
			}
			start(names, func(ctx context.Context, _ func(localmesh.CarrierStatus)) error {
				return localmesh.RunLAN(ctx, lan)
			})
		}
	})
}

func lanCarrierNames(cfg *localmesh.TCPConfig) []string {
	var names []string
	if cfg.Ethernet {
		names = append(names, localmesh.CarrierEthernet)
	}
	if cfg.InfrastructureWiFi {
		names = append(names, localmesh.CarrierInfrastructureWiFi)
	}
	return names
}
