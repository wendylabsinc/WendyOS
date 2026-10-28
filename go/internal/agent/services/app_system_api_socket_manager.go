package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/agent/data/live"
	"github.com/wendylabsinc/wendy/go/internal/agent/localsocket"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	systempb "github.com/wendylabsinc/wendy/go/proto/gen/systempb"
)

const (
	SystemAPICapabilityNotifications = "notifications"
	// SystemAPICapabilityDataRead serves DataService.StreamData, limited to the
	// app's data-read allowlist. It reads live records only: no history, no
	// recorded data, and no other DataService method.
	SystemAPICapabilityDataRead = "data-read"
	SystemAPISocketFilename     = "system.sock"
	appSystemAPIGroupGID        = 2000
)

// SystemAPIGrant is what one entitled service of an app may use on the app's
// System API socket.
type SystemAPIGrant struct {
	Capabilities []string
	// DataReadAllowlist narrows the data-read capability to these stream ids
	// and stream id prefixes; see live.Permits. Empty reads every stream.
	DataReadAllowlist []string
	// Camera is whether the service holds the camera entitlement (or its
	// deprecated alias, video) beside data-read. The agent's camera streams
	// (camera_grid.go) are pictures of what a camera sees, so data-read admits
	// them only with it, and only for the device paths in CameraAllowlist,
	// the camera entitlement's allowlist; empty admits every camera.
	Camera          bool
	CameraAllowlist []string
}

// SystemAPIGrantFor returns the System API grant that entitlements carry. A
// grant without capabilities means the service gets no System API socket.
// Pass only entitlements from the trusted parsed app configuration or from
// persisted container labels.
func SystemAPIGrantFor(entitlements []appconfig.Entitlement) SystemAPIGrant {
	var grant SystemAPIGrant
	readsAll, seesAll := false, false
	for _, e := range entitlements {
		switch e.Type {
		case appconfig.EntitlementNotifications:
			if !slices.Contains(grant.Capabilities, SystemAPICapabilityNotifications) {
				grant.Capabilities = append(grant.Capabilities, SystemAPICapabilityNotifications)
			}
		case appconfig.EntitlementDataRead:
			if !slices.Contains(grant.Capabilities, SystemAPICapabilityDataRead) {
				grant.Capabilities = append(grant.Capabilities, SystemAPICapabilityDataRead)
			}
			readsAll = readsAll || len(e.Allowlist) == 0
			grant.DataReadAllowlist = append(grant.DataReadAllowlist, e.Allowlist...)
		case appconfig.EntitlementCamera, appconfig.EntitlementVideo:
			grant.Camera = true
			seesAll = seesAll || len(e.Allowlist) == 0
			grant.CameraAllowlist = append(grant.CameraAllowlist, e.Allowlist...)
		}
	}
	if readsAll {
		grant.DataReadAllowlist = nil
	}
	if seesAll {
		grant.CameraAllowlist = nil
	}
	// Camera rights reach the System API through data-read alone.
	if !slices.Contains(grant.Capabilities, SystemAPICapabilityDataRead) {
		grant.Camera, grant.CameraAllowlist = false, nil
	}
	return grant
}

// AppSystemAPISocketRootPath is disk-backed so a container's directory bind
// mount remains valid while the Agent recreates the socket after a restart.
var AppSystemAPISocketRootPath = "/var/lib/wendy/app-system"

type appSystemAPIIdentity struct {
	AppID string `json:"app_id"`
}

type appSystemAPISocket struct {
	appID  string
	server *grpc.Server
	owners map[string]SystemAPIGrant
	// data serves data-read. Nil when the manager has no live data.
	data *systemDataService
}

// AppSystemAPISocketManager owns one private System API gRPC server per app.
// All System API capabilities for that app share this socket. owners tracks
// entitled containers so a multi-service app retains its listener until its
// final entitled service is deleted. SECURITY: the private mount is the caller
// credential; requests carry no app identity, and OCI grants GID 2000 only with
// the System API mount (covered by positive, negative, admin, and cross-app tests).
type AppSystemAPISocketManager struct {
	ctx                context.Context
	logger             *zap.Logger
	notificationSender NotificationSender
	// liveData serves the data-read capability. Nil leaves data-read
	// unavailable: Ensure refuses a grant that asks for it.
	liveData *live.Hub
	// appDataStreams counts the live data streams all apps hold open.
	appDataStreams *appDataStreamQuota
	mu             sync.RWMutex
	sockets        map[string]*appSystemAPISocket
}

