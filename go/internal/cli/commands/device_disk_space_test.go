package commands

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/registry"
	"google.golang.org/grpc"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

const testDiskTotal int64 = 32 << 30

// partitionWithFree describes a 32 GiB filesystem at mount with free bytes left.
func partitionWithFree(mount string, free int64) *agentpb.DiskPartition {
	return &agentpb.DiskPartition{Mountpoint: mount, UsedBytes: testDiskTotal - free, TotalBytes: testDiskTotal}
}

func TestDeviceDiskRefusesANearlyFullContainerStorage(t *testing.T) {
	err := checkDeviceDiskSpace(&agentpb.GetAgentVersionResponse{ContainerStorage: partitionWithFree("/data", 300<<20)})
	if ErrorClass(err) != "disk_full" {
		t.Fatalf("err = %v (class %q), want a disk_full refusal", err, ErrorClass(err))
	}
	if msg := err.Error(); !strings.Contains(msg, "300 MiB free") || !strings.Contains(msg, "/data") {
		t.Fatalf("message does not name the device disk and its free space: %q", msg)
	}
	if steps := strings.Join(NextSteps(err), "\n"); !strings.Contains(steps, "wendy device cache prune") || !strings.Contains(steps, "wendy device apps remove") {
		t.Fatalf("next steps = %q, want cache prune and app removal", steps)
	}
}

func TestDeviceDiskSpaceChoosesTheDiskThatHoldsImages(t *testing.T) {
	for _, tc := range []struct {
		name       string
		resp       *agentpb.GetAgentVersionResponse
		wantRefuse string // the mountpoint a refusal names; "" for no refusal
	}{
		{
			name: "container storage decides over a full root disk",
			resp: &agentpb.GetAgentVersionResponse{
				ContainerStorage: partitionWithFree("/data", 10<<30),
				Partitions:       []*agentpb.DiskPartition{partitionWithFree("/", 100<<20)},
			},
		},
		// An agent that does not report its container storage gives no reliable
		// answer: on WendyOS images live on /data, and the root slot can be
		// nearly full by design. This is a simulator's real layout.
		{
			name: "no container storage reported",
			resp: &agentpb.GetAgentVersionResponse{Partitions: []*agentpb.DiskPartition{
				partitionWithFree("/", 100<<20), partitionWithFree("/boot", 10<<20), partitionWithFree("/data", 4<<30),
			}},
		},
		{
			name: "legacy root fields only",
			resp: func() *agentpb.GetAgentVersionResponse {
				used, total := testDiskTotal-100<<20, testDiskTotal
				return &agentpb.GetAgentVersionResponse{DiskUsedBytes: &used, DiskTotalBytes: &total}
			}(),
		},
		{
			name: "plenty free",
			resp: &agentpb.GetAgentVersionResponse{ContainerStorage: partitionWithFree("/data", 2<<30)},
		},
		{
			name: "nothing reported",
			resp: &agentpb.GetAgentVersionResponse{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkDeviceDiskSpace(tc.resp)
			if tc.wantRefuse == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if ErrorClass(err) != "disk_full" || !strings.Contains(err.Error(), "free on "+tc.wantRefuse+",") {
				t.Fatalf("err = %v, want a refusal naming %s", err, tc.wantRefuse)
			}
		})
	}
}

func TestEveryDiskSpaceRefusalStartsWithThePhraseMCPMatches(t *testing.T) {
	fakeBuildDisks(t, map[string]int64{"/cache": 1 << 30})
	for name, err := range map[string]error{
		"local":  checkLocalBuildSpace([]string{"/cache"}, nil),
		"device": checkDeviceDiskSpace(&agentpb.GetAgentVersionResponse{ContainerStorage: partitionWithFree("/data", 100<<20)}),
		"image":  deviceImageSpaceError(deviceStorage{free: 1 << 20, mount: "/data"}, 2<<20),
	} {
		// The MCP run tool recognizes these refusals by this phrase
		// (runRefusals in package mcp).
		if err == nil || !strings.HasPrefix(err.Error(), "not enough free disk space") {
			t.Errorf("%s refusal = %v; want it to start with the phrase the MCP run tool matches", name, err)
		}
	}
}

func pushOneLayer(t *testing.T, ctx context.Context, fake *fakeContainerClient, layer localLayer) error {
	t.Helper()
	manifestCacheTestDir = t.TempDir()
	t.Cleanup(func() { manifestCacheTestDir = "" })
	_, err := pushLayersByChunksWithPrepareMode(ctx, fake, []localLayer{layer}, nil, nil, false, newChunkPushProgress(), gzipChunkUploadConfig)
	return err
}

