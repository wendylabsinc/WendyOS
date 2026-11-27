package appservice

import (
	"context"
	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/rpctrace"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// access shares publication's kernel identity, concurrency, per-message budgets,
// cancellation and generation fence. It never serves these methods on admin RPC.
func (r *rpcServer) access(parent context.Context, request proto.Message, run func(context.Context, *Fence) (proto.Message, error)) (proto.Message, error) {
	if err := r.enter(parent); err != nil {
		return nil, err
	}
	defer r.leave()
	s := r.service
	ctx, cancel := context.WithTimeout(parent, s.manager.cfg.Limits.RequestTimeout)
	stop := context.AfterFunc(s.ctx, cancel)
	defer func() { stop(); cancel() }()
	if err := s.authorize(ctx); err != nil {
		return nil, status.Error(codes.PermissionDenied, "service authority denied")
	}
	if request == nil || !request.ProtoReflect().IsValid() || proto.Size(request) > s.manager.cfg.Limits.MaxRequestBytes {
		return nil, status.Error(codes.InvalidArgument, "access request exceeds budget")
	}
	if !s.charge(proto.Size(request)) {
		return nil, status.Error(codes.ResourceExhausted, "service access budget exceeded")
	}
	fence := &Fence{service: s, ctx: ctx}
	reply, err := run(ctx, fence)
	committed := fence.finish()
	if err != nil {
		return nil, err
	}
	if !committed {
		return nil, status.Error(codes.Unavailable, "service access unavailable")
	}
	if reply == nil || !reply.ProtoReflect().IsValid() || proto.Size(reply) > s.manager.cfg.Limits.MaxResponseBytes || !s.charge(proto.Size(reply)) {
		return nil, status.Error(codes.ResourceExhausted, "service response budget exceeded; mutation outcome may be unknown")
	}
	if err = s.authorize(ctx); err != nil {
		return nil, status.Error(codes.Unavailable, "service authority changed; mutation outcome may be unknown")
	}
	if finalize := s.manager.cfg.FinalizeResponse; finalize != nil {
		reserved := proto.Size(reply)
		finalizing := rpctrace.Start(ctx, rpctrace.StageFinalize)
		reply, err = finalize(ctx, s.scope, reply)
		finalizing()
		if err != nil || reply == nil || !reply.ProtoReflect().IsValid() || proto.Size(reply) > reserved || ctx.Err() != nil || s.closed.Load() {
			return nil, status.Error(codes.Unavailable, "service response no longer available")
		}
	}
	if err = s.responseAlive(ctx); err != nil {
		return nil, status.Error(codes.PermissionDenied, "original service peer is no longer available")
	}
	return reply, nil
}
func (r *rpcServer) Query(ctx context.Context, p *appv1.QueryRequest) (*appv1.QueryReply, error) {
	out, e := r.access(ctx, p, func(ctx context.Context, f *Fence) (proto.Message, error) {
		q, e := appv1.DecodeQuery(p)
		if e != nil {
			return nil, status.Error(codes.InvalidArgument, "invalid query")
		}
		fn := r.service.manager.cfg.Query
		if fn == nil {
			return nil, status.Error(codes.Unimplemented, "query unavailable")
		}
		page, e := fn(ctx, r.service.scope, q, f)
		if e != nil {
			return nil, status.Error(codes.PermissionDenied, "query unavailable for service")
		}
		return &appv1.QueryReply{SignedRecordJson: page.SignedRecordsJSON, Next: page.Next}, nil
	})
	if e != nil {
		return nil, e
	}
	return out.(*appv1.QueryReply), nil
}
func (r *rpcServer) PutEvidence(ctx context.Context, p *appv1.PutEvidenceRequest) (*appv1.PutEvidenceReply, error) {
	out, e := r.access(ctx, p, func(ctx context.Context, f *Fence) (proto.Message, error) {
		q, e := appv1.DecodePutEvidence(p)
		if e != nil {
			return nil, status.Error(codes.InvalidArgument, "invalid evidence upload")
		}
		fn := r.service.manager.cfg.PutEvidence
		if fn == nil {
			return nil, status.Error(codes.Unimplemented, "evidence unavailable")
		}
		v, e := fn(ctx, r.service.scope, serviceorigin.PublishScope{ComponentType: p.ComponentType, Stream: p.Stream}, q, f)
		if e != nil {
			return nil, status.Error(codes.Unavailable, "evidence upload unavailable; outcome may be unknown")
		}
		return &appv1.PutEvidenceReply{Sha256: v.SHA256, Size: v.Size, Received: v.Received, Complete: v.Complete, ContentType: v.ContentType, RetainUntilUnixMs: v.RetainUntilUnixMS}, nil
	})
	if e != nil {
		return nil, e
	}
	return out.(*appv1.PutEvidenceReply), nil
}
func (r *rpcServer) GetEvidence(ctx context.Context, p *appv1.GetEvidenceRequest) (*appv1.GetEvidenceReply, error) {
	out, e := r.access(ctx, p, func(ctx context.Context, f *Fence) (proto.Message, error) {
		source, q, e := appv1.DecodeGetEvidence(p)
		if e != nil {
			return nil, status.Error(codes.InvalidArgument, "invalid evidence read")
		}
		fn := r.service.manager.cfg.GetEvidence
		if fn == nil {
			return nil, status.Error(codes.Unimplemented, "evidence unavailable")
		}
		v, e := fn(ctx, r.service.scope, source, q, f)
		if e != nil {
			return nil, status.Error(codes.PermissionDenied, "evidence unavailable for service")
		}
		return &appv1.GetEvidenceReply{Sha256: v.SHA256, Size: v.Size, Offset: v.Offset, Data: v.Data, ContentType: v.ContentType, RetainUntilUnixMs: v.RetainUntilUnixMS}, nil
	})
	if e != nil {
		return nil, e
	}
	return out.(*appv1.GetEvidenceReply), nil
}
