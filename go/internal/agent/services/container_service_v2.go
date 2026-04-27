package services

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	agentpb "github.com/wendylabsinc/wendy/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/proto/gen/agentpb/v2"
)

// ContainerServiceV2 implements agentpbv2.WendyContainerServiceServer by
// delegating to the v1 ContainerService where possible.
type ContainerServiceV2 struct {
	agentpbv2.UnimplementedWendyContainerServiceServer
	v1 *ContainerService
}

// NewContainerServiceV2 creates a new ContainerServiceV2 wrapping the given v1 service.
func NewContainerServiceV2(v1 *ContainerService) *ContainerServiceV2 {
	return &ContainerServiceV2{v1: v1}
}

// StartContainer starts an existing container and streams its output to the client.
func (s *ContainerServiceV2) StartContainer(req *agentpbv2.StartContainerRequest, stream grpc.ServerStreamingServer[agentpbv2.ContainerStreamResponse]) error {
	return s.v1.streamContainerOutput(stream.Context(), req.GetAppName(), &containerStreamV1Adapter{v2stream: stream})
}

// AttachContainer starts a container with stdin support, forwarding I/O bidirectionally.
func (s *ContainerServiceV2) AttachContainer(stream grpc.BidiStreamingServer[agentpbv2.AttachContainerRequest, agentpbv2.ContainerStreamResponse]) error {
	first, err := stream.Recv()
	if err == io.EOF {
		return status.Error(codes.InvalidArgument, "missing first attach message")
	}
	if err != nil {
		return err
	}
	appName := first.GetAppName()
	if appName == "" {
		return status.Error(codes.InvalidArgument, "app_name required as first message")
	}

	ctx := stream.Context()
	stdinR, stdinW := io.Pipe()
	defer stdinR.Close()

	go func() {
		defer stdinW.Close()
		for {
			msg, recvErr := stream.Recv()
			if recvErr != nil {
				return
			}
			if data := msg.GetStdinData(); len(data) > 0 {
				if _, writeErr := stdinW.Write(data); writeErr != nil {
					return
				}
			}
		}
	}()

	outputCh, err := s.v1.containerd.StartContainerWithStdin(ctx, appName, stdinR)
	if err != nil {
		stdinR.Close()
		return status.Errorf(codes.Internal, "failed to start container: %v", err)
	}

	if err := stream.Send(&agentpbv2.ContainerStreamResponse{
		ResponseType: &agentpbv2.ContainerStreamResponse_Started_{
			Started: &agentpbv2.ContainerStreamResponse_Started{},
		},
	}); err != nil {
		return err
	}

	var readCh <-chan ContainerOutput
	if s.v1.logManager != nil {
		subID, subCh := s.v1.logManager.Subscribe(appName)
		defer s.v1.logManager.Unsubscribe(appName, subID)
		readCh = subCh
		go func() {
			for output := range outputCh {
				s.v1.logManager.Publish(appName, output)
			}
			s.v1.logManager.Publish(appName, ContainerOutput{Done: true})
		}()
	} else {
		readCh = outputCh
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case output, ok := <-readCh:
			if !ok || output.Done {
				return nil
			}
			if len(output.Stdout) > 0 {
				if err := stream.Send(&agentpbv2.ContainerStreamResponse{
					ResponseType: &agentpbv2.ContainerStreamResponse_StdoutOutput{
						StdoutOutput: &agentpbv2.ContainerStreamResponse_ConsoleOutput{Data: output.Stdout},
					},
				}); err != nil {
					return err
				}
			}
			if len(output.Stderr) > 0 {
				if err := stream.Send(&agentpbv2.ContainerStreamResponse{
					ResponseType: &agentpbv2.ContainerStreamResponse_StderrOutput{
						StderrOutput: &agentpbv2.ContainerStreamResponse_ConsoleOutput{Data: output.Stderr},
					},
				}); err != nil {
					return err
				}
			}
		}
	}
}

