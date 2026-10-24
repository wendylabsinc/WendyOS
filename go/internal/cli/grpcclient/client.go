// Package grpcclient provides a gRPC client factory for connecting to the Wendy agent.
package grpcclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"

	wendy "github.com/wendylabsinc/wendy-go"
	"github.com/wendylabsinc/wendy/go/internal/cli/linklocal"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/devicepin"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
)

const (
	// Stream/connection flow-control windows are intentionally small so that
	// gRPC backpressure reaches the agent's Send() within ~250ms when the
	// consumer falls behind, engaging the camera pipeline's agent-side
	// frame-dropping. The floor (~128KB) keeps a single 1080p IDR (50–150KB)
	// from stalling on the window.
	grpcInitialStreamWindow = 256 * 1024
	grpcInitialConnWindow   = 512 * 1024
	grpcReadBufferSize      = 256 * 1024
	grpcWriteBufferSize     = 256 * 1024

	// NOTE: Keep direct-agent pings conservative. macOS agents may close
	// long-running build/deploy/log streams with ENHANCE_YOUR_CALM/too_many_pings
	// when clients ping near the server's HTTP/2 keepalive policy floor. This is
	// intentionally global for now because direct-agent connections share the same
	// gRPC client path; a target-specific keepalive policy can be added later if
	// we see availability regressions.
	grpcKeepaliveTime    = 15 * time.Minute
	grpcKeepaliveTimeout = 10 * time.Second

	// A connection probe and the command phase that consumes its metadata are
	// normally adjacent. Bound reuse so long-running watch/build sessions still
	// refresh mutable fields such as interfaces and disk usage.
	agentVersionCacheTTL = 10 * time.Second
)

// tlsDebugWriter is where WENDY_TLS_DEBUG resumption logging is written.
// Overridable in tests to capture output.
var tlsDebugWriter io.Writer = os.Stderr

type AgentConnection struct {
	device *wendy.GRPCDeviceConnection

	Conn *grpc.ClientConn
	Host string // hostname or IP of the connected agent
	// MeshHost is the device's stable mesh DNS name
	// (device-<assetID>.mesh.wendy.internal). Cloud transports populate it so
	// developer-side traffic can use the desktop VPN when it is active instead
	// of relying on a device-reported LAN address.
	MeshHost string
	// Addr is the full host:port this connection dialed — the endpoint that
	// actually answered, mTLS port included. Empty for unix-socket and
	// pre-built (NewFromConn) connections.
	Addr string
	// SimulatorName retains the VM pin identity independently of its loopback endpoint.
	SimulatorName  string
	IsMTLS         bool                    // true when connected via mutual TLS
	IsSessionProxy bool                    // true when Conn reaches a local session broker retaining the mTLS transport
	CertInfo       *config.CertificateInfo // cert used to establish mTLS; nil for plaintext
	RegistryDialer func(context.Context, int) (net.Conn, error)
	ExtraClosers   []io.Closer
	// Reconnect re-establishes a connection to the SAME device this connection
	// targets, after the agent restarts. It is set for transports where the
	// connection identity can't be re-derived from Host alone (e.g. the cloud
	// tunnel, which is pinned to a specific asset id). nil for plain LAN
	// connections, where the caller re-dials Host directly.
	Reconnect            func(context.Context) (*AgentConnection, error)
	AgentService         agentpb.WendyAgentServiceClient
	ContainerService     agentpb.WendyContainerServiceClient
	ShellService         agentpb.WendyShellServiceClient
	AudioService         agentpb.WendyAudioServiceClient
	AudioServiceV2       agentpbv2.WendyAudioServiceClient
	VideoService         agentpb.WendyVideoServiceClient
	ProvisioningService  agentpb.WendyProvisioningServiceClient
	TelemetryService     agentpb.WendyTelemetryServiceClient
	FileSyncService      agentpb.WendyFileSyncServiceClient
	TimeSyncService      agentpbv2.WendyTimeSyncServiceClient
	BuildService         agentpbv2.WendyBuildServiceClient
	SensorPairingService agentpbv2.WendySensorPairingServiceClient
	UnitreeService       agentpbv2.WendyUnitreeServiceClient
	RobotArmService      agentpbv2.WendyRobotArmServiceClient
	DriverService        agentpbv2.WendyDriverServiceClient
	DataService          agentpbv2.DataServiceClient
	// CalibratedFrameService serves RGB-D frames: colour plus depth aligned to
	// it, with the scale and intrinsics that make both measurable. Separate
	// from VideoService because a measurement is not a picture — see
	// Proto/wendy/agent/services/v2/calibrated_frame_service.proto.
	CalibratedFrameService agentpbv2.WendyCalibratedFrameServiceClient
	// cachedAgentVersion retains a successful liveness probe performed while
	// establishing this connection. Direct-agent connects already call
	// GetAgentVersion to force gRPC's lazy dial and authenticate the peer; run
	// commands can reuse that exact response instead of immediately issuing the
	// same RPC again. The cache is deliberately connection-local: it is never
	// persisted across processes or carried across reconnects.
	cachedAgentVersion atomic.Pointer[agentVersionCacheEntry]
	// Session proxies and externally verified connections retain host-provided
	// identity observations here. Direct connections read WendyGo's live evidence.
	observedServerOrg      *atomic.Int32
	observedServerIdentity *atomic.Pointer[certs.WendyIdentity]
}

