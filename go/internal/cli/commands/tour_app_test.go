package commands

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/vm"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestTourAppScaffoldResumesEditsAndAvoidsUnrelatedDirectories(t *testing.T) {
	root := t.TempDir()
	unrelated := filepath.Join(root, "apps", "patrol")
	if err := os.MkdirAll(unrelated, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unrelated, "patrol.py"), []byte("unrelated"), 0o644); err != nil {
		t.Fatal(err)
	}
	path, err := scaffoldTourApp(root, "patrol")
	if err != nil || path != filepath.Join(root, "apps", "patrol-2") {
		t.Fatalf("path=%q err=%v", path, err)
	}
	if err := os.WriteFile(filepath.Join(path, "patrol.py"), []byte("my route"), 0o644); err != nil {
		t.Fatal(err)
	}
	resumed, err := scaffoldTourApp(root, "patrol")
	if err != nil || resumed != path {
		t.Fatalf("resumed=%q err=%v", resumed, err)
	}
	for dir, want := range map[string]string{unrelated: "unrelated", path: "my route"} {
		got, _ := os.ReadFile(filepath.Join(dir, "patrol.py"))
		if string(got) != want {
			t.Fatalf("changed existing source in %s", dir)
		}
	}
}

func TestTourAppScaffoldDoesNotReuseSymlinksOrForeignMarkers(t *testing.T) {
	for _, symlink := range []bool{false, true} {
		t.Run(strconv.FormatBool(symlink), func(t *testing.T) {
			root := t.TempDir()
			outside, err := scaffoldTourApp(t.TempDir(), "patrol")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(root, "apps"), 0o755); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, "apps", "patrol")
			if symlink {
				err = os.Symlink(outside, path)
			} else {
				err = os.Mkdir(path, 0o755)
				if err == nil {
					err = os.WriteFile(filepath.Join(path, tourExampleMarker), []byte(`{"version":1,"kind":"roam"}`), 0o644)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			got, err := scaffoldTourApp(root, "patrol")
			if err != nil || got != path+"-2" {
				t.Fatalf("path=%q err=%v", got, err)
			}
		})
	}
}

func stubTourAppConnection(t *testing.T) *grpcclient.AgentConnection {
	t.Helper()
	previous := connectTourAppSimulatorFn
	conn := &grpcclient.AgentConnection{SimulatorName: "tour-test"}
	connectTourAppSimulatorFn = func(_ context.Context, choice *simulatorChoice, suppress bool) (*SelectedDevice, error) {
		if choice.Name != "tour-test" || choice.Create || !suppress {
			t.Fatalf("unexpected simulator selection %+v, suppress=%t", choice, suppress)
		}
		return &SelectedDevice{Agent: conn}, nil
	}
	t.Cleanup(func() { connectTourAppSimulatorFn = previous })
	return conn
}

func TestTourAppDeployUsesSelectedSimulatorAndOrdinaryAppProtections(t *testing.T) {
	path, err := scaffoldTourApp(t.TempDir(), "sensors")
	if err != nil {
		t.Fatal(err)
	}
	conn := stubTourAppConnection(t)
	previous := runTourAppWithAgentFn
	t.Cleanup(func() { runTourAppWithAgentFn = previous })
	called := false
	runTourAppWithAgentFn = func(_ context.Context, got *grpcclient.AgentConnection, cwd string, cfg *appconfig.AppConfig, opts runOptions) error {
		called = true
		if got != conn || cwd != path || cfg.AppID != "sh.wendy.simulator.go2.examples.sensors" {
			t.Fatal("deployment changed project or simulator")
		}
		if opts.managedRobot || opts.buildType != "docker" || opts.builder != "docker" || opts.dockerfile != "Dockerfile" || !opts.detach || !opts.noRestart || opts.restartUnlessStopped || len(opts.userArgs) != 0 {
			t.Fatalf("unexpected deploy options %+v", opts)
		}
		return nil
	}
	if err := runTourApp(context.Background(), "tour-test", path); err != nil || !called {
		t.Fatalf("err=%v called=%t", err, called)
	}
	conn.SimulatorName = "other-vm"
	called = false
	if err := runTourApp(context.Background(), "tour-test", path); err == nil || called {
		t.Fatal("accepted a connection to another simulator")
	}
}

