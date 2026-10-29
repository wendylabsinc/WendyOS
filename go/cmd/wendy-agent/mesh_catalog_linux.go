//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/hostnetwork"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/mesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/meshcatalog"
	"github.com/wendylabsinc/wendy/go/internal/agent/meshingress"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	"go.uber.org/zap"
)

type catalogApp struct {
	name, appID, ip, bridge, netnsPath, gateway string
	namespace                                   *os.File
	ports                                       map[uint16]uint16
	udpPorts                                    map[uint16]uint16
}

type catalogBridgeRun struct {
	cancel context.CancelFunc
	done   chan struct{}
}

type catalogActivation struct {
	device         meshid.Device
	vips           *mesh.VIPTable
	ctx            context.Context
	cancel         context.CancelFunc
	catalog        *meshcatalog.Catalog
	lanBrowser     *meshcatalog.LANBrowser
	lanDone        chan struct{}
	snapshot       func() localmesh.NodeSnapshot
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
	lanReady       bool
}

func newMeshCatalogManager(dir string, ingress *meshingress.Registry, logger *zap.Logger) *meshCatalogManager {
	return &meshCatalogManager{dir: dir, ingress: ingress, logger: logger, apps: map[string]catalogApp{}, lanReady: true}
}

func (m *meshCatalogManager) DisablePhysicalLANProjection() {
	m.mu.Lock()
	m.lanReady = false
	m.mu.Unlock()
}

func (m *meshCatalogManager) Activate(ctx context.Context, credentials *localmesh.Credentials, snapshot func() localmesh.NodeSnapshot, vips *mesh.VIPTable, ethernet, wifi bool, hub *localmesh.ControlHub) error {
	if credentials == nil || snapshot == nil || hub == nil {
		return errors.New("wendy net catalog requires a running node")
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
	catalog, err := meshcatalog.NewManifestCatalog("default", credentials.Device, authorize, hub.ReplaceServices, snapshot)
	if err != nil {
		return err
	}
	activeCtx, cancel := context.WithCancel(ctx)
	a := &catalogActivation{device: credentials.Device, vips: vips, ctx: activeCtx, cancel: cancel, catalog: catalog, snapshot: snapshot, done: make(chan struct{}), retryDone: make(chan struct{}), gatewayDone: make(chan struct{}), bridges: map[string]catalogBridgeRun{}}
	m.mu.Lock()
	lanReady := m.lanReady
	m.mu.Unlock()
	if (ethernet || wifi) && lanReady {
		a.lanBrowser = meshcatalog.NewLANBrowser(ethernet, wifi)
		a.lanDone = make(chan struct{})
	}
	m.mu.Lock()
	if m.active != nil {
		m.mu.Unlock()
		cancel()
		return errors.New("wendy net catalog already active")
	}
	m.active = a
	if a.lanBrowser != nil {
		go func() {
			defer close(a.lanDone)
			for activeCtx.Err() == nil {
				if err := a.lanBrowser.Run(activeCtx); err != nil && activeCtx.Err() == nil {
					m.logger.Warn("physical LAN DNS-SD browser stopped", zap.Error(err))
				}
				select {
				case <-activeCtx.Done():
					return
				case <-time.After(5 * time.Second):
				}
			}
		}()
	}

	for _, app := range m.apps {
		if err := m.startBridge(a, app); err != nil {
			m.logger.Warn("wendy net app mDNS bridge unavailable", zap.String("app_id", app.appID), zap.Error(err))
		}
	}
	m.mu.Unlock()
	go func() { defer close(a.done); <-activeCtx.Done() }()
	go m.retryBridges(a)
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
				m.logger.Debug("wendy net app mDNS bridge retry failed", zap.String("app_id", app.appID), zap.Error(err))
			}
		}
		m.mu.Unlock()
	}
}

func (m *meshCatalogManager) Deactivate() {
	m.mu.Lock()
	a := m.active
	m.active = nil
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
	if a.lanDone != nil {
		<-a.lanDone
	}
	<-a.retryDone
	<-a.done
}

// GatewayOffers returns the verified signed gateway offers this mesh sees,
// and SetGatewayOffer publishes or withdraws this host's own offer. The
// sharing controller drives both through the node adapter.
func (m *meshCatalogManager) GatewayOffers() []meshcatalog.Record {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active == nil {
		return nil
	}
	return m.active.catalog.GatewayOffers(time.Now())
}

func (m *meshCatalogManager) SetGatewayOffer(enable bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active == nil {
		// The old catalogue and its signed offers are already detached. A
		// pending sharing cleanup must be able to finish after node shutdown;
		// otherwise it prevents every subsequent carrier activation.
		if !enable {
			return nil
		}
		return errors.New("wendy net catalog is not active")
	}
	if enable {
		_, err := m.active.catalog.PublishGatewayOffer(time.Now())
		return err
	}
	_, err := m.active.catalog.WithdrawGatewayOffer(time.Now())
	return err
}

