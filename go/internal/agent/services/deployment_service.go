package services

import (
	"context"
	"fmt"
	"io"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// attachDeployment runs a verified deployment whose process reads stdin from the
// rest of an AttachContainer stream.
func (s *ContainerService) attachDeployment(req *agentpb.RunContainerLayersRequest, stream grpc.BidiStreamingServer[agentpb.AttachContainerRequest, agentpb.RunContainerLayersResponse]) error {
	if !req.GetVerified() {
		return status.Error(codes.InvalidArgument, "attached deploy requires verified")
	}
	stdinR, stdinW := io.Pipe()
	defer stdinR.Close()
	go func() {
		defer stdinW.Close()
		for {
			input, err := stream.Recv()
			if err != nil {
				return
			}
			if data := input.GetStdinData(); len(data) > 0 {
				if _, err := stdinW.Write(data); err != nil {
					return
				}
			}
		}
	}()
	return s.streamDeployment(req, stream, stdinR)
}

// streamDeployment owns cutover, verification, and recovery as one serialized
// operation. Prepare is cancellable; once cutover begins the agent completes
// bounded verification and recovery even if the client disconnects.
func (s *ContainerService) streamDeployment(req *agentpb.RunContainerLayersRequest, stream grpc.ServerStreamingServer[agentpb.RunContainerLayersResponse], stdin io.Reader) error {
	events := make(chan *agentpb.RunContainerLayersResponse, 128)
	done := make(chan error, 1)
	// A slow or disconnected log reader must not hold verification, recovery,
	// or the application lifecycle lock hostage in grpc.SendMsg.
	go func() {
		done <- s.deployContainer(req, &deploymentEventStream{ServerStreamingServer: stream, events: events}, stdin)
	}()
	for {
		select {
		case <-stream.Context().Done():
			return status.FromContextError(stream.Context().Err()).Err()
		case event := <-events:
			if err := stream.Send(event); err != nil {
				return err
			}
		case err := <-done:
			for len(events) > 0 {
				if sendErr := stream.Send(<-events); sendErr != nil {
					return sendErr
				}
			}
			return err
		}
	}
}

type deploymentEventStream struct {
	grpc.ServerStreamingServer[agentpb.RunContainerLayersResponse]
	events chan *agentpb.RunContainerLayersResponse
}

func (s *deploymentEventStream) Send(event *agentpb.RunContainerLayersResponse) error {
	select {
	case s.events <- event:
		return nil
	default:
	}
	if event.GetDeployment() != nil || event.GetStarted() != nil {
		// There is one producer. Make room for a control event by dropping an
		// old log event, mirroring the bounded live log manager's behavior.
		select {
		case <-s.events:
		default:
		}
		s.events <- event
	}
	return nil
}

func (s *ContainerService) deployContainer(candidate *agentpb.RunContainerLayersRequest, stream grpc.ServerStreamingServer[agentpb.RunContainerLayersResponse], stdin io.Reader) error {
	runtime, ok := s.containerd.(DeploymentRuntime)
	if !ok {
		return status.Error(codes.Unimplemented, "this runtime does not support recoverable deployments")
	}
	prober, ok := s.containerd.(ContainerReadinessProber)
	if !ok {
		return status.Error(codes.Unimplemented, "this runtime does not support agent-owned readiness")
	}
	if err := appconfig.ValidateAppID(candidate.GetAppName()); err != nil {
		return status.Errorf(codes.InvalidArgument, "invalid app name: %v", err)
	}
	if candidate.GetImageName() == "" {
		return status.Error(codes.InvalidArgument, "image_name is required")
	}
	cfg, err := parseAppConfig(candidate.GetAppConfig())
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "invalid app config: %v", err)
	}
	if cfg.AppID == "" {
		cfg.AppID = candidate.GetAppName()
	}
	if cfg.ContainerName() != candidate.GetAppName() {
		return status.Error(codes.InvalidArgument, "app_name must match the app config container identity")
	}
	if err := cfg.Validate(); err != nil {
		return status.Errorf(codes.InvalidArgument, "invalid app config: %v", err)
	}
	// Only an explicit probe counts: the caller resolves implicit readiness
	// (HTTP entitlement, service ownership, timeout overrides) into app_config.
	var probe *appconfig.ReadinessConfig
	if cfg.Readiness.HasProbe() {
		probe = cfg.Readiness
	}
	ctx := stream.Context()
	unlock := s.appMu.lockApp(candidate.GetAppName())
	defer unlock()
	if err := ctx.Err(); err != nil {
		return status.FromContextError(err).Err()
	}
	if err := s.verifyImageSignature(candidate.GetImageConfig(), candidate.GetImageSignature()); err != nil {
		return err
	}
	if err := s.validateSignedLayerBinding(candidate.GetImageConfig(), candidate.GetLayers()); err != nil {
		return err
	}
	if err := s.assembleDeploymentImage(ctx, candidate); err != nil {
		return err
	}
	create := &agentpb.CreateContainerRequest{
		ImageName: candidate.GetImageName(), AppName: candidate.GetAppName(), Cmd: candidate.GetCmd(),
		AppConfig: candidate.GetAppConfig(), WorkingDir: candidate.GetWorkingDir(),
		RestartPolicy: candidate.GetRestartPolicy(), UserArgs: candidate.GetUserArgs(), Env: candidate.GetEnv(),
	}
	tx, err := runtime.PrepareDeployment(ctx, create, cfg)
	if err != nil {
		return status.Errorf(codes.FailedPrecondition, "preparing deployment before cutover: %v", err)
	}
	// This independent deadline covers activation plus the readiness window.
	// Recovery gets its own bounded budget even if activation exhausts this one.
	readyTimeout := 30 * time.Second
	if probe != nil {
		readyTimeout = probe.Timeout()
	}
	operationCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), readyTimeout+2*time.Minute)
	defer cancel()
	closed := false
	closeTx := func() {
		if closed {
			return
		}
		closed = true
		cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cleanupCancel()
		if err := tx.Close(cleanupCtx); err != nil {
			s.logger.Error("closing deployment transaction", zap.String("app_name", create.AppName), zap.Error(err))
		}
	}
	defer closeTx()
	if err := ctx.Err(); err != nil {
		return status.FromContextError(err).Err()
	}
	result := &agentpb.DeploymentResult{}
	hadPrevious := tx.PreviousRevision() != ""
	var readCh <-chan ContainerOutput
	var releaseDrain func()
	activationErr := tx.Activate(operationCtx)
	if activationErr == nil {
		var output <-chan ContainerOutput
		if stdin != nil {
			output, activationErr = s.containerd.StartContainerWithStdin(operationCtx, create.AppName, stdin, postStartAgentHookFromContext(ctx), create.RestartPolicy)
		} else {
			output, activationErr = s.containerd.StartContainer(operationCtx, create.AppName, postStartAgentHookFromContext(ctx), create.RestartPolicy)
		}
		if activationErr == nil {
			readCh, releaseDrain = s.guaranteeOutputDrain(create.AppName, output)
			defer releaseDrain()
		}
	}
	// Failed sends only stop delivery of events, never verification/recovery.
	connected := true
	send := func(event *agentpb.RunContainerLayersResponse) {
		if connected && stream.Send(event) != nil {
			connected = false
		}
	}
	if activationErr == nil {
		send(&agentpb.RunContainerLayersResponse{ResponseType: &agentpb.RunContainerLayersResponse_Started_{Started: &agentpb.RunContainerLayersResponse_Started{}}})
		checked := make(chan error, 1)
		go func() { checked <- waitForAgentReadiness(operationCtx, prober, create.AppName, probe) }()
	verify:
		for {
			select {
			case activationErr = <-checked:
				break verify
			case output, ok := <-readCh:
				if !ok || output.Done {
					readCh = nil
					continue // the process-state probe decides the outcome
				}
				for _, event := range deploymentOutputEvents(output) {
					send(event)
				}
			}
		}
	}
	if activationErr == nil {
		activationErr = tx.Commit(operationCtx)
	}
	if activationErr != nil {
		result.State = agentpb.DeploymentState_FAILED
		result.Message = activationErr.Error()
		recoveryCtx, recoveryCancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		recovered, recoveryErr := tx.Rollback(recoveryCtx)
		recoveryCancel()
		if recovered != nil {
			_, release := s.guaranteeOutputDrain(create.AppName, recovered)
			release()
		}
		if recoveryErr != nil {
			result.State = agentpb.DeploymentState_ROLLBACK_FAILED
			result.Message += "; recovery failed: " + recoveryErr.Error()
		} else if hadPrevious {
			result.State = agentpb.DeploymentState_ROLLED_BACK
			result.Message += "; previous revision restored"
		}
		// Never let the restart monitor revive an unverified candidate after
		// failed recovery releases its suppression lease.
		monitorCtx, monitorCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		if recoveryErr != nil || !hadPrevious {
			if s.monitor != nil {
				s.monitor.Unregister(create.AppName)
				s.monitor.MarkExplicitStop(create.AppName)
			}
			_ = s.containerd.SetStoppedByUser(monitorCtx, create.AppName, true)
		} else if tx.PreviousWasRunning() {
			// Read the restored revision's policy, not the candidate override.
			s.registerContainerWithMonitor(monitorCtx, create.AppName, nil)
		} else if s.monitor != nil {
			// Registration resets ExplicitStop. A rollback to a stopped revision
			// must not silently start it on the monitor's next tick.
			s.monitor.Unregister(create.AppName)
			s.monitor.MarkExplicitStop(create.AppName)
		}
		monitorCancel()
		closeTx()
		unlock()
		send(&agentpb.RunContainerLayersResponse{ResponseType: &agentpb.RunContainerLayersResponse_Deployment{Deployment: result}})
		return nil
	}
	result.State = agentpb.DeploymentState_RUNNING
	result.Message = "application started; no readiness probe configured"
	if probe != nil {
		result.State = agentpb.DeploymentState_READY
		result.Message = "application readiness verified on the agent"
	}
	if s.monitor != nil {
		s.monitor.ClearExplicitStop(create.AppName)
	}
	if err := s.containerd.SetStoppedByUser(operationCtx, create.AppName, false); err != nil {
		s.logger.Warn("clearing deployment stop mark", zap.String("app_name", create.AppName), zap.Error(err))
	}
	s.registerContainerWithMonitor(operationCtx, create.AppName, create.RestartPolicy)
	closeTx()
	unlock() // log streaming must not block future deployments/stop/delete
	send(&agentpb.RunContainerLayersResponse{ResponseType: &agentpb.RunContainerLayersResponse_Deployment{Deployment: result}})
	for connected && readCh != nil {
		select {
		case <-ctx.Done():
			return nil
		case output, ok := <-readCh:
			if !ok || output.Done {
				return nil
			}
			for _, event := range deploymentOutputEvents(output) {
				send(event)
			}
		}
	}
	return nil
}

