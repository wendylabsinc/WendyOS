package services

import (
	"context"

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
	return s.v1.streamContainerOutput(stream.Context(), req.GetAppName(), postStartAgentHookFromContext(stream.Context()), nil, &containerStreamV1Adapter{v2stream: stream})
}

// AttachContainer starts a container with stdin support, forwarding I/O
// bidirectionally. It delegates to the v1 service via an adapter so that the
// monitor bookkeeping (ClearExplicitStop / restart-policy registration) and
// log-manager fan-out in the v1 path apply equally to v2 attach clients.
func (s *ContainerServiceV2) AttachContainer(stream grpc.BidiStreamingServer[agentpbv2.AttachContainerRequest, agentpbv2.ContainerStreamResponse]) error {
	return s.v1.AttachContainer(&attachStreamV1Adapter{
		containerStreamV1Adapter: &containerStreamV1Adapter{v2stream: stream},
		v2stream:                 stream,
	})
}

// StopContainer stops a running container by name. It delegates to the v1
// service so the monitor's explicit-stop bookkeeping is applied and an
// unless-stopped container is not restarted out from under the caller.
func (s *ContainerServiceV2) StopContainer(ctx context.Context, req *agentpbv2.StopContainerRequest) (*agentpbv2.StopContainerResponse, error) {
	if s.v1.containerd == nil {
		return nil, status.Error(codes.Internal, "containerd is not available")
	}
	if _, err := s.v1.StopContainer(ctx, &agentpb.StopContainerRequest{
		AppName: req.GetAppName(),
	}); err != nil {
		return nil, err
	}
	return &agentpbv2.StopContainerResponse{}, nil
}

// DeleteContainer deletes a container and optionally its image and volumes. It
// delegates to the v1 service so the container is unregistered from the monitor
// before removal, closing the delete/restart race.
func (s *ContainerServiceV2) DeleteContainer(ctx context.Context, req *agentpbv2.DeleteContainerRequest) (*agentpbv2.DeleteContainerResponse, error) {
	if s.v1.containerd == nil {
		return nil, status.Error(codes.Internal, "containerd is not available")
	}
	if _, err := s.v1.DeleteContainer(ctx, &agentpb.DeleteContainerRequest{
		AppName:       req.GetAppName(),
		DeleteImage:   req.GetDeleteImage(),
		DeleteVolumes: req.GetDeleteVolumes(),
	}); err != nil {
		return nil, err
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

// attachStreamV1Adapter adapts a v2 bidirectional attach stream to the v1
// grpc.BidiStreamingServer[agentpb.AttachContainerRequest, agentpb.RunContainerLayersResponse]
// interface. The embedded containerStreamV1Adapter supplies Send (v1->v2
// response translation) and the grpc.ServerStream methods; this type adds the
// Recv direction, translating v2 attach requests to their v1 equivalents.
type attachStreamV1Adapter struct {
	*containerStreamV1Adapter
	v2stream grpc.BidiStreamingServer[agentpbv2.AttachContainerRequest, agentpbv2.ContainerStreamResponse]
}

func (a *attachStreamV1Adapter) Recv() (*agentpb.AttachContainerRequest, error) {
	msg, err := a.v2stream.Recv()
	if err != nil {
		return nil, err
	}
	switch rt := msg.GetRequestType().(type) {
	case *agentpbv2.AttachContainerRequest_AppName:
		return &agentpb.AttachContainerRequest{
			RequestType: &agentpb.AttachContainerRequest_AppName{AppName: rt.AppName},
		}, nil
	case *agentpbv2.AttachContainerRequest_StdinData:
		return &agentpb.AttachContainerRequest{
			RequestType: &agentpb.AttachContainerRequest_StdinData{StdinData: rt.StdinData},
		}, nil
	default:
		return &agentpb.AttachContainerRequest{}, nil
	}
}
