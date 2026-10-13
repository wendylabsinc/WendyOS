//go:build linux

package main

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/meshcatalog"
	"github.com/wendylabsinc/wendy/go/internal/agent/meshsharing"
	"github.com/wendylabsinc/wendy/go/internal/agent/services"
	"go.uber.org/zap"
)

// meshSharingManager follows the lifetime of one Babel node. The controller
// owns only policies it installed and closes before the node is torn down.
type meshSharingManager struct {
	mu      sync.Mutex
	path    string
	logger  *zap.Logger
	catalog *meshCatalogManager
	cancel  context.CancelFunc
	done    chan struct{}
	state   services.LocalMeshRuntimeStatus
}

type sharingNode struct {
	node    *localmesh.Node
	catalog *meshCatalogManager
}

func (n sharingNode) Snapshot() localmesh.NodeSnapshot    { return n.node.Snapshot() }
func (n sharingNode) GatewayOffers() []meshcatalog.Record { return n.catalog.GatewayOffers() }
func (n sharingNode) SetRoaming(ctx context.Context, enabled bool) error {
	return n.node.SetRoaming(ctx, enabled)
}
func (n sharingNode) SetUplink(ctx context.Context, iface string) error {
	if iface == "" {
		// Withdraw routing first, then the catalog capability. Either step is
		// independently required by clients, so a failure remains fail closed.
		return errors.Join(n.node.SetUplink(ctx, ""), n.catalog.SetGatewayOffer(false))
	}
	if err := n.catalog.SetGatewayOffer(true); err != nil {
		return err
	}
	if err := n.node.SetUplink(ctx, iface); err != nil {
		return errors.Join(err, n.catalog.SetGatewayOffer(false))
	}
	return nil
}

func newMeshSharingManager(dir string, catalog *meshCatalogManager, logger *zap.Logger) *meshSharingManager {
	return &meshSharingManager{path: filepath.Join(dir, "nan-mesh.json"), catalog: catalog, logger: logger}
}

func (m *meshSharingManager) Status() services.LocalMeshRuntimeStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

func (m *meshSharingManager) setState(state services.LocalMeshRuntimeStatus) {
	m.mu.Lock()
	m.state = state
	m.mu.Unlock()
}

func (m *meshSharingManager) Activate(parent context.Context, node *localmesh.Node) {
	m.mu.Lock()
	if m.done != nil {
		m.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	m.cancel, m.done = cancel, done
	m.state = services.LocalMeshRuntimeStatus{Available: true, State: "starting", Detail: "Checking local mesh sharing policy."}
	m.mu.Unlock()
	go func() {
		defer close(done)
		m.run(ctx, node)
	}()
}

func (m *meshSharingManager) Deactivate() {
	m.mu.Lock()
	cancel, done := m.cancel, m.done
	m.cancel, m.done = nil, nil
	m.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
	m.setState(services.LocalMeshRuntimeStatus{})
}

func (m *meshSharingManager) run(ctx context.Context, node *localmesh.Node) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	var controller *meshsharing.Controller
	defer func() {
		if controller != nil {
			if err := controller.Close(); err != nil {
				m.logger.Warn("mesh sharing cleanup failed", zap.Error(err))
			}
		}
	}()
	for {
		config, loadErr := meshsharing.LoadConfig(m.path)
		if loadErr != nil {
			m.logger.Warn("invalid local mesh sharing policy", zap.Error(loadErr))
			config = meshsharing.Config{}
		}
		if config.Participate && controller == nil {
			var err error
			controller, err = meshsharing.NewLinux(ctx, node.Credentials.Org, node.Credentials.Asset, sharingNode{node, m.catalog})
			if err != nil {
				m.logger.Warn("local mesh sharing unavailable", zap.Error(err))
				m.setState(services.LocalMeshRuntimeStatus{Available: true, State: "error", Detail: err.Error(),
					AuthenticatedPeers: node.Snapshot().Peers})
			}
		}
		if controller != nil {
			stepCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			decision, stepErr := controller.Step(stepCtx, config)
			cancel()
			if stepErr != nil && !errors.Is(stepErr, context.Canceled) {
				m.logger.Warn("mesh sharing reconciliation failed", zap.Error(stepErr))
			}
			state := services.LocalMeshRuntimeStatus{Available: true, State: decision.Mode, Detail: decision.Reason,
				AuthenticatedPeers: node.Snapshot().Peers, GatewayAsset: decision.GatewayAsset}
			if stepErr != nil {
				state.State, state.Detail = "error", stepErr.Error()
			} else if loadErr != nil {
				state.State, state.Detail = "error", loadErr.Error()
			}
			m.setState(state)
			if !config.Participate {
				if closeErr := controller.Close(); closeErr != nil {
					m.logger.Warn("mesh sharing cleanup failed", zap.Error(closeErr))
				}
				controller = nil
			}
		} else if !config.Participate {
			state := services.LocalMeshRuntimeStatus{Available: true, State: "mesh-active", Detail: "Local mesh transport is active; Internet sharing is disabled.",
				AuthenticatedPeers: node.Snapshot().Peers}
			if loadErr != nil {
				state.State, state.Detail = "error", loadErr.Error()
			}
			m.setState(state)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