func NewAppSystemAPISocketManager(
	ctx context.Context,
	logger *zap.Logger,
	notificationSender NotificationSender,
	liveData *live.Hub,
) *AppSystemAPISocketManager {
	manager := &AppSystemAPISocketManager{
		ctx:                ctx,
		logger:             logger,
		notificationSender: notificationSender,
		liveData:           liveData,
		appDataStreams:     newAppDataStreamQuota(),
		sockets:            make(map[string]*appSystemAPISocket),
	}
	go func() {
		<-ctx.Done()
		manager.stopAll()
	}()
	return manager
}

// Ensure creates (or reuses) the app's socket and records the entitled service
// as an owner. The caller must pass only a grant derived from the trusted
// parsed app configuration or persisted container labels; see
// SystemAPIGrantFor.
func (m *AppSystemAPISocketManager) Ensure(appID, serviceName string, grant SystemAPIGrant) (string, error) {
	if err := appconfig.ValidateAppID(appID); err != nil {
		return "", fmt.Errorf("invalid app ID: %w", err)
	}
	if serviceName != "" {
		if err := appconfig.ValidateServiceName(serviceName); err != nil {
			return "", fmt.Errorf("invalid service name: %w", err)
		}
	}
	if len(grant.Capabilities) == 0 {
		return "", fmt.Errorf("at least one System API capability is required")
	}
	for _, capability := range grant.Capabilities {
		switch capability {
		case SystemAPICapabilityNotifications:
		case SystemAPICapabilityDataRead:
			if m.liveData == nil {
				return "", fmt.Errorf("System API capability %q is unavailable: the agent serves no live data", capability)
			}
		default:
			return "", fmt.Errorf("unsupported System API capability %q", capability)
		}
	}

	key := appSystemAPIKey(appID)
	directory := filepath.Join(AppSystemAPISocketRootPath, key)
	owner := systemAPIOwner(serviceName)

	// Registered before the unlock below, so it runs after it: narrow takes
	// the lock itself.
	var rescoped *systemDataService
	defer func() {
		if rescoped != nil {
			rescoped.narrow()
		}
	}()
	m.mu.Lock()
	defer m.mu.Unlock()
	if socket := m.sockets[key]; socket != nil {
		if socket.appID != appID {
			return "", fmt.Errorf("System API identity collision")
		}
		socket.owners[owner] = grant
		rescoped = socket.data
		return directory, nil
	}

	if err := os.MkdirAll(directory, 0o750); err != nil {
		return "", fmt.Errorf("create app System API directory: %w", err)
	}
	if err := os.Chmod(directory, 0o750); err != nil {
		return "", fmt.Errorf("set app System API directory permissions: %w", err)
	}
	if os.Geteuid() == 0 {
		if err := os.Chown(directory, 0, appSystemAPIGroupGID); err != nil {
			return "", fmt.Errorf("set app System API directory ownership: %w", err)
		}
	}
	identity, err := json.Marshal(appSystemAPIIdentity{AppID: appID})
	if err != nil {
		return "", fmt.Errorf("encode app System API identity: %w", err)
	}
	if err := syncWriteFile(filepath.Join(directory, "identity.json"), identity, 0o600); err != nil {
		return "", fmt.Errorf("persist app System API identity: %w", err)
	}

	socketPath := filepath.Join(directory, SystemAPISocketFilename)
	listener, err := localsocket.Listen(socketPath)
	if err != nil {
		return "", fmt.Errorf("listen for app System API: %w", err)
	}
	if os.Geteuid() == 0 {
		if err := os.Chown(socketPath, 0, appSystemAPIGroupGID); err != nil {
			_ = listener.Close()
			return "", fmt.Errorf("set app System API socket ownership: %w", err)
		}
	}

	socket := &appSystemAPISocket{
		appID:  appID,
		owners: map[string]SystemAPIGrant{owner: grant},
	}
	// A notification fits in 16 KiB. data-read takes the largest selection
	// the operator path accepts.
	maxRecv := 16 * 1024
	if m.liveData != nil {
		maxRecv = max(maxRecv, maxStreamDataRequestBytes())
	}
	server := grpc.NewServer(
		grpc.MaxRecvMsgSize(maxRecv),
		grpc.MaxConcurrentStreams(16),
		grpc.UnaryInterceptor(m.authorize(socket)),
		// Without a stream interceptor a streaming method would skip the
		// capability check entirely.
		grpc.StreamInterceptor(m.authorizeStream(socket)),
		grpc.KeepaliveParams(keepalive.ServerParameters{MaxConnectionIdle: 2 * time.Minute}),
	)
	socket.server = server
	systempb.RegisterNotificationServiceServer(server, NewSystemNotificationService(appID, m.notificationSender))
	if m.liveData != nil {
		socket.data = &systemDataService{hub: m.liveData, quota: m.appDataStreams, scope: func() ([]string, cameraScope, bool) {
			m.mu.RLock()
			defer m.mu.RUnlock()
			allow, ok := dataReadScope(socket)
			return allow, dataReadCameras(socket), ok
		}}
		agentpbv2.RegisterDataServiceServer(server, socket.data)
	}
	m.sockets[key] = socket

	go func() {
		m.logger.Info("app System API socket listening",
			zap.String("app_id", appID),
			zap.String("path", socketPath))
		if err := server.Serve(listener); err != nil && m.ctx.Err() == nil {
			m.logger.Error("app System API socket failed", zap.String("app_id", appID), zap.Error(err))
		}
	}()
	return directory, nil
}