func (m *meshCatalogManager) StartMeshApp(containerName, appID, ip, bridge, netnsPath, gateway string, ports []appconfig.PortMapping) error {
	address := net.ParseIP(ip)
	if containerName == "" || appID == "" || address == nil || address.To4() == nil || bridge == "" || netnsPath == "" || net.ParseIP(gateway) == nil {
		return errors.New("invalid wendy net app network for mDNS")
	}
	mapped := make(map[uint16]uint16, len(ports))
	udpMapped := make(map[uint16]uint16, len(ports))
	for _, port := range ports {
		target := mapped
		if port.Protocol == "udp" {
			target = udpMapped
		}
		if port.Container == 0 || port.Host == 0 || target[port.Container] != 0 {
			return errors.New("invalid or duplicate wendy net app mDNS port")
		}
		target[port.Container] = port.Host
	}
	// Pin the task namespace before retaining it for delayed LAN discovery.
	// A PID path can name an unrelated task after the original task exits.
	namespace, err := os.Open(netnsPath)
	if err != nil {
		return fmt.Errorf("pin wendy net app namespace: %w", err)
	}
	pinnedPath := fmt.Sprintf("/proc/%d/fd/%d", os.Getpid(), namespace.Fd())
	app := catalogApp{namespace: namespace, name: containerName, appID: appID, ip: ip, bridge: bridge, netnsPath: pinnedPath, gateway: gateway, ports: mapped, udpPorts: udpMapped}
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
	app := m.apps[containerName]
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
	if app.namespace != nil {
		_ = app.namespace.Close()
	}
}

// startBridge is called under m.mu. A bridge binds only its app's CNI bridge
// and accepts announcements only from that app's container IP.
func (m *meshCatalogManager) startBridge(a *catalogActivation, app catalogApp) error {
	if a.lanBrowser != nil {
		if err := hostnetwork.RevokeOrphanLANAppRoutes(app.netnsPath, app.ip, app.gateway); err != nil {
			return fmt.Errorf("revoke inherited LAN service routes: %w", err)
		}
	}
	iface, err := net.InterfaceByName(app.bridge)
	if err != nil {
		return err
	}
	scope := meshcatalog.AppScope{AppID: app.appID, AppIP: net.ParseIP(app.ip), BridgeIndex: iface.Index, Ports: app.ports, UDPPorts: app.udpPorts}
	policy := func(_ string, record meshcatalog.Record) bool {
		return record.Key.Mesh == "default" &&
			(record.Key.Device != a.device.Principal() || record.Key.AppID != app.appID)
	}
	bridge, err := meshcatalog.NewMDNSBridge(scope, a.catalog, a.vips, policy, func(meshcatalog.Record) {}, a.snapshot)
	if err != nil {
		return err
	}
	var lanAccess *meshcatalog.LANProjector
	var bridgeNamespace *os.File
	if a.lanBrowser != nil {
		// Deactivate detaches the activation before waiting for old bridges.
		// StopMeshApp may then release the app's descriptor concurrently, so
		// this bridge owns a separate descriptor through its final cleanup.
		bridgeNamespace, err = os.Open(app.netnsPath)
		if err != nil {
			return fmt.Errorf("pin LAN projection namespace: %w", err)
		}
		path := fmt.Sprintf("/proc/%d/fd/%d", os.Getpid(), bridgeNamespace.Fd())
		lanAccess = meshcatalog.NewLANProjector(path, app.ip, app.gateway, app.bridge)
		lanAccess.SetErrorHandler(func(err error) {
			m.logger.Warn("net app physical-LAN projection denied", zap.String("app_id", app.appID), zap.Error(err))
		})
		bridge.SetLANProjection(a.lanBrowser.Snapshot, lanAccess)
	}
	ctx, cancel := context.WithCancel(a.ctx)
	done := make(chan struct{})
	a.bridges[app.name] = catalogBridgeRun{cancel, done}
	go func() {
		defer close(done)
		if bridgeNamespace != nil {
			defer bridgeNamespace.Close()
			defer lanAccess.Close()
		}
		if err := bridge.Run(ctx); err != nil && ctx.Err() == nil {
			m.logger.Warn("wendy net app mDNS bridge stopped", zap.String("app_id", app.appID), zap.Error(err))
		}
	}()
	return nil
}

// SensorSources projects only live authenticated native SensorLink endpoints.
func (m *meshCatalogManager) SensorSources() []models.LANDevice {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active == nil || m.active.ctx.Err() != nil {
		return nil
	}
	return meshSensorSources(m.active.snapshot())
}
