package services

import (
	"context"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"math/rand/v2"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/unitree"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

const unitreeMaxParallelDials = 4

type unitreePairingStore interface {
	List() ([]unitree.Pairing, error)
	Save(unitree.Pairing) error
	Remove(string) error
	Clear() error
	BindOwner(unitree.PairingOwner)
}

type unitreeLink interface {
	Connected() bool
	Done() <-chan struct{}
	Close() error
}

type unitreeSession struct {
	cancel context.CancelFunc
	link   unitreeLink // guarded by UnitreeService.mu
	done   chan struct{}
}

// UnitreeService retains connections independently of the CLI's RPC lifetime.
// Only the robot key is persisted; account passwords and cloud tokens are not.
type UnitreeService struct {
	agentpbv2.UnimplementedWendyUnitreeServiceServer
	ctx       context.Context
	cancel    context.CancelFunc
	store     unitreePairingStore
	discover  func(context.Context, ...string) ([]unitree.Device, error)
	dial      func(context.Context, string, string, string) (unitreeLink, error)
	opMu      sync.Mutex
	mu        sync.RWMutex
	wg        sync.WaitGroup
	sessions  map[string]*unitreeSession
	retry     time.Duration
	owner     unitree.PairingOwner // current certificate scope; guarded by opMu
	stale     bool                 // a previous owner's store still needs clearing; guarded by opMu
	dialSlots chan struct{}
}

func NewUnitreeService(ctx context.Context, path string, owner unitree.PairingOwner) *UnitreeService {
	ctx, cancel := context.WithCancel(ctx)
	return &UnitreeService{
		ctx: ctx, cancel: cancel, store: unitree.NewStore(path, owner), owner: owner,
		discover: unitree.Discover,
		dial: func(ctx context.Context, address, token, key string) (unitreeLink, error) {
			return unitree.Connect(ctx, address, token, key)
		},
		sessions: make(map[string]*unitreeSession), retry: 10 * time.Second,
		dialSlots: make(chan struct{}, unitreeMaxParallelDials),
	}
}

// Rebind closes retained sessions and clears this feature's credentials when
// enrollment ownership changes. Unknown owner disables persistence until restart
// or a subsequent authenticated enrollment. A failed clear still binds loads
// to the new owner so old credentials cannot cross the ownership boundary, and
// is retried on the next rebind.
func (s *UnitreeService) Rebind(owner unitree.PairingOwner) error {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if s.owner.Matches(owner) {
		// A dual-SAN renewal proves the bridge without changing ownership.
		// Keep the full CURRENT scope for RPCs and persist proven vocabulary
		// for the next boot before an eventual UUID-only renewal.
		s.store.BindOwner(owner)
		s.owner = owner
		if s.stale {
			// Retry clearing the previous owner's keys; otherwise every load
			// under this owner keeps failing until ownership changes again.
			return s.clearStale()
		}
		_, err := s.store.List()
		if err != nil {
			// File/provenance failure must not leave old in-memory credentials
			// active under a newly asserted scope, even after org-only renewal.
			s.stopSessions()
		}
		return err
	}
	s.stopSessions()
	s.store.BindOwner(owner)
	s.owner = owner
	return s.clearStale()
}

// clearStale is called with opMu held after ownership has changed.
func (s *UnitreeService) clearStale() error {
	err := s.store.Clear()
	s.stale = err != nil
	return err
}

// Restore resumes saved pairings at agent startup without delaying startup on
// unreachable robots. A pairing stays disconnected until validation succeeds.
func (s *UnitreeService) Restore() error {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if err := s.ctx.Err(); err != nil {
		return err
	}
	pairings, err := s.store.List()
	if errors.Is(err, unitree.ErrForeignOwner) && s.owner.Local {
		// An unenrolled agent never owns enrolled pairings: they are left over
		// from unprovisioning whose clear failed. Remove them now.
		if err := s.clearStale(); err != nil {
			return errors.New("clearing a previous enrollment's Unitree pairings failed")
		}
		return nil
	}
	if err != nil {
		return errors.New("loading Unitree pairings failed")
	}
	for _, p := range pairings {
		s.replaceSession(p, nil)
	}
	return nil
}

func (s *UnitreeService) Close() {
	s.cancel()
	s.opMu.Lock()
	defer s.opMu.Unlock()
	s.mu.Lock()
	var links []unitreeLink
	for _, session := range s.sessions {
		session.cancel()
		if session.link != nil {
			links = append(links, session.link)
		}
	}
	s.sessions = make(map[string]*unitreeSession)
	s.mu.Unlock()
	for _, link := range links {
		_ = link.Close()
	}
	s.wg.Wait()
}

// The mTLS listener authenticates the certificate. Unitree RPCs additionally
// require the pairing owner's scope, even if the shared listener allows grace
// or off enforcement for other services. The local socket remains protected by
// the agent's socket permissions. Never expose this on the plaintext listener.
func requireUnitreeTransport(ctx context.Context) error {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return status.Error(codes.Unauthenticated, "Unitree pairing requires an authenticated agent connection")
	}
	if p.Addr != nil && p.Addr.Network() == "unix" {
		return nil
	}
	if info, ok := p.AuthInfo.(credentials.TLSInfo); ok && unitreeMutualTLS(info.State) {
		return nil
	}
	return status.Error(codes.Unauthenticated, "Unitree pairing requires mTLS or the local agent socket")
}