func TestTourAppControlRequiresServiceAcceptance(t *testing.T) {
	conn := stubTourAppConnection(t)
	node := tourAppNodeName("tour-test", "patrol", "dev.student.robot")
	previous := callTourAppServiceFn
	t.Cleanup(func() { callTourAppServiceFn = previous })
	for _, tc := range []struct {
		response string
		rpcOK    bool
		wantOK   bool
	}{
		{"std_srvs.srv.Trigger_Response(success=True, message='ready')", true, true},
		{"std_srvs.srv.Trigger_Response(success=False, message='sensor_stale')", true, false},
		{"success: true\nmessage: ready", true, true},
		{"waiting for service to become available", true, false},
		{"failed", false, false},
	} {
		t.Run(tc.response, func(t *testing.T) {
			callTourAppServiceFn = func(_ context.Context, got *grpcclient.AgentConnection, req *agentpbv2.CallROS2ServiceRequest) (*agentpbv2.CallROS2ServiceResponse, error) {
				if got != conn || req.GetService() != "/"+node+"/patrol/start" || req.GetType() != "std_srvs/srv/Trigger" || req.GetRequest() != "{}" || req.DomainId == nil || *req.DomainId != 0 {
					t.Fatalf("unexpected service target or request %v", req)
				}
				return &agentpbv2.CallROS2ServiceResponse{Success: tc.rpcOK, Response: tc.response}, nil
			}
			response, err := controlTourApp(context.Background(), "tour-test", "patrol", node, "start")
			if (err == nil) != tc.wantOK || response != tc.response {
				t.Fatalf("response=%q err=%v", response, err)
			}
		})
	}
	if _, err := controlTourApp(context.Background(), "tour-test", "teleop", node, "start"); err == nil {
		t.Fatal("accepted unsupported control request")
	}
}

func TestTourAppURLVerifiesMappingAndHTTP(t *testing.T) {
	path, err := scaffoldTourApp(t.TempDir(), "sensors")
	if err != nil {
		t.Fatal(err)
	}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	u, _ := url.Parse(server.URL)
	port, _ := strconv.Atoi(u.Port())
	previous := tourAppPortFn
	t.Cleanup(func() { tourAppPortFn = previous })
	tourAppPortFn = func(_ context.Context, name string, guestPort int) (int, error) {
		if name != "tour-test" || guestPort != 8904 {
			t.Fatalf("unexpected mapping lookup %s:%d", name, guestPort)
		}
		return port, nil
	}
	got, err := tourAppURL(context.Background(), "tour-test", path)
	if err != nil || got != server.URL || requests == 0 {
		t.Fatalf("url=%q err=%v requests=%d", got, err, requests)
	}
	tourAppPortFn = func(context.Context, string, int) (int, error) { return 0, nil }
	if _, err := tourAppURL(context.Background(), "tour-test", path); err == nil || !strings.Contains(err.Error(), "verified") {
		t.Fatalf("unverified mapping err=%v", err)
	}
	tourAppPortFn = func(context.Context, string, int) (int, error) { return 0, context.Canceled }
	if _, err := tourAppURL(context.Background(), "tour-test", path); !errors.Is(err, context.Canceled) {
		t.Fatalf("mapping cancellation err=%v", err)
	}
}

