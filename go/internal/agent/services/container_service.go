package services

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/containerd/errdefs"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	_ "google.golang.org/grpc/encoding/gzip" // accept compressed WriteChunks messages
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/agent/hoststats"
	"github.com/wendylabsinc/wendy/go/internal/agent/logfields"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/internal/shared/sigverify"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

type ContainerService struct {
	agentpb.UnimplementedWendyContainerServiceServer
	logger     *zap.Logger
	containerd ContainerdClient
	logManager *ContainerLogManager
	monitor    ContainerMonitorRegistrar

	// imageVerifier checks the container image signature (see RunContainer)
	// before the image is assembled or the container created. This verifies
	// against the per-org PUBLISHER key sourced from provisioning/PKI (the
	// cosign / per-org-registry trust model) — container images are per-org
	// artifacts, not first-party Wendy artifacts. It MUST NOT default to (or
	// otherwise use) sigverify.DefaultVerifier, which is the Wendy
	// build-embedded key reserved for first-party agent-binary / OS
	// artifact verification (see agent_service.go / agent_update_service.go).
	// Until the org publisher key is wired in from provisioning, this stays
	// a disabled placeholder (sigverify.Disabled()); settable within-package
	// so tests can inject an enabled verifier without changing the exported
	// constructor signature.
	imageVerifier *sigverify.Verifier

	// appMu serialises create/stop/delete operations per appID so that
	// ContainerIDsForApp and the subsequent monitor marks + containerd call are
	// atomic with respect to concurrent RPCs for the same app (TOCTOU prevention,
	// SOC2-CC6, NIST-AC-4).
	appMu appMutex
}

// appMutex provides per-app name mutual exclusion. Entries are permanent
// (never deleted) to avoid reference-counting deletion races under concurrent
// contention (SOC2-CC6, NIST-AC-4). Memory overhead is negligible: one
// *sync.Mutex per distinct appName seen during the process lifetime.
type appMutex struct {
	m sync.Map // map[string]*sync.Mutex
}

// lockApp acquires the per-app lock for appName and returns an unlock function.
func (a *appMutex) lockApp(appName string) func() {
	v, _ := a.m.LoadOrStore(appName, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

func NewContainerService(logger *zap.Logger, client ContainerdClient, opts ...ContainerServiceOption) *ContainerService {
	s := &ContainerService{
		logger:        logger,
		containerd:    client,
		imageVerifier: sigverify.Disabled(),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

type ContainerServiceOption func(*ContainerService)

func WithLogManager(lm *ContainerLogManager) ContainerServiceOption {
	return func(s *ContainerService) {
		s.logManager = lm
	}
}

// Containers started with a restart policy are registered for automatic restart monitoring.
func WithMonitor(m ContainerMonitorRegistrar) ContainerServiceOption {
	return func(s *ContainerService) {
		s.monitor = m
	}
}

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

// Chunks are streamed directly to the content store without buffering the entire blob in memory.
func (s *ContainerService) WriteLayer(stream grpc.BidiStreamingServer[agentpb.WriteLayerRequest, agentpb.WriteLayerResponse]) error {
	ctx := stream.Context()

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

	sr := &layerStreamReader{stream: stream, pending: first.GetData()}

	if err := s.containerd.WriteLayer(ctx, digest, sr, 0); err != nil {
		return status.Errorf(codes.Internal, "failed to write layer: %v", err)
	}

	// Drain any messages not consumed by WriteLayer (e.g. blob already existed).
	sr.drain()

	s.logger.Info("Layer written", zap.String("digest", digest))
	return stream.Send(&agentpb.WriteLayerResponse{})
}

type layerStreamReader struct {
	stream  grpc.BidiStreamingServer[agentpb.WriteLayerRequest, agentpb.WriteLayerResponse]
	pending []byte
	done    bool
}

func (r *layerStreamReader) Read(p []byte) (int, error) {
	for len(r.pending) == 0 {
		if r.done {
			return 0, io.EOF
		}
		msg, err := r.stream.Recv()
		if err == io.EOF {
			r.done = true
			return 0, io.EOF
		}
		if err != nil {
			return 0, err
		}
		r.pending = msg.GetData()
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

// drain consumes any remaining messages from the stream without processing them.
// This is needed when WriteLayer returns early (e.g. blob already exists) so
// the gRPC stream is not left in a half-read state.
func (r *layerStreamReader) drain() {
	if r.done {
		return
	}
	for {
		if _, err := r.stream.Recv(); err != nil {
			r.done = true
			return
		}
	}
}

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

// to32 converts a wire hash (raw 32 bytes) to a fixed array, rejecting bad sizes.
func to32(b []byte) ([32]byte, error) {
	var a [32]byte
	if len(b) != 32 {
		return a, status.Errorf(codes.InvalidArgument, "chunk hash must be 32 bytes, got %d", len(b))
	}
	copy(a[:], b)
	return a, nil
}

func (s *ContainerService) QueryChunks(ctx context.Context, req *agentpb.QueryChunksRequest) (*agentpb.QueryChunksResponse, error) {
	hashes := make([][32]byte, 0, len(req.GetChunkHashes()))
	for _, b := range req.GetChunkHashes() {
		h, err := to32(b)
		if err != nil {
			return nil, err
		}
		hashes = append(hashes, h)
	}
	missing, err := s.containerd.MissingChunks(ctx, hashes)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "querying chunks: %v", err)
	}
	out := make([][]byte, 0, len(missing))
	for _, h := range missing {
		hb := h
		out = append(out, hb[:])
	}
	return &agentpb.QueryChunksResponse{MissingHashes: out}, nil
}

func (s *ContainerService) QueryLayers(ctx context.Context, req *agentpb.QueryLayersRequest) (*agentpb.QueryLayersResponse, error) {
	present, err := s.containerd.PresentLayers(ctx, req.GetDiffIds())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "querying layers: %v", err)
	}
	out := make([]*agentpb.PresentLayer, 0, len(present))
	for diffID, size := range present {
		out = append(out, &agentpb.PresentLayer{DiffId: diffID, Size: size})
	}
	return &agentpb.QueryLayersResponse{Present: out}, nil
}

func (s *ContainerService) WriteChunks(stream grpc.ClientStreamingServer[agentpb.WriteChunksRequest, agentpb.WriteChunksResponse]) error {
	ctx := stream.Context()
	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			return stream.SendAndClose(&agentpb.WriteChunksResponse{})
		}
		if err != nil {
			return err
		}
		h, err := to32(msg.GetHash())
		if err != nil {
			return err
		}
		if err := s.containerd.StageChunk(ctx, h, msg.GetData()); err != nil {
			// Preserve an explicit gRPC code from the store (e.g. ResourceExhausted
			// when a staging limit is hit); otherwise treat it as a bad chunk.
			if _, ok := status.FromError(err); ok {
				return err
			}
			return status.Errorf(codes.InvalidArgument, "staging chunk: %v", err)
		}
	}
}

