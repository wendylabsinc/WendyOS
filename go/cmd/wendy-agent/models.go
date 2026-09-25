package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	agentcontainerd "github.com/wendylabsinc/wendy/go/internal/agent/containerd"
	agentdata "github.com/wendylabsinc/wendy/go/internal/agent/data"
	agentmodels "github.com/wendylabsinc/wendy/go/internal/agent/models"
	"github.com/wendylabsinc/wendy/go/internal/agent/services"
	"go.uber.org/zap"
)

// modelRoot holds model files, built engines and per-instance run directories.
const modelRoot = "/var/lib/wendy/models"

// newModelSupervisor builds the model supervisor and removes model hosts a
// previous agent process left running.
func newModelSupervisor(ctx context.Context, logger *zap.Logger, ctrd *agentcontainerd.Client, video *services.VideoService, dataManager *agentdata.Manager) (*agentmodels.Supervisor, error) {
	catalog, err := loadModelCatalog(logger, os.Getenv("WENDY_MODEL_CATALOG_FILE"))
	if err != nil {
		return nil, err
	}
	sup := agentmodels.NewSupervisor(agentmodels.Config{
		Catalog: catalog,
		Device:  services.ModelDeviceProfile(),
		Runtime: ctrd,
		Cameras: services.ModelCameras{Video: video, Data: dataManager},
		Files:   agentmodels.NewFileCache(filepath.Join(modelRoot, "files"), nil),
		Root:    modelRoot,
		Logger:  logger.Named("models"),
	})
	if err := sup.CleanupOrphans(ctx); err != nil {
		logger.Warn("Removing model hosts left by a previous agent failed", zap.Error(err))
	}
	return sup, nil
}

// loadModelCatalog returns the catalog built into the agent, or the file at
// path when it is set. The override exists for development, for example to
// run the fake model host. Only root can set the agent's environment.
func loadModelCatalog(logger *zap.Logger, path string) (agentmodels.Catalog, error) {
	if path == "" {
		return agentmodels.DefaultCatalog()
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return agentmodels.Catalog{}, fmt.Errorf("reading WENDY_MODEL_CATALOG_FILE: %w", err)
	}
	logger.Warn("Using a development model catalog", zap.String("path", path))
	return agentmodels.ParseCatalog(raw)
}
