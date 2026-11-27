package appservice

import (
	"context"
	"crypto/sha256"
	"encoding/hex"

	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func (r *rpcServer) GetRecordProof(ctx context.Context, p *appv1.GetRecordProofRequest) (*appv1.GetRecordProofReply, error) {
	out, err := r.access(ctx, p, func(ctx context.Context, f *Fence) (proto.Message, error) {
		reference, err := appv1.DecodeGetRecordProof(p, r.service.scope)
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, "invalid proof reference")
		}
		fn := r.service.manager.cfg.GetRecordProof
		if fn == nil {
			return nil, status.Error(codes.Unimplemented, "historical proof unavailable")
		}
		v, err := fn(ctx, r.service.scope, reference, f)
		if err != nil {
			return nil, status.Error(codes.PermissionDenied, "historical proof unavailable for service")
		}
		// This is a byte-identity check, not current proof verification. The
		// archive independently verifies the full exact reference on lookup.
		sum := sha256.Sum256(v.Envelope)
		if len(v.Envelope) == 0 || hex.EncodeToString(sum[:]) != reference.EnvelopeSHA256 || v.RetainUntilUnixMS <= 0 {
			return nil, status.Error(codes.Unavailable, "historical proof differs from request")
		}
		return &appv1.GetRecordProofReply{SignedRecordJson: v.Envelope, RetainUntilUnixMs: v.RetainUntilUnixMS, Historical: true}, nil
	})
	if err != nil {
		return nil, err
	}
	return out.(*appv1.GetRecordProofReply), nil
}
