package services

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/shared/buildargs"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// maxContextEntryBytes bounds a single build-context file so one entry cannot
// fill the build host's disk. 2 GiB is far above any plausible source file
// while still being a real ceiling.
const maxContextEntryBytes = 2 << 30

// maxContextBytes bounds a whole reassembled build context.
//
// This is a memory bound, not a disk one: the context is reassembled into RAM
// in one piece before extraction, and a build host may be an edge device with a
// few GiB of it. QueryChunks/WriteChunks is a generic sha256 blob store with no
// notion of what it is holding, so this is the only layer that can say how big
// a *build context* is allowed to be. 2 GiB is far past any real source tree —
// large blobs belong in an image layer, not in the context.
const maxContextBytes = 2 << 30

// buildHostEnabledFile, when present in the agent config dir, opts this device
// in as a build host. Unlike meshDisabledFile this is opt-IN, and deliberately
// so: BuildImage runs build instructions supplied by a client, so a device must
// volunteer for that role rather than acquire it by being reachable with an
// org certificate.
const buildHostEnabledFile = "build-host-enabled"

// DefaultBuildkitAddress is where buildkitd listens on a WendyOS device — the
// same socket the on-device buildctl path already uses.
const DefaultBuildkitAddress = "unix:///run/buildkit/buildkitd.sock"

// validPlatformRe matches the OCI platform strings buildctl accepts:
// os/arch with an optional variant, e.g. linux/arm64 or linux/arm/v7.
var validPlatformRe = regexp.MustCompile(`^[a-z0-9]+/[a-z0-9]+(/[a-z0-9]+)?$`)

// defaultBuildStateDir holds reassembled build contexts, one directory per app.
const defaultBuildStateDir = "/var/lib/wendy/buildctx"

// dockerConfigDirSuffix names the per-build credential directory beside the
// context directory. A sibling rather than a child: anything inside the context
// directory is build input, and the credential must not end up in the image.
const dockerConfigDirSuffix = ".auth"

// ChunkSource reads previously staged chunks back in order. It is the same
// content store WendyContainerService.WriteChunks writes into.
type ChunkSource interface {
	OpenChunkStream(ctx context.Context, hashes [][32]byte) io.Reader
}

// BuildContextLockSet serialises builds that use the same stable context
// directory. One set must be shared by every BuildService registered by an
// agent process, because its mTLS, plaintext, and local-socket listeners can
// all accept builds for the same app.
type BuildContextLockSet struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// NewBuildContextLockSet creates an empty process-wide build context lock set.
func NewBuildContextLockSet() *BuildContextLockSet {
	return &BuildContextLockSet{locks: map[string]*sync.Mutex{}}
}

func (s *BuildContextLockSet) lock(dir string) func() {
	s.mu.Lock()
	mu, ok := s.locks[dir]
	if !ok {
		mu = &sync.Mutex{}
		s.locks[dir] = mu
	}
	s.mu.Unlock()

	mu.Lock()
	return mu.Unlock
}

// BuildServiceOptions configures the build service.
type BuildServiceOptions struct {
	// ConfigPath is the agent config dir searched for the opt-in marker.
	ConfigPath string
	// BuildkitAddress overrides DefaultBuildkitAddress.
	BuildkitAddress string
	// StateDir overrides defaultBuildStateDir.
	StateDir string
	// MaxContextBytes overrides maxContextBytes. Zero means the default.
	//
	// Injectable so a test can exercise the ceiling without allocating one.
	// Proving the read is bounded means reading right up to the limit, and a
	// test doing that at 2 GiB is an out-of-memory kill on a modest CI runner —
	// which is exactly how this option came to exist.
	MaxContextBytes int64
	// Chunks resolves the build context the CLI staged through WriteChunks.
	Chunks ChunkSource
	// ContextLocks is shared across every listener in this agent process. Nil
	// gives this service a private set, which is convenient for isolated uses.
	ContextLocks *BuildContextLockSet
	// Peers dials another device in this org to deliver the finished image.
	Peers PeerDialer
	// PushTLS supplies the TLS config for the registry hop, PINNED to the asset
	// it is meant to reach.
	//
	// Taking the target asset id is the point of the signature. A config that
	// only validates the certificate chain proves the far end holds an
	// org-issued certificate, not that it is the device this image was built
	// for — and the mesh's LAN path selects its peer from an unauthenticated
	// mDNS TXT record. Image layers can carry proprietary code, so the wrong
	// peer terminating this hop is a disclosure, not just a misdelivery.
	//
	// A function rather than a value so a certificate rotated while the agent
	// runs is picked up on the next build.
	PushTLS func(targetAssetID int32) (*tls.Config, error)
	// TargetAgentPort is the fallback mTLS gRPC port for a PushTarget sent by a
	// legacy CLI without its own agent_port. Zero means DefaultTargetAgentPort.
	TargetAgentPort uint16
}

// BuildService lets a CLI delegate an image build to this device. See the
// service comment in build_service.proto for the trust model.
type BuildService struct {
	agentpbv2.UnimplementedWendyBuildServiceServer
	logger               *zap.Logger
	buildHostEnabledPath string
	buildkitAddress      string
	stateDir             string
	chunks               ChunkSource
	peers                PeerDialer
	pushTLS              func(targetAssetID int32) (*tls.Config, error)
	maxContextBytes      int64
	// dialTarget opens the gRPC hop to a target device's agent for chunked
	// delivery. See targetDialer.
	dialTarget         targetDialer
	buildkitProcDir    string
	buildkitConfigPath string

	// contextLocks serialises builds that share a context directory. See
	// lockContextDir.
	contextLocks *BuildContextLockSet
}