func (s *ContainerService) assembleDeploymentImage(ctx context.Context, req *agentpb.RunContainerLayersRequest) error {
	for _, layer := range req.GetLayers() {
		if len(layer.GetChunkHashes()) == 0 {
			continue
		}
		hashes := make([][32]byte, 0, len(layer.GetChunkHashes()))
		for _, raw := range layer.GetChunkHashes() {
			hash, err := to32(raw)
			if err != nil {
				return err
			}
			hashes = append(hashes, hash)
		}
		diffID := layer.GetDiffId()
		if diffID == "" {
			diffID = layer.GetDigest()
		}
		if err := s.containerd.AssembleLayerFromChunks(ctx, diffID, hashes); err != nil {
			return status.Errorf(codes.Internal, "assembling candidate layer: %v", err)
		}
	}
	if len(req.GetLayers()) > 0 {
		if err := s.containerd.AssembleImage(ctx, req.GetImageName(), req.GetLayers(), req.GetImageConfig()); err != nil {
			return status.Errorf(codes.Internal, "assembling candidate image: %v", err)
		}
	}
	return nil
}

func waitForAgentReadiness(ctx context.Context, prober ContainerReadinessProber, name string, cfg *appconfig.ReadinessConfig) error {
	if cfg == nil {
		attemptCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		return prober.ProbeReadiness(attemptCtx, name, nil)
	}
	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout())
	defer cancel()
	var lastErr error
	for {
		attempt, stop := context.WithTimeout(ctx, cfg.ProbeTimeout())
		lastErr = prober.ProbeReadiness(attempt, name, cfg)
		stop()
		if lastErr == nil && ctx.Err() == nil {
			return nil
		}
		timer := time.NewTimer(cfg.Period())
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("readiness did not pass within %s: %v (%w)", cfg.Timeout(), lastErr, ctx.Err())
		case <-timer.C:
		}
	}
}

func deploymentOutputEvents(output ContainerOutput) []*agentpb.RunContainerLayersResponse {
	var events []*agentpb.RunContainerLayersResponse
	if len(output.Stdout) > 0 {
		events = append(events, &agentpb.RunContainerLayersResponse{ResponseType: &agentpb.RunContainerLayersResponse_StdoutOutput{StdoutOutput: &agentpb.RunContainerLayersResponse_ConsoleOutput{Data: output.Stdout}}})
	}
	if len(output.Stderr) > 0 {
		events = append(events, &agentpb.RunContainerLayersResponse{ResponseType: &agentpb.RunContainerLayersResponse_StderrOutput{StderrOutput: &agentpb.RunContainerLayersResponse_ConsoleOutput{Data: output.Stderr}}})
	}
	return events
}
