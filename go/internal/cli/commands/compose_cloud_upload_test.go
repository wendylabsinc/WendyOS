package commands

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type composePrepareClient struct {
	*fakeContainerClient
	prepareErr          error
	waitForCancellation bool
	dropPrepareCode     codes.Code
}

func (c *composePrepareClient) PrepareImage(ctx context.Context, _ *agentpb.RunContainerLayersRequest, _ ...grpc.CallOption) (*agentpb.PrepareImageResponse, error) {
	if c.waitForCancellation {
		<-ctx.Done()
		return nil, status.Error(c.dropPrepareCode, "preparation failed after upload dropped")
	}
	return nil, c.prepareErr
}

// Exercise the shared Compose/multi-service helper with a real OCI fixture,
// without Docker or a device. Only the export subprocess is replaced.
func TestComposeCloudV2DoesNotFallBackToTheRegistry(t *testing.T) {
	userCache := setupComposeCloudUploadFixture(t)
	for _, tc := range []struct {
		name            string
		code            codes.Code
		drop            bool
		dropPrepareCode codes.Code
	}{
		{"upload drop cancels preparation", codes.Unavailable, true, codes.Canceled},
		{"upload drop makes preparation unavailable", codes.Unavailable, true, codes.Unavailable},
		{"preparation unavailable", codes.Unavailable, false, codes.OK},
		{"preparation refused", codes.PermissionDenied, false, codes.OK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := "cloud-prepare-test-" + sha256Hex([]byte(t.TempDir()))[:16]
			layout := chunkLayoutDir(userCache, repo, "linux/arm64")
			t.Cleanup(func() { os.RemoveAll(layout); os.Remove(layout + ".lock") })
			cause := status.Error(tc.code, "original chunk or preparation cause")
			cs := &composePrepareClient{fakeContainerClient: &fakeContainerClient{}, prepareErr: cause, waitForCancellation: tc.drop, dropPrepareCode: tc.dropPrepareCode}
			cs.queryFn = func(req *agentpb.QueryChunksRequest) *agentpb.QueryChunksResponse {
				if tc.drop {
					return &agentpb.QueryChunksResponse{MissingHashes: req.GetChunkHashes()}
				}
				return &agentpb.QueryChunksResponse{}
			}
			cs.writeFn = func(*agentpb.WriteChunksRequest) error { return cause }
			var registryCalls atomic.Int32
			conn := &grpcclient.AgentConnection{Host: "jetson", IsCloudV2: true, ContainerService: cs, RegistryDialer: func(context.Context, int) (net.Conn, error) {
				registryCalls.Add(1)
				return nil, errors.New("registry must not be reached")
			}}
			conn.Reconnect = func(context.Context) (*grpcclient.AgentConnection, error) {
				t.Fatal("Compose must not acquire a new retry loop")
				return nil, nil
			}
			conn.CacheAgentVersion(&agentpb.GetAgentVersionResponse{Os: "linux", Version: "test"})
			var logs bytes.Buffer
			err := buildAndPrepareComposeImageForAgent(t.Context(), conn, 5000, "linux", imageBuilderDocker, t.TempDir(), repo, "linux/arm64", "", nil, repo, io.Discard, &logs)
			if !errors.Is(err, cause) || registryCalls.Load() != 0 {
				t.Fatalf("cause/fallback: err=%v registry calls=%d", err, registryCalls.Load())
			}
			if !strings.Contains(logs.String(), cause.Error()) {
				t.Fatalf("raw cause was not retained in diagnostics: %s", &logs)
			}
			if strings.Contains(err.Error(), "original chunk") {
				t.Fatalf("raw cause exposed: %v", err)
			}
			if tc.drop {
				if ExecutionErrorClass(err) != "device_unreachable" || !strings.Contains(err.Error(), "registry fallback isn't available") {
					t.Fatalf("drop message/class: %v", err)
				}
			} else {
				if strings.Contains(err.Error(), "registry") || strings.Contains(err.Error(), "lost") {
					t.Fatalf("preparation failure mislabeled: %v", err)
				}
				if tc.code == codes.Unimplemented && !strings.Contains(err.Error(), "Update the Agent") {
					t.Fatalf("missing update hint: %v", err)
				}
			}
		})
	}
}