func NewBuildService(logger *zap.Logger, opts BuildServiceOptions) *BuildService {
	if opts.BuildkitAddress == "" {
		opts.BuildkitAddress = DefaultBuildkitAddress
	}
	if opts.StateDir == "" {
		opts.StateDir = defaultBuildStateDir
	}
	if opts.MaxContextBytes <= 0 {
		opts.MaxContextBytes = maxContextBytes
	}
	if opts.ContextLocks == nil {
		opts.ContextLocks = NewBuildContextLockSet()
	}
	if opts.TargetAgentPort == 0 {
		opts.TargetAgentPort = DefaultTargetAgentPort
	}
	return &BuildService{
		logger:               logger,
		buildHostEnabledPath: filepath.Join(opts.ConfigPath, buildHostEnabledFile),
		buildkitAddress:      opts.BuildkitAddress,
		stateDir:             opts.StateDir,
		chunks:               opts.Chunks,
		peers:                opts.Peers,
		pushTLS:              opts.PushTLS,
		maxContextBytes:      opts.MaxContextBytes,
		dialTarget:           meshTargetDialer(opts.Peers, opts.PushTLS, opts.TargetAgentPort),
		contextLocks:         opts.ContextLocks,
		buildkitProcDir:      "/proc",
		buildkitConfigPath:   defaultBuildkitConfigPath,
	}
}

// lockContextDir serialises builds that reassemble into the same directory,
// returning the unlock function.
//
// A build host is shared by design, so two builds of the same app can overlap —
// two developers, or one developer's re-run racing their own previous build.
// The context directory is deliberately stable per app (see contextDir), and
// BuildImage clears and re-extracts it, while buildctl reads it as
// --local context= for the entire build. Without this lock the second build's
// RemoveAll deletes the first build's sources mid-build, so the first developer
// gets an image built from the second's code — and it is pushed to the first
// developer's device. That is a wrong image and a source disclosure, not merely
// a failed build.
//
// Entries are never deleted: one zero-size mutex per app id ever built on this
// host is unbounded only in the sense that the set of app ids is, and dropping
// an entry would need a use count to avoid handing out two mutexes for one
// directory.
func (s *BuildService) lockContextDir(dir string) func() {
	return s.contextLocks.lock(dir)
}

// builderEnabled reports whether this device has opted in to serving builds.
// Read per call rather than cached, so SetBuildHostEnabled takes effect on the
// next RPC without restarting the agent.
func (s *BuildService) builderEnabled() bool {
	_, err := os.Stat(s.buildHostEnabledPath)
	return err == nil
}

// SetBuildHostEnabled turns the builder role on or off, so opting a device in
// does not require shell access to place a file on it.
//
// It requires a USER certificate and refuses asset (device) ones. See the RPC's
// comment in build_service.proto: without that split the opt-in gate would be
// decorative, because anything able to call BuildImage could call this first.
func (s *BuildService) SetBuildHostEnabled(ctx context.Context, req *agentpbv2.SetBuildHostEnabledRequest) (*agentpbv2.SetBuildHostEnabledResponse, error) {
	actor, err := userIdentityFromContext(ctx, "changing the builder role")
	if err != nil {
		return nil, err
	}

	if req.GetEnabled() {
		if err := os.MkdirAll(filepath.Dir(s.buildHostEnabledPath), 0o755); err != nil {
			return nil, status.Errorf(codes.Internal, "creating agent config directory: %v", err)
		}
		if err := os.WriteFile(s.buildHostEnabledPath, nil, 0o600); err != nil {
			return nil, status.Errorf(codes.Internal, "enabling the builder role: %v", err)
		}
	} else if err := os.Remove(s.buildHostEnabledPath); err != nil && !os.IsNotExist(err) {
		// Already-absent is success, so disabling twice is not an error.
		return nil, status.Errorf(codes.Internal, "disabling the builder role: %v", err)
	}

	if s.logger != nil {
		// Names the actor: this grants code execution on the device to the whole
		// org, so "who turned it on" is the one question an audit of it asks. Org
		// and entity id only — both are ints, and the CN may carry a username,
		// which the mTLS interceptor already declines to log per call.
		s.logger.Info("builder role changed",
			zap.Bool("enabled", req.GetEnabled()),
			zap.Int32("actorOrg", actor.OrgID),
			zap.String("actorUser", actor.EntityID))
	}
	return &agentpbv2.SetBuildHostEnabledResponse{Enabled: req.GetEnabled()}, nil
}

// onLocalAdminSocket reports whether the caller arrived over the agent's local
// unix socket rather than a network listener.
//
// That socket carries no TLS, so it has no certificate to inspect — but it is
// not therefore untrusted: access to it IS the credential. It lives at 0660
// under a root-owned directory that oci.applyAdmin bind-mounts only into
// containers holding the admin entitlement, so reaching it already means
// full-agent authority.
//
// The distinction matters because "no TLS" alone does not imply the local
// socket. The pre-provisioning plaintext TCP server on the agent port also has
// no TLS and is reachable by anyone on the LAN, and registerAllServices puts
// this service on both. The listener's network is what separates them.
func onLocalAdminSocket(p *peer.Peer) bool {
	return p != nil && p.Addr != nil && p.Addr.Network() == "unix"
}