// PrepareImage starts the device-side half of a chunk-diff deploy before all
// missing chunks have arrived. The containerd implementation waits for each
// layer's chunks, then assembles and unpacks layers from base to top while the
// CLI continues uploading later layers. RunContainer repeats the work through
// idempotent fast paths, so a lost response cannot leave deploy state ambiguous.
func (s *ContainerService) PrepareImage(ctx context.Context, req *agentpb.RunContainerLayersRequest) (*agentpb.PrepareImageResponse, error) {
	if err := s.verifyImageSignature(req.GetImageConfig(), req.GetImageSignature()); err != nil {
		return nil, err
	}
	if err := s.validateSignedLayerBinding(req.GetImageConfig(), req.GetLayers()); err != nil {
		return nil, err
	}

	preparer, ok := s.containerd.(ImagePreparer)
	if !ok {
		return nil, status.Error(codes.Unimplemented, "image preparation is not supported by this runtime")
	}
	if err := preparer.PrepareImage(ctx, req.GetImageName(), req.GetLayers(), req.GetImageConfig()); err != nil {
		return nil, status.Errorf(codes.Internal, "preparing image: %v", err)
	}
	return &agentpb.PrepareImageResponse{}, nil
}

func (s *ContainerService) verifyImageSignature(imageConfig, signature []byte) error {
	imageDigest := sha256.Sum256(imageConfig)
	if err := s.imageVerifier.Verify(imageDigest[:], signature); err != nil {
		switch {
		case errors.Is(err, sigverify.ErrUnsigned):
			return status.Error(codes.FailedPrecondition, "container image is unsigned; refusing to run")
		case errors.Is(err, sigverify.ErrBadSignature):
			return status.Error(codes.DataLoss, "container image signature verification failed; refusing to run")
		default:
			return status.Errorf(codes.Internal, "container image signature verification error: %v", err)
		}
	}
	return nil
}

// validateSignedLayerBinding ensures an enabled image signature authenticates
// the exact ordered diff IDs that preparation will persist. AssembleImage
// intentionally re-derives RootFS from the wire headers, so checking only the
// config signature without this comparison would let an authenticated config
// be paired with attacker-chosen layer content.
func (s *ContainerService) validateSignedLayerBinding(imageConfig []byte, layers []*agentpb.RunContainerLayerHeader) error {
	if !s.imageVerifier.Enabled() {
		return nil
	}
	var cfg struct {
		RootFS struct {
			DiffIDs []string `json:"diff_ids"`
		} `json:"rootfs"`
	}
	if err := json.Unmarshal(imageConfig, &cfg); err != nil {
		return status.Errorf(codes.InvalidArgument, "invalid signed image config: %v", err)
	}
	if len(cfg.RootFS.DiffIDs) != len(layers) {
		return status.Errorf(codes.InvalidArgument, "signed image config has %d diff IDs but request has %d layers", len(cfg.RootFS.DiffIDs), len(layers))
	}
	for i, layer := range layers {
		wireDiffID := layer.GetDiffId()
		if wireDiffID == "" {
			wireDiffID = layer.GetDigest()
		}
		if cfg.RootFS.DiffIDs[i] != wireDiffID {
			return status.Errorf(codes.InvalidArgument, "signed image config diff ID %d does not match requested layer", i)
		}
	}
	return nil
}

func (s *ContainerService) RunContainer(req *agentpb.RunContainerLayersRequest, stream grpc.ServerStreamingServer[agentpb.RunContainerLayersResponse]) error {
	ctx := stream.Context()

	appCfg, err := parseAppConfig(req.GetAppConfig())
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "invalid app config: %v", err)
	}

	// Verify the container image signature before assembling or creating the
	// container. Contract for the cross-repo image signer (must match
	// exactly): the signature is a detached ML-DSA65 signature over
	// sha256(image_config) — the SHA256 digest of the raw OCI image config
	// bytes (req.GetImageConfig()), not the assembled manifest, a layer
	// digest, or any other derived value. When the verifier is disabled (no
	// pinned key embedded yet), Verify is a fail-safe no-op and RunContainer
	// proceeds exactly as before this check existed.
	// Phase timings for the chunk-diff deploy's device-side half. The CLI sees
	// this whole RPC as a single number (its "runcontainer: device
	// create+start" mark), which is the largest remaining block of a warm
	// deploy; these fields say which part of it actually costs.
	runStartedAt := time.Now()
	phaseStart := runStartedAt
	var verifyDur, layerAssembleDur, imageAssembleDur, createDur time.Duration

	if err := s.verifyImageSignature(req.GetImageConfig(), req.GetImageSignature()); err != nil {
		return err
	}
	if err := s.validateSignedLayerBinding(req.GetImageConfig(), req.GetLayers()); err != nil {
		return err
	}

	verifyDur = time.Since(phaseStart)
	phaseStart = time.Now()

	if layers := req.GetLayers(); len(layers) > 0 {
		for _, l := range layers {
			if hs := l.GetChunkHashes(); len(hs) > 0 {
				order := make([][32]byte, 0, len(hs))
				for _, b := range hs {
					h, err := to32(b)
					if err != nil {
						return err
					}
					order = append(order, h)
				}
				diffID := l.GetDiffId()
				if diffID == "" {
					diffID = l.GetDigest()
				}
				if err := s.containerd.AssembleLayerFromChunks(ctx, diffID, order); err != nil {
					return status.Errorf(codes.Internal, "reassembling layer %s: %v", diffID, err)
				}
			}
		}
		layerAssembleDur = time.Since(phaseStart)
		phaseStart = time.Now()
		if err := s.containerd.AssembleImage(ctx, req.GetImageName(), layers, req.GetImageConfig()); err != nil {
			return status.Errorf(codes.Internal, "failed to assemble image: %v", err)
		}
		imageAssembleDur = time.Since(phaseStart)
	}
	phaseStart = time.Now()

	// Same CreateContainer call the registry path makes, so both transports
	// apply caller env identically and through the same validateUserEnv checks.
	createReq := &agentpb.CreateContainerRequest{
		ImageName:     req.GetImageName(),
		AppName:       req.GetAppName(),
		Cmd:           req.GetCmd(),
		AppConfig:     req.GetAppConfig(),
		WorkingDir:    req.GetWorkingDir(),
		RestartPolicy: req.GetRestartPolicy(),
		UserArgs:      req.GetUserArgs(),
		Env:           req.GetEnv(),
	}

	if err := s.containerd.CreateContainer(ctx, createReq, appCfg); err != nil {
		return status.Errorf(codes.Internal, "failed to create container: %v", err)
	}
	createDur = time.Since(phaseStart)

	s.logger.Info("RunContainer phase timings",
		zap.String(logfields.AppID, req.GetAppName()),
		zap.Int("layers", len(req.GetLayers())),
		zap.Duration("verify_signature", verifyDur),
		zap.Duration("assemble_layers", layerAssembleDur),
		zap.Duration("assemble_image", imageAssembleDur),
		zap.Duration("create_container", createDur),
		zap.Duration("total_before_start", time.Since(runStartedAt)))

	return s.streamContainerOutput(ctx, req.GetAppName(), postStartAgentHookFromContext(ctx), nil, stream)
}

