package commands

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/vm"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	g1bundle "github.com/wendylabsinc/wendy/go/simulator/g1"
	go2bundle "github.com/wendylabsinc/wendy/go/simulator/go2"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"
)

func robotTestStore(t *testing.T) *vm.Store {
	t.Helper()
	store := &vm.Store{Root: t.TempDir()}
	previous := robotVMStore
	robotVMStore = func() (*vm.Store, error) { return store, nil }
	t.Cleanup(func() { robotVMStore = previous })
	return store
}

func robotTestProfile(t *testing.T, store *vm.Store, name string) vm.RobotProfile {
	return robotTestProfileForKind(t, store, name, vm.RobotKindGo2)
}

func robotTestProfileForKind(t *testing.T, store *vm.Store, name, kind string) vm.RobotProfile {
	t.Helper()
	if err := os.MkdirAll(store.Dir(name), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.MetaPath(name), []byte(`{"imageVersion":"test"}`), 0600); err != nil {
		t.Fatal(err)
	}
	runtime, err := robotRuntimeForKind(kind)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := vm.NewRobotProfile(kind, runtime.sourceDigest(), runtime.policyBundle)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateRobotProfile(name, profile); err != nil {
		t.Fatal(err)
	}
	return profile
}

func robotTestStatus(name string, p vm.RobotProfile) robotRuntimeStatus {
	return robotRuntimeStatus{VMName: name, Simulation: true, ProfileVersion: p.Version,
		RobotKind: p.Kind, SourceDigest: p.SourceDigest, PolicyBundle: p.PolicyBundle,
		World: p.World, ClockMode: p.ClockMode, Seed: p.Seed, VisualDetail: p.VisualDetail,
		DDSIsolation: "udp-rtps-loopback", Healthy: true, Ready: true, Mode: "standing"}
}

func TestRobotStatusRejectsForeignRuntimeAndUnconfinedDDS(t *testing.T) {
	p, err := vm.NewGo2RobotProfile(go2bundle.SourceDigest(), go2PolicyBundle)
	if err != nil {
		t.Fatal(err)
	}
	baseline := robotTestStatus("one", p)
	if err := baseline.matches("one", p); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*robotRuntimeStatus){
		"other VM":                func(s *robotRuntimeStatus) { s.VMName = "two" },
		"physical source":         func(s *robotRuntimeStatus) { s.Simulation = false },
		"unknown profile":         func(s *robotRuntimeStatus) { s.ProfileVersion++ },
		"other robot":             func(s *robotRuntimeStatus) { s.RobotKind = "g1" },
		"changed source":          func(s *robotRuntimeStatus) { s.SourceDigest = "sha256:" + strings.Repeat("a", 64) },
		"changed policy":          func(s *robotRuntimeStatus) { s.PolicyBundle = "other" },
		"changed world":           func(s *robotRuntimeStatus) { s.World = "outdoor" },
		"changed clock":           func(s *robotRuntimeStatus) { s.ClockMode = "simulation" },
		"changed seed":            func(s *robotRuntimeStatus) { s.Seed++ },
		"changed renderer":        func(s *robotRuntimeStatus) { s.VisualDetail = "full" },
		"unmanaged DDS":           func(s *robotRuntimeStatus) { s.DDSIsolation = "unmanaged" },
		"missing DDS confinement": func(s *robotRuntimeStatus) { s.DDSIsolation = "" },
	} {
		t.Run(name, func(t *testing.T) {
			state := baseline
			change(&state)
			if err := state.matches("one", p); err == nil {
				t.Fatal("accepted a mismatched runtime")
			}
		})
	}
}

type robotTestTransport func(*http.Request) (*http.Response, error)

func (f robotTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func robotTestHTTP(t *testing.T, transport robotTestTransport) {
	t.Helper()
	previous := robotHTTPClient
	client := *previous
	client.Transport = transport
	robotHTTPClient = &client
	t.Cleanup(func() { robotHTTPClient = previous })
}

func TestRobotStatusProbesStayOnOwnedLoopbackEndpoint(t *testing.T) {
	for name, tc := range map[string]struct {
		code int
		body string
	}{
		"redirect":         {302, `{}`},
		"server failure":   {503, `{}`},
		"malformed":        {200, `{`},
		"multiple objects": {200, `{} {}`},
		"trailing junk":    {200, `{} garbage`},
		"oversized":        {200, `{"error":"` + strings.Repeat("x", 1<<20) + `"}`},
	} {
		t.Run(name, func(t *testing.T) {
			requests := 0
			robotTestHTTP(t, func(r *http.Request) (*http.Response, error) {
				requests++
				if r.Method != http.MethodGet || r.URL.String() != "http://127.0.0.1:18890/api/status" {
					t.Fatalf("unexpected status request: %s %s", r.Method, r.URL)
				}
				return &http.Response{StatusCode: tc.code, Header: http.Header{"Location": {"http://robot.local:8890/api/status"}}, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			})
			if _, err := readRobotStatus(context.Background(), 18890); err == nil {
				t.Fatal("accepted invalid status response")
			}
			if requests != 1 {
				t.Fatalf("status followed a redirect: %d requests", requests)
			}
		})
	}
}

func TestRobotReadinessRequiresCurrentHealthyIdentity(t *testing.T) {
	p, _ := vm.NewGo2RobotProfile(go2bundle.SourceDigest(), go2PolicyBundle)
	for name, change := range map[string]func(*robotRuntimeStatus){
		"ready":       func(*robotRuntimeStatus) {},
		"wrong robot": func(s *robotRuntimeStatus) { s.VMName = "other" },
		"fault":       func(s *robotRuntimeStatus) { s.Healthy, s.Ready, s.Error = false, false, "policy failed" },
		"paused":      func(s *robotRuntimeStatus) { s.Ready, s.Mode = false, "paused" },
	} {
		t.Run(name, func(t *testing.T) {
			state := robotTestStatus("one", p)
			change(&state)
			data, _ := json.Marshal(state)
			robotTestHTTP(t, func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(data)))}, nil
			})
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
			defer cancel()
			err := waitForRobot(ctx, "one", 18890, p)
			if (err == nil) != (name == "ready") {
				t.Fatalf("readiness returned %v", err)
			}
			if name == "paused" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("paused robot was not waited for: %v", err)
			}
		})
	}
}

