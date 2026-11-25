package grpcclient

import (
	"context"
	"errors"
	"sync"
	"time"

	"google.golang.org/grpc/connectivity"
)

// ConnectionPool shares channels, not RPC slots. Every acquired handle can run
// concurrent calls; Close only releases its reference. The zero value is ready.
type ConnectionPool struct {
	mu      sync.Mutex
	entries map[string]*pooledConnection
	all     map[*pooledConnection]struct{}
	closed  bool
	done    chan struct{}
}

type pooledConnection struct {
	ready          chan struct{}
	conn           *AgentConnection
	err            error
	cancel         context.CancelFunc
	users          int
	timer          *time.Timer
	idleGeneration uint64
}

const connectionIdleTimeout = 30 * time.Second

func (p *ConnectionPool) Acquire(ctx context.Context, key string, dial func(context.Context) (*AgentConnection, error)) (*AgentConnection, error) {
	return p.acquire(ctx, key, dial)
}

// Lookup retains an existing channel or joins its in-flight setup without
// starting a new dial. A miss returns (nil, nil).
func (p *ConnectionPool) Lookup(ctx context.Context, key string) (*AgentConnection, error) {
	return p.acquire(ctx, key, nil)
}

// Refresh replaces stale, a channel the pool returned for key, without
// interrupting its current users. Every refresh of the same stale channel
// shares one replacement, including after that replacement is ready.
func (p *ConnectionPool) Refresh(ctx context.Context, key string, stale *AgentConnection, dial func(context.Context) (*AgentConnection, error)) (*AgentConnection, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, context.Canceled
	}
	if e := p.entries[key]; e != nil && e.holds(stale) {
		delete(p.entries, key)
		if e.users == 0 {
			p.closeEntry(e)
		}
	}
	p.mu.Unlock()
	return p.acquire(ctx, key, dial)
}

// holds reports whether e's ready channel is conn or one of its handles.
func (e *pooledConnection) holds(conn *AgentConnection) bool {
	return e.conn != nil && conn != nil && (e.conn == conn || (conn.Conn != nil && e.conn.Conn == conn.Conn))
}

func (p *ConnectionPool) acquire(ctx context.Context, key string, dial func(context.Context) (*AgentConnection, error)) (*AgentConnection, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, context.Canceled
	}
	if p.done == nil {
		p.done = make(chan struct{})
	}
	e := p.entries[key]
	if e != nil && e.conn != nil && e.conn.Conn != nil {
		state := e.conn.Conn.GetState()
		if state == connectivity.Shutdown || state == connectivity.TransientFailure {
			// Existing users retain their channel while a new caller reconnects.
			delete(p.entries, key)
			if e.users == 0 {
				p.closeEntry(e)
			}
			e = nil
		}
	}
	if e == nil {
		if dial == nil {
			p.mu.Unlock()
			return nil, nil
		}
		if p.entries == nil {
			p.entries = make(map[string]*pooledConnection)
			p.all = make(map[*pooledConnection]struct{})
		}
		life, cancel := context.WithCancel(context.WithoutCancel(ctx))
		e = &pooledConnection{ready: make(chan struct{}), cancel: cancel}
		p.entries[key] = e
		p.all[e] = struct{}{}
		go p.connect(life, key, e, dial)
	}
	if e.timer != nil {
		e.timer.Stop()
		e.timer = nil
	}
	e.idleGeneration++
	e.users++
	done := p.done
	p.mu.Unlock()
	select {
	case <-done:
		p.release(key, e)
		return nil, context.Canceled
	case <-ctx.Done():
		p.release(key, e)
		return nil, ctx.Err()
	case <-e.ready:
	}
	p.mu.Lock()
	conn, err := e.conn, e.err
	if p.closed {
		err = context.Canceled
	} else if ctx.Err() != nil {
		err = ctx.Err()
	}
	p.mu.Unlock()
	if err != nil {
		p.release(key, e)
		return nil, err
	}
	return conn.sharedHandle(func() error { p.release(key, e); return nil }), nil
}