func (s *ContainerService) StartContainer(req *agentpb.StartContainerRequest, stream grpc.ServerStreamingServer[agentpb.RunContainerLayersResponse]) error {
	appName := req.GetAppName()
	ctx := stream.Context()

	// Multi-service groups: start each service container without streaming.
	// Streaming output from multiple containers concurrently is not supported;
	// group start behaves like --detach for every service.
	ids, err := s.containerd.ContainerIDsForApp(ctx, appName)
	if err == nil && len(ids) > 1 {
		return s.startGroup(ctx, appName, ids, req.GetRestartPolicy(), stream)
	}

	return s.streamContainerOutput(ctx, appName, postStartAgentHookFromContext(ctx), req.GetRestartPolicy(), stream)
}

// startGroup starts each service container in a multi-service app in detach
// mode and sends a single Started response. Output from individual services
// is discarded; use wendy device logs to tail per-service logs.
func (s *ContainerService) startGroup(
	ctx context.Context,
	appName string,
	containerIDs []string,
	restartPolicy *agentpb.RestartPolicy,
	stream grpc.ServerStreamingServer[agentpb.RunContainerLayersResponse],
) error {
	unlock := s.appMu.lockApp(appName)
	defer unlock()

	// See streamContainerOutput: a group start must survive the requesting
	// client disconnecting, so detach it from the request's cancellation.
	startCtx := context.WithoutCancel(ctx)

	for _, id := range containerIDs {
		outputCh, startErr := s.containerd.StartContainer(startCtx, id, "", restartPolicy)
		if startErr != nil {
			return status.Errorf(codes.Internal, "failed to start service %q: %v", id, startErr)
		}
		// Drain the output channel in the background so the containerd goroutine
		// does not block. The container runs independently after this.
		go func(ch <-chan ContainerOutput) {
			for range ch {
			}
		}(outputCh)

		if s.monitor != nil {
			s.monitor.ClearExplicitStop(id)
		}
		if err := s.containerd.SetStoppedByUser(startCtx, id, false); err != nil {
			s.logger.Warn("failed to clear stopped-by-user mark",
				zap.String("container_id", id), zap.Error(err))
		}
		s.registerContainerWithMonitor(startCtx, id, restartPolicy)
	}

	return stream.Send(&agentpb.RunContainerLayersResponse{
		ResponseType: &agentpb.RunContainerLayersResponse_Started_{
			Started: &agentpb.RunContainerLayersResponse_Started{},
		},
	})
}

func postStartAgentHookFromContext(ctx context.Context) string {
	values := metadata.ValueFromIncomingContext(ctx, appconfig.PostStartAgentHookMetadataKey)
	if len(values) == 0 {
		return ""
	}
	return values[len(values)-1]
}

// monitorPolicyIntFromLabel converts a raw restart policy label string (e.g.
// "unless-stopped", "on-failure:5", "no") to the integer constants used by
// ContainerMonitorRegistrar. Returns ok=false for an empty or "no" label.
func monitorPolicyIntFromLabel(label string) (policy int, maxRetries int, ok bool) {
	if label == "" || label == "no" {
		return RestartPolicyNo, 0, false
	}
	policyStr, retries, parseOK := parseRestartPolicyLabel(label)
	if !parseOK {
		return 0, 0, false
	}
	switch policyStr {
	case "always":
		return RestartPolicyAlways, 0, true
	case "unless-stopped":
		return RestartPolicyUnlessStopped, 0, true
	case "on-failure":
		return RestartPolicyOnFailure, retries, true
	default:
		return 0, 0, false
	}
}

// parseRestartPolicyLabel splits a label like "on-failure:5" into ("on-failure", 5, true).
// Returns ok=false when the retry count portion cannot be parsed as a non-negative integer.
func parseRestartPolicyLabel(label string) (policyStr string, retries int, ok bool) {
	parts := strings.SplitN(label, ":", 2)
	if len(parts) == 1 {
		return label, 0, true
	}
	n, err := strconv.Atoi(parts[1])
	if err != nil || n < 0 {
		return "", 0, false
	}
	return parts[0], n, true
}

// Returns ok=false when the policy should not be registered (nil or explicit NO).
func monitorPolicyInt(rp *agentpb.RestartPolicy) (policy int, maxRetries int, ok bool) {
	if rp == nil {
		return RestartPolicyNo, 0, false
	}
	switch rp.GetMode() {
	case agentpb.RestartPolicyMode_NO:
		return RestartPolicyNo, 0, false
	case agentpb.RestartPolicyMode_DEFAULT, agentpb.RestartPolicyMode_UNLESS_STOPPED:
		return RestartPolicyUnlessStopped, 0, true
	case agentpb.RestartPolicyMode_ON_FAILURE:
		retries := int(rp.GetOnFailureMaxRetries())
		if retries < 0 {
			retries = 0
		}
		return RestartPolicyOnFailure, retries, true
	default:
		return 0, 0, false // unknown mode — treat as no policy
	}
}

// When restartPolicy is nil the persisted containerd label is used.
func (s *ContainerService) registerContainerWithMonitor(ctx context.Context, appName string, restartPolicy *agentpb.RestartPolicy) {
	if s.monitor == nil {
		return
	}
	if policy, maxRetries, ok := monitorPolicyInt(restartPolicy); ok {
		s.monitor.Register(appName, policy, maxRetries)
	} else if restartPolicy != nil && restartPolicy.GetMode() == agentpb.RestartPolicyMode_NO {
		// Explicit NO — remove any existing registration.
		s.monitor.Unregister(appName)
	} else {
		// nil restartPolicy or unrecognized/forward-compatible mode: fall back
		// to the persisted label so we don't accidentally clear an existing
		// registration when a newer client sends a mode this agent doesn't know.
		if label, labelErr := s.containerd.GetContainerRestartPolicyLabel(ctx, appName); labelErr == nil {
			if label == "" || label == "no" {
				// No restart policy persisted — clear any stale registration.
				s.monitor.Unregister(appName)
			} else if policy, maxRetries, ok := monitorPolicyIntFromLabel(label); ok {
				s.monitor.Register(appName, policy, maxRetries)
			} else {
				// Unknown label value — treat as no restart policy.
				s.monitor.Unregister(appName)
			}
		} else {
			s.logger.Warn("failed to read restart policy label; monitor registration skipped",
				zap.String("app_name", appName),
				zap.Error(labelErr),
			)
		}
	}
}