// userIdentityFromContext admits only a caller presenting a wendy USER
// certificate, and returns that identity so the caller can name it in an audit
// log. Mirrors MeshService.assetIdentityFromContext, which makes the opposite
// demand: mesh dials are device-to-device, this is human-to-device.
//
// action names the operation being gated, so the refusal tells the caller what
// they were denied rather than naming whichever RPC happened to define the
// helper.
//
// Org equality is deliberately left to the server's mTLS interceptors here.
// Unlike MeshDial — which must reject cross-org callers even when
// WENDY_MTLS_ORG_ENFORCEMENT is off — this device has no independent notion of
// which org "owns" it beyond the certificate chain that already admitted the
// connection.
func userIdentityFromContext(ctx context.Context, action string) (certs.WendyIdentity, error) {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return certs.WendyIdentity{}, status.Errorf(codes.PermissionDenied, "%s requires mTLS", action)
	}
	tlsInfo, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok || len(tlsInfo.State.PeerCertificates) == 0 {
		return certs.WendyIdentity{}, status.Errorf(codes.PermissionDenied, "%s requires a client certificate", action)
	}
	ident, found, err := certs.IdentityFromCert(tlsInfo.State.PeerCertificates[0])
	if err != nil || !found {
		return certs.WendyIdentity{}, status.Error(codes.PermissionDenied, "client certificate carries no wendy identity")
	}
	if ident.EntityType != "user" {
		return certs.WendyIdentity{}, status.Errorf(codes.PermissionDenied,
			"%s requires a user certificate; a device certificate is not accepted", action)
	}
	return ident, nil
}

func (s *BuildService) GetBuildCapabilities(_ context.Context, _ *agentpbv2.GetBuildCapabilitiesRequest) (*agentpbv2.GetBuildCapabilitiesResponse, error) {
	available := buildkitSocketPresent(s.buildkitAddress)
	resp := &agentpbv2.GetBuildCapabilitiesResponse{
		BuildkitAvailable: available,
		Os:                runtime.GOOS,
		CpuArchitecture:   runtime.GOARCH,
		BuilderEnabled:    s.builderEnabled(),
		// Constant true: this agent reads push_targets. It is advertised rather
		// than assumed because proto3 drops unknown fields, so a newer CLI
		// talking to an older agent would otherwise have no way to tell that its
		// fleet of targets was discarded — and would report a deploy that went
		// nowhere.
		MultiTargetDelivery: true,
		// Lets a newer client distinguish an older agent, which has no root
		// fields, from this agent failing to inspect a daemon it expected to find.
		BuildkitRootInspectionSupported: true,
		// Lets a CLI sending CHUNKING_MODE_FORCE tell this agent from one that
		// would discard the field and push through the registry regardless.
		ChunkDelivery: true,
	}
	if available {
		// Only the host's own platform is claimed native. Emulated platforms stay
		// empty until binfmt detection lands: over-claiming here would turn a
		// fast CLI refusal into a slow, mysterious remote failure.
		resp.NativePlatforms = []string{runtime.GOOS + "/" + runtime.GOARCH}
		resp.BuildkitVersion = buildctlVersion()

		// Where the cache lands, and how much room is there. Reported even when
		// it looks fine, so a client can decide rather than guess -- see
		// buildkitRoot for why the default is dangerous on an image-based OS.
		if location, ok := buildkitRoot(s.buildkitProcDir, s.buildkitAddress, s.buildkitConfigPath); ok {
			resp.BuildkitRoot = location.displayPath
			resp.BuildkitRootTotalBytes, resp.BuildkitRootFreeBytes = buildkitRootSpaceWithin(
				location.statPath, location.statBoundary)
		}
	}
	return resp, nil
}

// buildctlVersion reports the installed buildctl's version, empty when it
// cannot be determined. Worth surfacing because a build host's buildkit is
// installed out-of-band — distro package, tarball, or none — so "which version
// is on that box" is otherwise invisible from the CLI, and the Stagefile LLB
// path will care.
func buildctlVersion() string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "buildctl", "--version").Output()
	if err != nil {
		return ""
	}
	// "buildctl github.com/moby/buildkit v0.32.2 <sha>" → "v0.32.2"
	for _, f := range strings.Fields(string(out)) {
		if strings.HasPrefix(f, "v") {
			return f
		}
	}
	return ""
}

var buildctlCommandContext = exec.CommandContext

// buildkitSocketPresent reports whether a unix-socket buildkitd address points
// at something that exists. A non-unix address is taken at face value: only the
// device socket form can be checked without dialing.
func buildkitSocketPresent(addr string) bool {
	path, ok := strings.CutPrefix(addr, "unix://")
	if !ok {
		return addr != ""
	}
	_, err := os.Stat(path)
	return err == nil
}

// authorizeBuildSubmission decides whether this caller may submit a build.
//
// BuildImage executes client-supplied instructions under buildkitd, so this is
// the boundary that decides who gets code execution on the build host.
// registerAllServices puts this service on three listeners, which are not
// equally trusted:
//
//   - The mTLS TCP server — a developer's CLI presenting a user certificate.
//     This covers the cloud tunnel too: the broker relays bytes, so the CLI's
//     own certificate is what terminates at the agent (see connectCloudAsset).
//     Allowed.
//   - The local unix socket — an on-device container holding the admin
//     entitlement. No TLS, but reaching the socket is already full-agent
//     authority. Allowed; see onLocalAdminSocket.
//   - The pre-provisioning plaintext TCP server — anyone on the LAN, no
//     credential whatsoever. Refused. It only runs before provisioning, when a
//     build host has nothing to build with, so this closes a hole rather than
//     removing a capability.
//
// A DEVICE certificate is refused everywhere. Builds are submitted by people;
// nothing in the design has one device build for another. SetBuildHostEnabled
// already makes exactly this demand, and without the same demand here the gate
// it protects would admit the certificate type it rejects — leaving a
// compromised device in the org with arbitrary code execution on the build
// host, which is the whole thing the opt-in was meant to prevent.
func authorizeBuildSubmission(ctx context.Context) error {
	if p, ok := peer.FromContext(ctx); ok && onLocalAdminSocket(p) {
		return nil
	}
	_, err := userIdentityFromContext(ctx, "submitting a build")
	return err
}

