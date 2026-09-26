package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/cli/analytics"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// captureDeployEvents records every deploy_completed event for the test.
func captureDeployEvents(t *testing.T) *[]map[string]string {
	t.Helper()
	var events []map[string]string
	analytics.SetTrackHookForTesting(func(event string, props map[string]string) {
		if event == "deploy_completed" {
			events = append(events, props)
		}
	})
	t.Cleanup(func() { analytics.SetTrackHookForTesting(nil) })
	return &events
}

// chunkPushKeys are the properties that describe a completed chunk push.
var chunkPushKeys = []string{
	"deploy_push_ms", "deploy_upload_ms", "deploy_device_prepare_ms", "deploy_bytes_sent",
	"deploy_chunks_sent", "deploy_chunks_total", "deploy_layers_total", "deploy_layers_reused",
	"deploy_compression",
}

func TestDeployMetricsPropertiesForAChunkDeploy(t *testing.T) {
	began := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	m := &deployMetrics{
		began:          began,
		started:        true,
		startedAt:      began.Add(5 * time.Second),
		command:        "wendy run",
		transport:      "chunk",
		targetPlatform: "linux/arm64",
		deviceType:     "jetson-orin-nano-devkit",
		chunk: &chunkDeployStats{
			imageBytes:    900_000_000,
			buildTime:     1500 * time.Millisecond,
			pushTime:      9 * time.Second,
			pushCompleted: true,
			startTime:     700 * time.Millisecond,
			push: chunkPushSnapshot{
				SentChunks: 999, SentBytes: 62_900_000, TotalChunks: 5216,
				LayersTotal: 3, LayersReused: 2,
				Uploaded: true, UploadTime: 1800 * time.Millisecond,
				Prepared: true, DeviceTime: 40500 * time.Millisecond,
			},
		},
	}
	p := m.properties(nil)
	want := map[string]string{
		"command_name":             "wendy run",
		"command_root":             "run",
		"duration_ms":              "5000",
		"success":                  "true",
		"deploy_started":           "true",
		"deploy_transport":         "chunk",
		"deploy_target_platform":   "linux/arm64",
		"deploy_device_type":       "jetson-orin-nano-devkit",
		"deploy_build_ms":          "1500",
		"deploy_push_ms":           "9000",
		"deploy_upload_ms":         "1800",
		"deploy_device_prepare_ms": "40500",
		"deploy_start_ms":          "700",
		"deploy_image_bytes":       "900000000",
		"deploy_bytes_sent":        "62900000",
		"deploy_chunks_sent":       "999",
		"deploy_layers_reused":     "2",
	}
	for k, v := range want {
		if p[k] != v {
			t.Errorf("%s = %q, want %q", k, p[k], v)
		}
	}
	for _, k := range []string{"deploy_fallback", "error_class"} {
		if v, ok := p[k]; ok {
			t.Errorf("%s = %q, want absent for a successful chunk deploy", k, v)
		}
	}
}

// TestDeployMetricsEndsAtTheStartedAcknowledgement: once the agent confirms
// the start, the deploy is done. Watching logs afterwards and a later failure
// of the log stream (Ctrl-C, an app crash, a tunnel drop) must neither
// lengthen the deploy nor turn it into a failure.
func TestDeployMetricsEndsAtTheStartedAcknowledgement(t *testing.T) {
	clock := &steppedClock{t: time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)}
	m := &deployMetrics{now: clock.now, began: clock.t, command: "wendy run", transport: "chunk"}
	clock.advance(12 * time.Second) // build, push, start
	m.markStarted()
	clock.advance(time.Second)
	m.markStarted() // a repeated acknowledgement must not move the mark
	clock.advance(10 * time.Minute)

	p := m.properties(fmt.Errorf("receiving container output: %w", status.Error(codes.Unavailable, "tunnel dropped")))
	want := map[string]string{"success": "true", "deploy_started": "true", "duration_ms": "12000"}
	for k, v := range want {
		if p[k] != v {
			t.Errorf("%s = %q, want %q", k, p[k], v)
		}
	}
	if v, ok := p["error_class"]; ok {
		t.Errorf("error_class = %q; a started deploy did not fail", v)
	}
}