// guaranteeOutputDrain wires outputCh — a running task's only output drain —
// to a consumer that lives for the full task lifetime, independent of the RPC
// handler that started the task. If outputCh is ever abandoned, back-pressure
// propagates through the agent's io.Pipes into the container's stdout FIFO and
// the app freezes in pipe_write once the buffers fill (WDY-1822). Callers must
// invoke this immediately after a successful start, before anything that can
// return early (monitor bookkeeping, sends to a client that may already be
// gone). It returns the channel the handler should stream from and a cleanup
// function to defer.
func (s *ContainerService) guaranteeOutputDrain(appName string, outputCh <-chan ContainerOutput) (<-chan ContainerOutput, func()) {
	if s.logManager != nil {
		subID, subCh := s.logManager.Subscribe(appName)
		// Pump containerd output into the log manager (which never blocks:
		// slow subscribers drop) until the task exits and closes outputCh.
		go func() {
			for output := range outputCh {
				s.logManager.Publish(appName, output)
			}
			// When the containerd channel closes, publish a Done marker.
			s.logManager.Publish(appName, ContainerOutput{Done: true})
		}()
		return subCh, func() { s.logManager.Unsubscribe(appName, subID) }
	}
	// Without a log manager the handler is the only consumer: hand whatever it
	// has not consumed to a background drainer when it returns, so a client
	// disconnect cannot leave the task's output pipeline undrained.
	return outputCh, func() {
		go func() {
			for range outputCh {
			}
		}()
	}
}