func (s *BuildService) BuildImage(stream agentpbv2.WendyBuildService_BuildImageServer) error {
	// Before the opt-in check, so an unauthorized caller cannot use this RPC to
	// probe whether a device is a build host.
	if err := authorizeBuildSubmission(stream.Context()); err != nil {
		return err
	}
	if !s.builderEnabled() {
		return status.Error(codes.FailedPrecondition,
			"this device is not configured as a build host; run `wendy device build-host enable --device <this device>` to allow remote builds")
	}
	ctx := stream.Context()

	first, err := stream.Recv()
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "reading build spec: %v", err)
	}
	spec := first.GetSpec()
	if spec == nil {
		return status.Error(codes.InvalidArgument, "the first message must carry a build spec")
	}
	if _, err := stream.Recv(); err != io.EOF {
		if err != nil {
			return status.Errorf(codes.InvalidArgument, "reading build request: %v", err)
		}
		return status.Error(codes.InvalidArgument, "only the first build request message may carry data")
	}
	df := spec.GetDockerfileBuild()
	if df == nil {
		return status.Error(codes.InvalidArgument, "build spec carries no build definition")
	}

	// Validate EVERY destination before any work: a build that cannot be
	// delivered is wasted minutes on a machine other people are sharing, and
	// with a fleet that arithmetic only gets worse. One bad asset id fails the
	// request now rather than after the last device has waited through a build.
	targets, err := deliveryTargets(spec)
	if err != nil {
		return err
	}
	for _, t := range targets {
		if err := validatePushTarget(t); err != nil {
			return err
		}
	}
	// Duplicates would push the same image to the same device twice and report
	// it as two deliveries — a fleet listed as larger than it is.
	seen := make(map[int32]struct{}, len(targets))
	for _, t := range targets {
		if _, dup := seen[t.GetAssetId()]; dup {
			return status.Errorf(codes.InvalidArgument, "asset %d is listed as a delivery target more than once", t.GetAssetId())
		}
		seen[t.GetAssetId()] = struct{}{}
	}
	if s.peers == nil {
		return status.Error(codes.FailedPrecondition, "this build host has no mesh dialer and cannot deliver an image to another device")
	}
	// The role can be enabled on a host whose buildkitd was never installed, or
	// has since stopped. GetBuildCapabilities reports that, but a preflight the
	// CLIENT performs is not a check the server has made — and without it the
	// context is reassembled and extracted first, only for buildctl to fail with
	// a bare "exit status 1" that names nothing.
	if !buildkitSocketPresent(s.buildkitAddress) {
		return status.Errorf(codes.FailedPrecondition,
			"this build host has no buildkit daemon at %s; install and start buildkitd there, or build elsewhere", s.buildkitAddress)
	}

	dir, err := s.contextDir(spec.GetAppId())
	if err != nil {
		return err
	}
	// Held for the whole build, not just the extraction: buildctl reads this
	// directory from start to finish. See lockContextDir.
	unlockContext := s.lockContextDir(dir)
	defer unlockContext()

	tarBytes, err := s.reassembleContext(ctx, spec.GetContext())
	if err != nil {
		return err
	}
	// Clear before extracting: a file deleted from the project must not survive
	// in the reused context directory and keep satisfying a COPY.
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("clearing stale build context: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("recreating build context: %w", err)
	}
	if err := extractContextTar(bytes.NewReader(tarBytes), dir); err != nil {
		return err
	}

	if s.pushTLS == nil {
		return status.Error(codes.FailedPrecondition, "this build host has no client certificate for delivering an image to another device")
	}
	// Credentials for every device are loaded — and pinned to that device —
	// before the build, for the reason the targets were validated up front: a
	// build that cannot be delivered is wasted minutes on a machine other people
	// share.
	for _, t := range targets {
		if _, err := s.pushTLS(t.GetAssetId()); err != nil {
			return status.Errorf(codes.FailedPrecondition, "loading delivery credentials for device %d: %v", t.GetAssetId(), err)
		}
	}

	// --chunking=off asks for the route this feature shipped with, whole: no
	// export, no chunk store, a registry push per device.
	if spec.GetChunking() == agentpbv2.ChunkingMode_CHUNKING_MODE_OFF {
		return s.deliverAllByRegistry(ctx, stream, spec, df, dir, targets)
	}

	// Build ONCE, exporting an OCI layout beside the context directory. Delivery
	// is then a separate step per device, fed from that export: which layers and
	// chunks a device lacks is a question about that device, and a rebuild does
	// not answer it. The registry push this replaces ran one buildctl pass per
	// device because the push WAS the export; it survives as the fallback for an
	// agent too old to receive chunks, and as the whole route when the CLI asks
	// for it (see deliver and deliverAllByRegistry).
	imageTar := dir + exportedImageSuffix
	defer os.Remove(imageTar)
	args, err := buildctlOCIArgs(dir, df.GetDockerfile(), spec.GetPlatform(), df.GetBuildArgs(), imageTar)
	if err != nil {
		return err
	}
	if err := s.runBuildctl(ctx, stream, args, ""); err != nil {
		return err
	}
	img, err := readExportedImage(imageTar, spec.GetPlatform())
	if err != nil {
		return status.Errorf(codes.Internal, "reading the built image: %v", err)
	}

	// Decompressed and chunked once per layer for the whole fleet, not once per
	// device: the second camera costs a diff, not another pass over a multi-GiB
	// layer. The price is that every missing layer's scratch stays on disk until
	// the last device is done — see the design note on disk.
	resolved := make(map[string]*deliveryLayer, len(img.layers))
	defer closeDeliveryLayers(resolved)

	prog := &buildProgress{stream: stream}
	deliveries := make([]*agentpbv2.DeliveryResult, 0, len(targets))
	for i, t := range targets {
		deliveryErr := s.deliver(ctx, prog, stream, spec, df, dir, img, resolved, i, t)
		if len(targets) == 1 && deliveryErr != nil {
			// Single-target behaviour is unchanged, including the code and the
			// message prefix a CLI built before fleet delivery already uses to
			// tell a delivery failure from a build failure.
			return status.Errorf(codes.Unavailable, "pushing the built image to the target device failed: %v", deliveryErr)
		}
		// With several devices, one device's problem is one device's problem.
		// Recording it and moving on is the whole point: a fleet deploy must not
		// be abandoned halfway because the third camera is offline.
		res := &agentpbv2.DeliveryResult{AssetId: t.GetAssetId(), Delivered: deliveryErr == nil}
		if deliveryErr != nil {
			res.Error = deliveryErr.Error()
		}
		deliveries = append(deliveries, res)
	}

	// The stream ends OK even when some devices failed. The per-device outcomes
	// carry that, and the CLI decides what to say and what to exit with —
	// because "the build succeeded, two of three devices have it" is neither a
	// failed build nor a successful deploy, and collapsing it into one status
	// code is how a device silently misses a change.
	return stream.Send(&agentpbv2.BuildImageProgress{
		Event: &agentpbv2.BuildImageProgress_Result{
			Result: &agentpbv2.BuildImageResult{ImageDigest: img.manifestDigest, Deliveries: deliveries},
		},
	})
}

