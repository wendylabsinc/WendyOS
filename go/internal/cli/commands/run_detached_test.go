package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/vm"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
)

type detachedStartClient struct {
	agentpb.WendyContainerServiceClient
	stream *detachedStartStream
}

func (c *detachedStartClient) StartContainer(context.Context, *agentpb.StartContainerRequest, ...grpc.CallOption) (grpc.ServerStreamingClient[agentpb.RunContainerLayersResponse], error) {
	return c.stream, nil
}

type detachedStartStream struct {
	grpc.ServerStreamingClient[agentpb.RunContainerLayersResponse]
	responses []*agentpb.RunContainerLayersResponse
}

func (s *detachedStartStream) Recv() (*agentpb.RunContainerLayersResponse, error) {
	if len(s.responses) == 0 {
		return nil, io.EOF
	}
	resp := s.responses[0]
	s.responses = s.responses[1:]
	return resp, nil
}

func detachedStarted() *agentpb.RunContainerLayersResponse {
	return &agentpb.RunContainerLayersResponse{ResponseType: &agentpb.RunContainerLayersResponse_Started_{Started: &agentpb.RunContainerLayersResponse_Started{}}}
}

func isolateDetachedOutput(t *testing.T) {
	t.Helper()
	oldJSON, oldStatuses, oldMapping := jsonOutput, vmStatusesFn, vmTCPPortMapping
	t.Cleanup(func() { jsonOutput, vmStatusesFn, vmTCPPortMapping = oldJSON, oldStatuses, oldMapping })
	jsonOutput = true
	vmStatusesFn = func() ([]vm.Status, error) {
		return []vm.Status{
			{Name: "first", Running: true, State: vm.State{NetMode: vm.NetUser, AgentPort: 51051}},
			{Name: "second", Running: true, State: vm.State{NetMode: vm.NetUser, AgentPort: 51053}},
		}, nil
	}
	vmTCPPortMapping = func(context.Context, string, int) (int, error) {
		t.Fatal("unexpected VM port lookup")
		return 0, nil
	}
}

func decodeDetachedResult(t *testing.T, stdout string) detachedRunResult {
	t.Helper()
	var result detachedRunResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("expected one clean JSON result: %v; stdout=%q", err, stdout)
	}
	if result.Status != "started" || result.Readiness != "not_checked" {
		t.Fatalf("result claims incorrect state: %+v", result)
	}
	return result
}

// Exercise both agent start transports and the native path through their
// actual output boundary. The reported URL must fetch the expected response,
// and reporting it must not perform a readiness request or open a browser.
func TestDetachedRunURLJourney(t *testing.T) {
	for _, transport := range []string{"start", "layers", "native"} {
		t.Run(transport, func(t *testing.T) {
			isolateDetachedOutput(t)
			hooks, browsers := swapPostStartExec(t), swapBrowserOpen(t)
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				io.WriteString(w, "detached-nonce")
			}))
			defer server.Close()
			const guestPort = 18881
			vmTCPPortMapping = func(_ context.Context, name string, port int) (int, error) {
				if name != "second" || port != guestPort {
					t.Fatalf("wrong VM mapping lookup: %s:%d", name, port)
				}
				return testPort(t, server.Listener), nil
			}
			cfg := &appconfig.AppConfig{AppID: "test.app", Entitlements: []appconfig.Entitlement{{Type: "http", Port: guestPort}},
				Readiness: &appconfig.ReadinessConfig{TCPSocket: &appconfig.TCPSocketProbe{Port: 19999}, TimeoutSeconds: 30},
				Hooks:     &appconfig.HooksConfig{PostStart: &appconfig.HookCommand{CLI: "must-not-run"}}}
			stream := &detachedStartStream{responses: []*agentpb.RunContainerLayersResponse{
				{ResponseType: &agentpb.RunContainerLayersResponse_StdoutOutput{StdoutOutput: &agentpb.RunContainerLayersResponse_ConsoleOutput{Data: []byte("pre-start log")}}},
				detachedStarted(),
			}}
			client := &detachedStartClient{stream: stream}
			conn := &grpcclient.AgentConnection{SimulatorName: "second", Host: "127.0.0.1", Addr: "127.0.0.1:51054", ContainerService: client}
			opts := runOptions{detach: true, detachedOutput: true}
			stdout, _ := captureBoth(t, func() {
				var err error
				switch transport {
				case "start":
					err = startExistingContainer(context.Background(), conn, cfg, opts)
				case "layers":
					// RunContainer can stream application logs before Started.
					err = streamRunContainer(context.Background(), conn, stream, cfg, opts)
				case "native":
					conn.ContainerService = &nativeDetachedClient{detachedStartClient: client}
					err = runMacOSNativeContainer(context.Background(), conn, cfg, &agentpb.CreateContainerRequest{}, opts)
				}
				if err != nil {
					t.Fatal(err)
				}
			})
			result := decodeDetachedResult(t, stdout)
			if result.App != cfg.AppID || result.Device != "vm:second" || result.URL != server.URL || len(result.Endpoints) != 1 {
				t.Fatalf("wrong app endpoint: %+v", result)
			}
			if requests != 0 || len(*hooks) != 0 || len(*browsers) != 0 {
				t.Fatal("detached reporting performed readiness or host hooks")
			}
			response, err := server.Client().Get(result.URL)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil || response.StatusCode != 200 || string(body) != "detached-nonce" {
				t.Fatalf("reported URL failed HTTP proof: %d %q %v", response.StatusCode, body, err)
			}
		})
	}
}