// When a ContainerLogManager is configured, reads from the log manager subscription
// instead of directly from containerd, enabling multi-subscriber fan-out and telemetry bridging.
func (s *ContainerService) streamContainerOutput(
	ctx context.Context,
	appName string,
	postStartAgentCommand string,
	restartPolicy *agentpb.RestartPolicy,
	stream grpc.ServerStreamingServer[agentpb.RunContainerLayersResponse],
) error {
	// Starting a container must not be abortable by the client that requested
	// it. A detached `apps start` returns the instant the stream is opened and
	// closes the connection, which cancels this RPC's context; if the start
	// were bound to it, the agent aborts mid-load ("loading container: context
	// canceled") and the container never runs. Detach the start (and the
	// post-start bookkeeping that records it succeeded) from the request's
	// cancellation while keeping its values; the output stream below still
	// honors the request ctx and ends on disconnect.
	startCtx := context.WithoutCancel(ctx)
	outputCh, err := s.containerd.StartContainer(startCtx, appName, postStartAgentCommand, restartPolicy)
	if err != nil {
		return status.Errorf(codes.Internal, "failed to start container: %v", err)
	}

	// Attach the task-lifetime drainer before any early return below can
	// abandon outputCh and wedge the app (WDY-1822).
	readCh, releaseDrain := s.guaranteeOutputDrain(appName, outputCh)
	defer releaseDrain()

	// The container started successfully. If it was previously explicitly
	// stopped, clear that mark so automatic restarts are re-enabled.
	// We clear it here (right after start succeeds) rather than after
	// streaming completes so that stream errors (e.g. client disconnect)
	// do not leave ExplicitStop set and suppress future automatic restarts.
	if s.monitor != nil {
		s.monitor.ClearExplicitStop(appName)
	}
	// Clear the persisted stop mark too, so a user-initiated start re-enables
	// boot reconcile for this app. Best-effort. (Only user starts reach this
	// path; the boot reconcile starts via the monitor and never clears it.)
	if err := s.containerd.SetStoppedByUser(startCtx, appName, false); err != nil {
		s.logger.Warn("failed to clear stopped-by-user mark",
			zap.String("app_name", appName), zap.Error(err))
	}

	s.registerContainerWithMonitor(startCtx, appName, restartPolicy)

	if err := stream.Send(&agentpb.RunContainerLayersResponse{
		ResponseType: &agentpb.RunContainerLayersResponse_Started_{
			Started: &agentpb.RunContainerLayersResponse_Started{},
		},
	}); err != nil {
		return err
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

// The first client message must set app_name; subsequent messages carry stdin data.
func (s *ContainerService) AttachContainer(stream grpc.BidiStreamingServer[agentpb.AttachContainerRequest, agentpb.RunContainerLayersResponse]) error {
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
	postStartAgentCommand := postStartAgentHookFromContext(ctx)
	// See streamContainerOutput: detach the start (and its post-start
	// bookkeeping) from the request's cancellation so a client disconnect
	// during startup cannot abort it. The stdin pump and output stream below
	// still honor the request ctx.
	startCtx := context.WithoutCancel(ctx)

	stdinR, stdinW := io.Pipe()
	defer stdinR.Close()

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

	outputCh, err := s.containerd.StartContainerWithStdin(startCtx, appName, stdinR, postStartAgentCommand, nil)
	if err != nil {
		stdinR.Close()
		return status.Errorf(codes.Internal, "failed to start container: %v", err)
	}

	// Attach the task-lifetime drainer before any early return below can
	// abandon outputCh and wedge the app (WDY-1822).
	readCh, releaseDrain := s.guaranteeOutputDrain(appName, outputCh)
	defer releaseDrain()

	// Mirror the same monitor bookkeeping as streamContainerOutput: clear any
	// prior explicit-stop mark and register with the persisted restart policy.
	if s.monitor != nil {
		s.monitor.ClearExplicitStop(appName)
	}
	if err := s.containerd.SetStoppedByUser(startCtx, appName, false); err != nil {
		s.logger.Warn("failed to clear stopped-by-user mark",
			zap.String("app_name", appName), zap.Error(err))
	}
	s.registerContainerWithMonitor(startCtx, appName, nil)

	if err := stream.Send(&agentpb.RunContainerLayersResponse{
		ResponseType: &agentpb.RunContainerLayersResponse_Started_{
			Started: &agentpb.RunContainerLayersResponse_Started{},
		},
	}); err != nil {
		return err
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

// ExecContainer runs a process inside a running container with an interactive
// PTY (docker `exec -it`). The first client message must be ExecStart; later
// messages carry stdin or window resizes. Output is streamed back as
// stdout/stderr frames, followed by a final exit_code.
func (s *ContainerService) ExecContainer(stream grpc.BidiStreamingServer[agentpb.ExecContainerRequest, agentpb.ExecContainerResponse]) error {
	execer, ok := s.containerd.(ContainerExecer)
	if !ok {
		return status.Error(codes.Unimplemented, "container exec is not supported by this agent")
	}

	first, err := stream.Recv()
	if err == io.EOF {
		return status.Error(codes.InvalidArgument, "missing first exec message")
	}
	if err != nil {
		return err
	}
	start := first.GetStart()
	if start == nil || start.GetAppName() == "" {
		return status.Error(codes.InvalidArgument, "first message must be ExecStart with app_name")
	}
	command := start.GetCommand()
	// Require an explicit command rather than defaulting to a shell: exec over
	// the admin socket runs arbitrary commands in any container, so a caller
	// should have to name what it runs (the wendy CLI always sends one — `claude`
	// or the command after `--`).
	if len(command) == 0 {
		return status.Error(codes.InvalidArgument, "ExecStart requires a non-empty command")
	}

	// Audit: this RPC is effectively `docker exec` into any running workload via
	// the unauthenticated admin socket. Log every invocation and its outcome so
	// there is a forensic trail of what ran where.
	s.logger.Info("container exec started",
		zap.String("app_name", start.GetAppName()),
		zap.Strings("command", command),
		zap.Bool("tty", start.GetTty()))
	execStartedAt := time.Now()

	ctx := stream.Context()
	stdinR, stdinW := io.Pipe()
	defer stdinR.Close()

	resize := make(chan [2]uint32, 8)
	if ts := start.GetTermSize(); ts != nil {
		resize <- [2]uint32{ts.GetRows(), ts.GetCols()}
	}

	// Forward stdin + resize frames from the client until it closes the stream.
	go func() {
		defer stdinW.Close()
		defer close(resize)
		for {
			msg, recvErr := stream.Recv()
			if recvErr != nil {
				return
			}
			switch {
			case len(msg.GetStdinData()) > 0:
				if _, werr := stdinW.Write(msg.GetStdinData()); werr != nil {
					return
				}
			case msg.GetResize() != nil:
				select {
				case resize <- [2]uint32{msg.GetResize().GetRows(), msg.GetResize().GetCols()}:
				default: // drop resizes if the consumer is momentarily behind
				}
			}
		}
	}()

	// gRPC forbids concurrent SendMsg on one stream. In non-TTY mode containerd
	// copies stdout and stderr on separate goroutines, and the final exit_code
	// frame below is sent from this goroutine, so every send is serialized
	// through one lock.
	sender := &execSender{stream: stream}
	stdout := &execWriter{sender: sender, stderr: false}
	stderr := &execWriter{sender: sender, stderr: true}

	code, err := execer.ExecInContainer(ctx, start.GetAppName(), command, start.GetTty(), stdinR, stdout, stderr, resize)
	if err != nil {
		s.logger.Warn("container exec failed",
			zap.String("app_name", start.GetAppName()),
			zap.Duration("duration", time.Since(execStartedAt)),
			zap.Error(err))
		return status.Errorf(codes.Internal, "exec failed: %v", err)
	}
	s.logger.Info("container exec completed",
		zap.String("app_name", start.GetAppName()),
		zap.Int("exit_code", code),
		zap.Duration("duration", time.Since(execStartedAt)))
	// ExecInContainer drains stdout/stderr before returning, so the exit_code
	// frame is guaranteed to follow the last output frame on the ordered stream.
	return sender.send(&agentpb.ExecContainerResponse{
		ResponseType: &agentpb.ExecContainerResponse_ExitCode{ExitCode: int32(code)},
	})
}

// execSender serializes every Send on the exec bidi stream: gRPC does not allow
// concurrent SendMsg, and the stdout writer, stderr writer, and exit_code frame
// can all originate from different goroutines.
type execSender struct {
	stream grpc.BidiStreamingServer[agentpb.ExecContainerRequest, agentpb.ExecContainerResponse]
	mu     sync.Mutex
}

func (s *execSender) send(resp *agentpb.ExecContainerResponse) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stream.Send(resp)
}

// execWriter adapts a container exec output stream to io.Writer so the PTY's
// stdout/stderr can be forwarded over gRPC.
type execWriter struct {
	sender *execSender
	stderr bool
}

func (w *execWriter) Write(p []byte) (int, error) {
	// Copy: the PTY may reuse its read buffer once Write returns.
	buf := append([]byte(nil), p...)
	var resp *agentpb.ExecContainerResponse
	if w.stderr {
		resp = &agentpb.ExecContainerResponse{ResponseType: &agentpb.ExecContainerResponse_StderrData{StderrData: buf}}
	} else {
		resp = &agentpb.ExecContainerResponse{ResponseType: &agentpb.ExecContainerResponse_StdoutData{StdoutData: buf}}
	}
	if err := w.sender.send(resp); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (s *ContainerService) StopContainer(ctx context.Context, req *agentpb.StopContainerRequest) (*agentpb.StopContainerResponse, error) {
	appName := req.GetAppName()
	// Validate before acquiring the mutex so that invalid names never reach the
	// appMutex map (preventing unbounded map growth from adversarial RPC input,
	// SOC2-CC6, NIST-SC-5) and so the monitor fallback below always receives a
	// well-formed identifier (SOC2-CC6 INFORMATIONAL-12).
	if err := appconfig.ValidateAppID(appName); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid app name: %v", err)
	}

	// Hold the per-app lock so that ContainerIDsForApp, MarkExplicitStop, and
	// the actual stop are atomic with respect to concurrent CreateContainer or
	// DeleteContainer calls for the same app (SOC2-CC6, NIST-AC-4).
	unlock := s.appMu.lockApp(appName)
	defer unlock()

	// Shared with the stop below, so monitor marks and stopped-by-user labels
	// are only ever written against containers that were actually stopped.
	ids, err := s.containerd.ResolveAppContainerIDs(ctx, appName)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil, status.Errorf(codes.NotFound, "no app or service named %q", appName)
		}
		return nil, status.Errorf(codes.Internal, "resolving containers for app %q: %v", appName, err)
	}

	// Mark BEFORE stop so the monitor cannot observe the exit and restart in
	// the window between StopContainer returning and MarkExplicitStop being
	// called. Revert all marks if the stop ultimately fails.
	if s.monitor != nil {
		for _, id := range ids {
			s.monitor.MarkExplicitStop(id)
		}
	}
	// Persist intent before beginning teardown. Besides surviving reboot, this
	// lets a group restart that was queued just before MarkExplicitStop observe
	// the stop even if its goroutine does not run until after the monitor's
	// in-memory suppression handles have been released.
	var persisted []string
	for _, id := range ids {
		if err := s.containerd.SetStoppedByUser(ctx, id, true); err != nil {
			s.logger.Warn("failed to persist stopped-by-user mark",
				zap.String("container_id", id), zap.Error(err))
			continue
		}
		persisted = append(persisted, id)
	}
	if err := s.containerd.StopContainer(ctx, appName); err != nil {
		if s.monitor != nil {
			for _, id := range ids {
				s.monitor.ClearExplicitStop(id)
			}
		}
		cleanupCtx := context.WithoutCancel(ctx)
		for _, id := range persisted {
			if clearErr := s.containerd.SetStoppedByUser(cleanupCtx, id, false); clearErr != nil {
				s.logger.Warn("failed to roll back stopped-by-user mark",
					zap.String("container_id", id), zap.Error(clearErr))
			}
		}
		return nil, status.Errorf(codes.Internal, "failed to stop container: %v", err)
	}
	s.logger.Info("App stopped", zap.String("app_name", appName), zap.Int("service_count", len(ids)))
	return &agentpb.StopContainerResponse{}, nil
}

func (s *ContainerService) DeleteContainer(ctx context.Context, req *agentpb.DeleteContainerRequest) (*agentpb.DeleteContainerResponse, error) {
	appName := req.GetAppName()
	if err := appconfig.ValidateAppID(appName); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid app name: %v", err)
	}

	// Hold the per-app lock so that ContainerIDsForApp, monitor unregister, and
	// the actual delete are atomic with respect to concurrent CreateContainer or
	// StopContainer calls for the same app (SOC2-CC6, NIST-AC-4).
	unlock := s.appMu.lockApp(appName)
	defer unlock()

	// Resolve all container IDs before deletion so the monitor can unregister
	// each one. Unregistering only the bare appName would leave
	// {appID}_{serviceName} monitor entries alive and potentially trigger
	// spurious restart attempts while the container is being removed.
	ids, err := s.containerd.ResolveAppContainerIDs(ctx, appName)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil, status.Errorf(codes.NotFound, "no app or service named %q", appName)
		}
		return nil, status.Errorf(codes.Internal, "resolving containers for app %q: %v", appName, err)
	}

	// Unregister from the monitor BEFORE deletion to close the window where the
	// monitor could attempt a restart while containers are being removed.
	if s.monitor != nil {
		for _, id := range ids {
			// Unregister alone closes the restart window: the monitor only
			// restarts containers it still has state for.
			s.monitor.Unregister(id)
		}
	}

	// Resolve which volumes the app owns BEFORE deleting its containers: the
	// persist entitlement labels that record ownership die with the container.
	var volumesToDelete []string
	if req.GetDeleteVolumes() {
		volumesToDelete = s.resolveDeletableVolumes(ctx, appName)
	}

	if err := s.containerd.DeleteContainer(ctx, appName, req.GetDeleteImage()); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to delete container: %v", err)
	}

	if req.GetDeleteVolumes() {
		s.deleteVolumes(volumesToDelete)
	}

	s.logger.Info("App deleted",
		zap.String("app_name", appName),
		zap.Bool("delete_image", req.GetDeleteImage()),
		zap.Bool("delete_volumes", req.GetDeleteVolumes()),
	)
	return &agentpb.DeleteContainerResponse{}, nil
}

