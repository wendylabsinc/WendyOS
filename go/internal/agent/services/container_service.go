package services

import (
	"bytes"
	"context"
	"encoding/json"
	"io"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/internal/shared/appconfig"
	agentpb "github.com/wendylabsinc/wendy/proto/gen/agentpb"
)

// ContainerService implements agentpb.WendyContainerServiceServer.
type ContainerService struct {
	agentpb.UnimplementedWendyContainerServiceServer
	logger     *zap.Logger
	containerd ContainerdClient
	logManager *ContainerLogManager
}

// NewContainerService creates a new ContainerService.
func NewContainerService(logger *zap.Logger, client ContainerdClient, opts ...ContainerServiceOption) *ContainerService {
	s := &ContainerService{
		logger:     logger,
		containerd: client,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// ContainerServiceOption configures a ContainerService.
type ContainerServiceOption func(*ContainerService)

// WithLogManager sets the ContainerLogManager on the ContainerService.
func WithLogManager(lm *ContainerLogManager) ContainerServiceOption {
	return func(s *ContainerService) {
		s.logManager = lm
	}
}

// ListLayers streams the OCI image layers present in containerd.
func (s *ContainerService) ListLayers(_ *agentpb.ListLayersRequest, stream grpc.ServerStreamingServer[agentpb.LayerHeader]) error {
	ctx := stream.Context()
	layers, err := s.containerd.ListLayers(ctx)
	if err != nil {
		return status.Errorf(codes.Internal, "failed to list layers: %v", err)
	}

	for _, layer := range layers {
		if err := stream.Send(layer); err != nil {
			return err
		}
	}
	return nil
}

// WriteLayer receives a streaming layer upload and writes it to the containerd
// content store. Chunks are buffered in memory before writing.
func (s *ContainerService) WriteLayer(stream grpc.BidiStreamingServer[agentpb.WriteLayerRequest, agentpb.WriteLayerResponse]) error {
	ctx := stream.Context()

	// Receive the first message to get the digest.
	first, err := stream.Recv()
	if err == io.EOF {
		return status.Error(codes.InvalidArgument, "empty layer upload stream")
	}
	if err != nil {
		return status.Errorf(codes.Internal, "error receiving first layer message: %v", err)
	}

	digest := first.GetDigest()
	if digest == "" {
		return status.Error(codes.InvalidArgument, "no digest provided in layer upload")
	}

	// Buffer all chunks.
	var data []byte
	if chunk := first.GetData(); len(chunk) > 0 {
		data = append(data, chunk...)
	}

	for {
		msg, recvErr := stream.Recv()
		if recvErr == io.EOF {
			break
		}
		if recvErr != nil {
			return status.Errorf(codes.Internal, "error receiving layer data: %v", recvErr)
		}
		if chunk := msg.GetData(); len(chunk) > 0 {
			data = append(data, chunk...)
		}
	}

	s.logger.Info("Received layer data",
		zap.String("digest", digest),
		zap.Int("bytes", len(data)),
	)

	// Write to containerd content store.
	if err := s.containerd.WriteLayer(ctx, digest, bytes.NewReader(data), int64(len(data))); err != nil {
		return status.Errorf(codes.Internal, "failed to write layer: %v", err)
	}

	s.logger.Info("Layer written", zap.String("digest", digest), zap.Int("size", len(data)))
	return stream.Send(&agentpb.WriteLayerResponse{})
}

// CreateContainer creates a container from an image with entitlements.
func (s *ContainerService) CreateContainer(ctx context.Context, req *agentpb.CreateContainerRequest) (*agentpb.CreateContainerResponse, error) {
	appCfg, err := parseAppConfig(req.GetAppConfig())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid app config: %v", err)
	}

	if err := s.containerd.CreateContainer(ctx, req, appCfg); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to create container: %v", err)
	}

	s.logger.Info("Container created",
		zap.String("app_name", req.GetAppName()),
		zap.String("image", req.GetImageName()),
	)
	return &agentpb.CreateContainerResponse{}, nil
}