// TCPDialError reports pre-TLS reachability evidence from the SDK connection.
func (c *AgentConnection) TCPDialError() error {
	if c != nil && c.device != nil {
		return c.device.Evidence().TCPDialError
	}
	return nil
}

type agentVersionCacheEntry struct {
	response *agentpb.GetAgentVersionResponse
	cachedAt time.Time
}

// CacheAgentVersion records a successful GetAgentVersion response for reuse by
// a later caller on this same live connection. Responses returned by grpc-go
// are immutable in Wendy's callers after receipt, so retaining the pointer is
// safe; atomic storage also covers probes completed on gRPC/spinner goroutines.
func (c *AgentConnection) CacheAgentVersion(resp *agentpb.GetAgentVersionResponse) {
	if c != nil && resp != nil {
		c.cachedAgentVersion.Store(&agentVersionCacheEntry{response: resp, cachedAt: time.Now()})
	}
}

// CachedAgentVersion returns the version response already obtained while
// proving this connection live, when one is available.
func (c *AgentConnection) CachedAgentVersion() (*agentpb.GetAgentVersionResponse, bool) {
	if c == nil {
		return nil, false
	}
	entry := c.cachedAgentVersion.Load()
	if entry == nil || time.Since(entry.cachedAt) > agentVersionCacheTTL {
		return nil, false
	}
	return entry.response, true
}

// verifiedIdentitySink returns the OnVerifiedServerIdentity callback that
// records the device identity behind a verified server certificate. Identities
// need either a legacy org or a complete, internally consistent tenant SPIFFE
// identity. PKI-issued identities deliberately have no numeric org ID.
//
// dst may be nil (observation not requested), in which case the returned sink is
// a no-op. This runs inside
// VerifyConnection, so an unguarded nil would panic mid-handshake.
func verifiedIdentitySink(dst *atomic.Pointer[certs.WendyIdentity]) func(certs.WendyIdentity) {
	return func(id certs.WendyIdentity) {
		if dst == nil || id.OrgID < 0 {
			return
		}
		if id.Principal != "" || id.TenantUUID != "" {
			parsed, err := certs.ParsePrincipal(id.Principal)
			if err != nil || parsed.TenantUUID != id.TenantUUID || parsed.EntityType != id.EntityType || parsed.EntityID != id.EntityID {
				return
			}
		} else if id.OrgID == 0 {
			return
		}
		stored := id
		dst.Store(&stored)
	}
}

func Connect(ctx context.Context, address string) (*AgentConnection, error) {
	return connectSDKDevice(ctx, address, nil, nil, nil)
}

// newUnixClient is the one place a local unix-domain gRPC channel is built —
// used by ConnectUnix (agent control socket) and ConnectSessionProxy (session
// broker socket) — so tuning such as window sizes and keepalive cannot drift
// between the two local transports.
func newUnixClient(socketPath, target string, extra ...grpc.DialOption) (*grpc.ClientConn, error) {
	dialer := func(ctx context.Context, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", socketPath)
	}
	opts := []grpc.DialOption{
		grpc.WithContextDialer(dialer),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithInitialWindowSize(grpcInitialStreamWindow),
		grpc.WithInitialConnWindowSize(grpcInitialConnWindow),
		grpc.WithReadBufferSize(grpcReadBufferSize),
		grpc.WithWriteBufferSize(grpcWriteBufferSize),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                grpcKeepaliveTime,
			Timeout:             grpcKeepaliveTimeout,
			PermitWithoutStream: false,
		}),
	}
	return grpc.NewClient("passthrough:///"+target, append(opts, extra...)...)
}

