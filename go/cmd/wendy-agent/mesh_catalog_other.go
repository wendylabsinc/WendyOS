//go:build !linux

package main

import (
	"context"
	"errors"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/meshcatalog"
	"github.com/wendylabsinc/wendy/go/internal/agent/meshingress"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"go.uber.org/zap"
)

type meshCatalogManager struct{}

func newMeshCatalogManager(string, *meshingress.Registry, *zap.Logger) *meshCatalogManager {
	return &meshCatalogManager{}
}
func (*meshCatalogManager) Activate(context.Context, *localmesh.Credentials, func() localmesh.NodeSnapshot, func()) error {
	return nil
}
func (*meshCatalogManager) Deactivate() {}
func (*meshCatalogManager) StartMeshApp(string, string, string, string, []appconfig.PortMapping) error {
	return nil
}
func (*meshCatalogManager) StopMeshApp(string) {}
func (*meshCatalogManager) SetGatewayOffer(enabled bool) error {
	if enabled {
		return errors.New("mesh catalog is unavailable on this platform")
	}
	return nil
}
func (*meshCatalogManager) GatewayOffers() []meshcatalog.Record { return nil }
