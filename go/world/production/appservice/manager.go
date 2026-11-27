package appservice

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wendylabsinc/wendy/go/world/production/hostpath"
	"github.com/wendylabsinc/wendy/go/world/production/rpctrace"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"golang.org/x/time/rate"
	"google.golang.org/grpc"
)

type Manager struct {
	cfg      Config
	mu       sync.Mutex
	services map[string]*service
	closed   bool
	// Tests can commission fake kernel credentials on a non-Linux host. No
	// exported configuration can disable the Linux kernel credential check.
	peerCredentials func(*net.UnixConn) (Peer, error)
	peerPin         func(*net.UnixConn) (peerPin, error)
}

type service struct {
	manager         *Manager
	scope           serviceorigin.Scope
	scopeJSON       string
	publisher       Publisher
	mount           Mount
	ctx             context.Context
	cancel          context.CancelFunc
	listener        *net.UnixListener
	server          *grpc.Server
	stopOnce        sync.Once
	closed          atomic.Bool
	watcherReason   atomic.Uint32
	connectionsMu   sync.Mutex
	connections     map[*trackedConn]struct{}
	admissionMu     sync.Mutex
	rpcs            chan struct{}
	messages, bytes *rate.Limiter
}

func New(cfg Config) (*Manager, error) {
	if cfg.Directory == "" || !filepath.IsAbs(cfg.Directory) || cfg.UID != os.Geteuid() || cfg.GID < 0 || cfg.Factory == nil || cfg.PeerCheck == nil || cfg.Accept == nil {
		return nil, errors.New("explicit agent-owned directory, ownership, publisher, peer check and acceptance sink required")
	}
	var err error
	if cfg.Limits, err = defaults(cfg.Limits); err != nil {
		return nil, err
	}
	if err = hostpath.EnsureDirectory(cfg.Directory, 0o750, true); err != nil {
		return nil, err
	}
	if err = os.Chmod(cfg.Directory, 0o750); err != nil {
		return nil, err
	}
	if err = os.Chown(cfg.Directory, cfg.UID, cfg.GID); err != nil {
		return nil, err
	}
	return &Manager{cfg: cfg, services: map[string]*service{}, peerCredentials: readPeerCredentials, peerPin: pinOriginalPeer}, nil
}

func scopeKey(s serviceorigin.Scope) string { return s.App + "\x00" + s.Service }

func canonicalScope(s serviceorigin.Scope) (serviceorigin.Scope, string, error) {
	if err := s.Validate(); err != nil {
		return s, "", err
	}
	s.Publish = append([]serviceorigin.PublishScope(nil), s.Publish...)
	sort.Slice(s.Publish, func(i, j int) bool {
		a, b := s.Publish[i], s.Publish[j]
		if a.ComponentType != b.ComponentType {
			return a.ComponentType < b.ComponentType
		}
		return a.Stream < b.Stream
	})
	b, err := json.Marshal(s)
	return s, string(b), err
}

// Ensure is idempotent for an unchanged valid scope. A replacement receives a
// new directory, so an existing container mount cannot inherit its authority.
// The caller must remount/restart the service after any replacement.
func (m *Manager) Ensure(ctx context.Context, scope serviceorigin.Scope) (Mount, error) {
	if ctx == nil {
		return Mount{}, errors.New("context required")
	}
	if err := ctx.Err(); err != nil {
		return Mount{}, err
	}
	scope, encoded, err := canonicalScope(scope)
	if err != nil {
		return Mount{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return Mount{}, ErrClosed
	}
	key := scopeKey(scope)
	if old := m.services[key]; old != nil {
		if old.scopeJSON == encoded && !old.closed.Load() {
			if err = old.publisher.Validate(ctx); err == nil {
				return old.mount, nil
			}
			if ctx.Err() != nil {
				return Mount{}, ctx.Err()
			}
			old.stop()
			delete(m.services, key)
			return Mount{}, err
		}
		old.stop()
		delete(m.services, key)
	}
	publisher, err := m.cfg.Factory(ctx, scope)
	if err != nil {
		return Mount{}, err
	}
	if publisher == nil {
		return Mount{}, errors.New("publisher factory returned no authority")
	}
	if publisher.Done() == nil {
		_ = publisher.Close()
		return Mount{}, errors.New("publisher invalidation signal required")
	}
	if err = publisher.Validate(ctx); err != nil {
		_ = publisher.Close()
		return Mount{}, err
	}
	dir, err := os.MkdirTemp(m.cfg.Directory, "service-")
	if err != nil {
		_ = publisher.Close()
		return Mount{}, err
	}
	clean := func() { _ = publisher.Close(); _ = os.RemoveAll(dir) }
	if err = os.Chmod(dir, 0o750); err != nil {
		clean()
		return Mount{}, err
	}
	if err = os.Chown(dir, m.cfg.UID, m.cfg.GID); err != nil {
		clean()
		return Mount{}, err
	}
	path := filepath.Join(dir, SocketFilename)
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		clean()
		return Mount{}, err
	}
	if err = os.Chmod(path, 0o660); err != nil {
		_ = listener.Close()
		clean()
		return Mount{}, err
	}
	if err = os.Chown(path, m.cfg.UID, m.cfg.GID); err != nil {
		_ = listener.Close()
		clean()
		return Mount{}, err
	}
	life, cancel := context.WithCancel(context.Background())
	s := &service{manager: m, scope: scope, scopeJSON: encoded, publisher: publisher, mount: Mount{dir, path}, ctx: life, cancel: cancel, listener: listener, connections: map[*trackedConn]struct{}{}, rpcs: make(chan struct{}, m.cfg.Limits.MaxConcurrentRPCs), messages: rate.NewLimiter(rate.Limit(m.cfg.Limits.MessagesPerSecond), m.cfg.Limits.MessageBurst), bytes: rate.NewLimiter(rate.Limit(m.cfg.Limits.BytesPerSecond), m.cfg.Limits.ByteBurst)}
	s.server = newServer(s)
	m.services[key] = s
	go func() { _ = s.server.Serve(&serviceListener{s}); s.stop() }()
	go s.watchAuthority()
	return s.mount, nil
}