func TestRobotROSAutoConfigurationPreservesManifestAndServices(t *testing.T) {
	cfg := &appconfig.AppConfig{AppID: "example.navigation",
		Frameworks:   &appconfig.FrameworksConfig{ROS2: &appconfig.ROS2Config{}},
		Entitlements: []appconfig.Entitlement{{Type: "camera"}},
		Services: map[string]*appconfig.ServiceConfig{
			"navigator":  {Context: "nav", Env: map[string]string{"ROLE": "navigation"}},
			"perception": {Context: "vision", Entitlements: []appconfig.Entitlement{{Type: "gpu"}}, Frameworks: &appconfig.FrameworksConfig{ROS2: &appconfig.ROS2Config{Distro: "Humble", RMW: "CycloneDDS"}}},
		},
	}
	before, _ := json.Marshal(cfg)
	got, err := normalizeRobotROSConfig(cfg, []string{"ROS_DOMAIN_ID=0", "ROS_LOCALHOST_ONLY=1"})
	if err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(cfg)
	if string(before) != string(after) {
		t.Fatal("normalization changed the source manifest")
	}
	for name, ros := range map[string]*appconfig.ROS2Config{"app": got.GetROS2Config(), "navigator": got.ResolveROS2ConfigForService("navigator"), "perception": got.ResolveROS2ConfigForService("perception")} {
		if ros.DomainID == nil || *ros.DomainID != 0 || ros.ResolvedRMW() != "rmw_cyclonedds_cpp" || ros.ResolvedDistro() != "humble" || ros.ResolvedDiscoveryScope() != "app" {
			t.Fatalf("%s resolved off the robot bus: %+v", name, ros)
		}
	}
	for name, ents := range map[string][]appconfig.Entitlement{"app": got.Entitlements, "navigator": got.Services["navigator"].Entitlements, "perception": got.Services["perception"].Entitlements} {
		if len(ents) != 2 || ents[1].Type != appconfig.EntitlementNetwork || ents[1].Mode != "host" {
			t.Fatalf("%s did not retain permissions and join guest networking: %+v", name, ents)
		}
	}
	if got.Services["navigator"].Context != "nav" || got.Services["perception"].Context != "vision" || got.Services["navigator"].Env["ROLE"] != "navigation" {
		t.Fatal("normalization lost service configuration")
	}
}

func TestRobotROSRejectsExplicitConflictsBeforeBuild(t *testing.T) {
	for name, change := range map[string]func(*appconfig.AppConfig){
		"domain":     func(c *appconfig.AppConfig) { n := 42; c.Frameworks.ROS2.DomainID = &n },
		"distro":     func(c *appconfig.AppConfig) { c.Frameworks.ROS2.Distro = "jazzy" },
		"middleware": func(c *appconfig.AppConfig) { c.Frameworks.ROS2.RMW = "fastdds" },
		"discovery":  func(c *appconfig.AppConfig) { c.Frameworks.ROS2.DiscoveryScope = "invalid" },
		"network": func(c *appconfig.AppConfig) {
			c.Entitlements = []appconfig.Entitlement{{Type: appconfig.EntitlementNetwork, Mode: "bridge"}}
		},
		"service environment": func(c *appconfig.AppConfig) {
			c.Services = map[string]*appconfig.ServiceConfig{"nav": {Context: ".", Env: map[string]string{"CYCLONEDDS_URI": "file:///etc/dds.xml"}}}
		},
		"service domain": func(c *appconfig.AppConfig) {
			n := 1
			c.Services = map[string]*appconfig.ServiceConfig{"nav": {Context: ".", Frameworks: &appconfig.FrameworksConfig{ROS2: &appconfig.ROS2Config{DomainID: &n}}}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := &appconfig.AppConfig{AppID: "example.nav", Frameworks: &appconfig.FrameworksConfig{ROS2: &appconfig.ROS2Config{}}}
			change(cfg)
			if _, err := normalizeRobotROSConfig(cfg, nil); err == nil {
				t.Fatal("accepted conflicting robot configuration")
			}
		})
	}
	for _, env := range []string{"ROS_DOMAIN_ID=42", "RMW_IMPLEMENTATION=rmw_fastrtps_cpp", "ROS_LOCALHOST_ONLY=0", "ROS_AUTOMATIC_DISCOVERY_RANGE=SUBNET", "CYCLONEDDS_URI=file:///etc/dds.xml", "ROS_DISCOVERY_SERVER=192.0.2.1:11811", "ROS_STATIC_PEERS=192.0.2.1", "FASTDDS_DEFAULT_PROFILES_FILE=/etc/dds.xml"} {
		t.Run(env, func(t *testing.T) {
			cfg := &appconfig.AppConfig{AppID: "example.nav", Frameworks: &appconfig.FrameworksConfig{ROS2: &appconfig.ROS2Config{}}}
			if _, err := normalizeRobotROSConfig(cfg, []string{env}); err == nil {
				t.Fatal("accepted conflicting run environment")
			}
		})
	}
}

