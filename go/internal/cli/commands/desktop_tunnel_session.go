package commands

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net"
	"os"
	"sync"

	"github.com/wendylabsinc/wendy/go/internal/shared/cloudrelay"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"google.golang.org/grpc"
)

// DesktopTunnelSession owns authorization transports for one foreground account
// scope. It shares setup, never tunnel grants, session keys, or device streams.
// Call SetActive(false) before changing accounts and on background/shutdown.
type DesktopTunnelSession struct {
	mu      sync.Mutex
	current *desktopTunnelGeneration
}
type desktopTunnelGeneration struct {
	mu      sync.Mutex
	closed  bool
	entries map[[32]byte]*tunnelAuthorization
}
type desktopTunnelContextKey struct{}
type tunnelAuthorization struct {
	conn     *grpc.ClientConn
	verifier *cloudrelay.Verifier
	sign     func([]byte) ([]byte, error)
}

func (s *DesktopTunnelSession) SetActive(active bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if active {
		if s.current == nil {
			s.current = &desktopTunnelGeneration{entries: make(map[[32]byte]*tunnelAuthorization)}
		}
		return
	}
	if g := s.current; g != nil {
		g.mu.Lock()
		g.closed = true
		for _, entry := range g.entries {
			entry.conn.Close()
		}
		g.entries = nil
		g.mu.Unlock()
		s.current = nil
	}
}

// Context captures the current generation. Old handles cannot acquire a new
// account's transport even after the session is resumed.
func (s *DesktopTunnelSession) Context(ctx context.Context) context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	g := s.current
	if g == nil {
		g = &desktopTunnelGeneration{closed: true}
	}
	return context.WithValue(ctx, desktopTunnelContextKey{}, g)
}
func carryDesktopTunnelSession(ctx, source context.Context) context.Context {
	if g, ok := source.Value(desktopTunnelContextKey{}).(*desktopTunnelGeneration); ok {
		return context.WithValue(ctx, desktopTunnelContextKey{}, g)
	}
	return ctx
}

func newTunnelAuthorization(auth *config.AuthConfig, issuer string) (*tunnelAuthorization, error) {
	snapshot := *auth
	snapshot.Certificates = append([]config.CertificateInfo(nil), auth.Certificates...)
	auth = &snapshot
	sign, err := tunnelPrincipalSigner(auth)
	if err != nil {
		return nil, err
	}
	conn, err := dialCloudGRPC(auth)
	if err != nil {
		return nil, err
	}
	return &tunnelAuthorization{conn: conn, verifier: &cloudrelay.Verifier{Issuer: issuer}, sign: sign}, nil
}
func tunnelAuthorizationFor(ctx context.Context, auth *config.AuthConfig) (*tunnelAuthorization, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	issuer, err := cloudrelay.Issuer(auth.CloudGRPC, os.Getenv("WENDY_CLOUD_GRANT_ISSUER"))
	if err != nil {
		return nil, nil, err
	}
	g, _ := ctx.Value(desktopTunnelContextKey{}).(*desktopTunnelGeneration)
	if g != nil {
		g.mu.Lock()
		defer g.mu.Unlock()
		if g.closed {
			return nil, nil, context.Canceled
		}
	}
	// Hash the full credential snapshot (never log it). Endpoint, tenant,
	// certificate, token and issuer changes cannot reuse an old transport.
	encoded, err := json.Marshal(struct {
		Auth   *config.AuthConfig
		Issuer string
	}{auth, issuer})
	if err != nil {
		return nil, nil, err
	}
	key := sha256.Sum256(encoded)
	clear(encoded)
	// File overrides may change without their paths changing; don't cache them.
	cache := g != nil && os.Getenv("WENDY_TUNNEL_SIGNING_CERT") == "" && os.Getenv("WENDY_TUNNEL_SIGNING_KEY") == ""
	if cache {
		if entry := g.entries[key]; entry != nil {
			return entry, func() {}, nil
		}
	}
	entry, err := newTunnelAuthorization(auth, issuer)
	if err != nil {
		return nil, nil, err
	}
	// Bound retained resources without evicting a transport used by another dial.
	if cache && len(g.entries) < 4 {
		g.entries[key] = entry
		return entry, func() {}, nil
	}
	return entry, func() { entry.conn.Close() }, nil
}

func openAuthorizedTunnel(ctx context.Context, auth *config.AuthConfig, assetID, service string) (net.Conn, error) {
	entry, release, err := tunnelAuthorizationFor(ctx, auth)
	if err != nil {
		return nil, err
	}
	defer release()
	// Always request a fresh grant; cloudrelay also creates fresh tunnel keys.
	cloudCtx, err := cloudContext(ctx, auth)
	if err != nil {
		return nil, err
	}
	return cloudrelay.OpenTCP(ctx, cloudCtx, entry.conn, entry.verifier, assetID, service, entry.sign)
}
