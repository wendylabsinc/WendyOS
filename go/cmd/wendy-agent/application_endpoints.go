package main

import (
	"context"
	"fmt"
	"net"
	"sync"

	"github.com/wendylabsinc/wendy/go/internal/agent/configpartition"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/meshsession"
	"github.com/wendylabsinc/wendy/go/internal/agent/udpmessage"
	"go.uber.org/zap"
)

// startApplicationEndpoints keeps LAN and Cloud application ingress independent
// of carrier participation. These servers receive the app-only registry, never
// the native-agent-service wrapper used by routed management.
func startApplicationEndpoints(ctx context.Context, credentials *localmesh.Credentials, ingress meshsession.Authorizer, messages *udpmessage.Manager, allowLAN bool, logger *zap.Logger, launch func(func())) (*meshsession.ApplicationListener, func()) {
	return startApplicationEndpointsWithLAN(ctx, credentials, ingress, messages, allowLAN, logger, launch,
		func() (net.Listener, error) {
			return net.Listen("tcp", fmt.Sprintf(":%d", meshsession.ApplicationLANPort))
		},
		func(port uint16) { configpartition.UpdateAvahiApplicationTLS(logger, port) })
}

func startApplicationEndpointsWithLAN(ctx context.Context, credentials *localmesh.Credentials, ingress meshsession.Authorizer, messages *udpmessage.Manager, allowLAN bool, logger *zap.Logger, launch func(func()), listen func() (net.Listener, error), advertise func(uint16)) (*meshsession.ApplicationListener, func()) {
	ctx, cancel := context.WithCancel(ctx)
	cloud, err := meshsession.NewServer(credentials, ingress, messages)
	if err != nil {
		logger.Error("Cloud application admission unavailable", zap.Error(err))
		cancel()
		return nil, func() {}
	}
	listener := meshsession.NewApplicationListener()
	var lan *meshsession.Server
	var endpointMu sync.Mutex
	stopped := false
	retire := func() {
		endpointMu.Lock()
		defer endpointMu.Unlock()
		if stopped {
			return
		}
		stopped = true
		cancel()
		_ = cloud.Close()
		if lan != nil {
			_ = lan.Close()
		}
		if allowLAN {
			advertise(0)
		}
	}
	launch(func() {
		defer cloud.Close()
		if err := cloud.ServeTLS(ctx, listener); err != nil && ctx.Err() == nil {
			logger.Error("Cloud application admission stopped", zap.Error(err))
		}
	})
	if !allowLAN {
		return listener, retire
	}
	// Reconcile a stale advertisement before binding. A failed bind must not
	// advertise an old or unrelated process as this agent's app endpoint.
	advertise(0)
	lan, err = meshsession.NewServer(credentials, ingress, messages)
	if err != nil {
		logger.Error("LAN application admission unavailable", zap.Error(err))
		return listener, retire
	}
	lanListener, err := listen()
	if err != nil {
		_ = lan.Close()
		logger.Error("LAN application admission could not bind", zap.Error(err))
		return listener, retire
	}
	launch(func() {
		defer lan.Close()
		defer advertise(0)
		if err := lan.ServeTLS(ctx, lanListener, func() {
			endpointMu.Lock()
			defer endpointMu.Unlock()
			if !stopped {
				advertise(meshsession.ApplicationLANPort)
			}
		}); err != nil && ctx.Err() == nil {
			logger.Error("LAN application admission stopped", zap.Error(err))
		}
	})
	return listener, retire
}

// Routed listener failures stop their sibling listener and carrier publication,
// but must not retire the independently bound LAN/Cloud application endpoints.
// The parent still owns all of them for identity retirement and shutdown.
func startRoutedApplicationSessions(ctx context.Context, server *meshsession.Server, tlsAddress, quicAddress string, logger *zap.Logger, launch func(func())) (context.Context, bool) {
	routed, stop := context.WithCancel(ctx)
	ready := make(chan struct{}, 2)
	onReady := func() { ready <- struct{}{} }
	launch(func() {
		defer stop()
		if err := server.RunTLS(routed, tlsAddress, onReady); err != nil && routed.Err() == nil {
			logger.Error("wendy net TLS app session stopped", zap.Error(err))
		}
	})
	launch(func() {
		defer stop()
		if err := server.Run(routed, quicAddress, onReady); err != nil && routed.Err() == nil {
			logger.Error("wendy net app session stopped", zap.Error(err))
		}
	})
	for range 2 {
		select {
		case <-ready:
		case <-routed.Done():
			return routed, false
		}
	}
	return routed, routed.Err() == nil
}