func TestTourAppCleanupStopsCapturedAppAndReleasesOnlyItsQuietGrant(t *testing.T) {
	for _, tc := range []struct {
		name                                            string
		replaced, foreign, live, wrongIdentity, noOwner bool
		stopErr                                         error
		wantRelease, wantErr                            bool
	}{
		{name: "own grant", wantRelease: true},
		{name: "replacement grant", replaced: true},
		{name: "different node", foreign: true},
		{name: "copied node still publishing", live: true},
		{name: "already removed app with stale grant", stopErr: status.Error(codes.NotFound, "missing"), wantRelease: true},
		{name: "app never created", stopErr: status.Error(codes.NotFound, "missing"), noOwner: true},
		{name: "already removed app with foreign grant", stopErr: status.Error(codes.NotFound, "missing"), foreign: true},
		{name: "already removed app with live copied publisher", stopErr: status.Error(codes.NotFound, "missing"), live: true},
		{name: "already removed app with wrong simulator identity", stopErr: status.Error(codes.NotFound, "missing"), wrongIdentity: true, wantErr: true},
		{name: "stop failed", stopErr: errors.New("stop failed"), wantErr: true},
		{name: "wrong simulator identity", wrongIdentity: true, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, err := scaffoldTourApp(t.TempDir(), "patrol")
			if err != nil {
				t.Fatal(err)
			}
			cleanup, nodeName, err := prepareTourAppCleanup("tour-test", "patrol", path)
			if err != nil {
				t.Fatal(err)
			}
			if nodeName != tourAppNodeName("tour-test", "patrol", "sh.wendy.simulator.go2.examples.patrol") {
				t.Fatalf("incorrect captured node: %s", nodeName)
			}
			// Later edits must never retarget cleanup to the robot runtime.
			if err := os.WriteFile(filepath.Join(path, "wendy.json"), []byte(`{"appId":"sh.wendy.simulator.go2"}`), 0o644); err != nil {
				t.Fatal(err)
			}
			conn := stubTourAppConnection(t)
			oldEndpoint, oldRequest, oldStop := tourAppRobotEndpointFn, tourAppRobotRequestFn, stopTourAppContainerFn
			t.Cleanup(func() {
				tourAppRobotEndpointFn, tourAppRobotRequestFn, stopTourAppContainerFn = oldEndpoint, oldRequest, oldStop
			})
			profile, err := vm.NewGo2RobotProfile("sha256:"+strings.Repeat("1", 64), go2PolicyBundle)
			if err != nil {
				t.Fatal(err)
			}
			tourAppRobotEndpointFn = func(_ context.Context, name string) (*vm.Store, vm.RobotProfile, int, error) {
				if name != "tour-test" {
					t.Fatal(name)
				}
				return nil, profile, 1234, nil
			}
			stopped, released, reads := false, false, 0
			stopTourAppContainerFn = func(_ context.Context, got *grpcclient.AgentConnection, appID string) error {
				if got != conn || appID != "sh.wendy.simulator.go2.examples.patrol" {
					t.Fatalf("wrong stop target: %s", appID)
				}
				stopped = true
				return tc.stopErr
			}
			tourAppRobotRequestFn = func(_ context.Context, port int, endpoint string, body, result any) error {
				if port != 1234 {
					t.Fatal(port)
				}
				if endpoint == "/api/disarm_ros" {
					if !stopped || reads < 2 {
						t.Fatal("released before stopping and rechecking")
					}
					released = true
					return json.Unmarshal([]byte(`{"released":true}`), result)
				}
				if endpoint != "/api/status" || body != nil {
					t.Fatalf("unexpected request %s", endpoint)
				}
				reads++
				owner, node, age := "ours", tourAppNodeName("tour-test", "patrol", "sh.wendy.simulator.go2.examples.patrol"), 500.0
				if tc.foreign {
					node = "another_node"
				}
				if tc.noOwner {
					owner = ""
				}
				if tc.live {
					age = 0
				}
				if tc.replaced && reads > 1 {
					owner = "replacement"
				}
				if released {
					owner = ""
				}
				state := tourAppControlStatus{tourRobotObservation: tourRobotObservation{
					robotRuntimeStatus: robotRuntimeStatus{VMName: "tour-test", Simulation: !tc.wrongIdentity, ProfileVersion: profile.Version, RobotKind: profile.Kind, SourceDigest: profile.SourceDigest, PolicyBundle: profile.PolicyBundle, World: profile.World, ClockMode: profile.ClockMode, Seed: profile.Seed, VisualDetail: profile.VisualDetail, DDSIsolation: "udp-rtps-loopback", Epoch: 1},
					Armed:              !released, Command: []float64{0, 0, 0}, Velocity: []float64{0, 0, 0},
				}}
				data, _ := json.Marshal(map[string]any{"owner": owner, "sources": []any{map[string]any{"publisher_gid": owner, "node_name": node, "node_namespace": "/", "kind": "sport", "age_ms": age}}})
				if err := json.Unmarshal(data, &state.ROSCommands); err != nil {
					t.Fatal(err)
				}
				*result.(*tourAppControlStatus) = state
				return nil
			}
			if err := cleanup(context.Background()); (err != nil) != tc.wantErr {
				t.Fatalf("err=%v, wantErr=%t", err, tc.wantErr)
			}
			if !stopped || released != tc.wantRelease {
				t.Fatalf("stopped=%t released=%t", stopped, released)
			}
		})
	}
}

