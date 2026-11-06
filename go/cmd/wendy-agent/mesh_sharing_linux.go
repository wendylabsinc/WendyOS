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
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"go.uber.org/zap"
)

// meshSharingManager follows the lifetime of one Babel node. The controller
// owns only policies it installed and closes before the node is torn down.
type meshSharingManager struct {
	lifecycleMu    sync.Mutex
	mu             sync.Mutex
	path           string
	logger         *zap.Logger
	catalog        *meshCatalogManager
	cancel         context.CancelFunc
	done           chan struct{}
	state          services.LocalMeshRuntimeStatus
	pendingCleanup interface{ Close() error }
	carriers       meshCarrierHealth
}

type sharingNode struct {
	node    *localmesh.Node
	catalog *meshCatalogManager
}

func (n sharingNode) Snapshot() localmesh.NodeSnapshot    { return n.node.Snapshot() }
func (n sharingNode) GatewayOffers() []meshcatalog.Record { return n.catalog.GatewayOffers() }
func (n sharingNode) SetGatewayOffer(ctx context.Context, iface string) error {
	return n.catalog.SetGatewayOffer(iface != "")
}

func newMeshSharingManager(dir string, catalog *meshCatalogManager, logger *zap.Logger) *meshSharingManager {
	return &meshSharingManager{path: filepath.Join(dir, "nan-mesh.json"), catalog: catalog, logger: logger}
}

func (m *meshSharingManager) Status() services.LocalMeshRuntimeStatus {
	m.mu.Lock()
	state := m.state
	m.mu.Unlock()
	return m.carriers.status(state)
}

func (m *meshSharingManager) setState(state services.LocalMeshRuntimeStatus) {
	m.mu.Lock()
	m.state = state
	m.mu.Unlock()
}

func (m *meshSharingManager) Activate(parent context.Context, node *localmesh.Node) {
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()
	m.mu.Lock()
	if m.done != nil {
		m.mu.Unlock()
		return
	}
	m.mu.Unlock()
	if err := m.retryCleanup(); err != nil {
		return
	}
	m.mu.Lock()
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	m.cancel, m.done = cancel, done
	m.state = services.LocalMeshRuntimeStatus{Available: true, State: "starting", Detail: "Checking Wendy Net sharing policy."}
	m.mu.Unlock()
	go func() {
		defer close(done)
		m.run(ctx, node)
	}()
}

func (m *meshSharingManager) Deactivate() error {
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()
	m.mu.Lock()
	cancel, done := m.cancel, m.done
	m.cancel, m.done = nil, nil
	m.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
	if err := m.retryCleanup(); err != nil {
		return err
	}
	m.setState(services.LocalMeshRuntimeStatus{})
	return nil
}

// closeController preserves the owner of incomplete cleanup. Dropping it
// would lose resolver and firewall state that a later reset must retry.
func (m *meshSharingManager) closeController(controller interface{ Close() error }) error {
	err := controller.Close()
	m.mu.Lock()
	if err != nil {
		m.pendingCleanup = controller
		m.state = services.LocalMeshRuntimeStatus{Available: true, State: "error", Detail: err.Error()}
	} else {
		m.pendingCleanup = nil
	}
	m.mu.Unlock()
	if err != nil && m.logger != nil {
		m.logger.Warn("Wendy Net Internet sharing cleanup failed", zap.Error(err))
	}
	return err
}

func (m *meshSharingManager) retryCleanup() error {
	m.mu.Lock()
	pending := m.pendingCleanup
	m.mu.Unlock()
	if pending == nil {
		return nil
	}
	return m.closeController(pending)
}

func (m *meshSharingManager) run(ctx context.Context, node *localmesh.Node) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	var controller *meshsharing.Controller
	closing := false
	defer func() {
		if controller != nil {
			_ = m.closeController(controller)
		}
	}()
	for {
		config, loadErr := meshsharing.LoadConfig(m.path)
		if loadErr != nil {
			m.logger.Warn("invalid Wendy Net sharing policy", zap.Error(loadErr))
			config = meshsharing.Config{}
		}
		if closing {
			if err := m.closeController(controller); err == nil {
				controller, closing = nil, false
			}
		}
		if config.Participate && controller == nil {
			var err error
			controller, err = meshsharing.NewLinux(ctx, node.Credentials, sharingNode{node, m.catalog})
			if err != nil {
				if controller != nil {
					if cleanupErr := m.closeController(controller); cleanupErr != nil {
						closing = true
						err = errors.Join(err, cleanupErr)
					} else {
						controller = nil
					}
				}
				m.logger.Warn("Wendy Net sharing unavailable", zap.Error(err))
				m.setState(services.LocalMeshRuntimeStatus{Available: true, State: "error", Detail: err.Error(),
					AuthenticatedPeers: node.Snapshot().Peers})
			}
		}
		if controller != nil && !closing {
			controller.SetLogger(m.logger)
			stepCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			decision, stepErr := controller.Step(stepCtx, config)
			cancel()
			if stepErr != nil && !errors.Is(stepErr, context.Canceled) {
				m.logger.Warn("Wendy Net Internet sharing reconciliation failed", zap.Error(stepErr))
			}
			state := services.LocalMeshRuntimeStatus{Available: true, State: decision.Mode, Detail: decision.Reason,
				AuthenticatedPeers: node.Snapshot().Peers}
			if decision.GatewayDevice != (meshid.Device{}) {
				state.GatewayPrincipal = decision.GatewayDevice.Principal()
			}
			if stepErr != nil {
				state.State, state.Detail = "error", stepErr.Error()
			} else if loadErr != nil {
				state.State, state.Detail = "error", loadErr.Error()
			}
			m.setState(state)
			if !config.Participate {
				if closeErr := m.closeController(controller); closeErr != nil {
					closing = true
				} else {
					controller = nil
				}
			}
		} else if !config.Participate && !closing {
			state := services.LocalMeshRuntimeStatus{Available: true, State: "mesh-active", Detail: "Wendy Net transport is active; Internet sharing is disabled.",
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