// ConnectUnix dials the agent over a local unix domain socket with plain h2c
// (no TLS). It is used inside an `admin`-entitled container, where the agent's
// control socket is bind-mounted in and WENDY_AGENT_SOCKET points at it. The
// socket itself is the entire trust boundary (see the admin entitlement); there
// is deliberately no authentication here.
func ConnectUnix(ctx context.Context, socketPath string) (*AgentConnection, error) {
	conn, err := newUnixClient(socketPath, "unix")
	if err != nil {
		return nil, fmt.Errorf("connecting to agent at unix:%s: %w", socketPath, err)
	}
	ac := newAgentConnection(conn)
	ac.Host = "unix:" + socketPath
	return ac, nil
}

func ConnectWithTLS(ctx context.Context, address string, certInfo *config.CertificateInfo) (*AgentConnection, error) {
	return ConnectWithTLSAndPins(ctx, address, certInfo, nil)
}

func ConnectWithTLSAndPins(ctx context.Context, address string, certInfo *config.CertificateInfo, pins certs.PinChecker) (*AgentConnection, error) {
	return ConnectWithTLSExpecting(ctx, address, certInfo, pins, nil)
}

// ConnectWithTLSExpecting is ConnectWithTLSAndPins with a required peer
// identity. A nil expected is exactly ConnectWithTLSAndPins.
//
// extraOpts extends the standard dial options for callers whose channel has
// different lifetime needs than a per-command connection — today the session
// broker, which must pin its retained transport open (grpc.WithIdleTimeout(0))
// because for it a teardown-and-redial is a trust event, not a transparency.
func ConnectWithTLSExpecting(ctx context.Context, address string, certInfo *config.CertificateInfo, pins certs.PinChecker, expected *certs.WendyIdentity, extraOpts ...grpc.DialOption) (*AgentConnection, error) {
	if certInfo == nil {
		return nil, errors.New("TLS device connection requires an operator certificate and key")
	}
	return connectSDKDevice(ctx, address, certInfo, pins, expected, extraOpts...)
}

// grpcTarget converts a host:port address into a gRPC target string.
// IPv6 link-local addresses contain a zone ID with a bare "%" (e.g.
// [fe80::1%en0]:50051). grpc.NewClient parses the target as a URL, where
// "%" starts a percent-encoding sequence — "%en" is invalid hex and fails.
// We use the passthrough scheme with url.URL which correctly escapes the
// zone "%" to "%25". The passthrough resolver decodes it back to the
// original zone ID before passing it to the dialer.
//
// The address MUST be bracketed for IPv6 (e.g. [fe80::1%en0]:50051).
// As a safety net, if an unbracketed IPv6 address is received, we add
// brackets before constructing the URL so the host is unambiguous.
func grpcTarget(address string) string {
	if !strings.Contains(address, "%") {
		return address
	}

	// Ensure IPv6 address is properly bracketed. net.SplitHostPort
	// handles [host]:port but fails for bare IPv6 like
	// fe80::1%en0:50051 where the colons are ambiguous.
	if _, _, err := net.SplitHostPort(address); err != nil && !strings.HasPrefix(address, "[") {
		// Zone IDs (interface names) never contain colons, so the
		// port follows the last ":".
		if i := strings.LastIndex(address, ":"); i > 0 {
			host, port := address[:i], address[i+1:]
			address = net.JoinHostPort(host, port)
		}
	}

	u := &url.URL{Scheme: "passthrough", Path: "/" + address}
	return u.String()
}

// hostFromAddress extracts the hostname/IP from a host:port address string.
// Handles IPv6 addresses like [::1]:50051.
func hostFromAddress(address string) string {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return address
	}
	return host
}

// Close closes the underlying gRPC connection.
//
// Safe on a nil receiver: callers defer Close on a connection variable that a
// later reconnect may set back to nil, and a failed reconnect must surface its
// own error rather than a panic from the deferred cleanup.
func (c *AgentConnection) Close() error {
	if c == nil {
		return nil
	}
	var errs []error
	if c.Conn != nil {
		errs = append(errs, c.Conn.Close())
	}
	for _, closer := range c.ExtraClosers {
		if closer != nil {
			errs = append(errs, closer.Close())
		}
	}
	return errors.Join(errs...)
}

// ObservedServerOrg returns the org ID observed in the device's server
// certificate during the TLS handshake, or (0, false) if none was observed
// (plaintext connection, cert without a Wendy identity, or a handshake that
// never reached the server certificate). Safe to call after the first RPC.
// The value comes from the peer's leaf certificate SAN and is NOT validated
// against any trust chain by this accessor; it is for diagnostic display only
// and must never be used for an authorization or trust decision.
func (c *AgentConnection) ObservedServerOrg() (int32, bool) {
	if c.device != nil {
		v := c.device.Evidence().ObservedOrg
		return v, v != 0
	}
	if c.observedServerOrg == nil {
		return 0, false
	}
	v := c.observedServerOrg.Load()
	return v, v != 0
}