// StopContainer stops a running container by name.
func (s *ContainerServiceV2) StopContainer(ctx context.Context, req *agentpbv2.StopContainerRequest) (*agentpbv2.StopContainerResponse, error) {
	if s.v1.containerd == nil {
		return nil, status.Error(codes.Internal, "containerd is not available")
	}
	if err := s.v1.containerd.StopContainer(ctx, req.GetAppName()); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to stop container: %v", err)
	}
	s.v1.logger.Info("Container stopped", zap.String("app_name", req.GetAppName()))
	return &agentpbv2.StopContainerResponse{}, nil
}

// DeleteContainer deletes a container and optionally its image and volumes.
func (s *ContainerServiceV2) DeleteContainer(ctx context.Context, req *agentpbv2.DeleteContainerRequest) (*agentpbv2.DeleteContainerResponse, error) {
	if s.v1.containerd == nil {
		return nil, status.Error(codes.Internal, "containerd is not available")
	}
	if err := s.v1.containerd.DeleteContainer(ctx, req.GetAppName(), req.GetDeleteImage()); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to delete container: %v", err)
	}
	if req.GetDeleteVolumes() {
		deleteVolumesByAppName(s.v1.logger, req.GetAppName())
	}
	return &agentpbv2.DeleteContainerResponse{}, nil
}

// ListContainers streams all known containers to the client.
func (s *ContainerServiceV2) ListContainers(_ *agentpbv2.ListContainersRequest, stream grpc.ServerStreamingServer[agentpbv2.ListContainersResponse]) error {
	if s.v1.containerd == nil {
		return nil
	}
	containers, err := s.v1.containerd.ListContainers(stream.Context())
	if err != nil {
		return status.Errorf(codes.Internal, "failed to list containers: %v", err)
	}
	for _, c := range containers {
		if err := stream.Send(&agentpbv2.ListContainersResponse{
			Container: mapAppContainerToV2(c),
		}); err != nil {
			return err
		}
	}
	return nil
}

// ListVolumes delegates to v1 ListVolumes and maps the response to v2 types.
func (s *ContainerServiceV2) ListVolumes(ctx context.Context, _ *agentpbv2.ListVolumesRequest) (*agentpbv2.ListVolumesResponse, error) {
	v1resp, err := s.v1.ListVolumes(ctx, &agentpb.ListVolumesRequest{})
	if err != nil {
		return nil, err
	}
	v2vols := make([]*agentpbv2.VolumeInfo, len(v1resp.Volumes))
	for i, v := range v1resp.Volumes {
		v2vols[i] = &agentpbv2.VolumeInfo{
			Name:      v.Name,
			Path:      v.Path,
			SizeBytes: v.SizeBytes,
			CreatedAt: v.CreatedAt,
			UsedBy:    v.UsedBy,
		}
	}
	return &agentpbv2.ListVolumesResponse{Volumes: v2vols}, nil
}

// RemoveVolume delegates to v1 RemoveVolume.
func (s *ContainerServiceV2) RemoveVolume(ctx context.Context, req *agentpbv2.RemoveVolumeRequest) (*agentpbv2.RemoveVolumeResponse, error) {
	if _, err := s.v1.RemoveVolume(ctx, &agentpb.RemoveVolumeRequest{Name: req.GetName()}); err != nil {
		return nil, err
	}
	return &agentpbv2.RemoveVolumeResponse{}, nil
}

// ListContainerStats returns memory and storage stats for all managed containers.
func (s *ContainerServiceV2) ListContainerStats(ctx context.Context, _ *agentpbv2.ListContainerStatsRequest) (*agentpbv2.ListContainerStatsResponse, error) {
	if s.v1.containerd == nil {
		return &agentpbv2.ListContainerStatsResponse{}, nil
	}
	stats, err := s.v1.containerd.GetContainerStats(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get container stats: %v", err)
	}
	v2stats := make([]*agentpbv2.ContainerStats, len(stats))
	for i, st := range stats {
		v2stats[i] = &agentpbv2.ContainerStats{
			AppName:      st.AppName,
			MemoryBytes:  st.MemoryBytes,
			StorageBytes: st.StorageBytes,
		}
	}
	return &agentpbv2.ListContainerStatsResponse{Stats: v2stats}, nil
}

