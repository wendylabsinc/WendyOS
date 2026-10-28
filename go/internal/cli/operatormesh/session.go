// Package operatormesh owns the broker's ongoing full Babel router. Carrier
// lifetimes are shorter than its routing/checkpoint and verified-directory state.
package operatormesh

import (
	"context"
	"crypto"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/meshsession"
	"github.com/wendylabsinc/wendy/go/internal/agent/mtls"
	"github.com/wendylabsinc/wendy/go/internal/agent/netwire"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/netmesh"
	"github.com/wendylabsinc/wendy/go/internal/cli/netmesh/carriers"
	"github.com/wendylabsinc/wendy/go/internal/shared/atomicfile"
	"github.com/wendylabsinc/wendy/go/internal/shared/ble/central"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/devicepin"
	"github.com/wendylabsinc/wendy/go/internal/shared/flock"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshoperator"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshoperatorlink"
)

// JoinSetupTimeout is the existing BLE TLS setup ceiling, shared by cold broker startup.
const JoinSetupTimeout = 60 * time.Second

type Options struct {
	Automatic      bool
	CarrierSources []carriers.Source
	Discover       func(context.Context) ([]Options, error)
	LeaseDuration  time.Duration
	StateDirectory string
	Certificate    config.CertificateInfo
	RouterAddress  string
	Transport      string
	BLEPSM         uint16
	RouterUUID     string
}
type Info struct {
	Address  string `json:"address" cbor:"a"`
	Router   string `json:"router" cbor:"r"`
	Revision uint64 `json:"revision" cbor:"R"`
}
type Session struct {
	agentMu      sync.Mutex
	agentClients map[meshid.Device]*grpcclient.AgentConnection
	certificate  config.CertificateInfo

	runMu           sync.Mutex
	started, closed bool
	runCancel       context.CancelFunc
	runDone         chan struct{}
	revision        *atomic.Uint64
	Endpoint        *netmesh.Network
	Node            *localmesh.Node
	options         Options
	routerConfig    meshoperatorlink.ClientConfig
	routerTLS       *tls.Config
	initialRouter   *meshoperatorlink.RouterLink
	nodeCancel      context.CancelFunc
	linkMu          sync.RWMutex
	links           map[*meshoperatorlink.RouterLink]managedCarrier
	initialOnce     sync.Once
	linkChanged     chan struct{}
	adjacentPeer    meshid.Device
	link            *meshoperatorlink.Link
	info            Info

	release func()
	once    sync.Once
}

func (s *Session) Info() Info {
	info := s.info
	s.linkMu.RLock()
	if s.adjacentPeer.IsValid() {
		info.Router = s.adjacentPeer.Principal()
	}
	s.linkMu.RUnlock()
	info.Revision = s.revision.Load()
	if s.Node != nil {
		for _, m := range s.Node.Snapshot().Devices {
			if m.Owner != nil && m.Owner.Principal == s.Node.Credentials.Device.Principal() && m.Owner.Instance == s.Node.Credentials.Device.UUID() {
				info.Revision = m.Revision
				break
			}
		}
	}
	return info
}
func (s *Session) Close() error {
	s.once.Do(func() {
		s.runMu.Lock()
		s.closed = true
		cancel, done := s.runCancel, s.runDone
		s.runMu.Unlock()
		if cancel != nil {
			cancel()
		}
		if s.nodeCancel != nil {
			s.nodeCancel()
		}
		s.linkMu.RLock()
		link := s.link
		bound := make([]*meshoperatorlink.RouterLink, 0, len(s.links))
		for carrier := range s.links {
			bound = append(bound, carrier)
		}
		s.linkMu.RUnlock()
		for _, carrier := range bound {
			_ = carrier.Close()
		}
		if link != nil {
			_ = link.Close()
		}
		s.agentMu.Lock()
		agents := make([]*grpcclient.AgentConnection, 0, len(s.agentClients))
		for _, agent := range s.agentClients {
			agents = append(agents, agent)
		}
		s.agentMu.Unlock()
		for _, agent := range agents {
			_ = agent.Close()
		}
		s.Endpoint.Close()
		if done != nil {
			<-done
		}
		if link != nil {
			link.Wait()
		}
		s.release()
	})
	return nil
}