type nativeDetachedClient struct{ *detachedStartClient }

func (c *nativeDetachedClient) CreateContainer(context.Context, *agentpb.CreateContainerRequest, ...grpc.CallOption) (*agentpb.CreateContainerResponse, error) {
	return &agentpb.CreateContainerResponse{}, nil
}

func TestDetachedRunMissingOrWrongVMForward(t *testing.T) {
	for _, test := range []struct {
		name string
		conn *grpcclient.AgentConnection
		port int
		err  error
	}{
		{"missing forward", &grpcclient.AgentConnection{SimulatorName: "first", Addr: "127.0.0.1:51051"}, 0, nil},
		{"QMP failure", &grpcclient.AgentConnection{SimulatorName: "first", Addr: "127.0.0.1:51051"}, 0, errors.New("monitor unavailable")},
		{"reused agent port", &grpcclient.AgentConnection{SimulatorName: "first", Addr: "127.0.0.1:51053"}, 18881, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			isolateDetachedOutput(t)
			vmTCPPortMapping = func(_ context.Context, name string, port int) (int, error) { return test.port, test.err }
			stdout, stderr := captureBoth(t, func() {
				err := (runOptions{detachedOutput: true}).reportDetachedRun(context.Background(), test.conn, "app", &appconfig.AppConfig{AppID: "app", Entitlements: []appconfig.Entitlement{{Type: "http", Port: 18881}}})
				if err != nil {
					t.Fatal(err)
				}
			})
			result := decodeDetachedResult(t, stdout)
			if result.URL != "" || len(result.Endpoints) != 0 || result.Device != "vm:first" || !strings.Contains(stderr, "URL could not be determined") {
				t.Fatalf("invented an endpoint or lost the target: %+v; stderr=%q", result, stderr)
			}
		})
	}
}

func TestDetachedRunTextAndHTTPPorts(t *testing.T) {
	isolateDetachedOutput(t)
	jsonOutput = false
	vmTCPPortMapping = func(_ context.Context, name string, port int) (int, error) { return port + 1000, nil }
	conn := &grpcclient.AgentConnection{Host: "127.0.0.1", Addr: "127.0.0.1:51051"}
	cfg := &appconfig.AppConfig{AppID: "app", Entitlements: []appconfig.Entitlement{{Type: "http", Port: 8080}, {Type: "http", Port: 9000}},
		Hooks: &appconfig.HooksConfig{PostStart: &appconfig.HookCommand{OpenURL: "https://${WENDY_HOSTNAME}:9000/health?app=${WENDY_APP_ID}"}}}
	stdout, stderr := captureBoth(t, func() {
		if err := (runOptions{detachedOutput: true}).reportDetachedRun(context.Background(), conn, "app", cfg); err != nil {
			t.Fatal(err)
		}
	})
	if stdout != "" || !strings.Contains(stderr, "Readiness not checked") || !strings.Contains(stderr, "https://127.0.0.1:10000/health?app=app") || !strings.Contains(stderr, "http://127.0.0.1:9080") {
		t.Fatalf("incorrect text endpoint output: stdout=%q stderr=%q", stdout, stderr)
	}
}