func allChunksMissing() *fakeContainerClient {
	return &fakeContainerClient{queryFn: func(req *agentpb.QueryChunksRequest) *agentpb.QueryChunksResponse {
		return &agentpb.QueryChunksResponse{MissingHashes: req.GetChunkHashes()}
	}}
}

func testLayer(data []byte) localLayer {
	return localLayer{Digest: "sha256:" + sha256Hex(data), DiffID: "sha256:" + sha256Hex(data), MediaType: "application/vnd.oci.image.layer.v1.tar", Blob: data}
}

func deviceWithFree(free int64) *agentpb.GetAgentVersionResponse {
	return &agentpb.GetAgentVersionResponse{ContainerStorage: partitionWithFree("/data", free)}
}

func TestChunkPushRefusesLayersTheDeviceCannotHold(t *testing.T) {
	ctx, cancel := context.WithTimeout(withDeviceStorage(context.Background(), deviceWithFree(100_000)), 30*time.Second)
	defer cancel()
	fake := allChunksMissing()

	err := pushOneLayer(t, ctx, fake, testLayer(variedChunkTestData(300_000)))
	if ErrorClass(err) != "disk_full" || !strings.Contains(err.Error(), "/data") {
		t.Fatalf("err = %v (class %q), want a disk_full refusal naming /data", err, ErrorClass(err))
	}
	if n := fake.chunksWrittenLocked(); n != 0 {
		t.Fatalf("sent %d chunk(s) before refusing", n)
	}
}

// Only the layers the device lacks take space; a layer it already holds is
// reused in place.
func TestChunkPushCountsOnlyLayersTheDeviceLacks(t *testing.T) {
	ctx, cancel := context.WithTimeout(withDeviceStorage(context.Background(), deviceWithFree(1)), 30*time.Second)
	defer cancel()
	layer := testLayer(variedChunkTestData(300_000))
	fake := allChunksMissing()
	fake.queryLayersFn = func(*agentpb.QueryLayersRequest) *agentpb.QueryLayersResponse {
		return &agentpb.QueryLayersResponse{Present: []*agentpb.PresentLayer{{DiffId: layer.DiffID, Size: 300_000}}}
	}
	if err := pushOneLayer(t, ctx, fake, layer); err != nil {
		t.Fatalf("refused a deploy whose layers are all on the device: %v", err)
	}
}

func TestChunkPushWithoutDeviceDiskDataDoesNotCheck(t *testing.T) {
	ctx, cancel := context.WithTimeout(withDeviceStorage(context.Background(), &agentpb.GetAgentVersionResponse{}), 30*time.Second)
	defer cancel()
	if err := pushOneLayer(t, ctx, allChunksMissing(), testLayer(variedChunkTestData(300_000))); err != nil {
		t.Fatalf("an agent that reports no disk usage blocked the push: %v", err)
	}
}

// The compressed layers fit, but unpacked they may not: the push goes ahead
// and says so once the progress display is done.
func TestChunkPushWarnsWhenUnpackedLayersMayNotFit(t *testing.T) {
	ctx, cancel := context.WithTimeout(withDeviceStorage(context.Background(), deviceWithFree(600_000)), 30*time.Second)
	defer cancel()
	manifestCacheTestDir = t.TempDir()
	t.Cleanup(func() { manifestCacheTestDir = "" })

	var err error
	stderr := captureStderr(t, func() {
		_, err = pushLayersWithProgress(ctx, allChunksMissing(), []localLayer{testLayer(variedChunkTestData(300_000))}, nil, gzipChunkUploadConfig, nil)
	})
	if err != nil {
		t.Fatalf("refused layers that fit compressed: %v", err)
	}
	if !strings.Contains(stderr, "Warning:") || !strings.Contains(stderr, "/data") {
		t.Fatalf("no warning naming the device disk:\n%s", stderr)
	}
}