// CreateContainerWithProgress creates a container and streams progress.
func (s *ContainerService) CreateContainerWithProgress(req *agentpb.CreateContainerRequest, stream grpc.ServerStreamingServer[agentpb.CreateContainerProgressResponse]) error {
	appCfg, err := parseAppConfig(req.GetAppConfig())
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "invalid app config: %v", err)
	}

	onProgress := func(p *agentpb.CreateContainerProgress) {
		if err := stream.Send(&agentpb.CreateContainerProgressResponse{
			ResponseType: &agentpb.CreateContainerProgressResponse_Progress{
				Progress: p,
			},
		}); err != nil {
			s.logger.Warn("failed to send progress update", zap.Error(err))
		}
	}

	if err := s.containerd.CreateContainerWithProgress(stream.Context(), req, appCfg, onProgress); err != nil {
		return status.Errorf(codes.Internal, "failed to create container: %v", err)
	}

	// Send completed response.
	return stream.Send(&agentpb.CreateContainerProgressResponse{
		ResponseType: &agentpb.CreateContainerProgressResponse_Completed{
			Completed: &agentpb.CreateContainerResponse{},
		},
	})
}

// RunContainer runs a container and streams stdout/stderr.
func (s *ContainerService) RunContainer(req *agentpb.RunContainerLayersRequest, stream grpc.ServerStreamingServer[agentpb.RunContainerLayersResponse]) error {
	ctx := stream.Context()

	// Parse app config.
	appCfg, err := parseAppConfig(req.GetAppConfig())
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "invalid app config: %v", err)
	}

	// Assemble the image from uploaded layers if layer headers are provided.
	if layers := req.GetLayers(); len(layers) > 0 {
		if err := s.containerd.AssembleImage(ctx, req.GetImageName(), layers); err != nil {
			return status.Errorf(codes.Internal, "failed to assemble image: %v", err)
		}
	}

	// Create the container.
	createReq := &agentpb.CreateContainerRequest{
		ImageName:     req.GetImageName(),
		AppName:       req.GetAppName(),
		Cmd:           req.GetCmd(),
		AppConfig:     req.GetAppConfig(),
		WorkingDir:    req.GetWorkingDir(),
		RestartPolicy: req.GetRestartPolicy(),
		UserArgs:      req.GetUserArgs(),
	}

	if err := s.containerd.CreateContainer(ctx, createReq, appCfg); err != nil {
		return status.Errorf(codes.Internal, "failed to create container: %v", err)
	}

	return s.streamContainerOutput(ctx, req.GetAppName(), stream)
}

// StartContainer starts an existing container and streams output.
func (s *ContainerService) StartContainer(req *agentpb.StartContainerRequest, stream grpc.ServerStreamingServer[agentpb.RunContainerLayersResponse]) error {
	return s.streamContainerOutput(stream.Context(), req.GetAppName(), stream)
}

// streamContainerOutput starts a container and streams its stdout/stderr to the client.
// When a ContainerLogManager is configured, it reads from the log manager subscription
// instead of directly from containerd, enabling multi-subscriber fan-out and telemetry bridging.
func (s *ContainerService) streamContainerOutput(
	ctx context.Context,
	appName string,
	stream grpc.ServerStreamingServer[agentpb.RunContainerLayersResponse],
) error {
	outputCh, err := s.containerd.StartContainer(ctx, appName)
	if err != nil {
		return status.Errorf(codes.Internal, "failed to start container: %v", err)
	}

	// Send started notification.
	if err := stream.Send(&agentpb.RunContainerLayersResponse{
		ResponseType: &agentpb.RunContainerLayersResponse_Started_{
			Started: &agentpb.RunContainerLayersResponse_Started{},
		},
	}); err != nil {
		return err
	}

	// If a log manager is configured, start a goroutine that publishes containerd
	// output to the log manager, and subscribe to read from it.
	var readCh <-chan ContainerOutput
	if s.logManager != nil {
		// Subscribe BEFORE starting the pump to avoid missing early output.
		subID, subCh := s.logManager.Subscribe(appName)
		defer s.logManager.Unsubscribe(appName, subID)
		readCh = subCh

		// Pump containerd output into the log manager.
		go func() {
			for output := range outputCh {
				s.logManager.Publish(appName, output)
			}
			// When containerd channel closes, publish a Done marker.
			s.logManager.Publish(appName, ContainerOutput{Done: true})
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
				if err := stream.Send(&agentpb.RunContainerLayersResponse{
					ResponseType: &agentpb.RunContainerLayersResponse_StdoutOutput{
						StdoutOutput: &agentpb.RunContainerLayersResponse_ConsoleOutput{
							Data: output.Stdout,
						},
					},
				}); err != nil {
					return err
				}
			}
			if len(output.Stderr) > 0 {
				if err := stream.Send(&agentpb.RunContainerLayersResponse{
					ResponseType: &agentpb.RunContainerLayersResponse_StderrOutput{
						StderrOutput: &agentpb.RunContainerLayersResponse_ConsoleOutput{
							Data: output.Stderr,
						},
					},
				}); err != nil {
					return err
				}
			}
		}
	}
}

