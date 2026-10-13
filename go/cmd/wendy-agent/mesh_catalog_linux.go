//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/meshcatalog"
	"github.com/wendylabsinc/wendy/go/internal/agent/meshingress"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"go.uber.org/zap"
)

type catalogApp struct {
	name, appID, ip, bridge string
	ports                   map[uint16]uint16
	udpPorts                map[uint16]uint16
}

type catalogBridgeRun struct {
	cancel context.CancelFunc
	done   chan struct{}
}

type catalogActivation struct {
	org            int32
	asset          int32
	ctx            context.Context
	cancel         context.CancelFunc
	catalog        *meshcatalog.Catalog
	runtime        *meshcatalog.Runtime
	cache          *localmesh.IdentityCache
	done           chan struct{}
	retryDone      chan struct{}
	gatewayDone    chan struct{}
	gatewayApplied bool
	notifyRoutes   func()
	bridges        map[string]catalogBridgeRun
}

type meshCatalogManager struct {
	mu             sync.Mutex
	dir            string
	ingress        *meshingress.Registry
	logger         *zap.Logger
	apps           map[string]catalogApp
	active         *catalogActivation
	gatewayDesired bool
}

func newMeshCatalogManager(dir string, ingress *meshingress.Registry, logger *zap.Logger) *meshCatalogManager {
	return &meshCatalogManager{dir: dir, ingress: ingress, logger: logger, apps: map[string]catalogApp{}}
}

func (m *meshCatalogManager) Activate(ctx context.Context, credentials *localmesh.Credentials, snapshot func() localmesh.NodeSnapshot, notifyRoutes func()) error {
	if credentials == nil || snapshot == nil {
		return errors.New("mesh catalog requires a running node")
	}
	stateDir := filepath.Join(m.dir, "local-mesh", fmt.Sprintf("catalog-%d-%d", credentials.Org, credentials.Asset))
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return err
	}
	now := time.Now()
	cache, err := localmesh.OpenIdentityCache(filepath.Join(stateDir, "identities.json"), localmesh.DefaultCacheLimits(), credentials.Verify, now)
	if err != nil {
		return err
	}
	store, err := meshcatalog.NewReceiptStore(filepath.Join(stateDir, "receipts.json"), 4096)
	if err != nil {
		return err
	}
	receipts, err := store.Load()
	if err != nil {
		return err
	}
	authorize := func(appID, serviceType string, hostPort uint16) error {
		allowed := m.ingress.AllowedApp(appID, hostPort)
		if strings.HasSuffix(serviceType, "._udp") {
			allowed = m.ingress.AllowedUDPApp(appID, hostPort)
		}
		if !allowed {
			return meshingress.ErrPortDenied
		}
		return nil
	}
	catalog, err := meshcatalog.NewCatalog("default", credentials.Org, credentials.Asset, 4096, credentials, cache, authorize, receipts, store.Save, now)
	if err != nil {
		return err
	}
	runtime, err := meshcatalog.NewRuntime(catalog, snapshot)
	if err != nil {
		return err
	}
	runtime.SetGatewayChangeNotifier(notifyRoutes)
	activeCtx, cancel := context.WithCancel(ctx)
	a := &catalogActivation{org: credentials.Org, asset: credentials.Asset, ctx: activeCtx, cancel: cancel, catalog: catalog, runtime: runtime, cache: cache, done: make(chan struct{}), retryDone: make(chan struct{}), gatewayDone: make(chan struct{}), bridges: map[string]catalogBridgeRun{}, notifyRoutes: notifyRoutes}
	m.mu.Lock()
	if m.active != nil {
		m.mu.Unlock()
		cancel()
		return errors.New("mesh catalog already active")
	}
	if err := m.applyGatewayLocked(a, now); err != nil {
		m.mu.Unlock()
		cancel()
		return fmt.Errorf("restore mesh gateway offer: %w", err)
	}
	m.active = a
	if notifyRoutes != nil {
		notifyRoutes()
	}
	for _, app := range m.apps {
		if err := m.startBridge(a, app); err != nil {
			m.logger.Warn("mesh app mDNS bridge unavailable", zap.String("app_id", app.appID), zap.Error(err))
		}
	}
	m.mu.Unlock()
	go func() {
		defer close(a.done)
		if err := runtime.Run(activeCtx); err != nil && activeCtx.Err() == nil {
			m.logger.Error("mesh catalog synchronization stopped", zap.Error(err))
		}
	}()
	go m.retryBridges(a)
	go m.renewGateway(a)
	return nil
}

// retryBridges recovers both startup races (the CNI bridge is not present yet)
// and a bridge socket that exits after startup. The app remains registered
// until its container stops; every replacement starts with fresh observation
// state and the old bridge withdraws its signed publications before exiting.
func (m *meshCatalogManager) retryBridges(a *catalogActivation) {
	defer close(a.retryDone)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-a.ctx.Done():
			return
		case <-ticker.C:
		}
		m.mu.Lock()
		if m.active != a {
			m.mu.Unlock()
			return
		}
		for _, app := range m.apps {
			if run, exists := a.bridges[app.name]; exists {
				select {
				case <-run.done:
					delete(a.bridges, app.name)
				default:
					continue
				}
			}
			if err := m.startBridge(a, app); err != nil {
				m.logger.Debug("mesh app mDNS bridge retry failed", zap.String("app_id", app.appID), zap.Error(err))
			}
		}
		m.mu.Unlock()
	}
}