// TestDeployMetricsClassifiesAFailureBeforeStart: a deploy that fails before
// the start reports the whole time it took and a bounded error class, never
// the error text (which here names a host, an IP and a path).
func TestDeployMetricsClassifiesAFailureBeforeStart(t *testing.T) {
	events := captureDeployEvents(t)
	clock := &steppedClock{t: time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)}
	m := &deployMetrics{now: clock.now, began: clock.t, command: "wendy run", transport: "chunk", chunk: &chunkDeployStats{}}
	clock.advance(3 * time.Second)
	m.emit(fmt.Errorf("chunk-diff deploy failed and --chunking=force disables the registry-push fallback: %w",
		status.Error(codes.Unimplemented, "jetson.local (10.0.0.7) has no /var/lib/wendy/chunks")))

	if len(*events) != 1 {
		t.Fatalf("events = %v, want one", *events)
	}
	p := (*events)[0]
	want := map[string]string{"success": "false", "deploy_started": "false", "duration_ms": "3000", "error_class": "grpc_unimplemented"}
	for k, v := range want {
		if p[k] != v {
			t.Errorf("%s = %q, want %q", k, p[k], v)
		}
	}
	for k, v := range p {
		for _, leak := range []string{"jetson", "10.0.0.7", "/var/lib"} {
			if strings.Contains(v, leak) {
				t.Errorf("property %s leaks error text: %q", k, v)
			}
		}
	}
}

// TestDeployMetricsOmitsPhasesThatDidNotComplete: a phase's keys appear only
// once that phase completed, so a "0" never stands in for "did not happen".
func TestDeployMetricsOmitsPhasesThatDidNotComplete(t *testing.T) {
	cases := []struct {
		name            string
		stats           chunkDeployStats
		present, absent []string
	}{
		{
			name: "push failed after the build",
			stats: chunkDeployStats{
				imageBytes: 900_000_000, buildTime: 1500 * time.Millisecond, pushTime: 4 * time.Second,
				push: chunkPushSnapshot{SentChunks: 12, SentBytes: 700_000, TotalChunks: 5216, LayersTotal: 3},
			},
			present: []string{"deploy_build_ms", "deploy_image_bytes"},
			absent:  slices.Concat(chunkPushKeys, []string{"deploy_start_ms"}),
		},
		{
			name:   "failed before the build ran",
			stats:  chunkDeployStats{},
			absent: slices.Concat(chunkPushKeys, []string{"deploy_build_ms", "deploy_image_bytes", "deploy_start_ms"}),
		},
		{
			name: "push completed but PrepareImage failed",
			stats: chunkDeployStats{
				imageBytes: 900_000_000, buildTime: 1500 * time.Millisecond, pushTime: 2 * time.Second, pushCompleted: true,
				push: chunkPushSnapshot{SentChunks: 12, SentBytes: 700_000, TotalChunks: 5216, LayersTotal: 3, Uploaded: true, UploadTime: 900 * time.Millisecond},
			},
			present: []string{"deploy_push_ms", "deploy_upload_ms", "deploy_bytes_sent"},
			absent:  []string{"deploy_device_prepare_ms", "deploy_start_ms"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &deployMetrics{command: "wendy run", transport: "chunk", chunk: &tc.stats}
			p := m.properties(status.Error(codes.Unavailable, "tunnel dropped"))
			for _, k := range tc.present {
				if _, ok := p[k]; !ok {
					t.Errorf("%s missing from %v", k, p)
				}
			}
			for _, k := range tc.absent {
				if v, ok := p[k]; ok {
					t.Errorf("%s = %q, want absent: that phase did not complete", k, v)
				}
			}
		})
	}
}

func TestDeployMetricsEmitsOnlyOnceADeployPathWasChosen(t *testing.T) {
	var events []map[string]string
	analytics.SetTrackHookForTesting(func(event string, props map[string]string) {
		if event == "deploy_completed" {
			events = append(events, props)
		}
	})
	t.Cleanup(func() { analytics.SetTrackHookForTesting(nil) })

	(&deployMetrics{command: "wendy run"}).emit(errors.New("no Dockerfile"))
	if len(events) != 0 {
		t.Fatalf("emitted %d events before any deploy path ran", len(events))
	}
	(&deployMetrics{command: "wendy watch", transport: "registry", fallback: "unimplemented"}).emit(errors.New("boom"))
	if len(events) != 1 || events[0]["success"] != "false" || events[0]["deploy_fallback"] != "unimplemented" || events[0]["command_root"] != "watch" {
		t.Fatalf("events = %v", events)
	}
	for k, v := range events[0] {
		if strings.Contains(v, "boom") {
			t.Fatalf("property %s leaks error text: %q", k, v)
		}
	}
}

