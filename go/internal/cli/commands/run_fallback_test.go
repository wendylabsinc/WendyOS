package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestRegistryFallbackPlan covers the gate that decides whether a registry-push
// fallback needs interactive confirmation. Confirmation is required only when
// the fallback is both large (>= largeRegistryFallbackBytes) and a human is
// actually present to answer: non-interactive runs (CI, `wendy watch`) and
// --yes must always proceed on the loud notice alone, since there is nobody to
// ask and this path must never hard-fail a CI deploy (WDY-2432).
func TestRegistryFallbackPlan(t *testing.T) {
	const small = largeRegistryFallbackBytes - 1
	const large = largeRegistryFallbackBytes

	cases := []struct {
		name        string
		imageBytes  int64
		interactive bool
		assumeYes   bool
		want        registryFallbackAction
	}{
		{"small, interactive, no --yes", small, true, false, fallbackProceedLoud},
		{"small, interactive, --yes", small, true, true, fallbackProceedLoud},
		{"small, non-interactive, no --yes", small, false, false, fallbackProceedLoud},
		{"small, non-interactive, --yes", small, false, true, fallbackProceedLoud},
		{"zero bytes, interactive, no --yes", 0, true, false, fallbackProceedLoud},
		{"large, interactive, no --yes", large, true, false, fallbackConfirm},
		{"large, interactive, --yes", large, true, true, fallbackProceedLoud},
		{"large, non-interactive, no --yes", large, false, false, fallbackProceedLoud},
		{"large, non-interactive, --yes", large, false, true, fallbackProceedLoud},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := registryFallbackPlan(tc.imageBytes, tc.interactive, tc.assumeYes); got != tc.want {
				t.Fatalf("registryFallbackPlan(%d, interactive=%v, assumeYes=%v) = %v, want %v",
					tc.imageBytes, tc.interactive, tc.assumeYes, got, tc.want)
			}
		})
	}
}

// TestFormatRegistryFallbackNotice verifies the chunk-diff error is always
// surfaced (it used to be silently dropped), and that large fallbacks get a
// louder, more specific message about how much data is about to be
// re-uploaded, while small/unknown-size fallbacks skip the size clause
// entirely rather than claiming to know a size we don't.
func TestFormatRegistryFallbackNotice(t *testing.T) {
	chunkErr := errors.New("QueryChunks unimplemented")

	t.Run("large image includes size and FULL registry push wording", func(t *testing.T) {
		// ~1.9GB decimal.
		got := formatRegistryFallbackNotice(chunkErr, 1_900_000_000)
		if !strings.Contains(got, chunkErr.Error()) {
			t.Fatalf("notice %q does not contain the chunk-diff error %q", got, chunkErr.Error())
		}
		if !strings.Contains(got, "1.9GB") {
			t.Fatalf("notice %q does not contain the expected size %q", got, "1.9GB")
		}
		if !strings.Contains(got, "FULL registry push") {
			t.Fatalf("notice %q does not contain %q", got, "FULL registry push")
		}
	})

	t.Run("zero bytes omits the size clause", func(t *testing.T) {
		got := formatRegistryFallbackNotice(chunkErr, 0)
		if !strings.Contains(got, chunkErr.Error()) {
			t.Fatalf("notice %q does not contain the chunk-diff error %q", got, chunkErr.Error())
		}
		if strings.Contains(got, "GB") || strings.Contains(got, "MB") || strings.Contains(got, "will be re-uploaded") {
			t.Fatalf("notice %q should not contain a size clause for zero bytes", got)
		}
	})

	t.Run("small image omits the size clause", func(t *testing.T) {
		got := formatRegistryFallbackNotice(chunkErr, largeRegistryFallbackBytes-1)
		if strings.Contains(got, "will be re-uploaded") {
			t.Fatalf("notice %q should not contain the large-fallback size clause below the threshold", got)
		}
	})
}

