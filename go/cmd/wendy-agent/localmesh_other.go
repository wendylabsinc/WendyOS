//go:build !linux

package main

import (
	"context"
	"net"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/mesh"
	"go.uber.org/zap"
)

func runConfiguredNetCarriers(ctx context.Context, configDir string, id localmesh.TCPIdentity, _ *zap.Logger, _ *meshCatalogManager, _ *mesh.VIPTable, _ *meshSharingManager, observe func(func() localmesh.NodeSnapshot), agent ...func(context.Context, net.Conn) error) error {
	return localmesh.RunConfiguredTCPObserved(ctx, configDir, id, observe)
}
