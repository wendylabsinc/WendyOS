package main

import (
	"context"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/meshsession"
	"github.com/wendylabsinc/wendy/go/internal/agent/udpmessage"
	"go.uber.org/zap"
)

// startCloudApplicationEndpoint accepts broker transports in memory. It uses
// the app-port registry without the management port admitted by routed mesh.
func startCloudApplicationEndpoint(ctx context.Context, credentials *localmesh.Credentials, ingress meshsession.Authorizer, messages *udpmessage.Manager, logger *zap.Logger, launch func(func())) (*meshsession.ApplicationListener, func()) {
	server, err := meshsession.NewServer(credentials, ingress, messages)
	if err != nil {
		logger.Error("Cloud application admission unavailable", zap.Error(err))
		return nil, func() {}
	}
	ctx, cancel := context.WithCancel(ctx)
	listener := meshsession.NewApplicationListener()
	launch(func() {
		defer server.Close()
		if err := server.ServeTLS(ctx, listener); err != nil && ctx.Err() == nil {
			logger.Error("Cloud application admission stopped", zap.Error(err))
		}
	})
	return listener, func() {
		cancel()
		_ = listener.Close()
		_ = server.Close()
	}
}