func TestDetachedRunDoesNotInventHTTPFromReadiness(t *testing.T) {
	isolateDetachedOutput(t)
	conn := &grpcclient.AgentConnection{Host: "192.0.2.5", Addr: "192.0.2.5:50051"}
	cfg := &appconfig.AppConfig{AppID: "redis", Readiness: &appconfig.ReadinessConfig{TCPSocket: &appconfig.TCPSocketProbe{Port: 6379}}}
	stdout, _ := captureBoth(t, func() {
		if err := (runOptions{detachedOutput: true}).reportDetachedRun(context.Background(), conn, "redis", cfg); err != nil {
			t.Fatal(err)
		}
	})
	if result := decodeDetachedResult(t, stdout); result.URL != "" || len(result.Endpoints) != 0 {
		t.Fatalf("TCP probe presented as HTTP: %+v", result)
	}
}

func TestDetachedRunEarlyEOFHasNoSuccessResult(t *testing.T) {
	isolateDetachedOutput(t)
	conn := &grpcclient.AgentConnection{ContainerService: &detachedStartClient{stream: &detachedStartStream{}}}
	stdout, _ := captureBoth(t, func() {
		if err := startExistingContainer(context.Background(), conn, &appconfig.AppConfig{AppID: "app"}, runOptions{detach: true, detachedOutput: true}); err == nil {
			t.Fatal("early EOF was reported as a successful start")
		}
	})
	if stdout != "" {
		t.Fatalf("failure emitted a success result: %s", stdout)
	}
}

func TestDetachedRunGroupHasOneResult(t *testing.T) {
	isolateDetachedOutput(t)
	vmTCPPortMapping = func(_ context.Context, name string, port int) (int, error) { return port, nil }
	configs := map[string]*appconfig.AppConfig{}
	for i, name := range []string{"api", "web"} {
		configs[name] = &appconfig.AppConfig{AppID: "group", ServiceName: name, Entitlements: []appconfig.Entitlement{{Type: "http", Port: 18880 + i}}}
	}
	conn := &grpcclient.AgentConnection{Addr: "127.0.0.1:51051", ContainerService: &hookSvcContainerClient{}}
	stdout, _ := captureBoth(t, func() {
		if err := startAndStreamServices(context.Background(), conn, "group", []string{"api", "web"}, nil, runOptions{detach: true, detachedOutput: true}, func(string) error { return nil }, configs, configs, nil); err != nil {
			t.Fatal(err)
		}
	})
	result := decodeDetachedResult(t, stdout)
	if len(result.Endpoints) != 2 || result.App != "group" {
		t.Fatalf("wrong group result: %+v", result)
	}
	for i, endpoint := range result.Endpoints {
		if !strings.HasSuffix(endpoint.URL, ":"+strconv.Itoa(18880+i)) {
			t.Fatalf("wrong service endpoint: %+v", endpoint)
		}
	}
}

func TestDetachedRunFastPathStillReportsURL(t *testing.T) {
	for _, state := range []agentpb.AppRunningState{agentpb.AppRunningState_RUNNING, agentpb.AppRunningState_STOPPED} {
		t.Run(state.String(), func(t *testing.T) {
			isolateDetachedOutput(t)
			isolateFingerprintCache(t)
			const layer = "sha256:detached-test"
			cfg := &appconfig.AppConfig{AppID: "app", Entitlements: []appconfig.Entitlement{{Type: "http", Port: 8080}}}
			saveDeployFingerprint(cfg.AppID, "device", deployFingerprint{InputHash: "input", LayerDiffIDs: []string{layer}})
			client := &fastPathContainerClient{appName: cfg.AppID, state: state, presentLayers: map[string]bool{layer: true}}
			conn := &grpcclient.AgentConnection{Host: "192.0.2.5", Addr: "192.0.2.5:50051", ContainerService: client}
			stdout, _ := captureBoth(t, func() {
				done, err := tryDeployFastPath(context.Background(), conn, cfg, "device", "input", runOptions{detach: true, detachedOutput: true})
				if err != nil || !done {
					t.Fatalf("fast path: done=%v err=%v", done, err)
				}
			})
			if result := decodeDetachedResult(t, stdout); result.URL != "http://192.0.2.5:8080" {
				t.Fatalf("unchanged deploy lost endpoint: %+v", result)
			}
		})
	}
}

