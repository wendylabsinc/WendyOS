package livepeople

import (
	"context"
	"time"

	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/serviceevidence"
	"google.golang.org/grpc"
)

func (s *Session) image(ctx context.Context, p proof) ([]byte, time.Time, error) {
	ref, err := p.capture.Evidence()
	if err != nil || ref == nil || ref.Validate() != nil || ref.Size > 8<<20 || ref.ContentType != "image/png" && ref.ContentType != "image/jpeg" {
		return nil, time.Time{}, ErrMedia
	}
	data := make([]byte, 0, ref.Size)
	var retained int64
	for int64(len(data)) < ref.Size {
		length := min(int64(serviceevidence.MaxChunk), ref.Size-int64(len(data)))
		rpc, cancel := context.WithTimeout(ctx, time.Duration(s.cfg.RequestTimeoutMS)*time.Millisecond)
		reply, err := s.api.GetEvidence(rpc, &appv1.GetEvidenceRequest{Source: selector(s.sources[p.record.Publication.Stream].selector()), Sha256: ref.SHA256, Offset: int64(len(data)), Length: int32(length)}, grpc.MaxCallRecvMsgSize(serviceevidence.MaxChunk+4096), grpc.MaxRetryRPCBufferSize(0))
		cancel()
		if err != nil || reply == nil || len(reply.ProtoReflect().GetUnknown()) != 0 || reply.Sha256 != ref.SHA256 || reply.Size != ref.Size || reply.Offset != int64(len(data)) || reply.ContentType != ref.ContentType || int64(len(reply.Data)) != length || reply.RetainUntilUnixMs <= 0 || retained != 0 && retained != reply.RetainUntilUnixMs {
			return nil, time.Time{}, ErrMedia
		}
		retained = reply.RetainUntilUnixMs
		data = append(data, reply.Data...)
	}
	if digest(data) != ref.SHA256 {
		return nil, time.Time{}, ErrMedia
	}
	return data, time.UnixMilli(retained), nil
}