// Start publishes ownership before returning, so callers may immediately open
// management while the packet/control pumps start. Close joins this one driver.
func (s *Session) Start(ctx context.Context) (<-chan error, error) {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	if s.closed || s.started {
		return nil, errors.New("operator session is closed or already running")
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	result := make(chan error, 1)
	s.started = true
	s.runCancel = cancel
	s.runDone = done
	go func() {
		defer cancel()
		defer close(done)
		nodeResult := make(chan error, 1)
		go func() { nodeResult <- s.Node.Run() }()
		carriersDone := make(chan struct{})
		go func() { defer close(carriersDone); _ = s.runCarriers(ctx) }()
		var runErr error
		nodeStopped := false
		select {
		case runErr = <-nodeResult:
			nodeStopped = true
		case <-ctx.Done():
			runErr = ctx.Err()
		}
		cancel()
		s.nodeCancel()
		s.linkMu.RLock()
		live := s.link
		s.linkMu.RUnlock()
		if live != nil {
			_ = live.Close()
		}
		<-carriersDone
		if !nodeStopped {
			<-nodeResult
		}
		result <- runErr
	}()
	return result, nil
}
func (s *Session) Run(ctx context.Context) error {
	result, err := s.Start(ctx)
	if err != nil {
		return err
	}
	return <-result
}

func (s *Session) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return s.Endpoint.Dial(ctx, network, address)
}

// DialAppTLS opens the ordinary end-to-end app TLS/WAS1 session using this
// retained router's real primary identity and userspace source address.
func (s *Session) DialAppTLS(ctx context.Context, peer meshid.Device, port uint16) (net.Conn, error) {
	return meshsession.DialTLSWithDialer(ctx, s.Node.Credentials, peer,
		netip.AddrPortFrom(peer.MeshAddress(), meshsession.TLSPort), port, s.DialContext)
}

type endpointState struct {
	Version   int                     `json:"version" cbor:"v"`
	Principal string                  `json:"principal" cbor:"p"`
	Instance  meshoperator.InstanceID `json:"instance" cbor:"n"`
	Revision  uint64                  `json:"revision" cbor:"R"`
}

// reserveEndpoint holds installation ownership through the entire attachment.
// Counter persistence precedes every handshake/signature, including failed dials.
func reserveEndpoint(directory string, operator meshoperator.Operator) (endpointState, func(), error) {
	var state endpointState
	if !operator.IsValid() || directory == "" {
		return state, nil, errors.New("missing operator endpoint state directory or principal")
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		return state, nil, err
	}
	path := endpointStatePath(directory, operator)
	release, err := flock.Acquire(path+".lock", time.Second)
	if err != nil {
		return state, nil, fmt.Errorf("operator endpoint already owned or state unavailable: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			release()
		}
	}()
	file, err := os.Open(path)
	if err == nil {
		data, err := io.ReadAll(io.LimitReader(file, 2049))
		closeErr := file.Close()
		if err != nil {
			return state, nil, err
		}
		if closeErr != nil {
			return state, nil, closeErr
		}
		if len(data) > 2048 {
			return state, nil, errors.New("oversized operator endpoint state")
		}
		if state, err = decodeEndpointState(data); err != nil {
			return state, nil, err
		}
		if state.Version != 1 || state.Principal != operator.Principal() {
			return state, nil, errors.New("operator endpoint state identity/version differs")
		}
	} else if errors.Is(err, os.ErrNotExist) {
		state.Version = 1
		state.Principal = operator.Principal()
		state.Instance, err = meshoperator.NewInstanceID()
		if err != nil {
			return state, nil, err
		}
	} else {
		return state, nil, err
	}
	if state.Revision == ^uint64(0) {
		return state, nil, errors.New("operator endpoint revision exhausted")
	}
	state.Revision++
	data, err := netwire.Marshal(state)
	if err != nil {
		return state, nil, err
	}
	if err = atomicfile.Write(path, data, 0600); err != nil {
		return state, nil, err
	}
	ok = true
	return state, release, nil
}