func unitreeMutualTLS(state tls.ConnectionState) bool {
	return state.HandshakeComplete && len(state.PeerCertificates) > 0 && state.PeerCertificates[0] != nil
}

// authorize is called with opMu held, so a rebind cannot change the credential
// owner between this check and the operation. Reuse the existing Wendy scope
// parser; do not infer tenant identity from TLS completion or an arbitrary CN.
func (s *UnitreeService) authorize(ctx context.Context) error {
	if err := requireUnitreeTransport(ctx); err != nil {
		return err
	}
	p, _ := peer.FromContext(ctx)
	if p.Addr != nil && p.Addr.Network() == "unix" {
		return nil
	}
	info := p.AuthInfo.(credentials.TLSInfo)
	scope, known, err := certs.ScopeFromCert(info.State.PeerCertificates[0])
	if err == nil && known && !s.owner.Local && s.owner.Scope().Matches(scope) {
		return nil
	}
	return status.Error(codes.PermissionDenied, "Unitree pairing owner scope is required")
}

func (s *UnitreeService) robot(d unitree.Device, paired bool) *agentpbv2.UnitreeRobot {
	s.mu.RLock()
	session := s.sessions[d.Serial]
	connected := session != nil && session.link != nil && session.link.Connected()
	s.mu.RUnlock()
	return &agentpbv2.UnitreeRobot{Serial: d.Serial, Name: d.Name, Address: d.Address, Paired: paired, Connected: connected}
}

func (s *UnitreeService) ListUnitreePairings(ctx context.Context, _ *agentpbv2.ListUnitreePairingsRequest) (*agentpbv2.ListUnitreePairingsResponse, error) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if err := s.authorize(ctx); err != nil {
		return nil, err
	}
	pairings, err := s.store.List()
	if err != nil {
		return nil, status.Error(codes.Internal, "loading Unitree pairings failed")
	}
	resp := &agentpbv2.ListUnitreePairingsResponse{}
	for _, p := range pairings {
		resp.Robots = append(resp.Robots, s.robot(p.Device, true))
	}
	return resp, nil
}

func (s *UnitreeService) DiscoverUnitreeRobots(ctx context.Context, req *agentpbv2.DiscoverUnitreeRobotsRequest) (*agentpbv2.DiscoverUnitreeRobotsResponse, error) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if err := s.authorize(ctx); err != nil {
		return nil, err
	}
	if s.ctx.Err() != nil {
		return nil, status.Error(codes.Unavailable, "agent is shutting down")
	}
	if len(req.GetKnownSerials()) > 256 {
		return nil, status.Error(codes.InvalidArgument, "too many Unitree serial numbers")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	stop := context.AfterFunc(s.ctx, cancel)
	defer stop()
	defer cancel()
	devices, err := s.discover(ctx, req.GetKnownSerials()...)
	if err != nil {
		return nil, status.Error(codes.Unavailable, "Unitree LAN discovery failed; enter the robot IP manually")
	}
	resp := &agentpbv2.DiscoverUnitreeRobotsResponse{}
	for _, d := range devices {
		resp.Robots = append(resp.Robots, s.robot(d, false))
	}
	return resp, nil
}