func TestRunRefusesWhenTheDeviceDiskIsNearlyFull(t *testing.T) {
	isolateFingerprintCache(t)
	fakeAllBuildDisks(t, 100<<30)
	failDockerUse(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &appconfig.AppConfig{AppID: "full-device", Platform: "linux"}
	opts := runOptions{detach: true}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn := &grpcclient.AgentConnection{ContainerService: &fastPathContainerClient{appName: cfg.AppID, state: agentpb.AppRunningState_STOPPED}}
	version := deviceWithFree(100 << 20)
	version.Os, version.CpuArchitecture = "linux", "arm64"
	conn.CacheAgentVersion(version)

	err := runWithAgentRecorded(ctx, conn, dir, cfg, opts, newDeployMetrics(opts))
	if ErrorClass(err) != "disk_full" || !strings.Contains(err.Error(), "/data") {
		t.Fatalf("err = %v (class %q), want a disk_full refusal naming /data", err, ErrorClass(err))
	}
}

// A run whose image is already on the device sends nothing, so a full device
// disk must not stop it.
func TestRunFastPathIgnoresAFullDeviceDisk(t *testing.T) {
	isolateFingerprintCache(t)
	fakeAllBuildDisks(t, 100<<30)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &appconfig.AppConfig{AppID: "full-device-fastpath", Platform: "linux"}
	opts := runOptions{detach: true}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	version := deviceWithFree(100 << 20)
	version.Os, version.CpuArchitecture = "linux", "arm64"
	inputs, err := singleServiceDesiredHash(dir, "", "linux/arm64", resolvedStagefileBackend(ctx),
		map[string]string{"WENDY_PLATFORM": "generic", "WENDY_DEBUG": "false", "WENDY_HAS_CUDA": "false"}, mergeEnvEntries(resolveServiceEnv(cfg), opts.env), cfg, opts)
	if err != nil {
		t.Fatal(err)
	}
	saveDeployFingerprint(cfg.AppID, deviceFingerprintKey(version), deployFingerprint{InputHash: inputs, LayerDiffIDs: []string{"layer"}})
	conn := &grpcclient.AgentConnection{ContainerService: &fastPathContainerClient{appName: cfg.AppID, state: agentpb.AppRunningState_STOPPED, presentLayers: map[string]bool{"layer": true}}}
	conn.CacheAgentVersion(version)

	if err := runWithAgentRecorded(ctx, conn, dir, cfg, opts, newDeployMetrics(opts)); err != nil {
		t.Fatalf("fast path refused: %v", err)
	}
}

// A build host delivers the image to the device, so its space counts there too.
func TestRunWithBuildHostChecksTheDeviceDisk(t *testing.T) {
	isolateFingerprintCache(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &appconfig.AppConfig{AppID: "full-device-buildhost", Platform: "linux"}
	opts := runOptions{detach: true, buildHost: "127.0.0.1:1"}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn := &grpcclient.AgentConnection{ContainerService: &fastPathContainerClient{appName: cfg.AppID, state: agentpb.AppRunningState_STOPPED}}
	version := deviceWithFree(100 << 20)
	version.Os, version.CpuArchitecture = "linux", "arm64"
	conn.CacheAgentVersion(version)

	if err := runWithAgentRecorded(ctx, conn, dir, cfg, opts, newDeployMetrics(opts)); ErrorClass(err) != "disk_full" {
		t.Fatalf("err = %v (class %q), want a disk_full refusal before contacting the build host", err, ErrorClass(err))
	}
}

// After a failed fast deploy, a registry push needs at least the same space
// on the device and the same Buildx plugin, so those failures end the run.
func TestChunkFailureRepeatsOnRegistryPush(t *testing.T) {
	origGOOS, origGOARCH := imageBuilderHostGOOS, imageBuilderHostGOARCH
	t.Cleanup(func() { imageBuilderHostGOOS, imageBuilderHostGOARCH = origGOOS, origGOARCH })
	imageBuilderHostGOOS = func() string { return "darwin" }
	imageBuilderHostGOARCH = func() string { return "arm64" }

	space := deviceImageSpaceError(deviceStorage{free: 1 << 20, mount: "/data"}, 2<<20)
	buildx := buildxMissingTestError()
	for _, tc := range []struct {
		name    string
		err     error
		builder string
		want    bool
	}{
		{"device space, default builder", classifyCommandError(errTransferFailed, space), "", true},
		{"missing Buildx, docker builder", buildx, "docker", true},
		{"missing Buildx, Apple Container first", buildx, "", false},
		{"other failure", errors.New("chunk upload failed"), "docker", false},
	} {
		if got := chunkFailureRepeatsOnRegistryPush(tc.err, tc.builder); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

func buildxMissingTestError() error {
	return commandErrorf(errBuilderUnavailable, "%w; wendy builds images with it", errBuildxMissing)
}

// fakeDockerBuildingLayout puts a docker on PATH whose OCI export writes a
// real layout with one layer, so a prepare runs its true push and fallback.
func fakeDockerBuildingLayout(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("stub docker is a POSIX shell script")
	}
	layout := t.TempDir()
	layer := bytes.Repeat([]byte("full-device-layer-"), 4096)
	writeOCILayoutDir(t, layout, minimalOCILayoutEntries(t, layer, "application/vnd.oci.image.layer.v1.tar", layer))
	script := `#!/bin/sh
for a in "$@"; do
	case "$a" in
	*dest=*) d=${a#*dest=}; d=${d%%,*}; mkdir -p "$d" && cp -R "$FAKE_OCI_LAYOUT"/. "$d"/ ;;
	esac
done
`
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_OCI_LAYOUT", layout)
	t.Setenv("DOCKER_CONFIG", t.TempDir())
}

// Compose and multi-service builds prepare their images through their own
// chunk path, whose registry fallback must stop at a disk refusal too: the
// push would write at least as much to the same disk.
func TestComposePrepareRefusalSkipsTheRegistryFallback(t *testing.T) {
	isolateFingerprintCache(t)
	manifestCacheTestDir = t.TempDir()
	t.Cleanup(func() { manifestCacheTestDir = "" })
	fakeDockerBuildingLayout(t)

	var uploads atomic.Int64
	reg := registry.New()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			uploads.Add(1)
		}
		reg.ServeHTTP(w, r)
	}))
	defer srv.Close()
	host, portText, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	conn := &grpcclient.AgentConnection{ContainerService: allChunksMissing(), Host: host}
	conn.CacheAgentVersion(&agentpb.GetAgentVersionResponse{Os: "linux", CpuArchitecture: "arm64"})
	ctx, cancel := context.WithTimeout(withDeviceStorage(context.Background(), deviceWithFree(1)), 30*time.Second)
	defer cancel()

	err = buildAndPrepareComposeImageForAgent(ctx, conn, port, "linux", "docker", dir, "proj-web", "linux/arm64", "Dockerfile", nil, "proj-web", io.Discard, io.Discard)
	if ErrorClass(err) != "disk_full" || !strings.Contains(err.Error(), "/data") {
		t.Errorf("err = %v (class %q), want a disk_full refusal naming /data", err, ErrorClass(err))
	}
	if n := uploads.Load(); n != 0 {
		t.Errorf("sent %d write request(s) to the registry of a device with no room", n)
	}
}