// TestDeployMetricsSkipsManagedRobotProvisioning: provisioning a managed
// robot reuses the deploy pipeline for an internal build, which is not a
// `wendy run` and must not be reported as one.
func TestDeployMetricsSkipsManagedRobotProvisioning(t *testing.T) {
	events := captureDeployEvents(t)

	robot := newDeployMetrics(runOptions{managedRobot: true})
	robot.transport = "chunk"
	robot.emit(nil)
	if len(*events) != 0 {
		t.Fatalf("managed-robot provisioning emitted %v", *events)
	}

	user := newDeployMetrics(runOptions{})
	user.transport = "chunk"
	user.emit(nil)
	if len(*events) != 1 || (*events)[0]["command_name"] != "wendy run" {
		t.Fatalf("events = %v, want one wendy run deploy", *events)
	}
}

// TestDeployMetricsPropertiesForARegistryFallback: a fallback event keeps
// what the abandoned chunk-diff attempt cost and the image size, reports the
// registry build+push that produced the deploy, and carries none of the
// attempt's chunk-push phases, even for an attempt whose push completed.
func TestDeployMetricsPropertiesForARegistryFallback(t *testing.T) {
	m := &deployMetrics{command: "wendy run", transport: "chunk", chunk: &chunkDeployStats{
		imageBytes: 900_000_000, buildTime: 20 * time.Second, pushTime: 3 * time.Second, pushCompleted: true,
		push: chunkPushSnapshot{SentChunks: 10, SentBytes: 600_000, TotalChunks: 100, LayersTotal: 3, Uploaded: true, UploadTime: time.Second, Prepared: true},
	}}
	m.fellBack(status.Error(codes.Internal, "assembly failed"), 23500*time.Millisecond)
	m.buildPushTime = 41 * time.Second // the registry build+push that follows
	m.markStarted()

	p := m.properties(nil)
	want := map[string]string{
		"deploy_transport":        "registry",
		"deploy_fallback":         "other",
		"deploy_build_push_ms":    "41000",
		"deploy_image_bytes":      "900000000",
		"deploy_chunk_attempt_ms": "23500",
		"deploy_started":          "true",
	}
	for k, v := range want {
		if p[k] != v {
			t.Errorf("%s = %q, want %q", k, p[k], v)
		}
	}
	for _, k := range slices.Concat(chunkPushKeys, []string{"deploy_build_ms", "deploy_start_ms"}) {
		if v, ok := p[k]; ok {
			t.Errorf("property %s = %q, want absent after a fallback", k, v)
		}
	}
}

