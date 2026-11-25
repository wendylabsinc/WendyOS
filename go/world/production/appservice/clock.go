package appservice

import (
	"bytes"
	"context"

	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func (r *rpcServer) Clock(ctx context.Context, p *appv1.ClockRequest) (*appv1.ClockReply, error) {
	out, err := r.access(ctx, p, func(ctx context.Context, f *Fence) (proto.Message, error) {
		nonce, err := appv1.DecodeClock(p)
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, "invalid clock request")
		}
		fn := r.service.manager.cfg.Clock
		if fn == nil {
			return nil, status.Error(codes.Unimplemented, "clock unavailable")
		}
		reply, err := fn(ctx, r.service.scope, nonce, f)
		if err != nil {
			return nil, status.Error(codes.Unavailable, "clock unavailable for service")
		}
		if _, err = appv1.DecodeClockReply(reply); err != nil || !bytes.Equal(p.Nonce, reply.Nonce) {
			return nil, status.Error(codes.Unavailable, "invalid clock sample")
		}
		s := r.service.scope
		if reply.Tenant != s.Tenant || reply.Site != s.Site || reply.Device != s.Device || reply.App != s.App || reply.Service != s.Service || reply.ExecutionDomain != s.ExecutionDomain || reply.DeploymentGeneration != s.DeploymentGeneration {
			return nil, status.Error(codes.Unavailable, "clock scope changed")
		}
		return reply, nil
	})
	if err != nil {
		return nil, err
	}
	return out.(*appv1.ClockReply), nil
}