func (m *meshCatalogManager) Deactivate() {
	m.mu.Lock()
	a := m.active
	m.active = nil
	if a != nil && a.notifyRoutes != nil {
		a.notifyRoutes()
	}
	if a == nil {
		m.mu.Unlock()
		return
	}
	bridges := a.bridges
	a.bridges = map[string]catalogBridgeRun{}
	m.mu.Unlock()
	for _, bridge := range bridges {
		bridge.cancel()
	}
	for _, bridge := range bridges {
		<-bridge.done
	}
	a.cancel()
	<-a.retryDone
	<-a.done
	<-a.gatewayDone
	if err := a.cache.Flush(time.Now()); err != nil {
		m.logger.Warn("mesh catalog identity cache flush failed", zap.Error(err))
	}
}

// SetGatewayOffer publishes or withdraws the agent's signed, endpoint-free
// gateway capability. The desired state survives catalog reactivation.
func (m *meshCatalogManager) SetGatewayOffer(enabled bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active == nil {
		if enabled {
			return errors.New("mesh catalog is inactive; gateway offer cannot be signed")
		}
		m.gatewayDesired = false
		return nil
	}
	changed := m.gatewayDesired != enabled
	m.gatewayDesired = enabled
	if !changed && m.active.gatewayApplied {
		return nil
	}
	return m.applyGatewayLocked(m.active, time.Now())
}

// GatewayOffers returns only verified, live gateway capabilities. Callers
// must separately check that the origin is reachable and policy permits use.
func (m *meshCatalogManager) GatewayOffers() []meshcatalog.Record {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active == nil {
		return nil
	}
	return m.active.catalog.GatewayOffers(time.Now())
}

// applyGatewayLocked serializes local generations with activation and
// shutdown. It may persist a receipt and must be called while holding m.mu.
func (m *meshCatalogManager) applyGatewayLocked(a *catalogActivation, now time.Time) error {
	var w meshcatalog.SignedRecord
	var err error
	if m.gatewayDesired {
		w, err = a.catalog.PublishGatewayOffer(now)
	} else {
		w, err = a.catalog.WithdrawGatewayOffer(now)
	}
	if err != nil {
		a.gatewayApplied = false
		return err
	}
	a.gatewayApplied = true
	if len(w.Body) != 0 {
		a.runtime.Broadcast(w)
		if a.notifyRoutes != nil {
			a.notifyRoutes()
		}
	}
	return nil
}

func (m *meshCatalogManager) renewGateway(a *catalogActivation) {
	defer close(a.gatewayDone)
	ticker := time.NewTicker(meshcatalog.GatewayOfferLease / 2)
	defer ticker.Stop()
	for {
		select {
		case <-a.ctx.Done():
			return
		case <-ticker.C:
		}
		m.mu.Lock()
		if m.active != a || a.ctx.Err() != nil {
			m.mu.Unlock()
			return
		}
		if m.gatewayDesired || !a.gatewayApplied {
			if err := m.applyGatewayLocked(a, time.Now()); err != nil {
				m.logger.Warn("mesh gateway offer refresh failed", zap.Error(err))
			}
		}
		m.mu.Unlock()
	}
}

func (m *meshCatalogManager) StartMeshApp(containerName, appID, ip, bridge string, ports []appconfig.PortMapping) error {
	address := net.ParseIP(ip)
	if containerName == "" || appID == "" || address == nil || address.To4() == nil || bridge == "" {
		return errors.New("invalid mesh app network for mDNS")
	}
	mapped := make(map[uint16]uint16, len(ports))
	udpMapped := make(map[uint16]uint16, len(ports))
	for _, port := range ports {
		target := mapped
		if port.Protocol == "udp" {
			target = udpMapped
		}
		if port.Container == 0 || port.Host == 0 || target[port.Container] != 0 {
			return errors.New("invalid or duplicate mesh app mDNS port")
		}
		target[port.Container] = port.Host
	}
	app := catalogApp{name: containerName, appID: appID, ip: ip, bridge: bridge, ports: mapped, udpPorts: udpMapped}
	m.StopMeshApp(containerName)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.apps[containerName] = app
	if m.active != nil {
		return m.startBridge(m.active, app)
	}
	return nil
}

func (m *meshCatalogManager) StopMeshApp(containerName string) {
	m.mu.Lock()
	delete(m.apps, containerName)
	var bridge catalogBridgeRun
	if m.active != nil {
		bridge = m.active.bridges[containerName]
		delete(m.active.bridges, containerName)
	}
	m.mu.Unlock()
	if bridge.cancel != nil {
		bridge.cancel()
		<-bridge.done
	}
}

// startBridge is called under m.mu. A bridge binds only its app's CNI bridge
// and accepts announcements only from that app's container IP.
func (m *meshCatalogManager) startBridge(a *catalogActivation, app catalogApp) error {
	iface, err := net.InterfaceByName(app.bridge)
	if err != nil {
		return err
	}
	scope := meshcatalog.AppScope{AppID: app.appID, AppIP: net.ParseIP(app.ip), BridgeIndex: iface.Index, Ports: app.ports, UDPPorts: app.udpPorts}
	policy := func(_ string, record meshcatalog.Record) bool {
		return record.Key.Org == a.org && record.Key.Mesh == "default" &&
			(record.Key.Asset != a.asset || record.Key.AppID != app.appID)
	}
	bridge, err := meshcatalog.NewMDNSBridge(scope, a.catalog, policy, a.runtime.Broadcast)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(a.ctx)
	done := make(chan struct{})
	a.bridges[app.name] = catalogBridgeRun{cancel, done}
	go func() {
		defer close(done)
		if err := bridge.Run(ctx); err != nil && ctx.Err() == nil {
			m.logger.Warn("mesh app mDNS bridge stopped", zap.String("app_id", app.appID), zap.Error(err))
		}
	}()
	return nil
}