// volumesDir is the base directory for persistent volumes. It's a variable
// (not const) so tests can override it with a temp directory.
var volumesDir = "/var/lib/wendy/volumes"

// resolveDeletableVolumes returns the persistent volumes to remove when
// appName is deleted with --delete-volumes: exactly the volume names the app's
// containers declare via persist entitlements, minus any name that another
// deployed app also declares (volumes are shared across apps by name — see
// oci.applyPersist). Ambiguity fails safe: when ownership cannot be resolved,
// or for apps deployed before entitlement labels existed, nothing is deleted —
// a leaked volume can still be removed with `wendy device volumes rm`, whereas
// a wrongly deleted one is unrecoverable.
func (s *ContainerService) resolveDeletableVolumes(ctx context.Context, appName string) []string {
	declared, err := s.containerd.AppDeclaredVolumes(ctx)
	if err != nil {
		s.logger.Warn("Skipping volume deletion: cannot resolve volume ownership",
			zap.String("app_name", appName), zap.Error(err))
		return nil
	}
	owned := declared[appName]
	if len(owned) == 0 {
		s.logger.Info("App declares no persistent volumes; none deleted",
			zap.String("app_name", appName))
		return nil
	}

	otherOwners := make(map[string][]string) // volume name → other apps declaring it
	for app, vols := range declared {
		if app == appName {
			continue
		}
		for _, v := range vols {
			otherOwners[v] = append(otherOwners[v], app)
		}
	}

	var deletable []string
	for _, v := range owned {
		if others := otherOwners[v]; len(others) > 0 {
			s.logger.Warn("Keeping shared volume: also declared by other apps",
				zap.String("volume", v),
				zap.String("app_name", appName),
				zap.Strings("also_declared_by", others))
			continue
		}
		deletable = append(deletable, v)
	}
	return deletable
}

// deleteVolumes removes the given volume directories under volumesDir. The
// names come from persist entitlement labels; re-apply the same base-name
// sanitization applyPersist used when creating them so no label value can
// escape the volumes directory (SOC2-CC6, ISO27001-A.8, NIST-SI-10).
func (s *ContainerService) deleteVolumes(names []string) {
	for _, name := range names {
		if name != filepath.Base(name) || name == "." || name == ".." || name == "/" || name == "" {
			s.logger.Warn("Skipping volume with unsafe name", zap.String("volume", name))
			continue
		}
		path := filepath.Join(volumesDir, name)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			continue // declared but never materialized on disk
		}
		if err := os.RemoveAll(path); err != nil {
			s.logger.Warn("Failed to remove volume", zap.String("path", path), zap.Error(err))
		} else {
			s.logger.Info("Volume removed", zap.String("path", path))
		}
	}
}

func (s *ContainerService) ListVolumes(ctx context.Context, _ *agentpb.ListVolumesRequest) (*agentpb.ListVolumesResponse, error) {
	entries, err := os.ReadDir(volumesDir)
	if err != nil {
		if os.IsNotExist(err) {
			return &agentpb.ListVolumesResponse{}, nil
		}
		return nil, status.Errorf(codes.Internal, "reading volumes dir: %v", err)
	}

	usedBy := s.buildVolumeUsageMap(ctx)

	var volumes []*agentpb.VolumeInfo
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		path := filepath.Join(volumesDir, name)
		info, err := e.Info()
		if err != nil {
			continue
		}

		volumes = append(volumes, &agentpb.VolumeInfo{
			Name:      name,
			Path:      path,
			SizeBytes: dirSize(path),
			CreatedAt: info.ModTime().UTC().Format("2006-01-02T15:04:05Z"),
			UsedBy:    usedBy[name],
		})
	}

	return &agentpb.ListVolumesResponse{Volumes: volumes}, nil
}