func TestRobotProfileOnlyChangesDeclaredROSonManagedVM(t *testing.T) {
	store := robotTestStore(t)
	robotTestProfile(t, store, "robot")
	cfg := &appconfig.AppConfig{AppID: "example.web", Entitlements: []appconfig.Entitlement{{Type: "network", Mode: "bridge"}}, Env: map[string]string{"ROS_DOMAIN_ID": "7"}}
	for _, conn := range []*grpcclient.AgentConnection{nil, {Host: "robot.local"}, {SimulatorName: "generic"}, {SimulatorName: "robot"}} {
		got, err := prepareRobotAppConfig(conn, cfg, nil)
		if err != nil || !reflect.DeepEqual(got, cfg) {
			t.Fatalf("ordinary app changed: %+v %v", got, err)
		}
	}
	reserved := &appconfig.AppConfig{AppID: go2RuntimeAppID}
	if _, err := prepareRobotAppConfig(&grpcclient.AgentConnection{SimulatorName: "robot"}, reserved, nil); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("user deployment could replace robot runtime: %v", err)
	}
	if _, err := prepareRobotAppConfig(&grpcclient.AgentConnection{SimulatorName: "generic"}, reserved, nil); err != nil {
		t.Fatalf("reserved ID leaked outside managed profile: %v", err)
	}
}

func TestRobotProvisionLockSerializesSameVMAndAllowsDifferentVMs(t *testing.T) {
	store := robotTestStore(t)
	robotTestProfile(t, store, "one")
	robotTestProfile(t, store, "two")
	first, err := lockRobotProvision(context.Background(), store, "one")
	if err != nil {
		t.Fatal(err)
	}
	second, err := lockRobotProvision(context.Background(), store, "two")
	if err != nil {
		first()
		t.Fatal(err)
	}
	second()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if unexpected, err := lockRobotProvision(ctx, store, "one"); !errors.Is(err, context.DeadlineExceeded) {
		if unexpected != nil {
			unexpected()
		}
		first()
		t.Fatalf("same-VM provisioning did not serialize: %v", err)
	}
	first()
	last, err := lockRobotProvision(context.Background(), store, "one")
	if err != nil {
		t.Fatal(err)
	}
	last()
}

func TestRobotStartAndUpdateRejectGenericVMWithoutBooting(t *testing.T) {
	store := robotTestStore(t)
	for _, action := range []string{"start", "restart", "update"} {
		cmd := &cobra.Command{}
		cmd.SetContext(context.Background())
		if err := runVMRobot(cmd, action, "generic"); !errors.Is(err, vm.ErrRobotProfileMissing) {
			t.Fatalf("%s ordinary VM: %v", action, err)
		}
	}
	entries, err := os.ReadDir(store.Root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("robot action created VM files: %v %v", entries, err)
	}
}

func TestRobotConfigureAttachesPinnedProfileWithoutTouchingExistingVM(t *testing.T) {
	for _, running := range []bool{false, true} {
		t.Run(map[bool]string{false: "stopped", true: "running"}[running], func(t *testing.T) {
			store := robotTestStore(t)
			if err := os.MkdirAll(store.Dir("existing"), 0700); err != nil {
				t.Fatal(err)
			}
			files := map[string]string{
				store.MetaPath("existing"): `{"imageVersion":"test","hostname":"keep-me","agentPort":50053}`,
				store.DiskPath("existing"): "existing VM disk contents",
				store.VarsPath("existing"): "existing EFI variables",
			}
			if running {
				files[store.StatePath("existing")] = `{"pid":123456,"agentPort":50053,"cpus":6,"memoryMiB":8192}`
			}
			for path, contents := range files {
				if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
					t.Fatal(err)
				}
			}
			robotTestHTTP(t, func(*http.Request) (*http.Response, error) {
				t.Fatal("configuration contacted a robot endpoint")
				return nil, errors.New("unexpected request")
			})
			cmd := newVMRobotCmd()
			cmd.SetArgs([]string{"configure", "existing"})
			cmd.SetOut(io.Discard)
			if err := cmd.ExecuteContext(context.Background()); err != nil {
				t.Fatal(err)
			}
			profile, exists, err := store.ReadRobotProfile("existing")
			if err != nil || !exists || profile.Kind != vm.RobotKindGo2 || profile.SourceDigest != go2bundle.SourceDigest() || profile.PolicyBundle != go2PolicyBundle {
				t.Fatalf("configuration did not pin the embedded robot: %+v %t %v", profile, exists, err)
			}
			if profile.SandboxHostPort != 0 || profile.RuntimeDigest != "" {
				t.Fatalf("configuration invented a live runtime: %+v", profile)
			}
			for path, contents := range files {
				if got, err := os.ReadFile(path); err != nil || string(got) != contents {
					t.Fatalf("configuration changed %s: %q %v", path, got, err)
				}
			}
			for _, path := range []string{store.QMPPath("existing"), filepath.Join(store.Dir("existing"), "robot-provision.lock")} {
				if _, err := os.Lstat(path); !os.IsNotExist(err) {
					t.Fatalf("configuration started provisioning: %s: %v", path, err)
				}
			}
			if !running {
				if _, err := os.Lstat(store.StatePath("existing")); !os.IsNotExist(err) {
					t.Fatalf("configuration booted the stopped VM: %v", err)
				}
			}
		})
	}
}