// composeLayersOnDevice reports every layer as present, so an unchanged
// Compose service qualifies to skip its build.
type composeLayersOnDevice struct {
	agentpb.WendyContainerServiceClient
}

func (c composeLayersOnDevice) QueryLayers(_ context.Context, req *agentpb.QueryLayersRequest, _ ...grpc.CallOption) (*agentpb.QueryLayersResponse, error) {
	resp := &agentpb.QueryLayersResponse{}
	for _, id := range req.GetDiffIds() {
		resp.Present = append(resp.Present, &agentpb.PresentLayer{DiffId: id, Size: 1})
	}
	return resp, nil
}

func cacheLinuxDeviceWithFree(conn *grpcclient.AgentConnection, free int64) {
	version := deviceWithFree(free)
	version.Os, version.CpuArchitecture = "linux", "arm64"
	conn.CacheAgentVersion(version)
}

// startComposeBuildProject writes a one-service Compose project that builds,
// with a stub build that records its calls and the device storage it was given.
func startComposeBuildProject(t *testing.T) (conn *grpcclient.AgentConnection, dir string, builds *[]deviceStorage) {
	t.Helper()
	isolateFingerprintCache(t)
	dir = t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("services:\n  web:\n    build: .\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\nCOPY compose.yaml /\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	builds = &[]deviceStorage{}
	orig := buildComposeServiceImage
	t.Cleanup(func() { buildComposeServiceImage = orig })
	buildComposeServiceImage = func(ctx context.Context, _ *grpcclient.AgentConnection, _ int, _, _, _, _, _, _ string, _ map[string]string, _ string, streamOutput, _ io.Writer) error {
		storage, _ := deviceStorageFor(ctx)
		*builds = append(*builds, storage)
		if reporter, ok := streamOutput.(interface{ ReportImageContent([]string) }); ok {
			reporter.ReportImageContent([]string{"sha256:" + strings.Repeat("a", 64)})
		}
		return nil
	}
	conn, cleanup := startFakeMacRunServer(t, &fakeMacRunState{})
	t.Cleanup(cleanup)
	conn.ContainerService = composeLayersOnDevice{conn.ContainerService}
	return conn, dir, builds
}

// An unchanged Compose project builds and sends nothing, so neither a full
// device disk nor a full local disk may stop it.
func TestComposeWithNothingToBuildIgnoresFullDisks(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		localFree, freeOnDevice int64
	}{
		{"full device", 100 << 30, 100 << 20},
		{"full local disk", 1 << 20, 100 << 30},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeAllBuildDisks(t, 100<<30)
			conn, dir, builds := startComposeBuildProject(t)
			cacheLinuxDeviceWithFree(conn, 100<<30)
			if err := runComposeWithAgent(context.Background(), conn, dir, runOptions{deploy: true}); err != nil {
				t.Fatalf("first run: %v", err)
			}
			if len(*builds) != 1 {
				t.Fatalf("first run built %d time(s), want 1", len(*builds))
			}

			fakeAllBuildDisks(t, tc.localFree)
			cacheLinuxDeviceWithFree(conn, tc.freeOnDevice)
			if err := runComposeWithAgent(context.Background(), conn, dir, runOptions{deploy: true}); err != nil {
				t.Fatalf("refused a run with nothing to build: %v", err)
			}
			if len(*builds) != 1 {
				t.Fatalf("second run built again (%d builds), so it did not exercise the skip", len(*builds))
			}
		})
	}
}