// deliver gets the built image onto one device: by chunks into its content
// store, or — for an agent that predates the RPCs that needs — through the
// registry push the feature shipped with, as a second buildctl pass that
// BuildKit's cache turns into a re-export.
//
// The fallback is taken ONLY on errChunkDeliveryUnsupported, and only when the
// CLI's --chunking allows it. A genuine failure is reported as one: retrying it
// over the slower path would blame the wrong leg, and on a link that just
// dropped a whole-image push is the transfer least likely to survive.
func (s *BuildService) deliver(
	ctx context.Context,
	prog *buildProgress,
	stream agentpbv2.WendyBuildService_BuildImageServer,
	spec *agentpbv2.BuildSpec,
	df *agentpbv2.DockerfileBuild,
	dir string,
	img *exportedImage,
	resolved map[string]*deliveryLayer,
	index int,
	target *agentpbv2.PushTarget,
) error {
	err := s.deliverByChunks(ctx, prog, index, img, target, resolved)
	if !errors.Is(err, errChunkDeliveryUnsupported) {
		return err
	}
	if spec.GetChunking() == agentpbv2.ChunkingMode_CHUNKING_MODE_FORCE {
		// force exists so a chunk-delivery problem is surfaced rather than
		// masked by a slower path. An agent that cannot take chunks is one.
		return fmt.Errorf("device %d predates chunked delivery, and --chunking=force forbids the registry push it would otherwise get; update its agent, or use --chunking=auto",
			target.GetAssetId())
	}
	prog.logf("#%d 0.000 device %d predates chunked delivery; pushing through its registry instead",
		deliveryVertexBase+index, target.GetAssetId())
	buildErr, deliveryErr := s.buildAndDeliver(ctx, stream, spec, df, dir, target)
	if deliveryErr != nil {
		return deliveryErr
	}
	// The export pass already proved the Dockerfile builds, so a failure here is
	// this device's pass failing, not the build.
	return buildErr
}

// deliverAllByRegistry is the route this feature shipped with, kept whole for
// CHUNKING_MODE_OFF: one buildctl pass per device, its output pushed into that
// device's registry. No export and no chunk store. The image is built once in
// any real sense — BuildKit has solved it after the first pass, so the rest are
// cache hits that re-export and push — and each pass gets its OWN proxy and
// asset-pinned TLS config: one pinned identity standing in for several devices
// is exactly the property the pin exists to deny.
func (s *BuildService) deliverAllByRegistry(
	ctx context.Context,
	stream agentpbv2.WendyBuildService_BuildImageServer,
	spec *agentpbv2.BuildSpec,
	df *agentpbv2.DockerfileBuild,
	dir string,
	targets []*agentpbv2.PushTarget,
) error {
	prog := &buildProgress{stream: stream}
	deliveries := make([]*agentpbv2.DeliveryResult, 0, len(targets))
	multi := len(targets) > 1
	for i, t := range targets {
		prog.logf("#%d 0.000 device %d: pushing through its registry, as --chunking=off asks",
			deliveryVertexBase+i, t.GetAssetId())
		buildErr, deliveryErr := s.buildAndDeliver(ctx, stream, spec, df, dir, t)

		// A build failure on the FIRST pass is a build failure, full stop: no
		// device can receive this image and continuing would report N identical
		// failures for one broken Dockerfile.
		if buildErr != nil && i == 0 {
			return buildErr
		}
		if !multi {
			// Single-target behaviour keeps the code and the message prefix a
			// CLI built before fleet delivery already distinguishes.
			if deliveryErr != nil {
				return status.Errorf(codes.Unavailable, "pushing the built image to the target device failed: %v", deliveryErr)
			}
			if buildErr != nil {
				return buildErr
			}
			deliveries = append(deliveries, &agentpbv2.DeliveryResult{AssetId: t.GetAssetId(), Delivered: true})
			break
		}

		// Past the first pass, one device's problem is one device's problem.
		res := &agentpbv2.DeliveryResult{AssetId: t.GetAssetId(), Delivered: true}
		switch {
		case deliveryErr != nil:
			res.Delivered, res.Error = false, deliveryErr.Error()
		case buildErr != nil:
			res.Delivered, res.Error = false, buildErr.Error()
		}
		deliveries = append(deliveries, res)
	}
	// No export was read, so no manifest digest is known — as before chunked
	// delivery existed.
	return stream.Send(&agentpbv2.BuildImageProgress{
		Event: &agentpbv2.BuildImageProgress_Result{
			Result: &agentpbv2.BuildImageResult{Deliveries: deliveries},
		},
	})
}