func TestRobotConfigureRefusesExistingProfilesWithoutReplacingThem(t *testing.T) {
	for _, malformed := range []bool{false, true} {
		t.Run(map[bool]string{false: "pinned", true: "malformed"}[malformed], func(t *testing.T) {
			store := robotTestStore(t)
			robotTestProfile(t, store, "existing")
			if malformed {
				if err := os.WriteFile(store.RobotProfilePath("existing"), []byte(`{"version":999}`), 0600); err != nil {
					t.Fatal(err)
				}
			} else if err := store.UpdateRobotProfile("existing", func(p *vm.RobotProfile) error {
				p.SourceDigest = "sha256:" + strings.Repeat("a", 64)
				p.Seed, p.SandboxHostPort = 42, 18890
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(store.RobotProfilePath("existing"))
			err := runVMRobot(&cobra.Command{}, "configure", "existing")
			if err == nil || (!malformed && !errors.Is(err, vm.ErrRobotProfileExists)) {
				t.Fatalf("configuration did not reject existing profile: %v", err)
			}
			after, _ := os.ReadFile(store.RobotProfilePath("existing"))
			if string(before) != string(after) {
				t.Fatal("configuration replaced an existing profile")
			}
		})
	}
}

func TestRobotConfigureRejectsMissingVMAndInvalidNameWithoutCreatingFiles(t *testing.T) {
	store := robotTestStore(t)
	for _, name := range []string{"missing", "../escape"} {
		if err := runVMRobot(&cobra.Command{}, "configure", name); err == nil {
			t.Fatalf("configuration accepted %q", name)
		}
	}
	entries, err := os.ReadDir(store.Root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("invalid configuration created files: %v %v", entries, err)
	}
}

func TestRobotConfigureG1SelectsItsOwnBundleAndPreservesOtherVMs(t *testing.T) {
	store := robotTestStore(t)
	go2 := robotTestProfile(t, store, "go2-vm")
	before, _ := os.ReadFile(store.RobotProfilePath("go2-vm"))
	if err := os.MkdirAll(store.Dir("humanoid"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.MetaPath("humanoid"), []byte(`{"imageVersion":"existing-image"}`), 0600); err != nil {
		t.Fatal(err)
	}
	robotTestHTTP(t, func(*http.Request) (*http.Response, error) {
		t.Fatal("configuration contacted a runtime")
		return nil, errors.New("unexpected request")
	})
	cmd := newVMRobotCmd()
	cmd.SetArgs([]string{"configure", "humanoid", "--profile", "g1"})
	cmd.SetOut(io.Discard)
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	profile, exists, err := store.ReadRobotProfile("humanoid")
	if err != nil || !exists || profile.Kind != vm.RobotKindG1 || profile.SourceDigest != g1bundle.SourceDigest() || profile.PolicyBundle != "g1-29dof-velocity-v0-4960b847-v1" {
		t.Fatalf("G1 configuration used the wrong runtime bundle: %+v %t %v", profile, exists, err)
	}
	if profile.SourceDigest == go2.SourceDigest || profile.SandboxHostPort != 0 || profile.RuntimeDigest != "" {
		t.Fatalf("G1 reused Go2 provenance or invented a live runtime: %+v", profile)
	}
	after, _ := os.ReadFile(store.RobotProfilePath("go2-vm"))
	if string(before) != string(after) {
		t.Fatal("configuring G1 changed another VM's Go2 profile")
	}
	info := readSimulatorRobots(context.Background(), []vm.Status{{Name: "go2-vm"}, {Name: "humanoid"}})
	if info["go2-vm"].Kind != "Unitree Go2" || info["humanoid"].Kind != "Unitree G1" || info["humanoid"].State != "stopped" {
		t.Fatalf("picker mislabeled robot kinds: %+v", info)
	}
	if _, err := os.Lstat(store.StatePath("humanoid")); !os.IsNotExist(err) {
		t.Fatalf("configuration or picker booted G1: %v", err)
	}
	for _, kind := range []string{"generic", "unknown"} {
		cmd := newVMRobotCmd()
		cmd.SetArgs([]string{"configure", "humanoid", "--profile", kind})
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		if err := cmd.ExecuteContext(context.Background()); err == nil || !strings.Contains(err.Error(), "unsupported robot kind") {
			t.Fatalf("robot configure accepted %q: %v", kind, err)
		}
	}
}

func TestRobotBundleEnvironmentRetainsGo2AndSeparatesG1(t *testing.T) {
	for _, kind := range []string{vm.RobotKindGo2, vm.RobotKindG1} {
		t.Run(kind, func(t *testing.T) {
			runtime, err := robotRuntimeForKind(kind)
			if err != nil {
				t.Fatal(err)
			}
			profile, err := vm.NewRobotProfile(kind, runtime.sourceDigest(), runtime.policyBundle)
			if err != nil {
				t.Fatal(err)
			}
			profile.Seed, profile.VisualDetail = 42, "full"
			prefix := map[string]string{"go2": "GO2_", "g1": "G1_"}[kind]
			want := []string{prefix + "VM_NAME=robot", prefix + "SOURCE_DIGEST=" + profile.SourceDigest,
				prefix + "WORLD=indoor", prefix + "SEED=42", prefix + "VISUAL_DETAIL=full"}
			if kind == vm.RobotKindGo2 {
				want = append(want, "GO2_AUTO_APP_CONTROL=1")
			}
			if got := runtime.environment("robot", profile); !reflect.DeepEqual(got, want) {
				t.Fatalf("runtime environment changed the profile contract: got %v want %v", got, want)
			}
			state := robotTestStatus("robot", profile)
			if err := state.matches("robot", profile); err != nil {
				t.Fatal(err)
			}
			state.RobotKind = map[string]string{"go2": "g1", "g1": "go2"}[kind]
			if err := state.matches("robot", profile); err == nil {
				t.Fatal("accepted another robot kind with copied profile identity")
			}
		})
	}
}

func TestRobotUpdateSelectsOnlyThePersistedKindsSource(t *testing.T) {
	for _, kind := range []string{vm.RobotKindGo2, vm.RobotKindG1} {
		t.Run(kind, func(t *testing.T) {
			store := robotTestStore(t)
			profile := robotTestProfileForKind(t, store, "robot", kind)
			if err := store.UpdateRobotProfile("robot", func(p *vm.RobotProfile) error {
				p.SourceDigest, p.PolicyBundle = "sha256:"+strings.Repeat("a", 64), "old-bundle"
				p.RuntimeDigest, p.Seed = "sha256:"+strings.Repeat("b", 64), 42
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			conn := robotTestRunningVM(t, "robot", profile)
			expected := errors.New("stop at verified port mapping before deployment")
			robotSandboxPort = func(*vm.Store, context.Context, string) (int, error) { return 0, expected }
			if err := reconcileRobot(context.Background(), conn, true); !errors.Is(err, expected) {
				t.Fatalf("kind-specific update failed before resource/capability checks completed: %v", err)
			}
			got, _, err := store.ReadRobotProfile("robot")
			if err != nil || got.Kind != kind || got.SourceDigest != profile.SourceDigest || got.PolicyBundle != profile.PolicyBundle || got.RuntimeDigest != "" || got.Seed != 42 {
				t.Fatalf("update changed kind or pinned another robot's source: %+v %v", got, err)
			}
		})
	}
}

func TestRobotKindsReserveBothRuntimeIDsAndSelectOnlyTheirOwnContainer(t *testing.T) {
	for _, kind := range []string{vm.RobotKindGo2, vm.RobotKindG1} {
		t.Run(kind, func(t *testing.T) {
			store := robotTestStore(t)
			robotTestProfileForKind(t, store, "robot", kind)
			runtime, err := robotRuntimeForKind(kind)
			if err != nil {
				t.Fatal(err)
			}
			for _, appID := range []string{go2RuntimeAppID, g1RuntimeAppID} {
				conn := &grpcclient.AgentConnection{SimulatorName: "robot"}
				cfg := &appconfig.AppConfig{AppID: appID}
				if _, err := prepareRobotAppConfig(conn, cfg, nil); err == nil || !strings.Contains(err.Error(), "reserved") {
					t.Fatalf("user deployment could claim managed app %s: %v", appID, err)
				}
				client := &robotRestartClient{app: &agentpb.AppContainer{AppName: appID}}
				conn.ContainerService = client
				selected, err := findRobotContainer(context.Background(), conn, runtime.appID)
				if err != nil || (selected != nil) != (appID == runtime.appID) {
					t.Fatalf("%s selected foreign managed container %s: %+v %v", kind, appID, selected, err)
				}
			}
		})
	}
}

func TestRobotG1RejectsGo2CapabilityBeforeLifecycleOperations(t *testing.T) {
	store := robotTestStore(t)
	profile := robotTestProfileForKind(t, store, "robot", vm.RobotKindG1)
	conn := robotTestRunningVM(t, "robot", profile)
	info := conn.AgentService.(*fakeAgentVersionClient).resp
	info.Featureset = []string{"go2-virtual-robot"}
	robotSandboxPort = func(*vm.Store, context.Context, string) (int, error) {
		t.Fatal("G1 used Go2 capability to reach port mapping")
		return 0, nil
	}
	for _, action := range []func() error{
		func() error { return reconcileRobot(context.Background(), conn, false) },
		func() error { return restartRobot(context.Background(), conn) },
	} {
		if err := action(); err == nil || !strings.Contains(err.Error(), "g1-virtual-robot") {
			t.Fatalf("G1 accepted Go2-only agent support: %v", err)
		}
	}
	info.Featureset = append(info.Featureset, "g1-virtual-robot")
	if err := requireRobotAgentCapability(context.Background(), conn, vm.RobotKindG1); err != nil {
		t.Fatalf("G1 rejected an explicitly capable agent: %v", err)
	}
	if err := store.UpdateRobotProfile("robot", func(p *vm.RobotProfile) error { p.SourceDigest = go2bundle.SourceDigest(); return nil }); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"start", "restart"} {
		cmd := &cobra.Command{}
		cmd.SetContext(context.Background())
		if err := runVMRobot(cmd, action, "robot"); err == nil || !strings.Contains(err.Error(), "different Unitree G1 runtime source") {
			t.Fatalf("%s accepted the Go2 source for a G1 profile: %v", action, err)
		}
	}
}

func TestSimulatorRobotRefreshIsReadOnlyAndSeparatesPowerFromReadiness(t *testing.T) {
	store := robotTestStore(t)
	p := robotTestProfile(t, store, "robot")
	before, _ := os.ReadFile(store.RobotProfilePath("robot"))
	statuses := []vm.Status{{Name: "generic", Exists: true}, {Name: "robot", Exists: true}}
	info := readSimulatorRobots(context.Background(), statuses)
	rows := simulatorRowsWithRobots(statuses, info)
	if rows[0].Size != "Generic" || rows[0].Parameters != "" || rows[1].Size != "Unitree Go2" || rows[1].Parameters != "stopped" {
		t.Fatalf("independent profile/readiness not reflected: %+v", rows)
	}
	if got := rows[1].Value.(*simulatorChoice); got.Name != "robot" || got.Create || got.Address != "" {
		t.Fatalf("refresh invented running robot endpoint: %+v", got)
	}
	after, _ := os.ReadFile(store.RobotProfilePath("robot"))
	if string(before) != string(after) {
		t.Fatal("refresh changed desired profile")
	}
	for _, path := range []string{store.StatePath("robot"), filepath.Join(store.Dir("robot"), "robot-provision.lock")} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("refresh started provisioning: %s: %v", path, err)
		}
	}
	resources, err := simulatorResources(store, "robot")
	if err != nil || resources.cpus != p.CPUs || resources.memoryMiB != p.MemoryMiB {
		t.Fatalf("robot restart lost resources: %+v %v", resources, err)
	}
	if err := store.UpdateRobotProfile("robot", func(p *vm.RobotProfile) error { p.CPUs, p.MemoryMiB = 6, 8192; return nil }); err != nil {
		t.Fatal(err)
	}
	resources, err = simulatorResources(store, "robot")
	if err != nil || resources.cpus != 6 || resources.memoryMiB != 8192 {
		t.Fatalf("robot restart ignored persisted resources: %+v %v", resources, err)
	}
}

func TestRobotReconcileDoesNotSilentlyUpgradePinnedSource(t *testing.T) {
	store := robotTestStore(t)
	robotTestProfile(t, store, "robot")
	if err := store.UpdateRobotProfile("robot", func(p *vm.RobotProfile) error { p.SourceDigest = "sha256:" + strings.Repeat("a", 64); return nil }); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(store.RobotProfilePath("robot"))
	// No live agent, QEMU or container clients: a changed source must be
	// rejected before consulting any deployment endpoint.
	err := reconcileRobot(context.Background(), &grpcclient.AgentConnection{SimulatorName: "robot"}, false)
	if err == nil || !strings.Contains(err.Error(), "wendy vm robot update robot") {
		t.Fatalf("changed source was not made explicit: %v", err)
	}
	after, _ := os.ReadFile(store.RobotProfilePath("robot"))
	if string(before) != string(after) {
		t.Fatal("normal connection updated pinned robot source")
	}
}

func robotTestRunningVM(t *testing.T, name string, profile vm.RobotProfile) *grpcclient.AgentConnection {
	t.Helper()
	previousStatuses, previousPort := vmStatusesFn, robotSandboxPort
	t.Cleanup(func() { vmStatusesFn, robotSandboxPort = previousStatuses, previousPort })
	vmStatusesFn = func() ([]vm.Status, error) {
		return []vm.Status{{Name: name, Running: true, State: vm.State{
			NetMode: vm.NetUser, AgentPort: 50053, CPUs: profile.CPUs, MemoryMiB: profile.MemoryMiB,
		}}}, nil
	}
	robotSandboxPort = func(_ *vm.Store, _ context.Context, got string) (int, error) {
		if got != name {
			t.Fatalf("mapped another VM: %s", got)
		}
		return 18890, nil
	}
	deviceType := "vm-arm64"
	return &grpcclient.AgentConnection{SimulatorName: name, Addr: "127.0.0.1:50053",
		AgentService: &fakeAgentVersionClient{resp: &agentpb.GetAgentVersionResponse{
			// GetAgentVersion reports the WendyOS distribution, although the
			// agent's kernel platform/runtime.GOOS is Linux.
			Os: "wendyos", DeviceType: &deviceType, Featureset: []string{profile.Kind + "-virtual-robot"},
		}},
	}
}

type robotRestartClient struct {
	agentpb.WendyContainerServiceClient
	app           *agentpb.AppContainer
	calls         []string
	stopErr       error
	startErr      error
	secondListErr error
	onStart       func(*agentpb.StartContainerRequest)
}

func (f *robotRestartClient) ListContainers(context.Context, *agentpb.ListContainersRequest, ...grpc.CallOption) (grpc.ServerStreamingClient[agentpb.ListContainersResponse], error) {
	f.calls = append(f.calls, "list")
	if len(f.calls) == 2 && f.secondListErr != nil {
		return nil, f.secondListErr
	}
	return &fakeListContainersStream{resp: &agentpb.ListContainersResponse{Container: f.app}}, nil
}

func (f *robotRestartClient) StopContainer(_ context.Context, req *agentpb.StopContainerRequest, _ ...grpc.CallOption) (*agentpb.StopContainerResponse, error) {
	f.calls = append(f.calls, "stop:"+req.GetAppName())
	return &agentpb.StopContainerResponse{}, f.stopErr
}

func (f *robotRestartClient) StartContainer(_ context.Context, req *agentpb.StartContainerRequest, _ ...grpc.CallOption) (grpc.ServerStreamingClient[agentpb.RunContainerLayersResponse], error) {
	f.calls = append(f.calls, "start:"+req.GetAppName())
	if f.onStart != nil {
		f.onStart(req)
	}
	return &robotStartedStream{}, f.startErr
}

type robotStartedStream struct {
	grpc.ServerStreamingClient[agentpb.RunContainerLayersResponse]
}

func (*robotStartedStream) Recv() (*agentpb.RunContainerLayersResponse, error) {
	return &agentpb.RunContainerLayersResponse{ResponseType: &agentpb.RunContainerLayersResponse_Started_{Started: &agentpb.RunContainerLayersResponse_Started{}}}, nil
}

func TestRobotRestartRecoversUnhealthyRuntimeAndSerializesProvisioning(t *testing.T) {
	for _, kind := range []string{vm.RobotKindGo2, vm.RobotKindG1} {
		t.Run(kind, func(t *testing.T) {
			store := robotTestStore(t)
			profile := robotTestProfileForKind(t, store, "robot", kind)
			runtime, err := robotRuntimeForKind(kind)
			if err != nil {
				t.Fatal(err)
			}
			conn := robotTestRunningVM(t, "robot", profile)
			state := robotTestStatus("robot", profile)
			state.Healthy, state.Ready, state.Error = false, false, "renderer failed"
			client := &robotRestartClient{app: &agentpb.AppContainer{AppName: runtime.appID, AppVersion: robotAppVersion(profile)}}
			client.onStart = func(req *agentpb.StartContainerRequest) {
				if req.GetRestartPolicy().GetMode() != agentpb.RestartPolicyMode_UNLESS_STOPPED {
					t.Fatal("restart lost persisted automatic restart behavior")
				}
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
				defer cancel()
				if unlock, err := lockRobotProvision(ctx, store, "robot"); !errors.Is(err, context.DeadlineExceeded) {
					if unlock != nil {
						unlock()
					}
					t.Fatalf("restart released the provisioning lock before start completed: %v", err)
				}
				state = robotTestStatus("robot", profile)
			}
			conn.ContainerService = client
			robotTestHTTP(t, func(*http.Request) (*http.Response, error) {
				body, _ := json.Marshal(state)
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
			})
			before, _ := os.ReadFile(store.RobotProfilePath("robot"))
			if err := restartRobot(context.Background(), conn); err != nil {
				t.Fatal(err)
			}
			want := []string{"list", "stop:" + runtime.appID, "start:" + runtime.appID}
			if !reflect.DeepEqual(client.calls, want) {
				t.Fatalf("restart operations = %v, want %v", client.calls, want)
			}
			after, _ := os.ReadFile(store.RobotProfilePath("robot"))
			if string(before) != string(after) {
				t.Fatal("restart changed the pinned profile")
			}
		})
	}
}

func TestRobotRestartRejectsMismatchesAndPropagatesLifecycleFailures(t *testing.T) {
	for _, scenario := range []string{"wrong version", "foreign endpoint", "stop failure", "start failure", "foreign restarted endpoint", "not ready"} {
		t.Run(scenario, func(t *testing.T) {
			store := robotTestStore(t)
			profile := robotTestProfile(t, store, "robot")
			conn := robotTestRunningVM(t, "robot", profile)
			state := robotTestStatus("robot", profile)
			client := &robotRestartClient{app: &agentpb.AppContainer{AppName: go2RuntimeAppID, AppVersion: robotAppVersion(profile)}}
			conn.ContainerService = client
			wantCalls := 3
			switch scenario {
			case "wrong version":
				client.app.AppVersion, wantCalls = "foreign", 1
			case "foreign endpoint":
				state.VMName, wantCalls = "another", 1
			case "stop failure":
				client.stopErr, wantCalls = errors.New("stop failed"), 2
			case "start failure":
				client.startErr = errors.New("start failed")
			case "foreign restarted endpoint":
				client.onStart = func(*agentpb.StartContainerRequest) { state.SourceDigest = "sha256:" + strings.Repeat("a", 64) }
			case "not ready":
				client.onStart = func(*agentpb.StartContainerRequest) { state.Ready = false }
			}
			robotTestHTTP(t, func(*http.Request) (*http.Response, error) {
				body, _ := json.Marshal(state)
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
			})
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
			defer cancel()
			if err := restartRobot(ctx, conn); err == nil {
				t.Fatal("restart reported success")
			}
			if len(client.calls) != wantCalls {
				t.Fatalf("unexpected lifecycle mutations: %v", client.calls)
			}
		})
	}
}

func TestRobotRestartMissingAppReconcilesWithoutTakingTheLockAgain(t *testing.T) {
	store := robotTestStore(t)
	profile := robotTestProfile(t, store, "robot")
	conn := robotTestRunningVM(t, "robot", profile)
	expected := errors.New("normal reconciliation reached the agent")
	client := &robotRestartClient{secondListErr: expected}
	conn.ContainerService = client
	robotTestHTTP(t, func(*http.Request) (*http.Response, error) { return nil, errors.New("runtime absent") })
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := restartRobot(ctx, conn); !errors.Is(err, expected) {
		t.Fatalf("absent runtime did not enter normal reconciliation: %v", err)
	}
	if !reflect.DeepEqual(client.calls, []string{"list", "list"}) {
		t.Fatalf("attempted to restart an absent app: %v", client.calls)
	}
}

func TestRobotResourceMismatchFailsBeforeContactingTheRuntime(t *testing.T) {
	for _, resources := range []struct{ cpus, memory int }{{2, 4096}, {4, 2048}, {0, 0}, {4, 4096}, {8, 8192}} {
		store := robotTestStore(t)
		profile := robotTestProfile(t, store, "robot")
		conn := robotTestRunningVM(t, "robot", profile)
		vmStatusesFn = func() ([]vm.Status, error) {
			return []vm.Status{{Name: "robot", Running: true, State: vm.State{NetMode: vm.NetUser, AgentPort: 50053, CPUs: resources.cpus, MemoryMiB: resources.memory}}}, nil
		}
		err := validateRobotVMResources(conn, profile)
		if resources.cpus >= profile.CPUs && resources.memory >= profile.MemoryMiB {
			if err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), "wendy vm stop robot") || !strings.Contains(err.Error(), "wendy vm robot start robot") {
			t.Fatalf("missing actionable resource error: %v", err)
		}
		// These connections have no container service: both paths must stop at
		// the resource gate, before HTTP/agent lifecycle operations can occur.
		if err := reconcileRobot(context.Background(), conn, false); err == nil {
			t.Fatal("normal reconciliation ignored insufficient resources")
		}
		if err := restartRobot(context.Background(), conn); err == nil {
			t.Fatal("explicit restart ignored insufficient resources")
		}
	}
}

func TestRobotStartAndRestartRejectChangedSourceBeforeBoot(t *testing.T) {
	store := robotTestStore(t)
	robotTestProfile(t, store, "robot")
	if err := store.UpdateRobotProfile("robot", func(p *vm.RobotProfile) error { p.SourceDigest = "sha256:" + strings.Repeat("a", 64); return nil }); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"start", "restart"} {
		if err := runVMRobot(&cobra.Command{}, action, "robot"); err == nil || !strings.Contains(err.Error(), "wendy vm robot update robot") {
			t.Fatalf("%s did not reject changed source before boot: %v", action, err)
		}
	}
	if _, err := os.Lstat(store.StatePath("robot")); !os.IsNotExist(err) {
		t.Fatalf("source mismatch booted the VM: %v", err)
	}
}