// TestIsChunkDeployCancellation covers the predicate the fallback ladder uses
// to decide "the user said stop" versus "chunk-diff genuinely failed". Both a
// cancelled context (e.g. `wendy watch` superseding a deploy, or Ctrl-C before
// the interactive progress bar starts) and ErrUserCancelled (the user backing
// out of the interactive chunk-push progress bar — Bubble Tea captures Ctrl-C
// as a key event there, so ctx itself is NOT cancelled in that case) must
// short-circuit the ladder before it ever reaches the registry-push fallback
// notice/prompt: falling back would start a bigger upload after the user
// asked to stop (finding 1, WDY-2432/2433 final-review fix wave).
func TestIsChunkDeployCancellation(t *testing.T) {
	t.Run("ErrUserCancelled with a live context", func(t *testing.T) {
		if !isChunkDeployCancellation(context.Background(), ErrUserCancelled) {
			t.Fatal("want true: the user cancelled the interactive progress bar")
		}
	})

	t.Run("wrapped ErrUserCancelled", func(t *testing.T) {
		err := fmt.Errorf("pushing changed layers: %w", ErrUserCancelled)
		if !isChunkDeployCancellation(context.Background(), err) {
			t.Fatal("want true: errors.Is must see through the wrap")
		}
	})

	t.Run("context cancelled, ordinary error", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if !isChunkDeployCancellation(ctx, errors.New("QueryChunks unimplemented")) {
			t.Fatal("want true: a cancelled context is itself a cancellation, regardless of err's shape")
		}
	})

	t.Run("neither cancelled: a genuine chunk-diff failure", func(t *testing.T) {
		if isChunkDeployCancellation(context.Background(), errors.New("QueryChunks unimplemented")) {
			t.Fatal("want false: this must fall through to the registry-push fallback classifiers")
		}
	})
}

// Drive the actual deploy ladder with a local image fixture. A provisioned
// registry with no local credentials stops the allowed fallback before any
// network access. Missing capabilities retain this route in auto mode;
// Cloud v2 transport drops, refusal, and force mode must stop first.
func TestRunRegistryFallbackPreservesMissingChunkCapability(t *testing.T) {
	defer forceBuildProgressInteractive(false)()
	setupCloudRunFixture(t)
	for _, tc := range []struct {
		name               string
		cloudV2, reconnect bool
		code               codes.Code
		mode               string
		wantRegistry       bool
	}{
		{name: "LAN", code: codes.Unimplemented, mode: chunkingAuto, wantRegistry: true},
		{name: "Cloud v1", reconnect: true, code: codes.Unimplemented, mode: chunkingAuto, wantRegistry: true},
		{name: "Cloud v2 missing capability", cloudV2: true, reconnect: true, code: codes.Unimplemented, mode: chunkingAuto, wantRegistry: true},
		{name: "Cloud v2 force missing capability", cloudV2: true, code: codes.Unimplemented, mode: chunkingForce},
		{name: "Cloud v2 drop", cloudV2: true, reconnect: true, code: codes.Unavailable, mode: chunkingAuto},
		{name: "Cloud v2 refused", cloudV2: true, code: codes.PermissionDenied, mode: chunkingAuto},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM busybox:latest\n"), 0600); err != nil {
				t.Fatal(err)
			}
			registry := &fallbackProvisioningClient{}
			cause := status.Error(tc.code, "fixture chunk capability response")
			conn := &grpcclient.AgentConnection{Host: "fixture", IsCloudV2: tc.cloudV2, ContainerService: &unavailableContainerClient{err: cause}, ProvisioningService: registry}
			conn.CacheAgentVersion(&agentpb.GetAgentVersionResponse{Os: "linux", CpuArchitecture: "arm64"})
			if tc.reconnect {
				conn.Reconnect = func(context.Context) (*grpcclient.AgentConnection, error) {
					if tc.code != codes.Unavailable {
						t.Fatal("only transport drops should reconnect")
					}
					return nil, context.DeadlineExceeded
				}
			}
			cfg := &appconfig.AppConfig{AppID: "fallback-fixture"}
			opts := runOptions{builder: imageBuilderAppleContainer, chunking: tc.mode, managedRobot: true, yes: true}
			err := runWithAgentRecorded(t.Context(), conn, dir, cfg, opts, newDeployMetrics(opts))
			if tc.wantRegistry {
				if registry.calls != 1 || !errors.Is(err, errRegistryAuth) {
					t.Fatalf("registry calls=%d err=%v, want the registry auth gate", registry.calls, err)
				}
			} else {
				if registry.calls != 0 || !errors.Is(err, cause) {
					t.Fatalf("registry calls=%d err=%v, want the original cause without fallback", registry.calls, err)
				}
				if tc.code == codes.Unavailable && ExecutionErrorClass(err) != "device_unreachable" {
					t.Fatalf("drop class=%s", ExecutionErrorClass(err))
				}
				if tc.code == codes.Unimplemented && !strings.Contains(err.Error(), "Update the Agent") {
					t.Fatalf("force mode must retain its Agent update hint: %v", err)
				}
			}
		})
	}
}

type fallbackProvisioningClient struct {
	agentpb.WendyProvisioningServiceClient
	calls int
}

func (c *fallbackProvisioningClient) IsProvisioned(context.Context, *agentpb.IsProvisionedRequest, ...grpc.CallOption) (*agentpb.IsProvisionedResponse, error) {
	c.calls++
	return &agentpb.IsProvisionedResponse{Response: &agentpb.IsProvisionedResponse_Provisioned{Provisioned: &agentpb.ProvisionedResponse{}}}, nil
}

