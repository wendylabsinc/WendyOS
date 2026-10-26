//go:build !linux

package main

import (
	"context"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/mesh"
	"go.uber.org/zap"
)

func runConfiguredNetCarriers(ctx context.Context, configDir string, id localmesh.TCPIdentity, _ *zap.Logger, _ *meshCatalogManager, _ *mesh.VIPTable, _ *meshSharingManager, observe func(func() localmesh.NodeSnapshot)) error {
	return localmesh.RunConfiguredTCPObserved(ctx, configDir, id, observe)
}