// deliveryTargets resolves where this build is delivered, preferring the fleet
// field and falling back to the single one so a CLI built before fleet delivery
// keeps working. Setting both is refused rather than resolved by precedence: it
// means the client disagrees with itself about where an image is going, and
// guessing is how an image reaches a device nobody asked for.
func deliveryTargets(spec *agentpbv2.BuildSpec) ([]*agentpbv2.PushTarget, error) {
	many, one := spec.GetPushTargets(), spec.GetPushTarget()
	if len(many) > 0 && one != nil {
		return nil, status.Error(codes.InvalidArgument, "set push_targets or push_target, not both")
	}
	if len(many) > 0 {
		return many, nil
	}
	if one != nil {
		return []*agentpbv2.PushTarget{one}, nil
	}
	return nil, status.Error(codes.InvalidArgument, "build spec names no delivery target")
}

// buildAndDeliver runs one buildctl pass whose output is pushed to a single
// device's registry, and separates the two failures that look identical from
// the outside. It is the delivery path for an agent too old to receive chunks
// (see deliver); a current agent gets deliverByChunks.
//
// A failed outbound push reaches buildctl only as a reset loopback connection,
// so its error says "exit status 1" and names nothing. When the proxy knows the
// real reason, that is the one worth reporting: the remedies diverge — a
// build-file fix versus mesh reachability or registry auth — so collapsing them
// sends the developer to debug the wrong layer.
func (s *BuildService) buildAndDeliver(
	ctx context.Context,
	stream agentpbv2.WendyBuildService_BuildImageServer,
	spec *agentpbv2.BuildSpec,
	df *agentpbv2.DockerfileBuild,
	dir string,
	target *agentpbv2.PushTarget,
) (buildErr, deliveryErr error) {
	// Pinned to this target's asset id, so only that device can terminate the
	// registry hop.
	tlsCfg, err := s.pushTLS(target.GetAssetId())
	if err != nil {
		return nil, fmt.Errorf("loading push credentials: %v", err)
	}
	proxy, err := startPushProxy(ctx, s.peers, target, tlsCfg)
	if err != nil {
		return nil, err
	}
	defer proxy.stop()

	// The credential goes in a file, not in args: buildctl's argv is world
	// readable through /proc/<pid>/cmdline, so anything secret there would be
	// readable by the local user this gate exists to stop. See
	// dockerConfigWithPushAuth.
	//
	// Per target, because the proxy address and credential are minted per pass.
	authDir := fmt.Sprintf("%s%s.%d", dir, dockerConfigDirSuffix, target.GetAssetId())
	if err := dockerConfigWithPushAuth(authDir, proxy.addr, proxy.credential); err != nil {
		return status.Errorf(codes.Internal, "%v", err), nil
	}
	defer os.RemoveAll(authDir)

	args, err := buildctlArgs(dir, df.GetDockerfile(), spec.GetPlatform(), proxy.addr+"/"+target.GetRepository(), df.GetBuildArgs())
	if err != nil {
		return err, nil
	}

	buildErr = s.runBuildctl(ctx, stream, args, authDir)
	// BuildKit retries registry 5xx responses itself. The proxy may therefore
	// have observed an outbound failure even though a later attempt completed
	// and buildctl exited successfully. In that case the successful solve is
	// authoritative; retaining the proxy's earlier error would turn recovery into
	// a false delivery failure.
	if buildErr == nil {
		return classifyBuildAndDeliveryResult(buildErr, proxy.latestError())
	}
	if proxyErr := proxy.latestError(); proxyErr != nil {
		// Say what a new run can reuse without implying that containerd resumes
		// the failed request: the build cache and fully committed layers survive,
		// but a partial monolithic layer upload starts again.
		if deliveryFailureStarted(proxyErr) {
			proxyErr = fmt.Errorf("%w: %w", errDeliveryIncomplete, proxyErr)
		}
		return classifyBuildAndDeliveryResult(buildErr, proxyErr)
	}
	return classifyBuildAndDeliveryResult(buildErr, nil)
}

// classifyBuildAndDeliveryResult keeps a recovered proxy attempt from
// outweighing buildctl's final result. BuildKit owns registry-request retries,
// so only a failed buildctl invocation can promote the proxy's diagnostic to a
// terminal delivery error.
func classifyBuildAndDeliveryResult(buildErr, proxyErr error) (error, error) {
	if buildErr == nil {
		return nil, nil
	}
	if proxyErr != nil {
		return nil, proxyErr
	}
	return buildErr, nil
}

