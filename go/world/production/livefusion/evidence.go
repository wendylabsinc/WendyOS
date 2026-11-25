package livefusion

import (
	"bytes"
	"context"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/serviceaccess"
	"github.com/wendylabsinc/wendy/go/world/production/serviceevidence"
	"google.golang.org/grpc"
)

type evidenceOwner struct {
	source serviceaccess.Source
	proof  inputProof
}

func (s *session) mediaLoader(proofs []inputProof, receipts *[]MediaReceipt, cache *nativeMediaCache) func(context.Context, sensing.EvidenceRef) ([]byte, error) {
	owners := map[sensing.EvidenceRef][]evidenceOwner{}
	for _, p := range proofs {
		ref, err := p.capture.Evidence()
		if err == nil && ref != nil {
			source := s.sources[p.record.Publication.Stream]
			owners[*ref] = append(owners[*ref], evidenceOwner{source.selector(), p})
		}
	}
	loaded := map[sensing.EvidenceRef][]byte{}
	total := int64(0)
	return func(ctx context.Context, ref sensing.EvidenceRef) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if data, ok := loaded[ref]; ok {
			return bytes.Clone(data), nil
		}
		bindings := owners[ref]
		if len(bindings) == 0 || ref.Validate() != nil || ref.Size > 8<<20 {
			return nil, ErrMedia
		}
		var selected []byte
		// Identical media may belong to two sensors. Verify each exact owner
		// rather than using digest possession to choose another service's blob.
		for _, owner := range bindings {
			if ref.Size > (16<<20)-total {
				return nil, ErrMedia
			}
			total += ref.Size
			key := mediaCacheKey{source: owner.source, ref: ref}
			data, retained, cached := cache.get(key)
			if cached {
				w, err := s.clock.Now()
				if err != nil {
					return nil, ErrMedia
				}
				if retained <= w.Latest.UnixMilli() {
					cache.remove(key)
					data, retained, cached = nil, 0, false
				}
			}
			if !cached {
				data = make([]byte, 0, int(ref.Size))
			}
			for int64(len(data)) < ref.Size {
				length := min(int64(serviceevidence.MaxChunk), ref.Size-int64(len(data)))
				rpc, cancel := context.WithTimeout(ctx, time.Duration(s.cfg.RequestTimeoutMS)*time.Millisecond)
				reply, err := s.api.GetEvidence(rpc, &appv1.GetEvidenceRequest{Source: selector(owner.source), Sha256: ref.SHA256, Offset: int64(len(data)), Length: int32(length)}, grpc.MaxCallRecvMsgSize(serviceevidence.MaxChunk+4096), grpc.MaxRetryRPCBufferSize(0))
				cancel()
				if err != nil || reply == nil || len(reply.ProtoReflect().GetUnknown()) != 0 || reply.Sha256 != ref.SHA256 || reply.Size != ref.Size || reply.Offset != int64(len(data)) || reply.ContentType != ref.ContentType || int64(len(reply.Data)) != length || reply.RetainUntilUnixMs <= 0 || retained != 0 && retained != reply.RetainUntilUnixMs {
					return nil, ErrMedia
				}
				retained = reply.RetainUntilUnixMs
				data = append(data, reply.Data...)
			}
			if !cached {
				w, err := s.clock.Now()
				if err != nil || retained <= w.Latest.UnixMilli() {
					return nil, ErrMedia
				}
			}
			if int64(len(data)) != ref.Size || digest(data) != ref.SHA256 {
				return nil, ErrMedia
			}
			if !cached {
				cache.put(key, data, retained)
			}
			if selected != nil && !bytes.Equal(selected, data) {
				return nil, ErrMedia
			}
			selected = data
			*receipts = append(*receipts, MediaReceipt{Source: owner.source, Reference: ref, Data: bytes.Clone(data)})
		}
		loaded[ref] = bytes.Clone(selected)
		return bytes.Clone(selected), nil
	}
}