// TestChunkSkipReason: a deploy that went straight to the registry push says
// why chunk-diff was not attempted.
func TestChunkSkipReason(t *testing.T) {
	cases := []struct {
		name          string
		isDarwinAgent bool
		opts          runOptions
		want          string
	}{
		{"darwin agent", true, runOptions{deploy: true, chunking: chunkingOff}, "darwin_agent"},
		{"deploy only", false, runOptions{deploy: true, chunking: chunkingOff}, "deploy_only"},
		{"chunking off", false, runOptions{chunking: chunkingOff}, "chunking_off"},
		{"none applies", false, runOptions{}, "not_attempted"},
	}
	for _, tc := range cases {
		if got := chunkSkipReason(tc.isDarwinAgent, tc.opts); got != tc.want {
			t.Errorf("%s: chunkSkipReason = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestChunkFallbackReason(t *testing.T) {
	cases := map[string]error{
		"unimplemented": status.Error(codes.Unimplemented, "old agent"),
		"transport":     status.Error(codes.Unavailable, "tunnel dropped"),
		"other":         status.Error(codes.Internal, "assembly failed"),
	}
	for want, err := range cases {
		if got := chunkFallbackReason(err); got != want {
			t.Errorf("chunkFallbackReason(%v) = %q, want %q", err, got, want)
		}
	}
	if got := chunkFallbackReason(io.ErrUnexpectedEOF); got != "transport" {
		t.Errorf("an EOF-shaped stream death is a transport failure, got %q", got)
	}
}

// startAckClient serves CreateContainer and answers StartContainer with the
// embedded client's stream; AttachContainer reports Unimplemented, like an
// agent without stdin support, so attached runs use StartContainer too.
type startAckClient struct {
	foregroundFastPathClient
}

func (c *startAckClient) CreateContainer(context.Context, *agentpb.CreateContainerRequest, ...grpc.CallOption) (*agentpb.CreateContainerResponse, error) {
	return &agentpb.CreateContainerResponse{}, nil
}

// TestDeployStartSitesReportTheStartedAcknowledgement: the registry path's
// start (startExistingContainer) and the native macOS start
// (runMacOSNativeContainer) report the agent's Started acknowledgement to the
// deploy recorder in every lifecycle mode, including an attached run whose
// log stream later fails, and never report a start the agent did not confirm.
func TestDeployStartSitesReportTheStartedAcknowledgement(t *testing.T) {
	startSites := map[string]func(context.Context, *grpcclient.AgentConnection, *appconfig.AppConfig, runOptions) error{
		"startExistingContainer": startExistingContainer,
		"runMacOSNativeContainer": func(ctx context.Context, conn *grpcclient.AgentConnection, appCfg *appconfig.AppConfig, opts runOptions) error {
			return runMacOSNativeContainer(ctx, conn, appCfg, &agentpb.CreateContainerRequest{AppName: appCfg.AppID}, opts)
		},
	}
	cases := []struct {
		name       string
		opts       runOptions
		stream     *deploymentAckStream
		wantStarts int
		wantErr    bool
	}{
		{"detached", runOptions{detach: true}, &deploymentAckStream{remaining: 1, err: io.EOF}, 1, false},
		{"detached, stream ends unconfirmed", runOptions{detach: true}, &deploymentAckStream{err: io.EOF}, 0, false},
		{"watch", runOptions{watchState: &watchDeployState{}}, &deploymentAckStream{remaining: 1, err: io.EOF}, 1, false},
		{"attached, logs later fail", runOptions{}, &deploymentAckStream{remaining: 1, err: errors.New("tunnel dropped")}, 1, true},
		{"attached, start fails", runOptions{}, &deploymentAckStream{err: errors.New("start failed")}, 0, true},
	}
	for site, start := range startSites {
		for _, tc := range cases {
			t.Run(site+"/"+tc.name, func(t *testing.T) {
				starts := 0
				opts := tc.opts
				opts.onDeployStarted = func() { starts++ }
				stream := *tc.stream // each run consumes its own copy
				client := &startAckClient{foregroundFastPathClient{fastPathContainerClient: fastPathContainerClient{appName: "ack-app"}, stream: &stream}}
				err := start(context.Background(), &grpcclient.AgentConnection{ContainerService: client}, &appconfig.AppConfig{AppID: "ack-app"}, opts)
				if (err != nil) != tc.wantErr {
					t.Fatalf("err = %v, want error: %v", err, tc.wantErr)
				}
				if starts != tc.wantStarts {
					t.Fatalf("reported %d starts, want %d", starts, tc.wantStarts)
				}
			})
		}
	}
}

// chunkDeployClient serves a chunk-diff deploy up to RunContainer: the chunk
// push succeeds, PrepareImage reports Unimplemented (as an older agent does)
// and RunContainer fails with runErr.
type chunkDeployClient struct {
	*fakeContainerClient
	runErr error
}

func (c *chunkDeployClient) PrepareImage(context.Context, *agentpb.RunContainerLayersRequest, ...grpc.CallOption) (*agentpb.PrepareImageResponse, error) {
	return nil, status.Error(codes.Unimplemented, "PrepareImage not implemented")
}

func (c *chunkDeployClient) RunContainer(context.Context, *agentpb.RunContainerLayersRequest, ...grpc.CallOption) (grpc.ServerStreamingClient[agentpb.RunContainerLayersResponse], error) {
	return nil, c.runErr
}

// TestDeployByChunkDiffTimesTheNativeRebuild covers the Stagefile inner loop:
// the native app-layer rebuild bypasses the timed buildx closure, and its time
// must still reach stats.buildTime. The push then completes and RunContainer
// fails, which also pins that pushCompleted marks a finished push and that no
// start time is recorded without a start.
func TestDeployByChunkDiffTimesTheNativeRebuild(t *testing.T) {
	// Hermetic: a temp user cache, no Docker on PATH (a regression that reaches
	// buildx fails fast instead of building), and a zero cache cap so the
	// deferred cache maintenance never runs `docker buildx prune`.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	t.Setenv("LocalAppData", filepath.Join(home, "AppData", "Local"))
	t.Setenv("PATH", t.TempDir())
	t.Setenv("WENDY_BUILD_CACHE_MAX_BYTES", "0")
	t.Setenv(chunkExportModeEnv, "")
	t.Setenv(nativeLayersEnv, "")
	t.Setenv(imageSignaturePathEnv, "")
	manifestCacheTestDir = t.TempDir()
	t.Cleanup(func() { manifestCacheTestDir = "" })

	const appID, platform, dockerfile = "native-timing", "linux/arm64", "Dockerfile.generated"
	proj := t.TempDir()
	writeFile(t, proj, "build.stagefile.yaml", `version: 1
stages:
  - name: app
    from: python:3.11-slim
    copy:
      - from: local
        paths: [main.py]
        dest: app/
`)
	writeFile(t, proj, dockerfile, "FROM python:3.11-slim AS app\nCOPY main.py app/\n")
	writeFile(t, proj, "main.py", "print('v1')\n")

	// The layout a first deploy leaves behind: a buildx image whose app layer
	// was adopted for native rebuilds.
	userCache, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	layout := chunkLayoutDir(userCache, appID, platform)
	writeTwoLayerLayoutDir(t, layout,
		tarBytes(t, map[string]string{"usr/lib/python/dep.py": "dep"}),
		tarBytes(t, map[string]string{"app/": "", "app/main.py": "print('v1')\n"}), "")
	sf, ok := nativeBuildEligibility(proj, dockerfile)
	if !ok {
		t.Fatal("the Stagefile project should be eligible for native layers")
	}
	ctx := context.Background()
	depsHash, err := nativeDepsHash(proj, dockerfile, platform, resolvedStagefileBackend(ctx), nil, sf)
	if err != nil {
		t.Fatal(err)
	}
	if adopted, err := adoptNativeLayers(layout, platform, proj, sf, depsHash); err != nil || !adopted {
		t.Fatalf("adoptNativeLayers = %v, %v", adopted, err)
	}
	writeFile(t, proj, "main.py", "print('v2')\n") // an app-only edit, rebuilt natively

	runErr := errors.New("RunContainer failed")
	client := &chunkDeployClient{fakeContainerClient: &fakeContainerClient{
		queryFn: func(req *agentpb.QueryChunksRequest) *agentpb.QueryChunksResponse {
			return &agentpb.QueryChunksResponse{MissingHashes: req.GetChunkHashes()}
		},
	}, runErr: runErr}
	var stats chunkDeployStats
	_, _, err = deployByChunkDiff(ctx, &grpcclient.AgentConnection{ContainerService: client}, proj, &appconfig.AppConfig{AppID: appID},
		platform, dockerfile, nil, nil, runOptions{builder: "docker", quietBuild: true, detach: true}, &stats, nil)
	if !errors.Is(err, runErr) {
		t.Fatalf("deployByChunkDiff err = %v, want the RunContainer failure", err)
	}
	if stats.buildTime <= 0 {
		t.Errorf("buildTime = %v, want the native rebuild's time", stats.buildTime)
	}
	if !stats.pushCompleted || stats.push.SentChunks == 0 {
		t.Errorf("pushCompleted = %v, SentChunks = %d; want a completed push", stats.pushCompleted, stats.push.SentChunks)
	}
	if stats.startTime != 0 {
		t.Errorf("startTime = %v, want zero without a start", stats.startTime)
	}
}