func endpointStatePath(directory string, operator meshoperator.Operator) string {
	key := sha256.Sum256([]byte(operator.Principal()))
	return filepath.Join(directory, fmt.Sprintf("operator-endpoint-%x.cbor", key))
}
func advanceEndpoint(directory string, operator meshoperator.Operator, state *endpointState) (uint64, error) {
	if state.Revision == ^uint64(0) {
		return 0, errors.New("operator endpoint revision exhausted")
	}
	next := *state
	next.Revision++
	data, err := netwire.Marshal(next)
	if err != nil {
		return 0, err
	}
	if err = atomicfile.Write(endpointStatePath(directory, operator), data, 0600); err != nil {
		return 0, err
	}
	*state = next
	return next.Revision, nil
}

// Open uses only cached selected credentials/trust. It performs no login,
// discovery, Cloud requests, system routing, or fallback credential selection.
func Open(ctx context.Context, options Options) (_ *Session, err error) {
	info := options.Certificate
	key, err := info.PrivateKeyPEM()
	if err != nil {
		return nil, err
	}
	certificate, err := certs.TLSKeyPair(info.PemCertificate, info.PemCertificateChain, key)
	if err != nil {
		return nil, err
	}
	signer, ok := certificate.PrivateKey.(crypto.Signer)
	if !ok {
		return nil, errors.New("operator key cannot sign")
	}
	anchors, err := mtls.PeerTrustAnchors(info.PemCertificateChain)
	if err != nil {
		return nil, err
	}
	roots := make([]*x509.Certificate, len(anchors))
	for i, der := range anchors {
		roots[i], err = x509.ParseCertificate(der)
		if err != nil {
			return nil, err
		}
	}
	tenant, err := meshid.ParseTenant(info.TenantUUID())
	if err != nil {
		return nil, err
	}
	identity, err := meshoperator.VerifyPrimary(certificate.Certificate, roots, tenant, time.Now())
	if err != nil {
		return nil, err
	}
	var router meshid.Device
	automatic := options.Automatic && options.RouterUUID == "" && options.RouterAddress == ""
	if !automatic {
		router, err = meshid.ParseDevice(tenant, options.RouterUUID)
		if err != nil {
			return nil, err
		}
	}
	state, release, err := reserveEndpoint(options.StateDirectory, identity.Operator())
	if err != nil {
		return nil, err
	}
	success := false
	defer func() {
		if !success {
			release()
		}
	}()
	var revision atomic.Uint64
	revision.Store(state.Revision)
	cfg := meshoperatorlink.ClientConfig{Identity: identity, Signer: signer, Instance: state.Instance, Router: router, Roots: roots}
	var tlsConfig *tls.Config
	var secure *tls.Conn
	if !automatic {
		tlsConfig, err = meshoperatorlink.ClientTLSConfig(cfg, certificate)
		if err != nil {
			return nil, err
		}
		tlsConfig.NextProtos = []string{meshoperatorlink.RouterALPN}
		pins, e := devicepin.Open(options.StateDirectory)
		if e != nil {
			return nil, e
		}
		verify := tlsConfig.VerifyConnection
		tlsConfig.VerifyConnection = func(state tls.ConnectionState) error {
			if err := verify(state); err != nil {
				return err
			}
			if err := pins.CheckAndUpdate(state.PeerCertificates[0], router.Principal()); err != nil {
				var blocking certs.BlockingPinError
				if errors.As(err, &blocking) {
					return err
				}
			}
			return nil
		}
		secure, err = dialOperatorTLS(ctx, options, tlsConfig)
		if err != nil {
			return nil, err
		}
	}
	closeInitial := func() {
		if secure != nil {
			_ = secure.NetConn().Close()
		}
	}
	credentials, err := localmesh.NewOperatorCredentials(info.PemCertificate, info.PemCertificateChain, key, uuid.UUID(state.Instance))
	if err != nil {
		closeInitial()
		return nil, err
	}
	endpoint, err := netmesh.NewNetwork(credentials.Device)
	if err != nil {
		closeInitial()
		return nil, err
	}
	directory := filepath.Join(options.StateDirectory, fmt.Sprintf("operator-router-%x", sha256.Sum256([]byte(identity.Operator().Principal()))))
	if err = os.MkdirAll(directory, 0700); err != nil {
		_ = endpoint.Close()
		closeInitial()
		return nil, err
	}
	parent, err := os.Open(options.StateDirectory)
	if err == nil {
		err = parent.Sync()
		_ = parent.Close()
	}
	if err != nil {
		_ = endpoint.Close()
		closeInitial()
		return nil, err
	}
	nodeCtx, nodeCancel := context.WithCancel(ctx)
	node, err := localmesh.NewNodeWithNetwork(nodeCtx, directory, credentials, "wendy CLI", 0, endpoint)
	if err != nil {
		nodeCancel()
		_ = endpoint.Close()
		closeInitial()
		return nil, err
	}
	var link *meshoperatorlink.RouterLink
	if secure != nil {
		link, err = meshoperatorlink.RouterClient(nodeCtx, secure, cfg)
		if err != nil {
			nodeCancel()
			_ = endpoint.Close()
			return nil, err
		}
	}
	session := &Session{certificate: options.Certificate, revision: &revision, Endpoint: endpoint, Node: node, nodeCancel: nodeCancel, initialRouter: link, adjacentPeer: router, linkChanged: make(chan struct{}, 1), options: options, routerConfig: cfg, routerTLS: tlsConfig, release: release, info: Info{endpoint.Address().String(), router.Principal(), state.Revision}}
	session.links = make(map[*meshoperatorlink.RouterLink]managedCarrier)
	if link != nil {
		session.link = link.Link
		session.links[link] = managedCarrier{peer: router, cost: session.seedCandidate().Cost}
	}
	session.agentClients = make(map[meshid.Device]*grpcclient.AgentConnection)
	success = true
	return session, nil
}