// An image-only project has no build at all, so this computer's disk is
// irrelevant, and the CLI cannot tell whether the device must pull anything.
func TestComposeImageOnlyIgnoresFullDisks(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		localFree, freeOnDevice int64
	}{
		{"full device", 100 << 30, 100 << 20},
		{"full local disk", 1 << 20, 100 << 30},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateFingerprintCache(t)
			fakeAllBuildDisks(t, tc.localFree)
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("services:\n  web:\n    image: docker.io/library/nginx:latest\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			state := &fakeMacRunState{}
			conn, cleanup := startFakeMacRunServer(t, state)
			defer cleanup()
			cacheLinuxDeviceWithFree(conn, tc.freeOnDevice)

			if err := runComposeWithAgent(context.Background(), conn, dir, runOptions{deploy: true}); err != nil {
				t.Fatalf("refused a project with no build: %v", err)
			}
			if len(state.createReqs) != 1 {
				t.Fatalf("CreateContainer calls = %d, want 1", len(state.createReqs))
			}
		})
	}
}

// A Compose service that does need a build is still refused before it starts.
func TestComposeWithABuildRefusesFullDisks(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		localFree, freeOnDevice int64
		wantMessage             string
	}{
		{"full device", 100 << 30, 100 << 20, "on the device"},
		{"full local disk", 1 << 20, 100 << 30, "to build"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeAllBuildDisks(t, tc.localFree)
			conn, dir, builds := startComposeBuildProject(t)
			cacheLinuxDeviceWithFree(conn, tc.freeOnDevice)

			err := runComposeWithAgent(context.Background(), conn, dir, runOptions{deploy: true})
			if ErrorClass(err) != "disk_full" || !strings.Contains(err.Error(), tc.wantMessage) {
				t.Fatalf("err = %v (class %q), want a disk_full refusal saying %q", err, ErrorClass(err), tc.wantMessage)
			}
			if len(*builds) != 0 {
				t.Fatalf("built %d time(s) before refusing", len(*builds))
			}
		})
	}
}

// The push compares the layers the device lacks with its free space, so a
// build that goes ahead must carry that free space.
func TestComposeBuildCarriesTheDeviceStorage(t *testing.T) {
	fakeAllBuildDisks(t, 100<<30)
	conn, dir, builds := startComposeBuildProject(t)
	cacheLinuxDeviceWithFree(conn, 2<<30)

	if err := runComposeWithAgent(context.Background(), conn, dir, runOptions{deploy: true}); err != nil {
		t.Fatalf("runComposeWithAgent: %v", err)
	}
	if len(*builds) != 1 || (*builds)[0].free != 2<<30 || (*builds)[0].mount != "/data" {
		t.Fatalf("builds saw device storage %+v, want one build with 2 GiB free on /data", *builds)
	}
}
