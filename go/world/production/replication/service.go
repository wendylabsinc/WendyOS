package replication

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"sync"
	"time"

	replicav1 "github.com/wendylabsinc/wendy/go/world/production/api/world/replica/v1"
	"github.com/wendylabsinc/wendy/go/world/production/jws"
	"github.com/wendylabsinc/wendy/go/world/production/proofref"
	"github.com/wendylabsinc/wendy/go/world/production/serviceaccess"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type Service struct {
	replicav1.UnimplementedWorldReplicaServiceServer
	cfg                               Config
	key                               [32]byte
	elapsed                           func() time.Duration
	mu                                sync.Mutex
	outbound, inbound                 map[string]bool
	evidenceOutbound, evidenceInbound map[string]bool
	evidenceSlots                     chan struct{}
	latestOutbound, latestInbound     map[string]bool
	latestSlots                       chan struct{}
	progress                          map[string]progress
	slots                             chan struct{}
}

func New(cfg Config) (*Service, error) {
	if !validEndpoint(cfg.Endpoint) || cfg.Store == nil || cfg.IncomingPeer == nil || cfg.Authorize == nil || cfg.Current == nil || cfg.RemoteAuthority == nil || cfg.Dial == nil {
		return nil, ErrDenied
	}
	l := cfg.Limits
	if l.MaxPeers == 0 {
		l.MaxPeers = 16
	}
	if l.MaxPages == 0 {
		l.MaxPages = 16
	}
	if l.MaxEntries == 0 {
		l.MaxEntries = 1024
	}
	if l.MaxBytes == 0 {
		l.MaxBytes = 8 << 20
	}
	if l.RoundTimeout == 0 {
		l.RoundTimeout = 30 * time.Second
	}
	if l.RequestTimeout == 0 {
		l.RequestTimeout = 2 * time.Second
	}
	if l.MaxPeers < 1 || l.MaxPeers > 32 || l.MaxPages < 1 || l.MaxPages > 64 || l.MaxEntries < 1 || l.MaxEntries > 4096 || l.MaxBytes < MaxPageBytes || l.MaxBytes > 32<<20 || l.RoundTimeout < time.Millisecond || l.RoundTimeout > time.Minute || l.RequestTimeout < time.Millisecond || l.RequestTimeout > 5*time.Second || l.RequestTimeout > l.RoundTimeout {
		return nil, ErrCapacity
	}
	cfg.Limits = l
	start := time.Now()
	s := &Service{cfg: cfg, elapsed: func() time.Duration { return time.Since(start) }, outbound: map[string]bool{}, inbound: map[string]bool{}, progress: map[string]progress{}, slots: make(chan struct{}, l.MaxPeers)}
	s.evidenceOutbound, s.evidenceInbound = map[string]bool{}, map[string]bool{}
	s.evidenceSlots = make(chan struct{}, l.MaxPeers)
	s.latestOutbound, s.latestInbound = map[string]bool{}, map[string]bool{}
	s.latestSlots = make(chan struct{}, l.MaxPeers)
	if _, err := rand.Read(s.key[:]); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Service) identity(p Identity) bool {
	tenant, err := serviceaccess.DeviceTenant(p.Device)
	return err == nil && tenant == s.cfg.Endpoint.Tenant && p.Device != s.cfg.Endpoint.Device && digest(p.BindingSHA256)
}
func (s *Service) authorize(ctx context.Context, p Identity, d Direction) (Access, error) {
	if ctx == nil || !s.identity(p) {
		return Access{}, ErrDenied
	}
	if err := ctx.Err(); err != nil {
		return Access{}, err
	}
	a, err := s.cfg.Authorize(ctx, p, d)
	if canceled := ctx.Err(); canceled != nil {
		return Access{}, canceled
	}
	if err != nil || a.Generation == 0 || len(a.Sources) > 64 {
		return Access{}, ErrDenied
	}
	a.Sources = append([]proofref.Source(nil), a.Sources...)
	seen := map[proofref.Source]bool{}
	origin := p.Device
	if d == Export {
		origin = s.cfg.Endpoint.Device
	}
	for _, source := range a.Sources {
		if source.Validate() != nil || source.Tenant != s.cfg.Endpoint.Tenant || source.Site != s.cfg.Endpoint.Site || source.Device != origin || !SupportedComponent(source.Component) || seen[source] {
			return Access{}, ErrDenied
		}
		seen[source] = true
	}
	if err = s.current(ctx, p, d, a.Generation); err != nil {
		return Access{}, err
	}
	return a, nil
}
func (s *Service) current(ctx context.Context, p Identity, d Direction, g uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	err := s.cfg.Current(ctx, p, d, g)
	if canceled := ctx.Err(); canceled != nil {
		return canceled
	}
	if err != nil {
		return ErrDenied
	}
	return nil
}
func rpcError(err error) error {
	switch {
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, "replica operation canceled")
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, "replica deadline exceeded")
	case errors.Is(err, ErrProtocol):
		return status.Error(codes.InvalidArgument, "replica protocol refused")
	case errors.Is(err, ErrCapacity), errors.Is(err, ErrBusy):
		return status.Error(codes.ResourceExhausted, "replica quota exceeded")
	case errors.Is(err, ErrDenied):
		return status.Error(codes.PermissionDenied, "replica access denied")
	default:
		return status.Error(codes.Unavailable, "replica state unavailable")
	}
}
func (s *Service) enter(ctx context.Context) (Identity, func(), error) {
	if ctx == nil {
		return Identity{}, nil, ErrDenied
	}
	select {
	case s.slots <- struct{}{}:
	default:
		return Identity{}, nil, ErrCapacity
	}
	p, err := s.cfg.IncomingPeer(ctx)
	if err != nil || !s.identity(p) {
		<-s.slots
		return Identity{}, nil, ErrDenied
	}
	s.mu.Lock()
	busy := s.inbound[p.Device]
	if !busy {
		s.inbound[p.Device] = true
	}
	s.mu.Unlock()
	if busy {
		<-s.slots
		return Identity{}, nil, ErrBusy
	}
	return p, func() { s.mu.Lock(); delete(s.inbound, p.Device); s.mu.Unlock(); <-s.slots }, nil
}

