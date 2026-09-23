//go:build !linux

package main

import (
	"github.com/wendylabsinc/wendy/go/internal/agent/services"
	"go.uber.org/zap"
)

type meshSharingManager struct{}

func newMeshSharingManager(_ string, _ *meshCatalogManager, _ *zap.Logger) *meshSharingManager {
	return &meshSharingManager{}
}
func (*meshSharingManager) Status() services.LocalMeshRuntimeStatus {
	return services.LocalMeshRuntimeStatus{}
}
