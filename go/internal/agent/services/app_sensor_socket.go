package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/agent/localsocket"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	appspbv1 "github.com/wendylabsinc/wendy/go/proto/gen/appspb/v1"
)

const (
	// SensorSocketFilename is the socket inside the app's private sensor
	// directory.
	SensorSocketFilename = "sensors.sock"
	appSensorGroupGID    = 2000
	// sensorMaxRecvBytes bounds a Subscribe request. Requests carry a handful of
	// source identifiers, so the default 4 MiB ceiling is needlessly generous.
	sensorMaxRecvBytes = 16 * 1024
	// sensorMaxSendBytes must admit one whole sample. maxFrameBytes is the
	// producer's own ceiling on a single encoded frame; the slack covers the
	// message's other fields.
	sensorMaxSendBytes = maxFrameBytes + 64*1024
	// sensorMaxConcurrentStreams bounds the subscriptions one app can hold.
	sensorMaxConcurrentStreams = 8
)

// AppSensorSocketRootPath is disk-backed so a container's directory bind mount
// stays valid while the agent recreates the socket after a restart.
var AppSensorSocketRootPath = "/var/lib/wendy/app-sensors"

type appSensorIdentity struct {
	AppID string `json:"app_id"`
}

type appSensorSocket struct {
	appID  string
	server *grpc.Server
	// owners maps an entitled service to the source allowlist it declared. The
	// union across owners is what the socket permits, matching how the System
	// API socket unions capabilities. Every allowlist is non-empty: the
	// sensor-read entitlement requires one, and Ensure refuses an owner without
	// one, so there is no "asked for everything" state to fall back to.
	owners map[string][]string
}

// permits reports whether the socket's current owner set allows a source id.
// Callers must hold at least a read lock on the manager.
func (s *appSensorSocket) permits(sourceID string) bool {
	for _, allowlist := range s.owners {
		for _, allowed := range allowlist {
			if allowed == sourceID {
				return true
			}
		}
	}
	return false
}

// AppSensorSocketManager owns one private sensor gRPC server per app. It is the
// enforcement point of the "sensor-read" entitlement: the socket serves
// SensorService and nothing else, so an app that reads sensors cannot also
// start episodes, deploy campaigns, or download data.
//
// SECURITY: the private mount is the caller credential, exactly as for the
// System API socket. Requests carry no app identity; the service instance is
// constructed bound to the app the socket was created for, and the OCI layer
// grants GID 2000 only together with the mount.
type AppSensorSocketManager struct {
	ctx     context.Context
	logger  *zap.Logger
	capture *data.Manager
	// providers are the producer-owning services a new per-app SensorService is
	// wired to. They are shared: every app subscribes to the same producers.
	providers []sensorProvider
	mu        sync.RWMutex
	sockets   map[string]*appSensorSocket
}

func NewAppSensorSocketManager(ctx context.Context, logger *zap.Logger, capture *data.Manager) *AppSensorSocketManager {
	m := &AppSensorSocketManager{ctx: ctx, logger: logger, capture: capture, sockets: map[string]*appSensorSocket{}}
	go func() {
		<-ctx.Done()
		m.stopAll()
	}()
	return m
}

// AddProvider registers a producer-owning service for every app socket. It must
// be called before the sockets an app uses are created; sockets already serving
// keep the provider set they were built with.
func (m *AppSensorSocketManager) AddProvider(provider sensorProvider) {
	if provider == nil {
		return
	}
	m.mu.Lock()
	m.providers = append(m.providers, provider)
	m.mu.Unlock()
}

