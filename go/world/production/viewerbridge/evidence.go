package viewerbridge

import (
	"bytes"
	"context"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/node"
	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/sensorrecord"
	"github.com/wendylabsinc/wendy/go/world/production/serviceaccess"
	"google.golang.org/grpc"
)

type EvidenceSource = node.NativeEvidenceSource

func evidenceSelector(s EvidenceSource) serviceaccess.Source {
	return serviceaccess.Source{Device: s.Device, App: s.App, Service: s.Service, ComponentType: s.ComponentType, Stream: s.Stream}
}

func normalizedEvidenceSources(sources []EvidenceSource, device string) ([]EvidenceSource, error) {
	if len(sources) > serviceaccess.MaxSubscriptions {
		return nil, ErrUnavailable
	}
	seen := map[EvidenceSource]bool{}
	for _, s := range sources {
		selector := evidenceSelector(s)
		resolved, err := selector.Resolve(device)
		if s.Device == "" || s.ComponentType != sensorrecord.ComponentType || err != nil || resolved != selector || seen[s] {
			return nil, ErrUnavailable
		}
		seen[s] = true
	}
	return append([]EvidenceSource(nil), sources...), nil
}

// readEvidence retrieves one range from its exact native owner. It deliberately
// does not query the result streams: retained originals outlive derived tracks.
// Native storage enforces each current object incarnation's retention; clients
// require one unchanged deadline while assembling a complete download.
func (s *Session) readEvidence(ctx context.Context, q node.NativeEvidenceRequest, allowed []EvidenceSource) (node.NativeEvidenceResponse, error) {
	var empty node.NativeEvidenceResponse
	if s == nil || ctx == nil || q.Validate() != nil {
		return empty, ErrUnavailable
	}
	permitted := false
	for _, source := range allowed {
		if source == q.Source {
			permitted = true
			break
		}
	}
	if !permitted {
		return empty, ErrUnavailable
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || ctx.Err() != nil {
		return empty, ErrUnavailable
	}
	before, err := s.clock.PhysicalObserve(ctx)
	if err != nil {
		return empty, err
	}
	rpc, cancel := context.WithTimeout(ctx, time.Duration(s.cfg.People.RequestTimeoutMS)*time.Millisecond)
	owner := q.Source
	reply, err := s.api.GetEvidence(rpc, &appv1.GetEvidenceRequest{Source: &appv1.SourceSelector{Device: owner.Device, App: owner.App, Service: owner.Service, ComponentType: owner.ComponentType, Stream: owner.Stream}, Sha256: q.SHA256, Offset: q.Offset, Length: int32(q.Length)}, grpc.MaxCallRecvMsgSize(node.MaxNativeEvidenceRange+4096), grpc.MaxRetryRPCBufferSize(0))
	cancel()
	if err != nil {
		return empty, err
	}
	if reply == nil || len(reply.ProtoReflect().GetUnknown()) != 0 {
		return empty, ErrUnavailable
	}
	result := node.NativeEvidenceResponse{Source: owner, SHA256: reply.Sha256, Size: reply.Size, ContentType: reply.ContentType, Offset: reply.Offset, Data: bytes.Clone(reply.Data), RetainUntilUnixMS: reply.RetainUntilUnixMs}
	if result.ValidateFor(q) != nil {
		return empty, ErrUnavailable
	}
	after, err := s.clock.PhysicalObserve(ctx)
	if err != nil {
		return empty, err
	}
	until := time.UnixMilli(result.RetainUntilUnixMS)
	if ctx.Err() != nil || !before.Physical.Latest.Before(until) || !after.Physical.Latest.Before(until) {
		return empty, ErrUnavailable
	}
	return result, nil
}