func (p *ConnectionPool) connect(ctx context.Context, key string, e *pooledConnection, dial func(context.Context) (*AgentConnection, error)) {
	// Bound setup without tying a successful channel to its first caller.
	timer := time.AfterFunc(65*time.Second, e.cancel)
	conn, err := dial(ctx)
	timer.Stop()
	if err == nil {
		err = ctx.Err()
	}
	if err == nil && conn == nil {
		err = errors.New("device connector returned no connection")
	}
	p.mu.Lock()
	if p.closed || p.entries[key] != e {
		err = context.Canceled
	}
	if err != nil {
		if p.entries[key] == e {
			delete(p.entries, key)
		}
		e.cancel()
	} else {
		e.conn = conn
	}
	e.err = err
	close(e.ready)
	p.mu.Unlock()
	if err != nil && conn != nil {
		conn.Close()
	}
}

func (p *ConnectionPool) release(key string, e *pooledConnection) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e.users--
	if e.users != 0 {
		return
	}
	if e.conn == nil || p.entries[key] != e || p.closed {
		if p.entries[key] == e {
			delete(p.entries, key)
		}
		p.closeEntry(e)
		return
	}
	e.idleGeneration++
	generation := e.idleGeneration
	e.timer = time.AfterFunc(connectionIdleTimeout, func() { p.expire(key, e, generation) })
}

func (p *ConnectionPool) expire(key string, e *pooledConnection, generation uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e.users == 0 && p.entries[key] == e && e.idleGeneration == generation {
		delete(p.entries, key)
		p.closeEntry(e)
	}
}

// Caller holds p.mu. Network shutdown never blocks unrelated acquisitions.
func (p *ConnectionPool) closeEntry(e *pooledConnection) {
	delete(p.all, e)
	if e.timer != nil {
		e.timer.Stop()
	}
	e.cancel()
	if e.conn != nil {
		go e.conn.Close()
	}
}

// Close retires an account generation, including in-flight setup and active RPCs.
func (p *ConnectionPool) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	if p.done != nil {
		close(p.done)
	}
	var connections []*AgentConnection
	for e := range p.all {
		if e.timer != nil {
			e.timer.Stop()
		}
		e.cancel()
		if e.conn != nil {
			connections = append(connections, e.conn)
		}
	}
	clear(p.entries)
	clear(p.all)
	p.mu.Unlock()
	for _, conn := range connections {
		conn.Close()
	}
}

// Copy service clients and immutable transport metadata, never mutexes or the
// owner's closers. Helpers added by one consumer belong to that handle alone.
func (c *AgentConnection) sharedHandle(release func() error) *AgentConnection {
	h := &AgentConnection{
		Conn: c.Conn, Host: c.Host, MeshHost: c.MeshHost, Addr: c.Addr,
		DevicePinKey: c.DevicePinKey, NoSavedIdentity: c.NoSavedIdentity,
		SimulatorName: c.SimulatorName, IsMTLS: c.IsMTLS, IsSessionProxy: c.IsSessionProxy,
		CertInfo: c.CertInfo, RegistryDialer: c.RegistryDialer, IsCloudV2: c.IsCloudV2,
		OperatorMeshTarget: c.OperatorMeshTarget, AppDialer: c.AppDialer, Reconnect: c.Reconnect,
		AgentService: c.AgentService, ContainerService: c.ContainerService, ShellService: c.ShellService,
		AudioService: c.AudioService, AudioServiceV2: c.AudioServiceV2, VideoService: c.VideoService,
		ProvisioningService: c.ProvisioningService, TelemetryService: c.TelemetryService,
		FileSyncService: c.FileSyncService, TimeSyncService: c.TimeSyncService, BuildService: c.BuildService,
		SensorPairingService: c.SensorPairingService, UnitreeService: c.UnitreeService,
		RobotArmService: c.RobotArmService, DriverService: c.DriverService, McuService: c.McuService,
		DataService: c.DataService, CalibratedFrameService: c.CalibratedFrameService,
		observedServerOrg: c.observedServerOrg, observedServerTenant: c.observedServerTenant,
		observedServerIdentity: c.observedServerIdentity, identityMismatch: c.identityMismatch,
		pinMismatch: c.pinMismatch, tcpDial: c.tcpDial, release: release,
		IsLocalOperator: c.IsLocalOperator, notLocalDevice: c.notLocalDevice, localDeviceVerified: c.localDeviceVerified,
		deviceRefusedKey: c.deviceRefusedKey,
	}
	h.cachedAgentVersion.Store(c.cachedAgentVersion.Load())
	return h
}