// Release drops one service's ownership. It removes the listener and stable
// directory only after the final entitled service has been deleted.
func (m *AppSystemAPISocketManager) Release(appID, serviceName string) {
	key := appSystemAPIKey(appID)
	owner := systemAPIOwner(serviceName)

	m.mu.Lock()
	socket := m.sockets[key]
	if socket == nil || socket.appID != appID {
		m.mu.Unlock()
		return
	}
	delete(socket.owners, owner)
	if len(socket.owners) != 0 {
		m.mu.Unlock()
		// The departing service may have widened the app's data-read scope.
		if socket.data != nil {
			socket.data.narrow()
		}
		return
	}
	delete(m.sockets, key)
	m.mu.Unlock()

	// Stop outside m.mu: an in-flight interceptor may briefly need its read lock.
	socket.server.Stop()
	m.mu.Lock()
	defer m.mu.Unlock()
	// A concurrent redeploy may already have recreated the same app entry.
	if m.sockets[key] != nil {
		return
	}
	if err := os.RemoveAll(filepath.Join(AppSystemAPISocketRootPath, key)); err != nil {
		m.logger.Warn("cannot remove unused app System API directory", zap.String("app_id", appID), zap.Error(err))
	}
}

// ReleaseApp removes all owners for an app. It is used to clean stale state
// after containerd confirms that the app has no remaining containers.
func (m *AppSystemAPISocketManager) ReleaseApp(appID string) {
	key := appSystemAPIKey(appID)
	m.mu.Lock()
	socket := m.sockets[key]
	if socket == nil || socket.appID != appID {
		m.mu.Unlock()
		return
	}
	delete(m.sockets, key)
	m.mu.Unlock()

	socket.server.Stop()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sockets[key] != nil {
		return
	}
	if err := os.RemoveAll(filepath.Join(AppSystemAPISocketRootPath, key)); err != nil {
		m.logger.Warn("cannot remove app System API directory", zap.String("app_id", appID), zap.Error(err))
	}
}

// SweepOrphanedRoots removes System API socket directories that belong to none
// of the given app identities. See AppDataSocketManager.SweepOrphanedRoots for
// why a directory can outlive every owner able to release it, and why restore
// is the one place the live set is known exactly.
func (m *AppSystemAPISocketManager) SweepOrphanedRoots(activeAppIDs []string) {
	live := make(map[string]struct{}, len(activeAppIDs))
	for _, appID := range activeAppIDs {
		live[appSystemAPIKey(appID)] = struct{}{}
	}
	entries, err := os.ReadDir(AppSystemAPISocketRootPath)
	if err != nil {
		if !os.IsNotExist(err) {
			m.logger.Warn("cannot sweep app System API directories", zap.Error(err))
		}
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		key := entry.Name()
		if _, ok := live[key]; ok {
			continue
		}
		if m.sockets[key] != nil {
			continue
		}
		if err := os.RemoveAll(filepath.Join(AppSystemAPISocketRootPath, key)); err != nil {
			m.logger.Warn("cannot remove orphaned app System API directory", zap.String("directory", key), zap.Error(err))
			continue
		}
		m.logger.Info("removed orphaned app System API directory", zap.String("directory", key))
	}
}