func (s *UnitreeService) PairUnitree(ctx context.Context, req *agentpbv2.PairUnitreeRequest) (*agentpbv2.PairUnitreeResponse, error) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if err := s.authorize(ctx); err != nil {
		return nil, err
	}
	if req == nil || !unitree.ValidSerial(strings.TrimSpace(req.Serial)) {
		return nil, status.Error(codes.InvalidArgument, "a valid Unitree robot serial is required")
	}
	if len(req.Name) > 256 {
		return nil, status.Error(codes.InvalidArgument, "Unitree robot name must be at most 256 bytes")
	}
	// Refuse anything but the fixed marker so a client bug cannot hand the
	// agent an account-wide token. Do not echo the rejected value.
	if req.Token != "" && req.Token != unitree.AccountFlowMarker {
		return nil, status.Error(codes.InvalidArgument, "token must be empty or the account-flow marker, never a Unitree account token")
	}
	address := strings.TrimSpace(req.Address)
	ip := net.ParseIP(address)
	// The contract requires an IPv4 literal; reject IPv4-mapped IPv6 spellings
	// exactly as the CLI does.
	if ip == nil || ip.To4() == nil || strings.Contains(address, ":") || ip.IsLoopback() || !(ip.IsGlobalUnicast() || ip.IsLinkLocalUnicast()) {
		return nil, status.Error(codes.InvalidArgument, "enter the Go2's LAN IPv4 address without a port")
	}
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	if s.ctx.Err() != nil {
		return nil, status.Error(codes.Unavailable, "agent is shutting down")
	}
	p := unitree.Pairing{Device: unitree.Device{Serial: strings.TrimSpace(req.Serial), Name: strings.TrimSpace(req.Name), Address: ip.String(), EncryptionKey: req.EncryptionKey}, PairedAt: time.Now().UTC()}
	pairings, err := s.store.List()
	if err != nil {
		return nil, status.Error(codes.Internal, "loading Unitree pairings failed")
	}
	var previous *unitree.Pairing
	for _, saved := range pairings {
		if saved.Device.Serial == p.Device.Serial {
			previous = &saved
			if p.Device.EncryptionKey == "" {
				p.Device.EncryptionKey = saved.Device.EncryptionKey
			}
			p.PairedAt = saved.PairedAt
			if p.Device.Name == "" {
				p.Device.Name = saved.Device.Name
			}
			break
		}
	}
	if p.Device.EncryptionKey == "" {
		// Legacy firmware does not expose a per-device key. Token presence is
		// only a marker from the CLI's account flow, not proof of account
		// ownership. It is never forwarded to the robot. Connect checks
		// whether the firmware requires a real per-device key.
		if previous == nil && req.Token == "" {
			return nil, status.Error(codes.InvalidArgument, "log in to Unitree before pairing this robot")
		}
	} else if key, err := hex.DecodeString(p.Device.EncryptionKey); err != nil || len(key) != 16 {
		return nil, status.Error(codes.InvalidArgument, "log in to Unitree to fetch this robot's encryption key")
	}
	if p.Device.Name == "" {
		p.Device.Name = "Go2 " + p.Device.Serial
	}
	if previous != nil && previous.Device.Address == p.Device.Address && previous.Device.EncryptionKey == p.Device.EncryptionKey && s.robot(previous.Device, true).Connected {
		// A second offer would consume another robot client slot. Reuse the
		// validated session when the endpoint and credential are unchanged.
		if err := s.store.Save(p); err != nil {
			return nil, status.Error(codes.Internal, "saving Unitree pairing failed")
		}
		return &agentpbv2.PairUnitreeResponse{Robot: s.robot(p.Device, true)}, nil
	}
	// Go2 can have a single WebRTC client slot. Wait for any in-flight
	// background reconnect to release it before making a manual offer.
	s.stopSession(p.Device.Serial)
	restorePrevious := previous != nil
	defer func() {
		if restorePrevious && s.ctx.Err() == nil {
			s.replaceSession(*previous, nil)
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	connectCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	stop := context.AfterFunc(s.ctx, cancel)
	defer stop()
	defer cancel()
	link, err := s.connect(connectCtx, p.Device.Address, "", p.Device.EncryptionKey)
	if err != nil {
		if ctx.Err() != nil {
			return nil, status.FromContextError(ctx.Err()).Err()
		}
		return nil, status.Error(codes.Unavailable, "Go2 connection failed; check the IP, account, and that the robot is on the agent's LAN")
	}
	if !link.Connected() || connectCtx.Err() != nil {
		_ = link.Close()
		return nil, status.Error(codes.Unavailable, "Go2 did not complete protocol validation")
	}
	if err := s.store.Save(p); err != nil {
		_ = link.Close()
		return nil, status.Error(codes.Internal, "saving Unitree pairing failed")
	}
	s.replaceSession(p, link)
	restorePrevious = false
	return &agentpbv2.PairUnitreeResponse{Robot: s.robot(p.Device, true)}, nil
}

func (s *UnitreeService) ForgetUnitree(ctx context.Context, req *agentpbv2.ForgetUnitreeRequest) (*agentpbv2.ForgetUnitreeResponse, error) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if err := s.authorize(ctx); err != nil {
		return nil, err
	}
	serial := strings.TrimSpace(req.GetSerial())
	if serial == "" {
		return nil, status.Error(codes.InvalidArgument, "a Unitree robot serial is required")
	}
	if err := s.store.Remove(serial); err != nil {
		return nil, status.Error(codes.Internal, "forgetting Unitree pairing failed")
	}
	s.stopSession(serial)
	return &agentpbv2.ForgetUnitreeResponse{}, nil
}