func setupCloudRunFixture(t *testing.T) {
	t.Helper()

	oldCommand, oldLookPath := imageBuilderCommandContext, imageBuilderLookPath
	oldGOOS, oldGOARCH := imageBuilderHostGOOS, imageBuilderHostGOARCH
	t.Cleanup(func() {
		imageBuilderCommandContext, imageBuilderLookPath = oldCommand, oldLookPath
		imageBuilderHostGOOS, imageBuilderHostGOARCH = oldGOOS, oldGOARCH
	})
	t.Setenv("WENDY_CONFIG_DIR", t.TempDir())
	t.Setenv("WENDY_AGENT_SOCKET", "")
	imageBuilderHostGOOS = func() string { return "darwin" }
	imageBuilderHostGOARCH = func() string { return "arm64" }
	imageBuilderLookPath = func(string) (string, error) { return "/fixture/container", nil }
	fakeCommand := fakeImageBuilderCommandContext(filepath.Join(t.TempDir(), "builder.log"))
	imageBuilderCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if len(args) >= 2 && args[0] == "image" && args[1] == "save" {
			for i := 2; i+1 < len(args); i++ {
				if args[i] == "-o" {
					writeMinimalOCILayout(t, args[i+1], []byte("fixture-layer"), "application/vnd.oci.image.layer.v1.tar", []byte("fixture-layer"))
					return fakeCommand(ctx, "container", "--version")
				}
			}
		}
		return fakeCommand(ctx, name, args...)
	}
}

type cloudStartDropClient struct {
	*fakeContainerClient
	cause      error
	rpcFailure bool
	started    bool
}

func (c *cloudStartDropClient) PrepareImage(context.Context, *agentpb.RunContainerLayersRequest, ...grpc.CallOption) (*agentpb.PrepareImageResponse, error) {
	return &agentpb.PrepareImageResponse{}, nil
}

func (c *cloudStartDropClient) RunContainer(context.Context, *agentpb.RunContainerLayersRequest, ...grpc.CallOption) (grpc.ServerStreamingClient[agentpb.RunContainerLayersResponse], error) {
	if c.rpcFailure {
		return nil, c.cause
	}
	remaining := 0
	if c.started {
		remaining = 1
	}
	return &deploymentAckStream{remaining: remaining, err: c.cause}, nil
}

func TestCloudRunConnectionDropBeforeStarted(t *testing.T) {
	defer forceBuildProgressInteractive(false)()
	defer setBuildProgressOut(io.Discard)()
	setupCloudRunFixture(t)
	manifestCacheTestDir = t.TempDir()
	t.Cleanup(func() { manifestCacheTestDir = "" })
	for _, tc := range []struct {
		name                string
		rpcFailure, started bool
		code                codes.Code
	}{
		{"RunContainer fails", true, false, codes.Unavailable},
		{"stream fails before Started", false, false, codes.Unavailable},
		{"stream fails after Started", false, true, codes.Unavailable},
		{"cancellation before Started", false, false, codes.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM busybox:latest\n"), 0600); err != nil {
				t.Fatal(err)
			}
			cause := status.Error(tc.code, "private container stream details")
			client := &cloudStartDropClient{fakeContainerClient: &fakeContainerClient{queryFn: func(*agentpb.QueryChunksRequest) *agentpb.QueryChunksResponse { return &agentpb.QueryChunksResponse{} }}, cause: cause, rpcFailure: tc.rpcFailure, started: tc.started}
			registry := &fallbackProvisioningClient{}
			conn := &grpcclient.AgentConnection{Host: "jetson", IsCloudV2: true, ContainerService: client, ProvisioningService: registry}
			conn.CacheAgentVersion(&agentpb.GetAgentVersionResponse{Os: "linux", CpuArchitecture: "arm64"})
			opts := runOptions{builder: imageBuilderAppleContainer, chunking: chunkingAuto, managedRobot: true, yes: true}
			err := runWithAgentRecorded(t.Context(), conn, dir, &appconfig.AppConfig{AppID: "cloud-start-fixture"}, opts, newDeployMetrics(opts))
			if !errors.Is(err, cause) || registry.calls != 0 {
				t.Fatalf("err=%v registry calls=%d", err, registry.calls)
			}
			if tc.started || tc.code == codes.Canceled {
				var cloud *cloudDeployError
				if errors.As(err, &cloud) {
					t.Fatalf("rewrote a post-start failure or cancellation: %v", err)
				}
				return
			}
			if ExecutionErrorClass(err) != "device_unreachable" || !strings.Contains(err.Error(), "The Cloud connection to jetson was lost while starting the app.") || !strings.Contains(err.Error(), cloudUploadRetryHint) || strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "during the upload") {
				t.Fatalf("unactionable/wrong-phase connection loss: %v", err)
			}
		})
	}
}