func TestTourAppCleanupRejectsManagedRuntimeIDs(t *testing.T) {
	path, err := scaffoldTourApp(t.TempDir(), "patrol")
	if err != nil {
		t.Fatal(err)
	}
	for _, appID := range []string{go2RuntimeAppID, g1RuntimeAppID, r2RuntimeAppID} {
		data, _ := json.Marshal(map[string]string{"appId": appID, "version": "0.1.0", "platform": "linux"})
		if err := os.WriteFile(filepath.Join(path, "wendy.json"), data, 0o644); err != nil {
			t.Fatal(err)
		}
		if cleanup, node, err := prepareTourAppCleanup("tour-test", "patrol", path); err == nil || cleanup != nil || node != "" {
			t.Fatalf("accepted cleanup for %s", appID)
		}
	}
}

func stubTourAppHTTP(t *testing.T, handler func(http.ResponseWriter, *http.Request, vm.RobotProfile)) {
	t.Helper()
	profile, err := vm.NewGo2RobotProfile("sha256:"+strings.Repeat("1", 64), go2PolicyBundle)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler(w, r, profile) }))
	t.Cleanup(server.Close)
	u, _ := url.Parse(server.URL)
	port, _ := strconv.Atoi(u.Port())
	oldEndpoint, oldRequest := tourAppRobotEndpointFn, tourAppRobotRequestFn
	t.Cleanup(func() { tourAppRobotEndpointFn, tourAppRobotRequestFn = oldEndpoint, oldRequest })
	tourAppRobotRequestFn = tourRobotRequest
	tourAppRobotEndpointFn = func(_ context.Context, name string) (*vm.Store, vm.RobotProfile, int, error) {
		if name != "tour-test" {
			t.Fatalf("unexpected simulator endpoint %s", name)
		}
		return nil, profile, port, nil
	}
}

func tourAppTestStatus(profile vm.RobotProfile, nodeName string) tourAppControlStatus {
	state := tourAppControlStatus{tourRobotObservation: tourRobotObservation{
		robotRuntimeStatus: robotRuntimeStatus{
			VMName: "tour-test", Simulation: true, ProfileVersion: profile.Version, RobotKind: profile.Kind,
			SourceDigest: profile.SourceDigest, PolicyBundle: profile.PolicyBundle, World: profile.World,
			ClockMode: profile.ClockMode, Seed: profile.Seed, VisualDetail: profile.VisualDetail,
			DDSIsolation: "udp-rtps-loopback", Epoch: 7,
		},
		Command: []float64{0, 0, 0}, Velocity: []float64{0, 0, 0},
	}}
	state.ROSCommands.Sources = []tourAppPublisher{{
		GID: strings.Repeat("a", 48), Kind: "sport", NodeName: nodeName, NodeNamespace: "/", AgeMS: 10,
	}}
	return state
}