func TestRobotAgentCapabilityGateUsesExplicitSupportBeforeRuntimeOperations(t *testing.T) {
	for _, scenario := range []string{"old release", "dev without feature", "future version without feature", "supported old version", "physical host"} {
		t.Run(scenario, func(t *testing.T) {
			store := robotTestStore(t)
			profile := robotTestProfile(t, store, "robot")
			conn := robotTestRunningVM(t, "robot", profile)
			info := conn.AgentService.(*fakeAgentVersionClient).resp
			info.Version = "0.19.1"
			wantSupport := false
			switch scenario {
			case "old release":
				info.Featureset = nil
			case "dev without feature":
				info.Version, info.Featureset = "dev", nil
			case "future version without feature":
				info.Version, info.Featureset = "9999.0.0", nil
			case "supported old version":
				info.Version, wantSupport = "0.1.0", true
			case "physical host":
				deviceType := "unitree-go2"
				info.DeviceType = &deviceType
			}
			err := requireRobotAgentCapability(context.Background(), conn, vm.RobotKindGo2)
			if wantSupport {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "go2-virtual-robot") || !strings.Contains(err.Error(), "device update --binary") {
				t.Fatalf("unsupported agent lacks actionable update error: %v", err)
			}
			robotSandboxPort = func(*vm.Store, context.Context, string) (int, error) {
				t.Fatal("unsupported agent reached sandbox mapping")
				return 0, nil
			}
			// No ContainerService: the capability gate must precede builds,
			// typed inspection assumptions, and all runtime lifecycle calls.
			if err := reconcileRobot(context.Background(), conn, false); err == nil {
				t.Fatal("normal reconciliation accepted unsupported agent")
			}
			if err := restartRobot(context.Background(), conn); err == nil {
				t.Fatal("restart accepted unsupported agent")
			}
		})
	}
}