// AttachContainer starts a container and multiplexes stdin from the client
// with stdout/stderr back to the client over a single bidirectional stream.
// The first client message must set app_name; subsequent messages carry stdin data.
func (s *ContainerService) AttachContainer(stream grpc.BidiStreamingServer[agentpb.AttachContainerRequest, agentpb.RunContainerLayersResponse]) error {
	// First message must identify the app.
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

	// Pipe client stdin messages into the container's stdin.
	stdinR, stdinW := io.Pipe()
	defer stdinR.Close()

	// Goroutine: forward stdin_data messages from the gRPC stream to stdinW.
	go func() {
		defer stdinW.Close()
		for {
			msg, recvErr := stream.Recv()
			if recvErr != nil {
				return // client disconnected or closed send
			}
			if data := msg.GetStdinData(); len(data) > 0 {
				if _, writeErr := stdinW.Write(data); writeErr != nil {
					return
				}
			}
		}
	}()

	outputCh, err := s.containerd.StartContainerWithStdin(ctx, appName, stdinR)
	if err != nil {
		stdinR.Close()
		return status.Errorf(codes.Internal, "failed to start container: %v", err)
	}

	// Send started notification.
	if err := stream.Send(&agentpb.RunContainerLayersResponse{
		ResponseType: &agentpb.RunContainerLayersResponse_Started_{
			Started: &agentpb.RunContainerLayersResponse_Started{},
		},
	}); err != nil {
		return err
	}

	// Fan-out via log manager if configured.
	var readCh <-chan ContainerOutput
	if s.logManager != nil {
		subID, subCh := s.logManager.Subscribe(appName)
		defer s.logManager.Unsubscribe(appName, subID)
		readCh = subCh

		go func() {
			for output := range outputCh {
				s.logManager.Publish(appName, output)
			}
			s.logManager.Publish(appName, ContainerOutput{Done: true})
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
				if err := stream.Send(&agentpb.RunContainerLayersResponse{
					ResponseType: &agentpb.RunContainerLayersResponse_StdoutOutput{
						StdoutOutput: &agentpb.RunContainerLayersResponse_ConsoleOutput{
							Data: output.Stdout,
						},
					},
				}); err != nil {
					return err
				}
			}
			if len(output.Stderr) > 0 {
				if err := stream.Send(&agentpb.RunContainerLayersResponse{
					ResponseType: &agentpb.RunContainerLayersResponse_StderrOutput{
						StderrOutput: &agentpb.RunContainerLayersResponse_ConsoleOutput{
							Data: output.Stderr,
						},
					},
				}); err != nil {
					return err
				}
			}
		}
	}
}

// StopContainer stops a running container.
func (s *ContainerService) StopContainer(ctx context.Context, req *agentpb.StopContainerRequest) (*agentpb.StopContainerResponse, error) {
	if err := s.containerd.StopContainer(ctx, req.GetAppName()); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to stop container: %v", err)
	}
	s.logger.Info("Container stopped", zap.String("app_name", req.GetAppName()))
	return &agentpb.StopContainerResponse{}, nil
}

// DeleteContainer deletes a container and optionally its image.
func (s *ContainerService) DeleteContainer(ctx context.Context, req *agentpb.DeleteContainerRequest) (*agentpb.DeleteContainerResponse, error) {
	if err := s.containerd.DeleteContainer(ctx, req.GetAppName(), req.GetDeleteImage()); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to delete container: %v", err)
	}
	s.logger.Info("Container deleted",
		zap.String("app_name", req.GetAppName()),
		zap.Bool("delete_image", req.GetDeleteImage()),
	)
	return &agentpb.DeleteContainerResponse{}, nil
}

// ListContainers lists running containers.
func (s *ContainerService) ListContainers(_ *agentpb.ListContainersRequest, stream grpc.ServerStreamingServer[agentpb.ListContainersResponse]) error {
	containers, err := s.containerd.ListContainers(stream.Context())
	if err != nil {
		return status.Errorf(codes.Internal, "failed to list containers: %v", err)
	}

	for _, c := range containers {
		if err := stream.Send(&agentpb.ListContainersResponse{Container: c}); err != nil {
			return err
		}
	}
	return nil
}

// parseAppConfig parses the wendy.json app config bytes.
func parseAppConfig(data []byte) (*appconfig.AppConfig, error) {
	if len(data) == 0 {
		return &appconfig.AppConfig{}, nil
	}
	var cfg appconfig.AppConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}
