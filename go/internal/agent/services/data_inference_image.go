package services

import (
	"context"

	"github.com/wendylabsinc/wendy/go/internal/agent/inference"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// InferenceWorkerImage tells the command line which image this agent runs a
// campaign's model worker in and whether the device has it, and hands it the
// files to build it from. An agent whose worker never runs in a container
// answers with no repository, and the command line builds nothing.
func (s *DataService) InferenceWorkerImage(ctx context.Context, _ *agentpbv2.DataInferenceWorkerImageRequest) (*agentpbv2.DataInferenceWorkerImageResponse, error) {
	if s.inference == nil {
		return &agentpbv2.DataInferenceWorkerImageResponse{}, nil
	}
	images, ok := s.inference.factory.(interface {
		WorkerImage(context.Context) inference.WorkerImageInfo
	})
	if !ok {
		return &agentpbv2.DataInferenceWorkerImageResponse{}, nil
	}
	info := images.WorkerImage(ctx)
	return &agentpbv2.DataInferenceWorkerImageResponse{Repository: info.Repository, DepsHash: info.DepsHash, Present: info.Present, BuildContext: info.BuildContext}, nil
}