type cursor struct {
	Version       int
	Peer, Binding string
	Generation    uint64
	After         string
	Expires       int64
}

func (s *Service) encodeCursor(p Identity, g uint64, after string) ([]byte, error) {
	now := s.elapsed()
	if now < 0 || !validOrder(after) {
		return nil, ErrProtocol
	}
	b, err := json.Marshal(cursor{Version, p.Device, p.BindingSHA256, g, after, int64(now + 2*time.Minute)})
	if err != nil {
		return nil, ErrProtocol
	}
	mac := hmac.New(sha256.New, s.key[:])
	_, _ = mac.Write(b)
	b = append(b, mac.Sum(nil)...)
	if len(b) > MaxCursorBytes {
		return nil, ErrCapacity
	}
	return b, nil
}
func (s *Service) decodeCursor(raw []byte, p Identity, g uint64) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	if len(raw) > MaxCursorBytes || len(raw) <= sha256.Size {
		return "", ErrProtocol
	}
	body, signature := raw[:len(raw)-sha256.Size], raw[len(raw)-sha256.Size:]
	mac := hmac.New(sha256.New, s.key[:])
	_, _ = mac.Write(body)
	var c cursor
	now := s.elapsed()
	if !hmac.Equal(mac.Sum(nil), signature) || jws.DecodeStrict(body, &c) != nil || c.Version != Version || c.Peer != p.Device || c.Binding != p.BindingSHA256 || c.Generation != g || !validOrder(c.After) || now < 0 || c.Expires <= int64(now) || c.Expires > int64(now+2*time.Minute) {
		return "", ErrProtocol
	}
	return c.After, nil
}