// stopSessions is called with opMu held, including on ownership uncertainty.
func (s *UnitreeService) stopSessions() {
	s.mu.RLock()
	serials := make([]string, 0, len(s.sessions))
	for serial := range s.sessions {
		serials = append(serials, serial)
	}
	s.mu.RUnlock()
	for _, serial := range serials {
		s.stopSession(serial)
	}
}

// stopSession is called with opMu held. Its wait includes any dial currently
// running in maintain and that dial's eventual connection cleanup.
func (s *UnitreeService) stopSession(serial string) {
	s.mu.Lock()
	session := s.sessions[serial]
	delete(s.sessions, serial)
	var link unitreeLink
	if session != nil {
		session.cancel()
		link = session.link
	}
	s.mu.Unlock()
	if link != nil {
		_ = link.Close()
	}
	if session != nil {
		<-session.done
	}
}

func (s *UnitreeService) replaceSession(p unitree.Pairing, link unitreeLink) {
	s.stopSession(p.Device.Serial)
	ctx, cancel := context.WithCancel(s.ctx)
	session := &unitreeSession{cancel: cancel, link: link, done: make(chan struct{})}
	s.mu.Lock()
	s.sessions[p.Device.Serial] = session
	s.mu.Unlock()
	s.wg.Add(1)
	go s.maintain(ctx, p, session, link)
}

// connect bounds expensive signaling/Pion setup across manual and restored
// sessions. Queued cancellation must never consume a robot connection slot.
func (s *UnitreeService) connect(ctx context.Context, address, token, key string) (unitreeLink, error) {
	select {
	case s.dialSlots <- struct{}{}:
		defer func() { <-s.dialSlots }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.dial(ctx, address, token, key)
}

func unitreeRetryDelay(base time.Duration) time.Duration {
	base = max(base, time.Millisecond)
	return base + time.Duration(rand.Int64N(int64(base/2)+1))
}

func (s *UnitreeService) maintain(ctx context.Context, p unitree.Pairing, session *unitreeSession, link unitreeLink) {
	defer s.wg.Done()
	defer close(session.done)
	defer func() {
		if link != nil {
			_ = link.Close()
		}
		s.mu.Lock()
		if s.sessions[p.Device.Serial] == session {
			delete(s.sessions, p.Device.Serial)
		}
		s.mu.Unlock()
	}()
	if link == nil {
		// Spread boot restoration without imposing a delay on explicit pairing.
		timer := time.NewTimer(time.Duration(rand.Int64N(int64(250 * time.Millisecond))))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
	for {
		if link != nil {
			select {
			case <-ctx.Done():
				return
			case <-link.Done():
			}
			_ = link.Close()
			link = nil
			s.mu.Lock()
			session.link = nil
			s.mu.Unlock()
			// A robot that validates and then drops the channel must not
			// cause an unbounded reconnect loop.
			if !s.waitRetry(ctx) {
				return
			}
		}
		if ctx.Err() != nil {
			return
		}
		connectCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		// Local reconnection uses only the saved robot key, never a saved
		// Unitree account token. Connect validates the data channel again.
		next, err := s.connect(connectCtx, p.Device.Address, "", p.Device.EncryptionKey)
		cancel()
		if err == nil {
			link = next
			s.mu.Lock()
			session.link = link
			s.mu.Unlock()
			continue
		}
		if !s.waitRetry(ctx) {
			return
		}
	}
}

func (s *UnitreeService) waitRetry(ctx context.Context) bool {
	timer := time.NewTimer(unitreeRetryDelay(s.retry))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