func (m *Manager) Release(app, serviceName string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := app + "\x00" + serviceName
	if s := m.services[key]; s != nil {
		s.stop()
		delete(m.services, key)
	}
}
func (m *Manager) ReleaseApp(app string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for key, s := range m.services {
		if s.scope.App == app {
			s.stop()
			delete(m.services, key)
		}
	}
}
func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil
	}
	m.closed = true
	for key, s := range m.services {
		s.stop()
		delete(m.services, key)
	}
	return nil
}

func (s *service) stop() {
	s.stopOnce.Do(func() {
		s.closed.Store(true)
		s.cancel()
		_ = s.listener.Close()
		s.connectionsMu.Lock()
		connections := make([]*trackedConn, 0, len(s.connections))
		for c := range s.connections {
			connections = append(connections, c)
		}
		s.connectionsMu.Unlock()
		for _, c := range connections {
			_ = c.Close()
		}
		// Connections close before waiting for an already-entered sink commit.
		s.server.Stop()
		s.admissionMu.Lock()
		_ = s.publisher.Close()
		s.admissionMu.Unlock()
		_ = os.RemoveAll(s.mount.Directory)
	})
}

func (s *service) watchAuthority() {
	ticker := time.NewTicker(s.manager.cfg.Limits.AuthorityCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-s.publisher.Done():
			s.rememberAuthorityWatcher(rpctrace.ReasonPublisherDone)
			s.stop()
			s.traceWatcher(rpctrace.MethodAuthorityWatch, rpctrace.PointAuthorityWatcher, pinObservation{reason: rpctrace.ReasonPublisherDone}, true)
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(s.ctx, s.manager.cfg.Limits.RequestTimeout)
			err := s.publisher.Validate(ctx)
			reason := rpctrace.ReasonPublisherRefused
			if s.manager.cfg.Trace != nil && err != nil {
				if contextErr := ctx.Err(); contextErr != nil {
					reason = contextRefusal(contextErr)
				}
			}
			cancel()
			if err != nil {
				s.rememberAuthorityWatcher(reason)
				s.stop()
				s.traceWatcher(rpctrace.MethodAuthorityWatch, rpctrace.PointAuthorityWatcher, pinObservation{reason: reason}, true)
				return
			}
			s.closeDepartedPeers()
		}
	}
}

type trackedConn struct {
	*net.UnixConn
	service       *service
	pin           peerPin
	once          sync.Once
	watcherReason atomic.Uint32
}

func (c *trackedConn) Close() error {
	var err error
	c.once.Do(func() {
		err = c.UnixConn.Close()
		_ = c.pin.Close()
		c.service.connectionsMu.Lock()
		delete(c.service.connections, c)
		c.service.connectionsMu.Unlock()
	})
	return err
}

type serviceListener struct{ *service }

func (l *serviceListener) Accept() (net.Conn, error) {
	for {
		c, err := l.listener.AcceptUnix()
		if err != nil {
			return nil, err
		}
		pin, err := l.manager.peerPin(c)
		if err != nil || pin == nil {
			_ = c.Close()
			continue
		}
		l.connectionsMu.Lock()
		if l.closed.Load() || len(l.connections) >= l.manager.cfg.Limits.MaxConnections {
			l.connectionsMu.Unlock()
			_ = c.Close()
			_ = pin.Close()
			continue
		}
		tracked := &trackedConn{UnixConn: c, service: l.service, pin: pin}
		l.connections[tracked] = struct{}{}
		l.connectionsMu.Unlock()
		return tracked, nil
	}
}
func (l *serviceListener) Close() error   { return l.listener.Close() }
func (l *serviceListener) Addr() net.Addr { return l.listener.Addr() }

func (s *service) closeDepartedPeers() {
	s.connectionsMu.Lock()
	connections := make([]*trackedConn, 0, len(s.connections))
	for conn := range s.connections {
		connections = append(connections, conn)
	}
	s.connectionsMu.Unlock()
	for _, conn := range connections {
		err, observed := observePeerPin(conn.pin, s.manager.cfg.Trace != nil)
		if err != nil {
			if s.manager.cfg.Trace != nil {
				conn.watcherReason.CompareAndSwap(0, uint32(observed.reason))
			}
			_ = conn.Close()
		}
		s.traceWatcher(rpctrace.MethodPeerWatch, rpctrace.PointPeerWatcher, observed, err != nil)
	}
}