// reassembleContext rebuilds the context tar from its chunk manifest. Every
// chunk must already be staged; a missing one is the client's error rather than
// something to paper over with a partial context that would build the wrong
// image.
func (s *BuildService) reassembleContext(ctx context.Context, m *agentpbv2.ChunkManifest) ([]byte, error) {
	if m == nil || len(m.GetChunkHashes()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "build spec carries no build context")
	}
	if s.chunks == nil {
		return nil, status.Error(codes.FailedPrecondition, "this build host has no chunk store configured")
	}
	// Refuse an oversized context before reading a byte of it. The declared size
	// is not trusted — the LimitReader below is what actually holds the line —
	// but checking it first turns a pathological request into a cheap error
	// instead of a gigabyte of I/O.
	if want := m.GetTotalSize(); want > s.maxContextBytes {
		return nil, status.Errorf(codes.InvalidArgument,
			"build context declares %d bytes, above the %d-byte maximum", want, s.maxContextBytes)
	}
	hashes := make([][32]byte, 0, len(m.GetChunkHashes()))
	for _, b := range m.GetChunkHashes() {
		if len(b) != 32 {
			return nil, status.Errorf(codes.InvalidArgument, "chunk hash must be 32 bytes, got %d", len(b))
		}
		var h [32]byte
		copy(h[:], b)
		hashes = append(hashes, h)
	}

	// Read one byte past the ceiling so exceeding it is detectable rather than
	// silently truncated into a context that would build the wrong image. A
	// manifest may declare total_size 0, so the limit — not the declaration — is
	// what bounds the agent's memory.
	data, err := io.ReadAll(io.LimitReader(s.chunks.OpenChunkStream(ctx, hashes), s.maxContextBytes+1))
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition,
			"reassembling build context: %v; re-send the context", err)
	}
	if int64(len(data)) > s.maxContextBytes {
		return nil, status.Errorf(codes.InvalidArgument,
			"build context exceeds the %d-byte maximum", s.maxContextBytes)
	}
	if want := m.GetTotalSize(); want != 0 && int64(len(data)) != want {
		return nil, status.Errorf(codes.InvalidArgument,
			"reassembled build context is %d bytes, manifest declares %d", len(data), want)
	}
	return data, nil
}

// runBuildctl streams buildctl's plain-mode output back as log lines and
// finishes with the result event.
//
// dockerConfigDir, when set, holds the loopback push credential of a registry
// push. buildctl reads it into the auth provider it attaches to the build
// session, which is how buildkitd answers the proxy's 401 challenge. An OCI
// export pushes nowhere and passes "".
func (s *BuildService) runBuildctl(ctx context.Context, stream agentpbv2.WendyBuildService_BuildImageServer, args []string, dockerConfigDir string) error {
	cmd := buildctlCommandContext(ctx, "buildctl", args...)
	cmd.Env = append(os.Environ(), "BUILDKIT_HOST="+s.buildkitAddress)
	if dockerConfigDir != "" {
		cmd.Env = append(cmd.Env, "DOCKER_CONFIG="+dockerConfigDir)
	}
	if s.logger != nil {
		// Build-arg VALUES can carry secrets, so the command line is never logged raw.
		s.logger.Info("remote build starting", zap.Strings("args", redactBuildctlArgs(args)))
	}

	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting buildctl: %w", err)
	}

	sc := bufio.NewScanner(pipe)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		if sendErr := stream.Send(&agentpbv2.BuildImageProgress{
			Event: &agentpbv2.BuildImageProgress_LogLine{LogLine: sc.Text()},
		}); sendErr != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return sendErr
		}
	}
	if scanErr := sc.Err(); scanErr != nil {
		// Scanner stops draining the pipe after an oversized line or read error.
		// Kill and reap buildctl before returning; waiting first can deadlock if
		// the child is blocked writing the rest of that line into the full pipe.
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return status.Errorf(codes.Internal, "reading buildctl output: %v", scanErr)
	}
	if err := cmd.Wait(); err != nil {
		return status.Errorf(codes.Internal, "build failed: %v", err)
	}
	// The result is sent by the caller, not here: with several targets this runs
	// once per device, and a result per pass would tell the CLI the build
	// finished N times. The caller sends exactly one, carrying every device's
	// outcome.
	return nil
}

// redactBuildctlArgs masks every --opt build-arg:KEY=VALUE value, keeping the
// key for debugging. Mirrors the CLI's redactBuildctlArgsForLog.
func redactBuildctlArgs(args []string) []string {
	out := make([]string, len(args))
	copy(out, args)
	for i, a := range out {
		rest, ok := strings.CutPrefix(a, "build-arg:")
		if !ok {
			continue
		}
		if k, _, found := strings.Cut(rest, "="); found && k != "" {
			out[i] = "build-arg:" + k + "=<redacted>"
		}
	}
	return out
}

// contextDir returns the per-app directory a build context is reassembled into.
// It is deliberately stable rather than a fresh temp dir: BuildKit keys its
// local-source cache on this path, so a path that changed between builds would
// make it re-transfer the whole context internally every time.
func (s *BuildService) contextDir(appID string) (string, error) {
	clean := sanitizeAppID(appID)
	if clean == "" {
		return "", status.Errorf(codes.InvalidArgument, "app id %q contains no usable characters", appID)
	}
	// Sanitising DROPS characters, so it is not injective: "sh.wendy.app" and
	// "shwendyapp" both reduce to the same name and would then share one context
	// directory — two unrelated apps clearing and re-extracting over each other.
	// The digest of the original id restores the distinction the sanitiser threw
	// away, while keeping the readable part readable.
	sum := sha256.Sum256([]byte(appID))
	dir := filepath.Join(s.stateDir, clean+"-"+hex.EncodeToString(sum[:4]))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("creating build context directory: %w", err)
	}
	return dir, nil
}

// sanitizeAppID reduces an app id to characters that cannot traverse or escape
// a path component. Dropping unknown characters rather than rejecting outright
// keeps ordinary ids working; an id that sanitises to nothing is rejected by
// contextDir.
func sanitizeAppID(appID string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return -1
		}
	}, appID)
}