func (s *ContainerService) RemoveVolume(_ context.Context, req *agentpb.RemoveVolumeRequest) (*agentpb.RemoveVolumeResponse, error) {
	name := filepath.Base(req.GetName())
	if name == "" || name == "." || name == ".." || name == "/" {
		return nil, status.Errorf(codes.InvalidArgument, "invalid volume name")
	}

	path := filepath.Join(volumesDir, name)
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil, status.Errorf(codes.NotFound, "volume %q not found", name)
		}
		return nil, status.Errorf(codes.Internal, "checking volume %q: %v", name, err)
	}

	if err := os.RemoveAll(path); err != nil {
		return nil, status.Errorf(codes.Internal, "removing volume: %v", err)
	}

	s.logger.Info("Volume removed", zap.String("name", name), zap.String("path", path))
	return &agentpb.RemoveVolumeResponse{}, nil
}

// buildVolumeUsageMap maps each volume name to the apps that declare it via
// persist entitlement labels — real ownership, not a name-prefix guess.
// Volumes are shared across apps by name, so several apps may appear for one
// volume. Volumes no deployed app declares (including those of apps deployed
// before entitlement labels existed) get an empty usedBy.
func (s *ContainerService) buildVolumeUsageMap(ctx context.Context) map[string][]string {
	usage := make(map[string][]string)
	declared, err := s.containerd.AppDeclaredVolumes(ctx)
	if err != nil {
		s.logger.Warn("Failed to resolve volume ownership", zap.Error(err))
		return usage
	}
	for app, vols := range declared {
		for _, v := range vols {
			usage[v] = append(usage[v], app)
		}
	}
	for _, apps := range usage {
		sort.Strings(apps)
	}
	return usage
}

func dirSize(path string) int64 {
	var size int64
	_ = filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		size += info.Size()
		return nil
	})
	return size
}

func (s *ContainerService) ListContainerStats(ctx context.Context, _ *agentpb.ListContainerStatsRequest) (*agentpb.ListContainerStatsResponse, error) {
	stats, err := s.containerd.GetContainerStats(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "getting container stats: %v", err)
	}
	return &agentpb.ListContainerStatsResponse{Stats: stats}, nil
}

// GetResourceStats returns host CPU/memory/GPU counters plus per-container CPU
// and memory for `wendy device top`. Host metrics are best-effort: a failed
// /proc read or absent GPU tool yields zero/empty fields rather than an error,
// so the command degrades gracefully on constrained hosts.
//
// Like every other method on this service, access is gated by the agent's gRPC
// transport (the device's trusted control channel); there is no per-RPC
// authorization layer, so the read-only host topology this returns is no more
// exposed than the existing container-stats RPCs. The call is logged for audit.
func (s *ContainerService) GetResourceStats(ctx context.Context, _ *agentpb.GetResourceStatsRequest) (*agentpb.GetResourceStatsResponse, error) {
	s.logger.Info("GetResourceStats")
	containers, err := s.containerd.GetResourceStats(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "getting resource stats: %v", err)
	}

	host := &agentpb.HostStats{}
	if cpu, cpuErr := hoststats.ReadCPU(); cpuErr == nil {
		host.CpuTotalJiffies = cpu.TotalJiffies
		host.CpuIdleJiffies = cpu.IdleJiffies
		host.CpuCount = cpu.CPUCount
	}
	if mem, memErr := hoststats.ReadMemory(); memErr == nil {
		host.MemTotalBytes = mem.TotalBytes
		host.MemAvailableBytes = mem.AvailableBytes
	}
	host.Gpus = gpuStatsToProto(hoststats.SampleGPU(ctx))
	host.ThermalZones = thermalZonesToProto(hoststats.ResolveThermal())
	host.Battery = batteryToProto(hoststats.ResolveBattery())

	return &agentpb.GetResourceStatsResponse{
		Host:       host,
		Containers: containers,
	}, nil
}

// thermalZonesToProto converts sampled thermal zones to their proto form.
func thermalZonesToProto(zones []hoststats.ThermalZone) []*agentpb.ThermalZone {
	if len(zones) == 0 {
		return nil
	}
	out := make([]*agentpb.ThermalZone, len(zones))
	for i, z := range zones {
		out[i] = &agentpb.ThermalZone{Name: z.Name, TempC: z.TempC}
	}
	return out
}

// batteryToProto converts a sampled battery to its proto form, passing nil
// through: a device without a battery reports no BatteryStats at all, which is
// what lets the CLI show nothing rather than a misleading 0%. Shared by
// GetResourceStats and GetAgentVersion.
func batteryToProto(b *hoststats.Battery) *agentpb.BatteryStats {
	if b == nil {
		return nil
	}
	out := &agentpb.BatteryStats{
		Percent: b.Percent,
		State:   batteryStateToProto(b.State),
	}
	// Zero means "no usable rate" — leave the optional field absent rather than
	// claiming zero seconds remaining.
	if b.SecondsRemaining > 0 {
		secs := b.SecondsRemaining
		out.SecondsRemaining = &secs
	}
	return out
}

func batteryStateToProto(s hoststats.BatteryState) agentpb.BatteryState {
	switch s {
	case hoststats.BatteryCharging:
		return agentpb.BatteryState_BATTERY_STATE_CHARGING
	case hoststats.BatteryDischarging:
		return agentpb.BatteryState_BATTERY_STATE_DISCHARGING
	case hoststats.BatteryFull:
		return agentpb.BatteryState_BATTERY_STATE_FULL
	case hoststats.BatteryNotCharging:
		return agentpb.BatteryState_BATTERY_STATE_NOT_CHARGING
	default:
		return agentpb.BatteryState_BATTERY_STATE_UNKNOWN
	}
}

// GetContainerPorts returns the listening TCP and bound UDP sockets for the
// given app, read from each of its containers' network namespaces. Loopback-bound
// ports are intentionally included so operators can see services that are exposed
// only on localhost. Access is gated by the agent's gRPC transport, the same
// trusted control channel that secures every other method here; the call is
// logged for audit.
func (s *ContainerService) GetContainerPorts(ctx context.Context, req *agentpb.GetContainerPortsRequest) (*agentpb.GetContainerPortsResponse, error) {
	if req.GetAppName() == "" {
		return nil, status.Errorf(codes.InvalidArgument, "app_name is required")
	}
	s.logger.Info("GetContainerPorts", zap.String("app_name", req.GetAppName()))
	ports, err := s.containerd.GetListeningPorts(ctx, req.GetAppName())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "getting container ports: %v", err)
	}
	return &agentpb.GetContainerPortsResponse{Ports: ports}, nil
}

