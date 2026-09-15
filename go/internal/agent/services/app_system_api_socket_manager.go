package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/agent/localsocket"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	systempb "github.com/wendylabsinc/wendy/go/proto/gen/systempb"
)

const (
	SystemAPICapabilityNotifications = "notifications"
	SystemAPISocketFilename          = "system.sock"
	appSystemAPIGroupGID             = 2000
)

// AppSystemAPISocketRootPath is disk-backed so a container's directory bind
// mount remains valid while the Agent recreates the socket after a restart.
var AppSystemAPISocketRootPath = "/var/lib/wendy/app-system"

type appSystemAPIIdentity struct {
	AppID string `json:"app_id"`
}

type appSystemAPISocket struct {
	appID  string
	server *grpc.Server
	owners map[string]map[string]struct{}
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
	mu                 sync.RWMutex
	sockets            map[string]*appSystemAPISocket
}

func NewAppSystemAPISocketManager(
	ctx context.Context,
	logger *zap.Logger,
	notificationSender NotificationSender,
) *AppSystemAPISocketManager {
	manager := &AppSystemAPISocketManager{
		ctx:                ctx,
		logger:             logger,
		notificationSender: notificationSender,
		sockets:            make(map[string]*appSystemAPISocket),
	}
	go func() {
		<-ctx.Done()
		manager.stopAll()
	}()
	return manager
}

// Ensure creates (or reuses) the app's socket and records the entitled service
// as an owner. The caller must pass only capabilities derived from the trusted
// parsed app configuration or persisted container labels.
func (m *AppSystemAPISocketManager) Ensure(appID, serviceName string, capabilities []string) (string, error) {
	if err := appconfig.ValidateAppID(appID); err != nil {
		return "", fmt.Errorf("invalid app ID: %w", err)
	}
	if serviceName != "" {
		if err := appconfig.ValidateServiceName(serviceName); err != nil {
			return "", fmt.Errorf("invalid service name: %w", err)
		}
	}
	if len(capabilities) == 0 {
		return "", fmt.Errorf("at least one System API capability is required")
	}
	for _, capability := range capabilities {
		if capability != SystemAPICapabilityNotifications {
			return "", fmt.Errorf("unsupported System API capability %q", capability)
		}
	}

	key := appSystemAPIKey(appID)
	directory := filepath.Join(AppSystemAPISocketRootPath, key)
	owner := systemAPIOwner(serviceName)

	m.mu.Lock()
	defer m.mu.Unlock()
	if socket := m.sockets[key]; socket != nil {
		if socket.appID != appID {
			return "", fmt.Errorf("System API identity collision")
		}
		socket.owners[owner] = capabilitySet(capabilities)
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
		owners: map[string]map[string]struct{}{owner: capabilitySet(capabilities)},
	}
	server := grpc.NewServer(
		grpc.MaxRecvMsgSize(16*1024),
		grpc.MaxConcurrentStreams(16),
		grpc.UnaryInterceptor(m.authorize(socket)),
		grpc.KeepaliveParams(keepalive.ServerParameters{MaxConnectionIdle: 2 * time.Minute}),
	)
	socket.server = server
	systempb.RegisterNotificationServiceServer(server, NewSystemNotificationService(appID, m.notificationSender))
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
		capability := systemAPICapabilityForMethod(info.FullMethod)
		if capability == "" {
			return nil, status.Error(codes.PermissionDenied, "System API method is not authorized")
		}
		m.mu.RLock()
		allowed := socketHasCapability(socket, capability)
		m.mu.RUnlock()
		if !allowed {
			return nil, status.Error(codes.PermissionDenied, "System API capability is not authorized for this app")
		}
		return handler(ctx, req)
	}
}

func systemAPICapabilityForMethod(method string) string {
	if method == systempb.NotificationService_Send_FullMethodName {
		return SystemAPICapabilityNotifications
	}
	return ""
}

func socketHasCapability(socket *appSystemAPISocket, capability string) bool {
	for _, capabilities := range socket.owners {
		if _, ok := capabilities[capability]; ok {
			return true
		}
	}
	return false
}

func capabilitySet(capabilities []string) map[string]struct{} {
	result := make(map[string]struct{}, len(capabilities))
	for _, capability := range capabilities {
		result[capability] = struct{}{}
	}
	return result
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
