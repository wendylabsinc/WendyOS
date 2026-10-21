package commands

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
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
// network access, letting us assert LAN and v1 still reach it and v2 does not.
func TestRunRegistryFallbackRespectsCloudV2Catalog(t *testing.T) {
	defer forceBuildProgressInteractive(false)()
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
	for _, tc := range []struct {
		name               string
		cloudV2, reconnect bool
	}{
		{name: "LAN"}, {name: "Cloud v1", reconnect: true}, {name: "Cloud v2", cloudV2: true, reconnect: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM busybox:latest\n"), 0600); err != nil {
				t.Fatal(err)
			}
			registry := &fallbackProvisioningClient{}
			conn := &grpcclient.AgentConnection{Host: "fixture", IsCloudV2: tc.cloudV2, ContainerService: &probeUnsupportedClient{}, ProvisioningService: registry}
			conn.CacheAgentVersion(&agentpb.GetAgentVersionResponse{Os: "linux", CpuArchitecture: "arm64"})
			if tc.reconnect {
				conn.Reconnect = func(context.Context) (*grpcclient.AgentConnection, error) {
					t.Fatal("Unimplemented must not reconnect")
					return nil, nil
				}
			}
			cfg := &appconfig.AppConfig{AppID: "fallback-fixture"}
			opts := runOptions{builder: imageBuilderAppleContainer, chunking: chunkingAuto, managedRobot: true, yes: true}
			err := runWithAgentRecorded(t.Context(), conn, dir, cfg, opts, newDeployMetrics(opts))
			if tc.cloudV2 {
				if err == nil || !isUnimplementedRPCError(err) || !strings.Contains(err.Error(), "Update the Agent, then retry.") || strings.Contains(err.Error(), "registry") {
					t.Fatalf("error = %v, want chunk error with unsupported-registry explanation", err)
				}
				if registry.calls != 0 {
					t.Fatalf("v2 attempted the registry fallback %d times", registry.calls)
				}
			} else {
				if registry.calls != 1 || !errors.Is(err, errRegistryAuth) {
					t.Fatalf("registry calls=%d err=%v, want the registry auth gate", registry.calls, err)
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