func (s *Service) Inventory(ctx context.Context, req *replicav1.InventoryRequest) (*replicav1.InventoryReply, error) {
	if ctx == nil || req == nil || !known(req.ProtoReflect()) || req.Version != Version || req.Limit > MaxInventoryEntries || len(req.Cursor) > MaxCursorBytes || proto.Size(req) > MaxInventoryBytes || len(req.Cursor) > 0 && req.ExpectedGeneration == 0 {
		return nil, rpcError(ErrProtocol)
	}
	ctx, cancel := context.WithTimeout(ctx, s.cfg.Limits.RequestTimeout)
	defer cancel()
	p, leave, err := s.enter(ctx)
	if err != nil {
		return nil, rpcError(err)
	}
	defer leave()
	a, err := s.authorize(ctx, p, Export)
	if err != nil {
		return nil, rpcError(err)
	}
	if req.ExpectedGeneration != 0 && req.ExpectedGeneration != a.Generation {
		return nil, rpcError(ErrDenied)
	}
	after, err := s.decodeCursor(req.Cursor, p, a.Generation)
	if err != nil {
		return nil, rpcError(err)
	}
	limit := int(req.Limit)
	if limit == 0 {
		limit = MaxInventoryEntries
	}
	var page InventoryPage
	if len(a.Sources) != 0 {
		page, err = s.cfg.Store.Inventory(ctx, append([]proofref.Source(nil), a.Sources...), after, limit)
		if err != nil {
			return nil, rpcError(err)
		}
	}
	if len(page.Entries) > limit || page.More && len(page.Entries) == 0 {
		return nil, rpcError(ErrProtocol)
	}
	response := &replicav1.InventoryReply{Version: Version, Scope: scopeWire(s.cfg.Endpoint), Generation: a.Generation}
	last := after
	for _, e := range page.Entries {
		if validateEntry(e, s.cfg.Endpoint, s.cfg.Endpoint.Device) != nil || !allowed(e, a.Sources) || e.OrderKey() <= last {
			return nil, rpcError(ErrProtocol)
		}
		last = e.OrderKey()
		response.Entries = append(response.Entries, entryWire(e))
	}
	if page.More {
		response.NextCursor, err = s.encodeCursor(p, a.Generation, last)
		if err != nil {
			return nil, rpcError(err)
		}
	}
	if proto.Size(response) > MaxInventoryBytes {
		return nil, rpcError(ErrCapacity)
	}
	if err = s.current(ctx, p, Export, a.Generation); err != nil {
		return nil, rpcError(err)
	}
	return response, nil
}

func (s *Service) Fetch(ctx context.Context, req *replicav1.FetchRequest) (*replicav1.FetchReply, error) {
	if ctx == nil || req == nil || !known(req.ProtoReflect()) || req.Version != Version || req.ExpectedGeneration == 0 || len(req.Entries) == 0 || len(req.Entries) > MaxFetchEntries || proto.Size(req) > MaxInventoryBytes {
		return nil, rpcError(ErrProtocol)
	}
	ctx, cancel := context.WithTimeout(ctx, s.cfg.Limits.RequestTimeout)
	defer cancel()
	p, leave, err := s.enter(ctx)
	if err != nil {
		return nil, rpcError(err)
	}
	defer leave()
	a, err := s.authorize(ctx, p, Export)
	if err != nil || a.Generation != req.ExpectedGeneration {
		return nil, rpcError(ErrDenied)
	}
	entries := make([]Entry, 0, len(req.Entries))
	seen := map[string]bool{}
	for _, wire := range req.Entries {
		e, err := entryValue(wire, s.cfg.Endpoint, s.cfg.Endpoint.Device)
		if err != nil || !allowed(e, a.Sources) || seen[e.OrderKey()] {
			return nil, rpcError(ErrProtocol)
		}
		seen[e.OrderKey()] = true
		entries = append(entries, e)
	}
	response := &replicav1.FetchReply{Version: Version, Scope: scopeWire(s.cfg.Endpoint), Generation: a.Generation}
	for _, e := range entries {
		item, found, err := s.cfg.Store.Fetch(ctx, e)
		if err != nil {
			return nil, rpcError(err)
		}
		if !found {
			response.Unavailable = append(response.Unavailable, entryWire(e))
			continue
		}
		item = cloneItem(item)
		if item.Entry != e || ValidateItem(item, s.cfg.Endpoint, s.cfg.Endpoint.Device) != nil {
			return nil, rpcError(ErrProtocol)
		}
		candidate := &replicav1.Item{Entry: entryWire(e), Envelope: item.Envelope}
		response.Items = append(response.Items, candidate)
		if proto.Size(response) > MaxPageBytes {
			response.Items = response.Items[:len(response.Items)-1]
			response.Unavailable = append(response.Unavailable, entryWire(e))
		}
	}
	if proto.Size(response) > MaxPageBytes {
		return nil, rpcError(ErrCapacity)
	}
	if err = s.current(ctx, p, Export, a.Generation); err != nil {
		return nil, rpcError(err)
	}
	return response, nil
}