func TestComposeCloudV2OtherBuildersStopBeforeRegistryUpload(t *testing.T) {
	for _, builder := range []string{imageBuilderAppleContainer, imageBuilderBuildkit} {
		t.Run(builder, func(t *testing.T) {
			conn := &grpcclient.AgentConnection{IsCloudV2: true}
			err := buildAndPrepareComposeImageForAgent(t.Context(), conn, 5000, "linux", builder, t.TempDir(), "app", "linux/arm64", "", nil, "", io.Discard, io.Discard)
			if !errors.Is(err, errBuilderUnavailable) || !strings.Contains(err.Error(), "--builder docker") {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestExplicitDeviceReachabilityOutranksReconnectDeadline(t *testing.T) {
	for _, category := range []error{errDeviceUnreachable, errDeviceOffline} {
		err := classifyCommandError(category, fmt.Errorf("reconnect: %w", context.DeadlineExceeded))
		if got := ExecutionErrorClass(err); got != ErrorClass(err) {
			t.Fatalf("class=%s, want %s", got, ErrorClass(err))
		}
	}
	if got := ExecutionErrorClass(classifyCommandError(errDeviceUnreachable, context.Canceled)); got != "context_canceled" {
		t.Fatalf("cancellation class=%s", got)
	}
}

func setupComposeCloudUploadFixture(t *testing.T) string {
	t.Helper()

	oldEnsure, oldRun := ensureOCIExportBuilderForBuild, runBuildxOCIExportCommand
	t.Cleanup(func() { ensureOCIExportBuilderForBuild, runBuildxOCIExportCommand = oldEnsure, oldRun })
	ensureOCIExportBuilderForBuild = func(context.Context, io.Writer) (string, error) { return "fixture", nil }
	layer := variedChunkTestData(200_000)
	runBuildxOCIExportCommand = func(_ context.Context, _ string, args, _ []string, _, _ io.Writer) error {
		for i := 0; i+1 < len(args); i++ {
			if args[i] == "--output" {
				for _, field := range strings.Split(args[i+1], ",") {
					if dir, ok := strings.CutPrefix(field, "dest="); ok {
						writeOCILayoutDir(t, dir, minimalOCILayoutEntries(t, layer, "application/vnd.oci.image.layer.v1.tar", layer))
						return nil
					}
				}
			}
		}
		return errors.New("fixture export destination missing")
	}
	manifestCacheTestDir = t.TempDir()
	t.Cleanup(func() { manifestCacheTestDir = "" })
	t.Setenv(imageSignaturePathEnv, "")
	t.Setenv("WENDY_CHUNK_COMPRESSION", "gzip")
	userCache, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("WENDY_CONFIG_DIR", t.TempDir())
	return userCache
}

func TestComposeCloudV2MissingChunkCapabilitiesKeepRegistryAuth(t *testing.T) {
	userCache := setupComposeCloudUploadFixture(t)
	for _, stage := range []string{"QueryChunks", "PrepareImage"} {
		for _, force := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/force=%v", stage, force), func(t *testing.T) {
				repo := "cloud-capability-test-" + sha256Hex([]byte(t.TempDir()))[:16]
				layout := chunkLayoutDir(userCache, repo, "linux/arm64")
				t.Cleanup(func() { os.RemoveAll(layout); os.Remove(layout + ".lock") })
				cause := status.Error(codes.Unimplemented, "old Agent capability missing")
				var cs agentpb.WendyContainerServiceClient = &composePrepareClient{
					fakeContainerClient: &fakeContainerClient{queryFn: func(*agentpb.QueryChunksRequest) *agentpb.QueryChunksResponse { return &agentpb.QueryChunksResponse{} }},
					prepareErr:          cause,
				}
				if stage == "QueryChunks" {
					cs = &unavailableContainerClient{err: cause}
				}
				conn := &grpcclient.AgentConnection{Host: "jetson", IsCloudV2: true, IsMTLS: true, ContainerService: cs, RegistryDialer: func(context.Context, int) (net.Conn, error) {
					t.Fatal("registry auth must be checked before opening a tunnel")
					return nil, nil
				}}
				conn.CacheAgentVersion(&agentpb.GetAgentVersionResponse{Os: "linux", Version: "test"})
				var logs bytes.Buffer
				err := buildAndPrepareComposeImage(t.Context(), conn, 5000, "linux", imageBuilderDocker, t.TempDir(), repo, "linux/arm64", "", nil, repo, io.Discard, &logs, !force)
				if force {
					if !errors.Is(err, cause) || errors.Is(err, errRegistryAuth) {
						t.Fatalf("force error=%v, want missing capability without fallback", err)
					}
				} else if !errors.Is(err, errRegistryAuth) || !strings.Contains(logs.String(), "falling back to registry push") {
					t.Fatalf("auto error=%v logs=%s, want existing registry auth checks", err, &logs)
				}
			})
		}
	}
}