func TestTourMovementAppsDeployAndAcquireOnlyTheirPublisher(t *testing.T) {
	for _, kind := range []string{"patrol", "roam", "teleop"} {
		t.Run(kind, func(t *testing.T) {
			path, err := scaffoldTourApp(t.TempDir(), kind)
			if err != nil {
				t.Fatal(err)
			}
			// The example marker, not an editable app ID suffix, selects the app.
			manifest := filepath.Join(path, "wendy.json")
			data, err := os.ReadFile(manifest)
			if err != nil {
				t.Fatal(err)
			}
			appID := "dev.student.myrobot"
			data = []byte(strings.ReplaceAll(string(data), "sh.wendy.simulator.go2.examples."+kind, appID))
			savedArgs := map[string][]string{"patrol": {"--laps", "2"}, "roam": {"--allow-scan-gaps"}, "teleop": {"--port", "8903"}}[kind]
			var document map[string]any
			if err := json.Unmarshal(data, &document); err != nil {
				t.Fatal(err)
			}
			document["run"] = map[string]any{"args": savedArgs}
			data, err = json.Marshal(document)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(manifest, data, 0o644); err != nil {
				t.Fatal(err)
			}
			conn := stubTourAppConnection(t)
			node := tourAppNodeName("tour-test", kind, appID)
			var deployed, armed atomic.Bool
			var statusReads, grants atomic.Int32
			oldRun, oldService := runTourAppWithAgentFn, callTourAppServiceFn
			t.Cleanup(func() { runTourAppWithAgentFn, callTourAppServiceFn = oldRun, oldService })
			runTourAppWithAgentFn = func(_ context.Context, got *grpcclient.AgentConnection, cwd string, cfg *appconfig.AppConfig, opts runOptions) error {
				wantArgs := append(slices.Clone(savedArgs), "--ros-args", "-r", "__node:="+node, "-r", "__ns:=/")
				if kind == "patrol" || kind == "roam" {
					wantArgs = append(wantArgs, "-r", "/"+kind+"/start:=/"+node+"/"+kind+"/start", "-r", "/"+kind+"/stop:=/"+node+"/"+kind+"/stop")
				}
				if got != conn || cwd != path || cfg.AppID != appID || !slices.Equal(opts.userArgs, wantArgs) ||
					opts.managedRobot || !opts.detach || !opts.noRestart || opts.restartUnlessStopped {
					t.Fatalf("unexpected deployment: app=%s opts=%+v", cfg.AppID, opts)
				}
				deployed.Store(true)
				return nil
			}
			callTourAppServiceFn = func(context.Context, *grpcclient.AgentConnection, *agentpbv2.CallROS2ServiceRequest) (*agentpbv2.CallROS2ServiceResponse, error) {
				t.Fatal("deployment must not start movement before the user's Start selection")
				return nil, nil
			}
			stubTourAppHTTP(t, func(w http.ResponseWriter, r *http.Request, profile vm.RobotProfile) {
				if !deployed.Load() {
					t.Error("control requested before app deployment")
				}
				switch r.URL.Path {
				case "/api/status":
					read := statusReads.Add(1)
					state := tourAppTestStatus(profile, node)
					ours := state.ROSCommands.Sources[0]
					foreign := ours
					foreign.GID = strings.Repeat("b", 48)
					foreign.NodeName = tourAppNodeName("tour-test", kind, "dev.someone.else")
					state.ROSCommands.Sources = []tourAppPublisher{foreign}
					if read > 1 {
						state.ROSCommands.Sources = append(state.ROSCommands.Sources, ours)
					}
					if armed.Load() {
						state.Armed, state.ROSCommands.Owner = true, ours.GID
					}
					_ = json.NewEncoder(w).Encode(state)
				case "/api/arm_ros":
					grants.Add(1)
					var request map[string]string
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil || r.Method != http.MethodPost || request["publisher_gid"] != strings.Repeat("a", 48) || statusReads.Load() < 2 {
						t.Errorf("unexpected grant request: %v err=%v", request, err)
					}
					armed.Store(true)
					_ = json.NewEncoder(w).Encode(map[string]any{"publisher_gid": strings.Repeat("a", 48), "epoch": 7})
				default:
					t.Errorf("unexpected control endpoint %s", r.URL.Path)
					http.NotFound(w, r)
				}
			})
			if err := runTourApp(context.Background(), "tour-test", path); err != nil {
				t.Fatal(err)
			}
			if grants.Load() != 1 || statusReads.Load() != 3 {
				t.Fatalf("grants=%d status reads=%d", grants.Load(), statusReads.Load())
			}
		})
	}
}