// deleteVolumesByAppName removes all volume directories belonging to the given app.
func deleteVolumesByAppName(logger *zap.Logger, appName string) {
	entries, err := os.ReadDir(volumesDir)
	if err != nil {
		logger.Warn("Failed to read volumes directory",
			zap.String("base", volumesDir),
			zap.String("app_name", appName),
			zap.Error(err),
		)
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if name == appName || strings.HasPrefix(name, appName+"-") {
			path := filepath.Join(volumesDir, name)
			if err := os.RemoveAll(path); err != nil {
				logger.Warn("Failed to remove volume", zap.String("path", path), zap.Error(err))
			} else {
				logger.Info("Volume removed", zap.String("path", path))
			}
		}
	}
}

// mapAppContainerToV2 converts a v1 AppContainer to its v2 equivalent,
// mapping the running state enum explicitly (v1 STOPPED=0/RUNNING=1 vs v2 STOPPED=1/RUNNING=2).
func mapAppContainerToV2(c *agentpb.AppContainer) *agentpbv2.AppContainer {
	var state agentpbv2.AppRunningState
	switch c.RunningState {
	case agentpb.AppRunningState_RUNNING:
		state = agentpbv2.AppRunningState_APP_RUNNING_STATE_RUNNING
	default:
		state = agentpbv2.AppRunningState_APP_RUNNING_STATE_STOPPED
	}
	return &agentpbv2.AppContainer{
		AppName:      c.AppName,
		AppVersion:   c.AppVersion,
		RunningState: state,
		FailureCount: c.FailureCount,
	}
}

// containerStreamV1Adapter adapts a v2 ServerStreamingServer to the v1
// grpc.ServerStreamingServer[agentpb.RunContainerLayersResponse] interface,
// translating each v1 response message to its v2 equivalent before sending.
type containerStreamV1Adapter struct {
	v2stream grpc.ServerStreamingServer[agentpbv2.ContainerStreamResponse]
}

func (a *containerStreamV1Adapter) Send(resp *agentpb.RunContainerLayersResponse) error {
	switch t := resp.ResponseType.(type) {
	case *agentpb.RunContainerLayersResponse_Started_:
		_ = t
		return a.v2stream.Send(&agentpbv2.ContainerStreamResponse{
			ResponseType: &agentpbv2.ContainerStreamResponse_Started_{
				Started: &agentpbv2.ContainerStreamResponse_Started{},
			},
		})
	case *agentpb.RunContainerLayersResponse_StdoutOutput:
		return a.v2stream.Send(&agentpbv2.ContainerStreamResponse{
			ResponseType: &agentpbv2.ContainerStreamResponse_StdoutOutput{
				StdoutOutput: &agentpbv2.ContainerStreamResponse_ConsoleOutput{Data: t.StdoutOutput.Data},
			},
		})
	case *agentpb.RunContainerLayersResponse_StderrOutput:
		return a.v2stream.Send(&agentpbv2.ContainerStreamResponse{
			ResponseType: &agentpbv2.ContainerStreamResponse_StderrOutput{
				StderrOutput: &agentpbv2.ContainerStreamResponse_ConsoleOutput{Data: t.StderrOutput.Data},
			},
		})
	}
	return nil
}

func (a *containerStreamV1Adapter) SetHeader(md metadata.MD) error  { return a.v2stream.SetHeader(md) }
func (a *containerStreamV1Adapter) SendHeader(md metadata.MD) error { return a.v2stream.SendHeader(md) }
func (a *containerStreamV1Adapter) SetTrailer(md metadata.MD)       { a.v2stream.SetTrailer(md) }
func (a *containerStreamV1Adapter) Context() context.Context        { return a.v2stream.Context() }
func (a *containerStreamV1Adapter) SendMsg(m any) error             { return a.v2stream.SendMsg(m) }
func (a *containerStreamV1Adapter) RecvMsg(m any) error             { return a.v2stream.RecvMsg(m) }
