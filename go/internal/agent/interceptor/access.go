package interceptor

import (
	"context"
	"fmt"

	"github.com/wendylabsinc/wendy/go/internal/agent/trustedkeys"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// ClosedListener is how a caller reached a Closed device.
type ClosedListener int

const (
	// ClosedKeyListener is the TLS listener: the caller proved a trusted key.
	ClosedKeyListener ClosedListener = iota
	// ClosedKeylessListener is the plaintext listener: no key. Only GetAccess
	// and, when the owner switched it on, the keyless apps set get through.
	ClosedKeylessListener
	// ClosedAdminSocket is the local socket of apps with the admin
	// entitlement. They act as operator, so an app cannot manage keys, change
	// the shape or enroll the device.
	ClosedAdminSocket
)

// keylessMethods is the one fixed "deploy and manage apps" set an owner may
// open to callers without a key: what `wendy run` and the app commands (list,
// logs, stop, start, remove, device version) call.
var keylessMethods = map[string]bool{
	"/wendy.agent.services.v1.WendyAgentService/GetAgentVersion":                 true,
	"/wendy.agent.services.v1.WendyProvisioningService/IsProvisioned":            true,
	"/wendy.agent.services.v2.WendyProvisioningService/IsProvisioned":            true,
	"/wendy.agent.services.v1.WendyContainerService/ValidateContainer":           true,
	"/wendy.agent.services.v1.WendyContainerService/QueryChunks":                 true,
	"/wendy.agent.services.v1.WendyContainerService/WriteChunks":                 true,
	"/wendy.agent.services.v1.WendyContainerService/QueryLayers":                 true,
	"/wendy.agent.services.v1.WendyContainerService/PrepareImage":                true,
	"/wendy.agent.services.v1.WendyContainerService/CreateContainer":             true,
	"/wendy.agent.services.v1.WendyContainerService/CreateContainerWithProgress": true,
	"/wendy.agent.services.v1.WendyContainerService/RunContainer":                true,
	"/wendy.agent.services.v1.WendyContainerService/StartContainer":              true,
	"/wendy.agent.services.v1.WendyContainerService/AttachContainer":             true,
	"/wendy.agent.services.v1.WendyContainerService/GetContainerPorts":           true,
	"/wendy.agent.services.v1.WendyContainerService/ListContainers":              true,
	"/wendy.agent.services.v1.WendyContainerService/ListContainerStats":          true,
	"/wendy.agent.services.v1.WendyContainerService/ListVolumes":                 true,
	"/wendy.agent.services.v1.WendyContainerService/StopContainer":               true,
	"/wendy.agent.services.v1.WendyContainerService/DeleteContainer":             true,
	"/wendy.agent.services.v1.WendyTelemetryService/StreamLogs":                  true,
}

// Caller is who a Closed device admitted for one call.
type Caller struct {
	Role trustedkeys.Role
	// Fingerprint of the caller's key; empty without a key.
	Fingerprint string
}

type callerKey struct{}

// CallerFromContext returns the caller a Closed device admitted.
func CallerFromContext(ctx context.Context) (Caller, bool) {
	c, ok := ctx.Value(callerKey{}).(Caller)
	return c, ok
}

// ClosedAuthorizer is the method-to-role check of every listener of a Closed
// device. Open and Enrolled devices do not run it.
type ClosedAuthorizer struct {
	Logger   *zap.Logger
	Listener ClosedListener
	Keys     *trustedkeys.Store
}

var (
	errNeedKey = status.Error(codes.PermissionDenied,
		"this device is closed and only accepts trusted keys; ask the device's owner to add your key")
	errKeyNotTrusted = status.Error(codes.PermissionDenied,
		"your key is not trusted by this device; ask the device's owner to add it")
	errHookOwnerOnly = status.Error(codes.PermissionDenied,
		"the post-start host command runs as root and needs the owner key; remove it from wendy.json or deploy with the owner key")
)

func (a *ClosedAuthorizer) authorize(ctx context.Context, method string) (context.Context, error) {
	min, mapped := methodRoles[method]
	if !mapped {
		return nil, a.deny(ctx, method, Caller{}, status.Errorf(codes.PermissionDenied,
			"%s is not available while this device is closed; update the agent if it should be", method))
	}
	c, err := a.identify(ctx)
	if err != nil {
		return nil, a.deny(ctx, method, c, err)
	}
	hook := len(metadata.ValueFromIncomingContext(ctx, appconfig.PostStartAgentHookMetadataKey)) > 0
	switch {
	case hook && c.Role < owner:
		return nil, a.deny(ctx, method, c, errHookOwnerOnly)
	case min == anyone:
	case a.Listener == ClosedKeylessListener:
		if !keylessMethods[method] || !a.Keys.KeylessApps() {
			return nil, a.deny(ctx, method, c, errNeedKey)
		}
		ctx = trustedkeys.WithKeyless(ctx)
	case c.Role < min:
		return nil, a.deny(ctx, method, c, status.Errorf(codes.PermissionDenied,
			"this needs the %s role and your key has %s; ask the device's owner for a higher role", min, c.Role))
	}
	return context.WithValue(ctx, callerKey{}, c), nil
}

func (a *ClosedAuthorizer) identify(ctx context.Context) (Caller, error) {
	switch a.Listener {
	case ClosedKeylessListener:
		return Caller{}, nil
	case ClosedAdminSocket:
		return Caller{Role: operator}, nil
	}
	p, ok := peer.FromContext(ctx)
	if !ok {
		return Caller{}, errKeyNotTrusted
	}
	tlsInfo, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok || len(tlsInfo.State.PeerCertificates) == 0 {
		return Caller{}, errKeyNotTrusted
	}
	// The handshake admitted the key; look it up again per call so a key
	// removed or demoted while its connection is open loses access.
	role, fp := a.Keys.Lookup(tlsInfo.State.PeerCertificates[0].RawSubjectPublicKeyInfo)
	if role == trustedkeys.RoleNone {
		return Caller{Fingerprint: fp}, errKeyNotTrusted
	}
	return Caller{Role: role, Fingerprint: fp}, nil
}

func (a *ClosedAuthorizer) deny(ctx context.Context, method string, c Caller, err error) error {
	remote := ""
	if p, ok := peer.FromContext(ctx); ok && p.Addr != nil {
		remote = p.Addr.String()
	}
	a.Logger.Warn("closed device refused a call",
		zap.String("method", method), zap.String("role", c.Role.String()),
		zap.String("fingerprint", c.Fingerprint), zap.String("remote", remote),
		zap.String("reason", status.Convert(err).Message()))
	return err
}

// Unary is the unary form of the check.
func (a *ClosedAuthorizer) Unary() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		ctx, err := a.authorize(ctx, info.FullMethod)
		if err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

// Stream is the streaming form of the check.
func (a *ClosedAuthorizer) Stream() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		ctx, err := a.authorize(ss.Context(), info.FullMethod)
		if err != nil {
			return err
		}
		return handler(srv, &callerStream{ServerStream: ss, ctx: ctx})
	}
}

type callerStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *callerStream) Context() context.Context { return s.ctx }

// Unmapped lists the methods srv serves that have no role. A Closed agent
// refuses to start with any: the call would be denied to everyone, so it is a
// missing role decision, not a working call.
func Unmapped(srv interface {
	GetServiceInfo() map[string]grpc.ServiceInfo
}) []string {
	var out []string
	for name, info := range srv.GetServiceInfo() {
		for _, m := range info.Methods {
			if full := fmt.Sprintf("/%s/%s", name, m.Name); !mapped(full) {
				out = append(out, full)
			}
		}
	}
	return out
}

func mapped(method string) bool {
	_, ok := methodRoles[method]
	return ok
}