// Ensure creates (or reuses) the app's sensor socket and records the entitled
// service as an owner together with the source allowlist it declared. The
// allowlist must be non-empty: the sensor-read entitlement requires one, and
// this is the agent-side half of that rule, so a manifest that reached the
// agent without passing CLI validation still cannot obtain a blanket grant.
// The caller must pass only allowlists derived from the trusted parsed app
// configuration or persisted container labels.
func (m *AppSensorSocketManager) Ensure(appID, serviceName string, allowlist []string) (string, error) {
	if err := appconfig.ValidateAppID(appID); err != nil {
		return "", fmt.Errorf("invalid app ID: %w", err)
	}
	if len(allowlist) == 0 {
		return "", fmt.Errorf("the sensor-read entitlement requires a non-empty allowlist of sensor source ids; declare the sources this app subscribes to in wendy.json")
	}
	if serviceName != "" {
		if err := appconfig.ValidateServiceName(serviceName); err != nil {
			return "", fmt.Errorf("invalid service name: %w", err)
		}
	}
	key := appSensorKey(appID)
	directory := filepath.Join(AppSensorSocketRootPath, key)
	owner := systemAPIOwner(serviceName)

	// Copy so a later mutation of the caller's slice cannot widen the grant of
	// a socket that is already serving.
	allowlist = append([]string(nil), allowlist...)

	m.mu.Lock()
	defer m.mu.Unlock()
	if socket := m.sockets[key]; socket != nil {
		if socket.appID != appID {
			return "", fmt.Errorf("sensor socket identity collision")
		}
		socket.owners[owner] = allowlist
		return directory, nil
	}
	if err := os.MkdirAll(directory, 0o750); err != nil {
		return "", fmt.Errorf("create app sensor directory: %w", err)
	}
	if err := os.Chmod(directory, 0o750); err != nil {
		return "", fmt.Errorf("set app sensor directory permissions: %w", err)
	}
	if os.Geteuid() == 0 {
		if err := os.Chown(directory, 0, appSensorGroupGID); err != nil {
			return "", fmt.Errorf("set app sensor directory ownership: %w", err)
		}
	}
	identity, err := json.Marshal(appSensorIdentity{AppID: appID})
	if err != nil {
		return "", fmt.Errorf("encode app sensor identity: %w", err)
	}
	if err := syncWriteFile(filepath.Join(directory, "identity.json"), identity, 0o600); err != nil {
		return "", fmt.Errorf("persist app sensor identity: %w", err)
	}
	socketPath := filepath.Join(directory, SensorSocketFilename)
	listener, err := localsocket.Listen(socketPath)
	if err != nil {
		return "", fmt.Errorf("listen for app sensor socket: %w", err)
	}
	if os.Geteuid() == 0 {
		if err := os.Chown(socketPath, 0, appSensorGroupGID); err != nil {
			_ = listener.Close()
			return "", fmt.Errorf("set app sensor socket ownership: %w", err)
		}
	}

	sensors := NewSensorService(appID, m.capture)
	for _, provider := range m.providers {
		sensors.AddProvider(provider)
	}
	socket := &appSensorSocket{appID: appID, owners: map[string][]string{owner: allowlist}}
	// Consulted live rather than snapshotted: a multi-service app adds and
	// removes owners while this socket keeps serving, and the grant must follow.
	sensors.SetSourcePermission(func(sourceID string) bool {
		m.mu.RLock()
		defer m.mu.RUnlock()
		return socket.permits(sourceID)
	})
	server := grpc.NewServer(
		grpc.MaxRecvMsgSize(sensorMaxRecvBytes),
		grpc.MaxSendMsgSize(sensorMaxSendBytes),
		grpc.MaxConcurrentStreams(sensorMaxConcurrentStreams),
		grpc.UnaryInterceptor(authorizeSensorUnary),
		grpc.StreamInterceptor(authorizeSensorStream),
		// Deliberately no MaxConnectionIdle: a subscription to a low-rate
		// sensor legitimately sits idle between samples, and tearing it down
		// would be indistinguishable from a producer failure to the app.
	)
	appspbv1.RegisterSensorServiceServer(server, sensors)
	socket.server = server
	m.sockets[key] = socket
	go func() {
		m.logger.Info("app sensor socket listening", zap.String("app_id", appID), zap.String("path", socketPath))
		if err := server.Serve(listener); err != nil && m.ctx.Err() == nil {
			m.logger.Error("app sensor socket failed", zap.String("app_id", appID), zap.Error(err))
		}
	}()
	return directory, nil
}

// Release drops one service's ownership, removing the listener and directory
// only after the final entitled service has been deleted.
func (m *AppSensorSocketManager) Release(appID, serviceName string) {
	key := appSensorKey(appID)
	m.mu.Lock()
	socket := m.sockets[key]
	if socket == nil || socket.appID != appID {
		m.mu.Unlock()
		return
	}
	delete(socket.owners, systemAPIOwner(serviceName))
	if len(socket.owners) != 0 {
		m.mu.Unlock()
		return
	}
	delete(m.sockets, key)
	m.mu.Unlock()
	socket.server.Stop()
	m.removeDirectory(key, appID)
}

// ReleaseApp removes all owners for an app.
func (m *AppSensorSocketManager) ReleaseApp(appID string) {
	key := appSensorKey(appID)
	m.mu.Lock()
	socket := m.sockets[key]
	if socket == nil || socket.appID != appID {
		m.mu.Unlock()
		return
	}
	delete(m.sockets, key)
	m.mu.Unlock()
	socket.server.Stop()
	m.removeDirectory(key, appID)
}

func (m *AppSensorSocketManager) removeDirectory(key, appID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// A concurrent redeploy may already have recreated the same app entry.
	if m.sockets[key] != nil {
		return
	}
	if err := os.RemoveAll(filepath.Join(AppSensorSocketRootPath, key)); err != nil {
		m.logger.Warn("cannot remove unused app sensor directory", zap.String("app_id", appID), zap.Error(err))
	}
}

func (m *AppSensorSocketManager) stopAll() {
	m.mu.Lock()
	sockets := make([]*appSensorSocket, 0, len(m.sockets))
	for key, socket := range m.sockets {
		sockets = append(sockets, socket)
		delete(m.sockets, key)
	}
	m.mu.Unlock()
	for _, socket := range sockets {
		socket.server.Stop()
	}
}

func appSensorKey(appID string) string {
	digest := sha256.Sum256([]byte(appID))
	return hex.EncodeToString(digest[:16])
}

// sensorMethodPrefix is the only gRPC service this socket serves. The server
// has just one service registered, so this check is defence in depth: it keeps
// a future registration on this server from silently becoming reachable by
// every app holding the sensor-read entitlement.
const sensorMethodPrefix = "/wendy.agent.apps.v1.SensorService/"

func authorizeSensorMethod(method string) error {
	if strings.HasPrefix(method, sensorMethodPrefix) {
		return nil
	}
	return status.Error(codes.PermissionDenied, "the sensor-read entitlement authorizes only wendy.agent.apps.v1.SensorService")
}

func authorizeSensorUnary(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	if err := authorizeSensorMethod(info.FullMethod); err != nil {
		return nil, err
	}
	return handler(ctx, req)
}

func authorizeSensorStream(srv any, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	if err := authorizeSensorMethod(info.FullMethod); err != nil {
		return err
	}
	return handler(srv, stream)
}