func TestRobotAgentCapabilityAcceptsTheReportedWendyOSVMContract(t *testing.T) {
	// This is the response shape from the actual WendyOS ARM64 VM. Using
	// Linux for both the kernel and the reported OS hid the rollout failure:
	// a capable VM agent was rejected before managed provisioning could run.
	const vmResponse = `{"version":"dev","os":"wendyos","cpuArchitecture":"arm64","deviceType":"vm-arm64","featureset":["go2-virtual-robot"]}`
	for _, scenario := range []string{"WendyOS VM", "Linux VM", "old WendyOS agent", "physical WendyOS device", "other kernel platform"} {
		t.Run(scenario, func(t *testing.T) {
			var info agentpb.GetAgentVersionResponse
			if err := protojson.Unmarshal([]byte(vmResponse), &info); err != nil {
				t.Fatal(err)
			}
			wantSupport := false
			switch scenario {
			case "WendyOS VM":
				wantSupport = true
			case "Linux VM":
				info.Os, wantSupport = "linux", true
			case "old WendyOS agent":
				info.Featureset = nil
			case "physical WendyOS device":
				deviceType := "jetson-orin-nano"
				info.DeviceType = &deviceType
			case "other kernel platform":
				info.Os = "darwin"
			}
			conn := &grpcclient.AgentConnection{SimulatorName: "go2-sim",
				AgentService: &fakeAgentVersionClient{resp: &info}}
			err := requireRobotAgentCapability(context.Background(), conn, vm.RobotKindGo2)
			if (err == nil) != wantSupport {
				t.Fatalf("reported OS %q, device %q, features %v: %v", info.GetOs(), info.GetDeviceType(), info.GetFeatureset(), err)
			}
		})
	}
}

