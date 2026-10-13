//go:build !linux

package main

import (
	"context"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"go.uber.org/zap"
)

func runConfiguredMeshCarriers(ctx context.Context, configDir string, id localmesh.TCPIdentity, _ *zap.Logger, _ *meshCatalogManager, observe func(func() localmesh.NodeSnapshot)) error {
	return localmesh.RunConfiguredTCPObserved(ctx, configDir, id, observe)
}