func TestTourAppGrantRejectsUnsafeOrUnacknowledgedControl(t *testing.T) {
	for _, mode := range []string{
		"foreign owner", "browser owner", "wrong identity", "wrong namespace", "wrong kind", "invalid gid",
		"stale", "requires restart", "ambiguous", "grant refused", "wrong ack gid", "wrong ack epoch",
		"unarmed after grant", "owner changed", "epoch changed", "identity changed",
	} {
		t.Run(mode, func(t *testing.T) {
			node := tourAppNodeName("tour-test", "patrol", "dev.student.patrol")
			var grants atomic.Int32
			stubTourAppHTTP(t, func(w http.ResponseWriter, r *http.Request, profile vm.RobotProfile) {
				if r.URL.Path == "/api/arm_ros" {
					grants.Add(1)
					if mode == "grant refused" {
						http.Error(w, "another controller took over", http.StatusForbidden)
						return
					}
					gid, epoch := strings.Repeat("a", 48), 7
					if mode == "wrong ack gid" {
						gid = strings.Repeat("b", 48)
					}
					if mode == "wrong ack epoch" {
						epoch++
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"publisher_gid": gid, "epoch": epoch})
					return
				}
				if r.URL.Path != "/api/status" {
					t.Errorf("unexpected endpoint %s", r.URL.Path)
					http.NotFound(w, r)
					return
				}
				state := tourAppTestStatus(profile, node)
				source := &state.ROSCommands.Sources[0]
				switch mode {
				case "foreign owner":
					state.Armed, state.ROSCommands.Owner = true, strings.Repeat("b", 48)
				case "browser owner":
					state.Armed = true
				case "wrong identity":
					state.VMName = "other-vm"
				case "wrong namespace":
					source.NodeNamespace = "/other"
				case "wrong kind":
					source.Kind = "twist"
				case "invalid gid":
					source.GID = "unverified"
				case "stale":
					source.AgeMS = 1000
				case "requires restart":
					source.RequiresRestart = true
				case "ambiguous":
					copy := *source
					copy.GID = strings.Repeat("b", 48)
					state.ROSCommands.Sources = append(state.ROSCommands.Sources, copy)
				}
				if grants.Load() > 0 {
					state.Armed, state.ROSCommands.Owner = true, strings.Repeat("a", 48)
					switch mode {
					case "unarmed after grant":
						state.Armed = false
					case "owner changed":
						state.ROSCommands.Owner = strings.Repeat("b", 48)
					case "epoch changed":
						state.Epoch++
					case "identity changed":
						state.VMName = "other-vm"
					}
				}
				_ = json.NewEncoder(w).Encode(state)
			})
			ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
			defer cancel()
			if err := grantTourAppControl(ctx, "tour-test", node); err == nil {
				t.Fatal("accepted unsafe or unacknowledged control")
			}
			wantGrants := int32(0)
			if slices.Contains([]string{"grant refused", "wrong ack gid", "wrong ack epoch", "unarmed after grant", "owner changed", "epoch changed", "identity changed"}, mode) {
				wantGrants = 1
			}
			if grants.Load() != wantGrants {
				t.Fatalf("grants=%d want=%d", grants.Load(), wantGrants)
			}
		})
	}
}