func TestDetachedJSONBuildProgressStaysOnStderr(t *testing.T) {
	// Even with interactive rendering selected, a JSON deploy keeps stdout
	// available for its single final result and uses plain build progress.
	defer forceBuildProgressInteractive(true)()
	ctx := context.WithValue(context.Background(), detachedJSONRunKey{}, true)
	stdout, stderr := captureBoth(t, func() {
		if err := runBuildWithProgress(ctx, "Building fixture", dumpRawAlways, func(ctx context.Context, stream, logw io.Writer) error {
			io.WriteString(logw, "fixture builder setup\n")
			io.WriteString(stream, "#1 [1/1] RUN fixture\n#1 DONE 0.1s\n")
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})
	if stdout != "" || !strings.Contains(stderr, "Building fixture") || !strings.Contains(stderr, "Built & pushed") {
		t.Fatalf("build contaminated JSON stdout: stdout=%q stderr=%q", stdout, stderr)
	}
}

func TestDetachedJSONChunkPushHeartbeatStaysOnStderr(t *testing.T) {
	// Piped output selects the plain chunk-push heartbeat. A JSON deploy's
	// stdout must still carry only its final result when a push outlasts a
	// heartbeat, so `wendy --json run --detach | jq` keeps parsing.
	defer forceBuildProgressInteractive(false)()
	manifestCacheTestDir = t.TempDir()
	oldOut, oldInterval := buildProgressOut, chunkPushPlainHeartbeatInterval
	t.Cleanup(func() {
		manifestCacheTestDir = ""
		buildProgressOut, chunkPushPlainHeartbeatInterval = oldOut, oldInterval
	})
	var stdout bytes.Buffer
	buildProgressOut = &stdout
	chunkPushPlainHeartbeatInterval = time.Millisecond

	diffID := "sha256:" + strings.Repeat("ab", 32)
	cs := &fakeContainerClient{
		queryFn: func(*agentpb.QueryChunksRequest) *agentpb.QueryChunksResponse {
			return &agentpb.QueryChunksResponse{}
		},
		queryLayersFn: func(*agentpb.QueryLayersRequest) *agentpb.QueryLayersResponse {
			time.Sleep(50 * time.Millisecond) // outlast several heartbeats
			return &agentpb.QueryLayersResponse{Present: []*agentpb.PresentLayer{{DiffId: diffID, Size: 4096}}}
		},
	}
	layers := []localLayer{{
		Digest:    "sha256:" + sha256Hex([]byte("compressed-bytes")),
		DiffID:    diffID,
		MediaType: "application/vnd.oci.image.layer.v1.tar+gzip",
		Blob:      []byte("this is not gzip"),
	}}
	ctx := context.WithValue(context.Background(), detachedJSONRunKey{}, true)
	stderr := captureStderr(t, func() {
		if _, err := pushLayersWithProgress(ctx, cs, layers, nil, gzipChunkUploadConfig, nil); err != nil {
			t.Fatal(err)
		}
	})
	if stdout.Len() != 0 || !strings.Contains(stderr, "  ...  ") {
		t.Fatalf("chunk push heartbeat contaminated JSON stdout: stdout=%q stderr=%q", stdout.String(), stderr)
	}
}

func TestDetachedRunCloudDoesNotReportPrivateLANAddress(t *testing.T) {
	isolateDetachedOutput(t)
	oldLookup := osLookupHostFn
	t.Cleanup(func() { osLookupHostFn = oldLookup })
	osLookupHostFn = func(context.Context, string) ([]string, error) { return nil, errors.New("no VPN") }
	conn := &grpcclient.AgentConnection{Host: "cloud-asset", Addr: "cloud-asset:50051", Reconnect: neverReconnect, MeshHost: "device.mesh"}
	cfg := &appconfig.AppConfig{AppID: "app", Entitlements: []appconfig.Entitlement{{Type: "http", Port: 8080}}}
	stdout, _ := captureBoth(t, func() {
		if err := (runOptions{detachedOutput: true}).reportDetachedRun(context.Background(), conn, cfg.AppID, cfg); err != nil {
			t.Fatal(err)
		}
	})
	if result := decodeDetachedResult(t, stdout); result.URL != "" || len(result.Endpoints) != 0 {
		t.Fatalf("unroutable cloud endpoint reported: %+v", result)
	}
}

func TestInternalAndDeployOnlyRunsDoNotEmitDetachedResult(t *testing.T) {
	isolateDetachedOutput(t)
	for _, opts := range []runOptions{{detach: true}, {detach: true, detachedOutput: true, deploy: true}} {
		stdout, stderr := captureBoth(t, func() {
			if err := opts.reportDetachedRun(context.Background(), nil, "app"); err != nil {
				t.Fatal(err)
			}
		})
		if stdout != "" || stderr != "" {
			t.Fatalf("internal/deploy-only run emitted a public result: stdout=%q stderr=%q", stdout, stderr)
		}
	}
}

func TestDetachedJSONConfigCreationStaysOnStderr(t *testing.T) {
	isolateDetachedOutput(t)
	stdout, stderr := captureBoth(t, func() {
		if _, err := ensureAppConfig(filepath.Join(t.TempDir(), "wendy.json"), true); err != nil {
			t.Fatal(err)
		}
	})
	if stdout != "" || !strings.Contains(stderr, "Created wendy.json") {
		t.Fatalf("config creation contaminated JSON stdout: stdout=%q stderr=%q", stdout, stderr)
	}
}

func TestDetachedRunIPv6URLs(t *testing.T) {
	for _, test := range []struct {
		name, host, hookURL, want string
	}{
		{"IPv6", "2001:db8::5", "", "http://[2001:db8::5]:8080"},
		{"IPv6 zone", "fe80::5%en0", "", "http://[fe80::5%25en0]:8080"},
		{"IPv6 zone hook", "fe80::5%en0", "https://${WENDY_HOSTNAME}:8080/health", "https://[fe80::5%25en0]:8080/health"},
		{"IPv4 mapped hook", "::ffff:192.0.2.5", "https://${WENDY_HOSTNAME}:8080/health", "https://192.0.2.5:8080/health"},
	} {
		t.Run(test.name, func(t *testing.T) {
			isolateDetachedOutput(t)
			conn := &grpcclient.AgentConnection{Host: test.host, Addr: net.JoinHostPort(test.host, "50051")}
			cfg := &appconfig.AppConfig{AppID: "app", Entitlements: []appconfig.Entitlement{{Type: "http", Port: 8080}}}
			if test.hookURL != "" {
				cfg.Hooks = &appconfig.HooksConfig{PostStart: &appconfig.HookCommand{OpenURL: test.hookURL}}
			}
			stdout, _ := captureBoth(t, func() {
				if err := (runOptions{detachedOutput: true}).reportDetachedRun(context.Background(), conn, cfg.AppID, cfg); err != nil {
					t.Fatal(err)
				}
			})
			result := decodeDetachedResult(t, stdout)
			if result.URL != test.want {
				t.Errorf("URL = %q, want %q", result.URL, test.want)
			}
			for _, endpoint := range result.Endpoints {
				if _, err := url.Parse(endpoint.URL); err != nil {
					t.Errorf("reported an invalid URL %q: %v", endpoint.URL, err)
				}
			}
		})
	}
}

func TestDetachedJSONXcodeDeployHasOneResult(t *testing.T) {
	isolateDetachedOutput(t)
	dir := newStubbedXcodeProject(t)
	state := &fakeMacRunState{sendStarted: true}
	conn, cleanup := startFakeMacRunServer(t, state)
	defer cleanup()
	cfg := &appconfig.AppConfig{AppID: "test.xcode", Platform: appconfig.PlatformDarwin, Xcode: &appconfig.XcodeConfig{Scheme: "MyScheme"}}
	ctx := context.WithValue(context.Background(), detachedJSONRunKey{}, true)
	stdout, stderr := captureBoth(t, func() {
		if err := runWithAgent(ctx, conn, dir, cfg, runOptions{detach: true, detachedOutput: true, skipCloudRegistration: true}); err != nil {
			t.Fatal(err)
		}
	})
	if result := decodeDetachedResult(t, stdout); result.App != cfg.AppID {
		t.Fatalf("wrong Xcode app result: %+v", result)
	}
	if !strings.Contains(stderr, "xcodebuild.log") {
		t.Fatal("lost the Xcode progress hint")
	}
}