func TestRobotAgentMaintenanceBypassIsLimitedToExplicitAgentUpdateCommands(t *testing.T) {
	previousChoice, previousDevice, previousStore := connectSimulatorChoiceFn, deviceFlag, robotVMStore
	t.Cleanup(func() {
		connectSimulatorChoiceFn, deviceFlag, robotVMStore = previousChoice, previousDevice, previousStore
	})
	t.Setenv("WENDY_AGENT_SOCKET", "")
	deviceFlag = "vm:robot"
	brokenProfile := errors.New("profile/runtime cannot be reconciled")
	robotVMStore = func() (*vm.Store, error) { return nil, brokenProfile }
	if err := reconcileSimulatorRobot(context.Background(), &grpcclient.AgentConnection{SimulatorName: "robot"}); !errors.Is(err, brokenProfile) {
		t.Fatalf("ordinary connections bypassed robot reconciliation: %v", err)
	}
	reachedVerifiedConnection := errors.New("named VM connection reached before agent upload")
	calls := 0
	connectSimulatorChoiceFn = func(ctx context.Context, choice *simulatorChoice, suppressUpdate bool) (*SelectedDevice, error) {
		calls++
		if choice.Name != "robot" || !suppressUpdate {
			t.Fatalf("agent maintenance lost named target/update behavior: %+v %t", choice, suppressUpdate)
		}
		if err := reconcileSimulatorRobot(ctx, &grpcclient.AgentConnection{SimulatorName: "robot"}); err != nil {
			t.Fatalf("agent update was blocked by the broken robot: %v", err)
		}
		return nil, reachedVerifiedConnection
	}
	binary := filepath.Join(t.TempDir(), "agent")
	if err := os.WriteFile(binary, []byte("not uploaded"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range []*cobra.Command{newDeviceUpdateCmd(), newDevicePushAgentCmd()} {
		cmd.SetContext(context.Background())
		if err := cmd.RunE(cmd, []string{binary}); !errors.Is(err, reachedVerifiedConnection) {
			t.Fatalf("%s could not reach the named VM maintenance path: %v", cmd.Name(), err)
		}
	}
	if calls != 2 {
		t.Fatalf("maintenance commands bypassed normal VM connection selection: %d", calls)
	}
}

func TestRobotROSNormalizesHardwareDiscoveryWithoutChangingSource(t *testing.T) {
	cfg := &appconfig.AppConfig{AppID: "example.go2", Frameworks: &appconfig.FrameworksConfig{ROS2: &appconfig.ROS2Config{DiscoveryScope: "host"}},
		Services: map[string]*appconfig.ServiceConfig{"driver": {Context: "driver", Frameworks: &appconfig.FrameworksConfig{ROS2: &appconfig.ROS2Config{DiscoveryScope: "host"}}}}}
	got, err := normalizeRobotROSConfig(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.GetROS2Config().ResolvedDiscoveryScope() != "app" || got.ResolveROS2ConfigForService("driver").ResolvedDiscoveryScope() != "app" {
		t.Fatal("hardware manifest escaped the guest loopback bus")
	}
	if cfg.GetROS2Config().ResolvedDiscoveryScope() != "host" || cfg.ResolveROS2ConfigForService("driver").ResolvedDiscoveryScope() != "host" {
		t.Fatal("source manifest was changed")
	}
}