func TestTourAppRejectsConflictingLaunchArgumentsBeforeDeploy(t *testing.T) {
	for _, args := range [][]string{
		{"--autostart"}, {"--auto"}, {"--autostart=true"},
		{"--ros-args", "-r", "__node:=other"}, {"--ros-args", "-r", "__name:=other"},
		{"--ros-args", "-r", "__ns:=/other"},
		{"--ros-args", "-r", "/patrol/start:=/other/start"}, {"--ros-args", "-r", "patrol/stop:=/other/stop"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			path, err := scaffoldTourApp(t.TempDir(), "patrol")
			if err != nil {
				t.Fatal(err)
			}
			manifest := filepath.Join(path, "wendy.json")
			data, _ := json.Marshal(map[string]any{"appId": "dev.student.robot", "version": "0.1.0", "platform": "linux", "run": map[string]any{"args": args}})
			if err := os.WriteFile(manifest, data, 0o644); err != nil {
				t.Fatal(err)
			}
			oldConnect := connectTourAppSimulatorFn
			t.Cleanup(func() { connectTourAppSimulatorFn = oldConnect })
			connectTourAppSimulatorFn = func(context.Context, *simulatorChoice, bool) (*SelectedDevice, error) {
				t.Fatal("invalid launch arguments reached deployment")
				return nil, nil
			}
			if err := runTourApp(context.Background(), "tour-test", path); err == nil || !strings.Contains(err.Error(), "run.args") {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestTourAppDeploymentSnapshotSurvivesManifestEdits(t *testing.T) {
	path, err := scaffoldTourApp(t.TempDir(), "patrol")
	if err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(path, "wendy.json")
	cfg, err := appconfig.LoadFromFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	writeConfig := func() {
		t.Helper()
		data, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(manifest, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg.AppID = "dev.student.first"
	cfg.Run = &appconfig.RunConfig{Args: []string{"--laps", "2"}}
	cfg.Entitlements = append(cfg.Entitlements, appconfig.Entitlement{Type: appconfig.EntitlementHTTP, Port: 8905})
	writeConfig()
	app, err := prepareTourAppDeployment("tour-test", "patrol", path)
	if err != nil {
		t.Fatal(err)
	}
	firstNode := tourAppNodeName("tour-test", "patrol", "dev.student.first")
	if app.nodeName != firstNode {
		t.Fatalf("captured node=%s", app.nodeName)
	}
	// An editor saves a new identity, launch arguments and HTTP port after
	// preparation, before the deployment pipeline receives its configuration.
	cfg.AppID = "dev.student.next"
	cfg.Run.Args[1] = "3"
	cfg.Entitlements[len(cfg.Entitlements)-1].Port = 8906
	writeConfig()

	conn := stubTourAppConnection(t)
	oldRun, oldStop, oldService, oldPort := runTourAppWithAgentFn, stopTourAppContainerFn, callTourAppServiceFn, tourAppPortFn
	t.Cleanup(func() {
		runTourAppWithAgentFn, stopTourAppContainerFn, callTourAppServiceFn, tourAppPortFn = oldRun, oldStop, oldService, oldPort
	})
	runCalls, serviceCalls, portLookups := 0, 0, 0
	runTourAppWithAgentFn = func(_ context.Context, got *grpcclient.AgentConnection, cwd string, deployed *appconfig.AppConfig, opts runOptions) error {
		runCalls++
		port, found := httpEntitlementPort(deployed.Entitlements)
		if got != conn || cwd != path || deployed.AppID != "dev.student.first" || !found || port != 8905 ||
			!slices.Equal(deployed.Run.Args, []string{"--laps", "2"}) ||
			!slices.Contains(opts.userArgs, "__node:="+firstNode) ||
			!slices.Contains(opts.userArgs, "/patrol/start:=/"+firstNode+"/patrol/start") {
			t.Fatalf("deployment reloaded or mixed the manifest: cfg=%+v opts=%+v", deployed, opts)
		}
		// Normalization downstream must not mutate the captured snapshot.
		deployed.AppID = "dev.normalized.copy"
		deployed.Run.Args[1] = "4"
		deployed.Entitlements[len(deployed.Entitlements)-1].Port = 8907
		opts.userArgs[1] = "5"
		return nil
	}
	var armed, stopped, released atomic.Bool
	stopTourAppContainerFn = func(_ context.Context, got *grpcclient.AgentConnection, appID string) error {
		if got != conn || appID != "dev.student.first" {
			t.Fatalf("cleanup retargeted to %s", appID)
		}
		stopped.Store(true)
		return nil
	}
	callTourAppServiceFn = func(_ context.Context, got *grpcclient.AgentConnection, request *agentpbv2.CallROS2ServiceRequest) (*agentpbv2.CallROS2ServiceResponse, error) {
		serviceCalls++
		if got != conn || request.GetService() != "/"+firstNode+"/patrol/start" {
			t.Fatalf("control retargeted to %s", request.GetService())
		}
		return &agentpbv2.CallROS2ServiceResponse{Success: true, Response: "success: true"}, nil
	}
	stubTourAppHTTP(t, func(w http.ResponseWriter, r *http.Request, profile vm.RobotProfile) {
		switch r.URL.Path {
		case "/api/status":
			state := tourAppTestStatus(profile, firstNode)
			state.Armed = armed.Load()
			if state.Armed {
				state.ROSCommands.Owner = strings.Repeat("a", 48)
			}
			if stopped.Load() {
				state.ROSCommands.Sources[0].AgeMS = 500
			}
			_ = json.NewEncoder(w).Encode(state)
		case "/api/arm_ros":
			var request map[string]string
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request["publisher_gid"] != strings.Repeat("a", 48) {
				t.Errorf("incorrect grant %v err=%v", request, err)
			}
			armed.Store(true)
			_ = json.NewEncoder(w).Encode(map[string]any{"publisher_gid": strings.Repeat("a", 48), "epoch": 7})
		case "/api/disarm_ros":
			if !stopped.Load() {
				t.Error("released before stopping the captured app")
			}
			released.Store(true)
			armed.Store(false)
			_ = json.NewEncoder(w).Encode(map[string]bool{"released": true})
		default:
			t.Errorf("unexpected endpoint %s", r.URL.Path)
			http.NotFound(w, r)
		}
	})
	appServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	t.Cleanup(appServer.Close)
	serverURL, _ := url.Parse(appServer.URL)
	forwardedPort, _ := strconv.Atoi(serverURL.Port())
	tourAppPortFn = func(_ context.Context, vmName string, guestPort int) (int, error) {
		portLookups++
		if vmName != "tour-test" || guestPort != 8905 {
			t.Fatalf("URL retargeted to %s:%d", vmName, guestPort)
		}
		return forwardedPort, nil
	}
	ctx := context.Background()
	if err := app.deploy(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := controlTourApp(ctx, app.vmName, app.kind, app.nodeName, "start"); err != nil {
		t.Fatal(err)
	}
	if got, err := app.url(ctx); err != nil || got != appServer.URL {
		t.Fatalf("URL=%s err=%v", got, err)
	}
	if err := app.cleanup(ctx); err != nil {
		t.Fatal(err)
	}
	if runCalls != 1 || serviceCalls != 1 || portLookups != 1 || !stopped.Load() || !released.Load() {
		t.Fatalf("run=%d services=%d ports=%d stopped=%t released=%t", runCalls, serviceCalls, portLookups, stopped.Load(), released.Load())
	}
	next, err := prepareTourAppDeployment("tour-test", "patrol", path)
	if err != nil {
		t.Fatal(err)
	}
	nextPort, _ := httpEntitlementPort(next.cfg.Entitlements)
	if next.cfg.AppID != "dev.student.next" || next.nodeName == app.nodeName || nextPort != 8906 || next.cfg.Run.Args[1] != "3" || app.cfg.Run.Args[1] != "2" {
		t.Fatal("the next attempt did not capture the edited manifest independently")
	}
}