// ObservedServerIdentity returns the device's Wendy identity (org + entity, the
// entity being "asset:<assetID>" for an agent) taken from a server certificate
// this connection VERIFIED — chain-checked against the CLI's CA and matched to
// the expected org. Returns (zero, false) for plaintext connections, certs
// carrying no Wendy identity, and handshakes that never completed.
//
// Unlike ObservedServerOrg — captured pre-verification and therefore only safe
// for diagnostics — this identity is what the peer proved, so it is the value
// device pinning compares against (see enforceDevicePin).
func (c *AgentConnection) ObservedServerIdentity() (certs.WendyIdentity, bool) {
	if c.device != nil {
		id, ok := c.device.VerifiedIdentity()
		return certs.WendyIdentity{OrgID: id.OrgID, TenantUUID: id.TenantUUID, EntityType: id.EntityType, EntityID: id.EntityID, Principal: id.Principal}, ok
	}
	if c.observedServerIdentity == nil {
		return certs.WendyIdentity{}, false
	}
	id := c.observedServerIdentity.Load()
	if id == nil {
		return certs.WendyIdentity{}, false
	}
	return *id, true
}

// IdentityMismatch reports the ExpectedIdentity rejection this connection hit,
// if any. It is the ladder's signal that the *device* is wrong — as opposed to
// our certificate being wrong — and therefore that retrying with other certs or
// other ports is pointless.
func (c *AgentConnection) IdentityMismatch() (*certs.IdentityMismatchError, bool) {
	if c.device != nil {
		var mismatch *wendy.DeviceIdentityMismatchError
		if errors.As(c.device.Evidence().IdentityMismatch, &mismatch) {
			return &certs.IdentityMismatchError{WantOrg: mismatch.WantOrg, WantAsset: mismatch.WantAsset, WantPrincipal: mismatch.WantPrincipal, GotOrg: mismatch.GotOrg, GotAsset: mismatch.GotAsset, GotPrincipal: mismatch.GotPrincipal}, true
		}
		return nil, false
	}
	return nil, false
}

// PinMismatch reports the SPKI pin-store rejection this connection hit, if any.
// Like IdentityMismatch it tells the ladder that the peer is the problem — the
// key behind a device's asset identity changed — so no other certificate or
// port can help, and the message the user needs is the one naming
// `wendy device unpin`, not gRPC's handshake wrapper.
func (c *AgentConnection) PinMismatch() (*devicepin.PinMismatchError, bool) {
	if c.device != nil {
		var mismatch *devicepin.PinMismatchError
		ok := errors.As(c.device.Evidence().PinMismatch, &mismatch)
		return mismatch, ok
	}
	return nil, false
}

func newAgentConnection(conn *grpc.ClientConn) *AgentConnection {
	return &AgentConnection{
		Conn:                 conn,
		AgentService:         agentpb.NewWendyAgentServiceClient(conn),
		ContainerService:     agentpb.NewWendyContainerServiceClient(conn),
		ShellService:         agentpb.NewWendyShellServiceClient(conn),
		AudioService:         agentpb.NewWendyAudioServiceClient(conn),
		AudioServiceV2:       agentpbv2.NewWendyAudioServiceClient(conn),
		VideoService:         agentpb.NewWendyVideoServiceClient(conn),
		ProvisioningService:  agentpb.NewWendyProvisioningServiceClient(conn),
		TelemetryService:     agentpb.NewWendyTelemetryServiceClient(conn),
		FileSyncService:      agentpb.NewWendyFileSyncServiceClient(conn),
		TimeSyncService:      agentpbv2.NewWendyTimeSyncServiceClient(conn),
		BuildService:         agentpbv2.NewWendyBuildServiceClient(conn),
		SensorPairingService: agentpbv2.NewWendySensorPairingServiceClient(conn),
		UnitreeService:       agentpbv2.NewWendyUnitreeServiceClient(conn),
		RobotArmService:      agentpbv2.NewWendyRobotArmServiceClient(conn),
		DriverService:        agentpbv2.NewWendyDriverServiceClient(conn),
		DataService:          agentpbv2.NewDataServiceClient(conn),

		CalibratedFrameService: agentpbv2.NewWendyCalibratedFrameServiceClient(conn),
	}
}

func NewFromConn(conn *grpc.ClientConn) *AgentConnection {
	return newAgentConnection(conn)
}

// linkLocalDialFn is a seam over linklocal.Dial for tests.
var linkLocalDialFn = linklocal.Dial
