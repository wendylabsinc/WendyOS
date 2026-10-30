package commands

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