// extractContextTar writes a build-context tar into dir, refusing any entry
// that would land outside it. The client is not trusted, even an in-org one.
func extractContextTar(r io.Reader, dir string) error {
	root, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading build context: %w", err)
		}
		if filepath.IsAbs(hdr.Name) || strings.HasPrefix(hdr.Name, "/") {
			return status.Errorf(codes.InvalidArgument, "build context entry %q is an absolute path", hdr.Name)
		}
		target := filepath.Join(root, filepath.FromSlash(hdr.Name))
		if target != root && !strings.HasPrefix(target, root+string(os.PathSeparator)) {
			return status.Errorf(codes.InvalidArgument, "build context entry %q escapes the context root", hdr.Name)
		}
		if hdr.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o700); err != nil {
				return err
			}
			continue
		}
		// Link entries are rejected rather than written. The name checks above
		// constrain where an entry LANDS, not where it POINTS, and whether a
		// symlink out of the context is then followed is BuildKit's decision,
		// not ours. The CLI's packer never emits one (it skips non-regular
		// files), so refusing costs nothing and beats the current silent
		// behaviour of writing an empty regular file in its place.
		if hdr.Typeflag == tar.TypeSymlink || hdr.Typeflag == tar.TypeLink {
			return status.Errorf(codes.InvalidArgument,
				"build context entry %q is a link, which is not accepted in a remote build context", hdr.Name)
		}
		// Everything else that is not a plain file — device nodes, fifos, sockets
		// — would otherwise be written out as an ordinary empty file, silently
		// changing what the Dockerfile sees. The CLI's packer emits only regular
		// files and directories, so refusing is free and honest.
		if hdr.Typeflag != tar.TypeReg {
			return status.Errorf(codes.InvalidArgument,
				"build context entry %q is not a regular file (tar type %q)", hdr.Name, string(hdr.Typeflag))
		}
		// A single entry must not be able to fill the build host's disk. Refuse
		// the declared size first, so a pathological header costs no I/O.
		if hdr.Size > maxContextEntryBytes {
			return status.Errorf(codes.InvalidArgument,
				"build context entry %q declares %d bytes, above the %d-byte per-file maximum",
				hdr.Name, hdr.Size, int64(maxContextEntryBytes))
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(hdr.Mode).Perm())
		if err != nil {
			return err
		}
		// The header is not trusted either, so the copy is bounded independently.
		// Read one byte past the ceiling: a truncating LimitReader would leave a
		// short file behind with no error, and a build from a silently truncated
		// source is worse than a failed one.
		n, err := io.Copy(f, io.LimitReader(tr, maxContextEntryBytes+1))
		if err != nil {
			f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return fmt.Errorf("writing build context entry %q: %w", hdr.Name, err)
		}
		if n > maxContextEntryBytes {
			return status.Errorf(codes.InvalidArgument,
				"build context entry %q exceeds the %d-byte per-file maximum", hdr.Name, int64(maxContextEntryBytes))
		}
	}
}

// buildctlArgs builds the buildctl invocation for the registry-push fallback:
// an image export that pushes to pushRef.
func buildctlArgs(contextDir, dockerfile, platform, pushRef string, buildArgs map[string]string) ([]string, error) {
	args, err := buildctlBaseArgs(contextDir, dockerfile, platform, buildArgs)
	if err != nil {
		return nil, err
	}
	return append(args, "--output", "type=image,name="+pushRef+",push=true"), nil
}

// buildctlOCIArgs builds the buildctl invocation for chunked delivery: an OCI
// layout tar written to dest on this host, which delivery reads layers from. It
// is the export the CLI's own buildkitOCIArgs asks for — the same tar the
// laptop path chunk-diffs from.
func buildctlOCIArgs(contextDir, dockerfile, platform string, buildArgs map[string]string, dest string) ([]string, error) {
	args, err := buildctlBaseArgs(contextDir, dockerfile, platform, buildArgs)
	if err != nil {
		return nil, err
	}
	return append(args, "--output", "type=oci,dest="+dest), nil
}

// buildctlBaseArgs is the invocation up to its --output, with every
// client-supplied field shape-checked.
func buildctlBaseArgs(contextDir, dockerfile, platform string, buildArgs map[string]string) ([]string, error) {
	// Shape-checked like everything else the client sends. Injection is not the
	// hazard — it becomes one argv element and no shell is involved — but an
	// unconstrained string here would reach the streamed build log, where
	// control characters can forge lines or emit terminal escapes.
	if !validPlatformRe.MatchString(platform) {
		return nil, status.Errorf(codes.InvalidArgument,
			"platform %q must be os/arch or os/arch/variant", platform)
	}
	if filepath.IsAbs(dockerfile) {
		return nil, status.Errorf(codes.InvalidArgument, "dockerfile %q must be relative to the build context", dockerfile)
	}
	target := filepath.Join(contextDir, filepath.FromSlash(dockerfile))
	if !strings.HasPrefix(target, contextDir+string(os.PathSeparator)) {
		return nil, status.Errorf(codes.InvalidArgument, "dockerfile %q escapes the build context", dockerfile)
	}

	keys, err := buildargs.SortedValidatedKeys(buildArgs)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid build arg: %v", err)
	}

	args := []string{
		"build",
		"--frontend", "dockerfile.v0",
		"--local", "context=" + contextDir,
		"--local", "dockerfile=" + contextDir,
		"--opt", "filename=" + dockerfile,
		"--opt", "platform=" + platform,
	}
	for _, k := range keys {
		args = append(args, "--opt", "build-arg:"+k+"="+buildArgs[k])
	}
	return args, nil
}