func gpuStatsToProto(in []hoststats.GPUStat) []*agentpb.GpuStats {
	out := make([]*agentpb.GpuStats, 0, len(in))
	for _, g := range in {
		pg := &agentpb.GpuStats{
			Index:         g.Index,
			Name:          g.Name,
			UtilPercent:   g.UtilPercent,
			MemUsedBytes:  g.MemUsedBytes,
			MemTotalBytes: g.MemTotalBytes,
			TempC:         g.TempC,
			PowerW:        g.PowerW,
		}
		out = append(out, pg)
	}
	return out
}

func (s *ContainerService) ListContainers(_ *agentpb.ListContainersRequest, stream grpc.ServerStreamingServer[agentpb.ListContainersResponse]) error {
	containers, err := s.containerd.ListContainers(stream.Context())
	if err != nil {
		return status.Errorf(codes.Internal, "failed to list containers: %v", err)
	}
	s.applyRestartStatus(containers)

	for _, c := range containers {
		if err := stream.Send(&agentpb.ListContainersResponse{Container: c}); err != nil {
			return err
		}
	}
	return nil
}

// applyRestartStatus merges the container monitor's live restart bookkeeping
// into a containerd container listing. containerd only knows whether a task is
// running right now, so a crash-looping app — dead in the ~10 s gaps between
// automatic restarts — reads as an ordinary STOPPED with failure_count 0
// (WDY-1826). This fills in failure_count and upgrades STOPPED to
// CRASH_LOOPING for any app the monitor has already restarted and will keep
// restarting. No-op when the monitor doesn't expose its bookkeeping.
func (s *ContainerService) applyRestartStatus(containers []*agentpb.AppContainer) {
	provider, ok := s.monitor.(RestartStatusProvider)
	if !ok {
		return
	}
	statuses := provider.RestartStatuses()
	if len(statuses) == 0 {
		return
	}

	for _, c := range containers {
		var failures int
		crashLooping := false

		// Monitor state is keyed per container: bare appID for single-container
		// apps, "{appID}_{serviceName}" for named services (see
		// containerd.ContainerName). Aggregate across an app's members.
		if svcs := c.GetServices(); len(svcs) > 0 {
			for _, svc := range svcs {
				name := c.GetAppName()
				if svc.GetName() != "" {
					name = c.GetAppName() + "_" + svc.GetName()
				}
				st, tracked := statuses[name]
				if !tracked {
					continue
				}
				failures += st.FailureCount
				if st.WillRestart && st.FailureCount > 0 && svc.GetRunningState() != agentpb.AppRunningState_RUNNING {
					crashLooping = true
					svc.RunningState = agentpb.AppRunningState_CRASH_LOOPING
				}
			}
		} else if st, tracked := statuses[c.GetAppName()]; tracked {
			failures = st.FailureCount
			crashLooping = st.WillRestart && st.FailureCount > 0 &&
				c.GetRunningState() != agentpb.AppRunningState_RUNNING
		}

		c.FailureCount = uint32(failures) //nolint:gosec // failure counts are small non-negative ints
		// Only upgrade the aggregate when the app isn't running: a group with one
		// healthy and one crash-looping service stays RUNNING at the top level,
		// with the crash loop visible on the service entry and failure_count.
		if crashLooping && c.GetRunningState() == agentpb.AppRunningState_STOPPED {
			c.RunningState = agentpb.AppRunningState_CRASH_LOOPING
		}
	}
}

// StreamMCP proxies a bidirectional gRPC stream to the container's MCP TCP port.
// The caller must supply an "app-name" metadata key identifying the target container.
func (s *ContainerService) StreamMCP(stream grpc.BidiStreamingServer[agentpb.MCPChunk, agentpb.MCPChunk]) error {
	ctx := stream.Context()
	md, _ := metadata.FromIncomingContext(ctx)
	vals := md.Get("app-name")
	if len(vals) == 0 || vals[0] == "" {
		return status.Errorf(codes.InvalidArgument, "app-name metadata is required")
	}
	appName := vals[0]

	mcpPort, err := s.containerd.GetContainerMCPPort(ctx, appName)
	if err != nil {
		return status.Errorf(codes.NotFound, "container %q: %v", appName, err)
	}
	if mcpPort == 0 {
		return status.Errorf(codes.NotFound, "container %q has no mcp entitlement", appName)
	}

	// Verify the container is running before attempting to dial its MCP port.
	containers, listErr := s.containerd.ListContainers(ctx)
	if listErr != nil {
		s.logger.Warn("failed to list containers for running check in StreamMCP", zap.Error(listErr))
	} else {
		running := false
		for _, c := range containers {
			if c.GetAppName() == appName && c.GetRunningState() == agentpb.AppRunningState_RUNNING {
				running = true
				break
			}
		}
		if !running {
			return status.Errorf(codes.FailedPrecondition, "container %q is not running", appName)
		}
	}

	tcpConn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", mcpPort))
	if err != nil {
		return status.Errorf(codes.Unavailable, "connecting to MCP server for %q on port %d: %v", appName, mcpPort, err)
	}
	defer tcpConn.Close()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	errc := make(chan error, 2)

	// gRPC → TCP
	go func() {
		for {
			chunk, err := stream.Recv()
			if err != nil {
				errc <- err
				return
			}
			if _, err := tcpConn.Write(chunk.Data); err != nil {
				errc <- err
				return
			}
		}
	}()

	// TCP → gRPC
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, readErr := tcpConn.Read(buf)
			if n > 0 {
				if sendErr := stream.Send(&agentpb.MCPChunk{Data: buf[:n]}); sendErr != nil {
					errc <- sendErr
					return
				}
			}
			if readErr != nil {
				errc <- readErr
				return
			}
		}
	}()

	select {
	case <-ctx.Done():
		return nil
	case err := <-errc:
		if err == io.EOF {
			return nil
		}
		return err
	}
}

func parseAppConfig(data []byte) (*appconfig.AppConfig, error) {
	if len(data) == 0 {
		return &appconfig.AppConfig{}, nil
	}
	var cfg appconfig.AppConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	// Reject unsafe app IDs at the RPC boundary so direct callers and generated
	// compose configs can't push comma/'='/newline characters into the env vars
	// and labels derived from appId. Empty appId is left to existing behaviour.
	if cfg.AppID != "" {
		if err := appconfig.ValidateUserAppID(cfg.AppID); err != nil {
			return nil, err
		}
	}
	return &cfg, nil
}
