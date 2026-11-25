package appservice

import (
	"bytes"
	"context"

	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/serviceaccess"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func (r *rpcServer) GetSourceAuthority(ctx context.Context, p *appv1.GetSourceAuthorityRequest) (*appv1.SourceAuthorityReply, error) {
	out, err := r.access(ctx, p, func(ctx context.Context, f *Fence) (proto.Message, error) {
		nonce, sources, err := appv1.DecodeSourceAuthorityRequest(p)
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, "invalid source authority request")
		}
		fn := r.service.manager.cfg.SourceAuthority
		if fn == nil {
			return nil, status.Error(codes.Unimplemented, "source authority unavailable")
		}
		reply, err := fn(ctx, r.service.scope, nonce, sources, f)
		if err != nil {
			return nil, status.Error(codes.PermissionDenied, "source authority unavailable for service")
		}
		if _, err = appv1.DecodeSourceAuthorityReply(reply); err != nil || !bytes.Equal(reply.Nonce, p.Nonce) || len(reply.Authorities) != len(sources) {
			return nil, status.Error(codes.Unavailable, "invalid source authority response")
		}
		s := r.service.scope
		if reply.Tenant != s.Tenant || reply.Site != s.Site || reply.Device != s.Device || reply.App != s.App || reply.Service != s.Service || reply.ExecutionDomain != s.ExecutionDomain || reply.DeploymentGeneration != s.DeploymentGeneration {
			return nil, status.Error(codes.Unavailable, "source authority reader changed")
		}
		for i, item := range reply.Authorities {
			got := serviceaccess.Source{Device: item.Source.Device, App: item.Source.App, Service: item.Source.Service, ComponentType: item.Source.ComponentType, Stream: item.Source.Stream}
			if got != sources[i] {
				return nil, status.Error(codes.Unavailable, "source authority selection changed")
			}
		}
		return reply, nil
	})
	if err != nil {
		return nil, err
	}
	return out.(*appv1.SourceAuthorityReply), nil
}
