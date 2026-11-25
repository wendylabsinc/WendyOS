package replication

import (
	"bytes"
	"context"
	"time"

	replicav1 "github.com/wendylabsinc/wendy/go/world/production/api/world/replica/v1"
	"github.com/wendylabsinc/wendy/go/world/production/proofref"
	"github.com/wendylabsinc/wendy/go/world/production/sensorrecord"
	"github.com/wendylabsinc/wendy/go/world/production/serviceevidence"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

const (
	MaxEvidenceBytes        = 8 << 20
	MaxEvidenceRangeBytes   = serviceevidence.MaxChunk
	MaxEvidenceRequestBytes = 4096
	MaxEvidenceReplyBytes   = MaxEvidenceRangeBytes + MaxEvidenceRequestBytes
)

func evidenceOwner(source proofref.Source) serviceevidence.Owner {
	return serviceevidence.Owner{Tenant: source.Tenant, Site: source.Site, Device: source.Device, App: source.App, Service: source.Service, Component: source.Component, Stream: source.Stream}
}

func evidenceSourceValid(source proofref.Source, endpoint Endpoint, device string) bool {
	return source.Validate() == nil && source.Tenant == endpoint.Tenant && source.Site == endpoint.Site && source.Device == device && source.Component == sensorrecord.ComponentType
}

func evidenceAllowed(source proofref.Source, sources []proofref.Source) bool {
	for _, allowed := range sources {
		if source == allowed {
			return true
		}
	}
	return false
}

func evidenceSourceWire(source proofref.Source) *replicav1.EvidenceSource {
	return &replicav1.EvidenceSource{Actor: &replicav1.Actor{Tenant: source.Tenant, Site: source.Site, Device: source.Device, App: source.App, Service: source.Service}, ComponentType: source.Component, Stream: source.Stream}
}

func evidenceSourceValue(source *replicav1.EvidenceSource) (proofref.Source, error) {
	if source == nil || source.Actor == nil || !known(source.ProtoReflect()) {
		return proofref.Source{}, ErrProtocol
	}
	a := source.Actor
	s := proofref.Source{Tenant: a.Tenant, Site: a.Site, Device: a.Device, App: a.App, Service: a.Service, Component: source.ComponentType, Stream: source.Stream}
	if s.Validate() != nil {
		return proofref.Source{}, ErrProtocol
	}
	return s, nil
}

func evidenceRangeValid(request serviceevidence.GetRequest) bool {
	return digest(request.SHA256) && request.Offset >= 0 && request.Offset < MaxEvidenceBytes && request.Length > 0 && request.Length <= MaxEvidenceRangeBytes
}

func evidenceResponseValid(source proofref.Source, request serviceevidence.GetRequest, response serviceevidence.GetResponse) bool {
	owner := evidenceOwner(source)
	hash, err := owner.SHA256()
	until := time.UnixMilli(response.RetainUntilUnixMS)
	return err == nil && response.Owner == owner && response.OwnerSHA256 == hash && response.SHA256 == request.SHA256 &&
		response.Size > 0 && response.Size <= MaxEvidenceBytes && response.Offset == request.Offset && response.Offset < response.Size &&
		int64(len(response.Data)) == min(int64(request.Length), response.Size-response.Offset) &&
		(response.ContentType == "image/png" || response.ContentType == "image/jpeg" || response.ContentType == "audio/wav") &&
		response.RetainUntilUnixMS > 0 && until.UnixMilli() == response.RetainUntilUnixMS && until.Year() <= 9999
}

// Evidence has separate bounded slots from state repair. A scheduled inventory
// round must not occupy the only slot for an ordinary app's media range.
func (s *Service) enterEvidence(ctx context.Context) (Identity, func(), error) {
	select {
	case s.evidenceSlots <- struct{}{}:
	default:
		return Identity{}, nil, ErrCapacity
	}
	p, err := s.cfg.IncomingPeer(ctx)
	if err != nil || !s.identity(p) {
		<-s.evidenceSlots
		return Identity{}, nil, ErrDenied
	}
	s.mu.Lock()
	busy := s.evidenceInbound[p.Device]
	if !busy {
		s.evidenceInbound[p.Device] = true
	}
	s.mu.Unlock()
	if busy {
		<-s.evidenceSlots
		return Identity{}, nil, ErrBusy
	}
	return p, func() {
		s.mu.Lock()
		delete(s.evidenceInbound, p.Device)
		s.mu.Unlock()
		<-s.evidenceSlots
	}, nil
}

// FetchEvidence reads one original local-owner range. The host callback uses
// its existing local evidence store and current peer/export guard; it never
// relays a third origin or makes a network call.
func (s *Service) FetchEvidence(ctx context.Context, req *replicav1.FetchEvidenceRequest) (*replicav1.FetchEvidenceReply, error) {
	if ctx == nil || req == nil || !known(req.ProtoReflect()) || req.Version != Version || proto.Size(req) > MaxEvidenceRequestBytes {
		return nil, rpcError(ErrProtocol)
	}
	source, err := evidenceSourceValue(req.Source)
	request := serviceevidence.GetRequest{SHA256: req.Sha256, Offset: req.Offset, Length: int(req.Length)}
	if err != nil || !evidenceSourceValid(source, s.cfg.Endpoint, s.cfg.Endpoint.Device) || !evidenceRangeValid(request) {
		return nil, rpcError(ErrProtocol)
	}
	if s.cfg.ReadEvidence == nil {
		return nil, status.Error(codes.Unimplemented, "replica evidence unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, s.cfg.Limits.RequestTimeout)
	defer cancel()
	peer, leave, err := s.enterEvidence(ctx)
	if err != nil {
		return nil, rpcError(err)
	}
	defer leave()
	access, err := s.authorize(ctx, peer, Export)
	if err != nil || !evidenceAllowed(source, access.Sources) {
		return nil, rpcError(ErrDenied)
	}
	read, err := s.cfg.ReadEvidence(ctx, peer, access.Generation, source, request)
	if err != nil {
		return nil, rpcError(err)
	}
	read.Data = bytes.Clone(read.Data)
	if !evidenceResponseValid(source, request, read) {
		return nil, rpcError(ErrProtocol)
	}
	response := &replicav1.FetchEvidenceReply{Version: Version, Scope: scopeWire(s.cfg.Endpoint), Generation: access.Generation,
		Source: evidenceSourceWire(source), OwnerSha256: read.OwnerSHA256, Sha256: read.SHA256, Size: read.Size, Offset: read.Offset,
		Data: read.Data, ContentType: read.ContentType, RetainUntilUnixMs: read.RetainUntilUnixMS}
	if proto.Size(response) > MaxEvidenceReplyBytes {
		return nil, rpcError(ErrCapacity)
	}
	if err = s.current(ctx, peer, Export, access.Generation); err != nil {
		return nil, rpcError(err)
	}
	return response, nil
}

// ReadPeerEvidence retrieves one range through the host's existing authenticated
// dialer. It retries neither dialing nor reads and does not admit or cache bytes.
// The caller checks the complete object's digest and original retention using
// its authenticated clock. No policy/store/bookkeeping lock spans network work.
func (s *Service) ReadPeerEvidence(ctx context.Context, device string, source proofref.Source, request serviceevidence.GetRequest) (out serviceevidence.GetResponse, err error) {
	if ctx == nil || device == s.cfg.Endpoint.Device || !evidenceSourceValid(source, s.cfg.Endpoint, device) || !evidenceRangeValid(request) {
		return out, ErrDenied
	}
	if err = ctx.Err(); err != nil {
		return out, err
	}
	defer func() {
		if err != nil {
			out = serviceevidence.GetResponse{}
		}
	}()
	s.mu.Lock()
	if s.evidenceOutbound[device] {
		s.mu.Unlock()
		return out, ErrBusy
	}
	if len(s.evidenceOutbound) >= s.cfg.Limits.MaxPeers {
		s.mu.Unlock()
		return out, ErrCapacity
	}
	s.evidenceOutbound[device] = true
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.evidenceOutbound, device); s.mu.Unlock() }()
	ctx, cancel := context.WithTimeout(ctx, s.cfg.Limits.RoundTimeout)
	defer cancel()
	dialCtx, stopDial := context.WithTimeout(ctx, s.cfg.Limits.RequestTimeout)
	connection, err := s.cfg.Dial(dialCtx, device)
	stopDial()
	if connection.Close != nil {
		defer func() {
			if closeErr := connection.Close.Close(); err == nil && closeErr != nil {
				err = closeErr
			}
		}()
	}
	if err != nil {
		return out, err
	}
	if connection.Client == nil || connection.Close == nil || connection.Identity.Device != device || !s.identity(connection.Identity) {
		return out, ErrDenied
	}
	peer := connection.Identity
	access, err := s.authorize(ctx, peer, Import)
	if err != nil || !evidenceAllowed(source, access.Sources) {
		return out, ErrDenied
	}
	req := &replicav1.FetchEvidenceRequest{Version: Version, Source: evidenceSourceWire(source), Sha256: request.SHA256, Offset: request.Offset, Length: uint32(request.Length)}
	if proto.Size(req) > MaxEvidenceRequestBytes {
		return out, ErrProtocol
	}
	rpcCtx, stop := context.WithTimeout(ctx, s.cfg.Limits.RequestTimeout)
	response, err := connection.Client.FetchEvidence(rpcCtx, req, grpc.MaxCallRecvMsgSize(MaxEvidenceReplyBytes), grpc.MaxCallSendMsgSize(MaxEvidenceRequestBytes), grpc.MaxRetryRPCBufferSize(0))
	stop()
	if err != nil {
		return out, err
	}
	remote := s.cfg.Endpoint
	remote.Device = device
	if response == nil || !known(response.ProtoReflect()) || response.Version != Version || response.Generation == 0 || !scopeEqual(response.Scope, remote) || proto.Size(response) > MaxEvidenceReplyBytes {
		return out, ErrProtocol
	}
	receivedSource, e := evidenceSourceValue(response.Source)
	if e != nil || receivedSource != source {
		return out, ErrProtocol
	}
	out = serviceevidence.GetResponse{Reference: serviceevidence.Reference{Owner: evidenceOwner(source), OwnerSHA256: response.OwnerSha256, SHA256: response.Sha256,
		Size: response.Size, ContentType: response.ContentType, RetainUntilUnixMS: response.RetainUntilUnixMs}, Offset: response.Offset, Data: bytes.Clone(response.Data)}
	if !evidenceResponseValid(source, request, out) {
		return serviceevidence.GetResponse{}, ErrProtocol
	}
	if err = s.current(ctx, peer, Import, access.Generation); err != nil {
		return serviceevidence.GetResponse{}, err
	}
	return out, nil
}