func (m *AppSystemAPISocketManager) authorize(socket *appSystemAPISocket) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if err := m.checkCapability(socket, info.FullMethod); err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

func (m *AppSystemAPISocketManager) authorizeStream(socket *appSystemAPISocket) grpc.StreamServerInterceptor {
	return func(srv any, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if err := m.checkCapability(socket, info.FullMethod); err != nil {
			return err
		}
		return handler(srv, stream)
	}
}

func (m *AppSystemAPISocketManager) checkCapability(socket *appSystemAPISocket, method string) error {
	capability := systemAPICapabilityForMethod(method)
	if capability == "" {
		return status.Error(codes.PermissionDenied, "System API method is not authorized")
	}
	m.mu.RLock()
	allowed := socketHasCapability(socket, capability)
	m.mu.RUnlock()
	if !allowed {
		return status.Error(codes.PermissionDenied, "System API capability is not authorized for this app")
	}
	return nil
}

// systemAPICapabilityForMethod names the capability each System API method
// needs. A method missing here is refused, which is what keeps the rest of
// DataService, registered for StreamData alone, off the socket.
func systemAPICapabilityForMethod(method string) string {
	switch method {
	case systempb.NotificationService_Send_FullMethodName:
		return SystemAPICapabilityNotifications
	case agentpbv2.DataService_StreamData_FullMethodName:
		return SystemAPICapabilityDataRead
	}
	return ""
}

func socketHasCapability(socket *appSystemAPISocket, capability string) bool {
	for _, grant := range socket.owners {
		if slices.Contains(grant.Capabilities, capability) {
			return true
		}
	}
	return false
}

// dataReadScope is the app's data-read allowlist: the union of its entitled
// services' allowlists, sorted. Those services share one socket and cannot be
// told apart on it, so one service without an allowlist lets the whole app
// read every stream. ok is false when no service holds data-read. The caller
// holds m.mu.
func dataReadScope(socket *appSystemAPISocket) (allow []string, ok bool) {
	for _, grant := range socket.owners {
		if !slices.Contains(grant.Capabilities, SystemAPICapabilityDataRead) {
			continue
		}
		if len(grant.DataReadAllowlist) == 0 {
			return nil, true
		}
		ok = true
		allow = append(allow, grant.DataReadAllowlist...)
	}
	slices.Sort(allow)
	return slices.Compact(allow), ok
}

// dataReadCameras is the camera rights of the app's data-read services,
// united as dataReadScope unites their allowlists. The caller holds m.mu.
func dataReadCameras(socket *appSystemAPISocket) cameraScope {
	var scope cameraScope
	for _, grant := range socket.owners {
		if !grant.Camera || !slices.Contains(grant.Capabilities, SystemAPICapabilityDataRead) {
			continue
		}
		if len(grant.CameraAllowlist) == 0 {
			return cameraScope{granted: true}
		}
		scope.granted = true
		scope.devices = append(scope.devices, grant.CameraAllowlist...)
	}
	slices.Sort(scope.devices)
	scope.devices = slices.Compact(scope.devices)
	return scope
}

func systemAPIOwner(serviceName string) string {
	if serviceName == "" {
		return "<app>"
	}
	return serviceName
}

func appSystemAPIKey(appID string) string {
	identityDigest := sha256.Sum256([]byte(appID))
	return hex.EncodeToString(identityDigest[:16])
}

func (m *AppSystemAPISocketManager) stopAll() {
	m.mu.Lock()
	sockets := make([]*appSystemAPISocket, 0, len(m.sockets))
	for key, socket := range m.sockets {
		sockets = append(sockets, socket)
		delete(m.sockets, key)
	}
	m.mu.Unlock()
	for _, socket := range sockets {
		socket.server.Stop()
	}
}