// The operator role and proof exchange are identical over LAN and BLE. Only
// socket setup and the finite TLS flight budget depend on the carrier.
func dialOperatorTLS(ctx context.Context, options Options, config *tls.Config) (*tls.Conn, error) {
	switch options.Transport {
	case "", "tcp":
		if options.BLEPSM != 0 {
			return nil, errors.New("BLE PSM requires the ble transport")
		}
		setup, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		raw, err := (&tls.Dialer{Config: config}).DialContext(setup, "tcp", options.RouterAddress)
		if err != nil {
			return nil, err
		}
		return raw.(*tls.Conn), nil
	case "ble":
		psm := options.BLEPSM
		if psm == 0 {
			psm = 0x81
		}
		stream, err := dialOperatorBLE(ctx, options.RouterAddress, psm)
		if err != nil {
			return nil, err
		}
		config = config.Clone()
		config.CurvePreferences = []tls.CurveID{tls.X25519}
		secure := tls.Client(stream, config)
		setup, cancel := context.WithTimeout(ctx, JoinSetupTimeout)
		defer cancel()
		if err = secure.HandshakeContext(setup); err != nil {
			stream.Close()
			return nil, fmt.Errorf("BLE operator TLS: %w", err)
		}
		return secure, nil
	default:
		return nil, errors.New("operator transport must be tcp or ble")
	}
}

var dialOperatorBLE = central.DialL2CAPContext

func (s *Session) Snapshot() localmesh.NodeSnapshot {
	if s == nil || s.Node == nil {
		return localmesh.NodeSnapshot{}
	}
	return s.Node.Snapshot()
}

func (s *Session) AdjacentLive() bool {
	s.linkMu.RLock()
	defer s.linkMu.RUnlock()
	return s.link != nil && s.link.Live()
}

func (s *Session) AdjacentPeer() (meshid.Device, bool) {
	s.linkMu.RLock()
	defer s.linkMu.RUnlock()
	if s.link == nil || !s.link.Live() {
		return meshid.Device{}, false
	}
	return s.adjacentPeer, s.adjacentPeer.IsValid()
}

func (s *Session) HasRecentTransit(window time.Duration) bool {
	return s != nil && s.Endpoint != nil && s.Endpoint.HasRecentTransit(window)
}
