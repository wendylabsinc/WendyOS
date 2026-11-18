package commands

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"sync"

	"github.com/wendylabsinc/wendy/go/internal/shared/cloudrelay"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
)

// TunnelSession owns authorization transports for one foreground account
// scope. It shares setup, never tunnel grants, session keys, or device streams.
// Call SetActive(false) before changing accounts and on background/shutdown.
type TunnelSession struct {
	mu      sync.Mutex
	current *tunnelGeneration
}
type tunnelGeneration struct {
	mu        sync.Mutex
	closed    bool
	entries   map[[32]byte]*tunnelAuthorization
	transient map[*cloudrelay.Verifier]struct{}
}
type tunnelContextKey struct{}
type tunnelAuthorization struct {
	conn       *grpc.ClientConn
	verifier   *cloudrelay.Verifier
	sign       func([]byte) ([]byte, error)
	closeRelay func()
}

func (s *TunnelSession) SetActive(active bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if active {
		if s.current == nil {
			s.current = &tunnelGeneration{entries: make(map[[32]byte]*tunnelAuthorization)}
		}
		return
	}
	if g := s.current; g != nil {
		g.mu.Lock()
		g.closed = true
		for _, entry := range g.entries {
			entry.verifier.Close()
			entry.conn.Close()
		}
		for verifier := range g.transient {
			verifier.Close()
		}
		g.transient = nil
		g.entries = nil
		g.mu.Unlock()
		s.current = nil
	}
}

// Context captures the current generation. Old handles cannot acquire a new
// account's transport even after the session is resumed.
func (s *TunnelSession) Context(ctx context.Context) context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	g := s.current
	if g == nil {
		g = &tunnelGeneration{closed: true}
	}
	return context.WithValue(ctx, tunnelContextKey{}, g)
}
func carryTunnelSession(ctx, source context.Context) context.Context {
	if g, ok := source.Value(tunnelContextKey{}).(*tunnelGeneration); ok {
		return context.WithValue(ctx, tunnelContextKey{}, g)
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
	g, _ := ctx.Value(tunnelContextKey{}).(*tunnelGeneration)
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
	var replacing *tunnelAuthorization
	if cache {
		if entry := g.entries[key]; entry != nil {
			// A connection that failed to reconnect refuses requests until its
			// backoff expires, up to two minutes after Cloud is reachable
			// again. Replace it so this dial tries now, as an unshared one would.
			if state := entry.conn.GetState(); state != connectivity.TransientFailure && state != connectivity.Shutdown {
				return entry, func() {}, nil
			}
			replacing = entry
		}
	}
	entry, err := newTunnelAuthorization(auth, issuer)
	if err != nil {
		return nil, nil, err
	}
	if replacing != nil {
		// The exact credential/issuer snapshot is unchanged. Keep its relay owner
		// so healthy tunnel sessions survive authorization reconnects and remain
		// owned by this generation until account change or shutdown.
		entry.verifier.Close()
		entry.verifier = replacing.verifier
		replacing.conn.Close()
		g.entries[key] = entry
		return entry, func() {}, nil
	}
	// Bound retained resources without evicting a transport used by another dial.
	if cache && len(g.entries) < 4 {
		g.entries[key] = entry
		return entry, func() {}, nil
	}
	// Uncached authorizations still belong to the account generation while
	// their returned tunnel is live. Setup completion closes only the Cloud
	// authorization connection; tunnel completion releases its relay owner.
	if g != nil {
		if g.transient == nil {
			g.transient = make(map[*cloudrelay.Verifier]struct{})
		}
		g.transient[entry.verifier] = struct{}{}
	}
	var once sync.Once
	entry.closeRelay = func() {
		once.Do(func() {
			if g != nil {
				g.mu.Lock()
				delete(g.transient, entry.verifier)
				g.mu.Unlock()
			}
			entry.verifier.Close()
		})
	}
	return entry, func() { entry.conn.Close() }, nil
}

func openAuthorizedTunnel(ctx context.Context, auth *config.AuthConfig, assetID, service string) (net.Conn, error) {
	entry, release, err := tunnelAuthorizationFor(ctx, auth)
	if err != nil {
		return nil, err
	}
	defer release()
	retained := false
	defer func() {
		if !retained && entry.closeRelay != nil {
			entry.closeRelay()
		}
	}()
	// Always request a fresh grant; cloudrelay also creates fresh tunnel keys.
	cloudCtx, err := cloudContext(ctx, auth)
	if err != nil {
		return nil, err
	}
	conn, err := cloudrelay.OpenTCP(ctx, cloudCtx, entry.conn, entry.verifier, assetID, service, entry.sign)
	if err != nil {
		return nil, err
	}
	retained = true
	if entry.closeRelay != nil {
		stop := context.AfterFunc(ctx, entry.closeRelay)
		return &ownedTunnelConn{Conn: conn, release: func() { stop(); entry.closeRelay() }}, nil
	}
	return conn, nil
}

// An uncached tunnel owns its verifier until its stream ends. Generation
// shutdown can also close that verifier, independently of consumer cleanup.
type ownedTunnelConn struct {
	net.Conn
	release func()
}

func (c *ownedTunnelConn) Close() error { err := c.Conn.Close(); c.release(); return err }
func (c *ownedTunnelConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if terminalTunnelError(err) {
		c.release()
	}
	return n, err
}
func (c *ownedTunnelConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if terminalTunnelError(err) {
		c.release()
	}
	return n, err
}

func terminalTunnelError(err error) bool {
	// Session.Conn closes its pipe on relay EOF/half-close. Deadlines on the
	// returned pipe remain recoverable and must not cancel the relay session.
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe) || errors.Is(err, net.ErrClosed)
}
