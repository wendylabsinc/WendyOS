package containerd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/board"
	"github.com/wendylabsinc/wendy/go/internal/agent/dbusproxy"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// TestRequireDBusProxyRefusesBluetoothWithoutProxy is the regression test for
// WDY-1093: a container that declares the bluetooth (D-Bus) entitlement must be
// refused when xdg-dbus-proxy is unavailable. Without the proxy there is no way
// to scope D-Bus to org.bluez, so starting the container would silently break
// bluetooth (or, in older builds, expose the unfiltered system bus). The agent
// must fail loudly instead of degrading silently.
func TestRequireDBusProxyRefusesBluetoothWithoutProxy(t *testing.T) {
	client := &Client{logger: zap.NewNop(), proxyManager: nil}
	cfg := &appconfig.AppConfig{Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementBluetooth}}}
	if err := client.requireDBusProxy(cfg, "demo-app"); err == nil {
		t.Fatal("expected error when bluetooth entitlement is declared but xdg-dbus-proxy is unavailable")
	}
}

func TestRequireDBusProxyAllowsBluetoothWithProxy(t *testing.T) {
	client := &Client{logger: zap.NewNop(), proxyManager: dbusproxy.NewManager(zap.NewNop())}
	cfg := &appconfig.AppConfig{Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementBluetooth}}}
	if err := client.requireDBusProxy(cfg, "demo-app"); err != nil {
		t.Fatalf("expected no error when xdg-dbus-proxy is available; got %v", err)
	}
}

func TestRequireDBusProxyAllowsNonBluetoothWithoutProxy(t *testing.T) {
	client := &Client{logger: zap.NewNop(), proxyManager: nil}
	cfg := &appconfig.AppConfig{Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementNetwork}}}
	if err := client.requireDBusProxy(cfg, "demo-app"); err != nil {
		t.Fatalf("expected no error for a non-bluetooth app without the proxy; got %v", err)
	}
}

// TestROS2SidecarName maps each RMW to a distinct, prefix-scoped sidecar name
// and collapses empty RMW to the CycloneDDS (config-default) sidecar — the
// basis for one persistent sidecar per RMW (WDY-1594, WDY-1703).
func TestROS2SidecarName(t *testing.T) {
	cyc := ros2SidecarName("rmw_cyclonedds_cpp")
	fast := ros2SidecarName("rmw_fastrtps_cpp")
	def := ros2SidecarName("")
	if cyc == fast {
		t.Errorf("distinct RMWs must map to distinct sidecars: %q == %q", cyc, fast)
	}
	if def != cyc {
		t.Errorf("empty RMW should map to the CycloneDDS (config-default) sidecar (WDY-1703): %q vs %q", def, cyc)
	}
	if got := ros2SidecarName("rmw_bogus"); got != ros2SidecarPrefix+"-default" {
		t.Errorf("unknown RMW = %q, want %q", got, ros2SidecarPrefix+"-default")
	}
	for _, n := range []string{cyc, fast, def} {
		if !strings.HasPrefix(n, ros2SidecarPrefix+"-") {
			t.Errorf("sidecar name %q lacks the expected prefix", n)
		}
	}
}

func TestCreateContainerProgressMappingUsesApplyPhase(t *testing.T) {
	progress := UnpackProgress{
		Phase:       "layer",
		LayerIndex:  2,
		TotalLayers: 5,
		LayerSize:   1234,
		Reused:      true,
	}

	got := toCreateContainerProgress(progress)

	if got.GetPhase() != agentpb.CreateContainerProgress_APPLYING_LAYER {
		t.Fatalf("phase = %v; want APPLYING_LAYER", got.GetPhase())
	}
	if got.GetLayerIndex() != 2 {
		t.Fatalf("layer index = %d; want 2", got.GetLayerIndex())
	}
	if got.GetTotalLayers() != 5 {
		t.Fatalf("total layers = %d; want 5", got.GetTotalLayers())
	}
	if got.GetLayerSize() != 1234 {
		t.Fatalf("layer size = %d; want 1234", got.GetLayerSize())
	}
	if !got.GetReusedSnapshot() {
		t.Fatal("expected reused snapshot to be true")
	}
}

func TestCreateContainerProgressMappingUsesUnpackingPhaseForStart(t *testing.T) {
	progress := UnpackProgress{
		Phase:       "start",
		TotalLayers: 3,
	}

	got := toCreateContainerProgress(progress)

	if got.GetPhase() != agentpb.CreateContainerProgress_UNPACKING {
		t.Fatalf("phase = %v; want UNPACKING", got.GetPhase())
	}
	if got.GetTotalLayers() != 3 {
		t.Fatalf("total layers = %d; want 3", got.GetTotalLayers())
	}
	if got.GetLayerIndex() != 0 {
		t.Fatalf("layer index = %d; want 0", got.GetLayerIndex())
	}
}

func TestCreateContainerProgressMappingUsesUnpackingPhaseForLayerStart(t *testing.T) {
	progress := UnpackProgress{
		Phase:       "layer-start",
		LayerIndex:  1,
		TotalLayers: 4,
		LayerSize:   2048,
	}

	got := toCreateContainerProgress(progress)

	if got.GetPhase() != agentpb.CreateContainerProgress_UNPACKING {
		t.Fatalf("phase = %v; want UNPACKING", got.GetPhase())
	}
	if got.GetLayerIndex() != 1 {
		t.Fatalf("layer index = %d; want 1", got.GetLayerIndex())
	}
	if got.GetTotalLayers() != 4 {
		t.Fatalf("total layers = %d; want 4", got.GetTotalLayers())
	}
	if got.GetLayerSize() != 2048 {
		t.Fatalf("layer size = %d; want 2048", got.GetLayerSize())
	}
}

func TestBuildContainerBaseEnvIncludesWendyHostname(t *testing.T) {
	old := deviceHostnameWithSuffix
	t.Cleanup(func() { deviceHostnameWithSuffix = old })
	deviceHostnameWithSuffix = func() string { return "wendyos-test-device.local" }

	env, err := buildContainerBaseEnv("demo-app", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := "WENDY_HOSTNAME=wendyos-test-device.local"
	for _, kv := range env {
		if kv == want {
			return
		}
	}
	t.Errorf("env missing %q; got %v", want, env)
}

func TestBuildContainerBaseEnvOmitsWendyHostnameWhenUnavailable(t *testing.T) {
	old := deviceHostnameWithSuffix
	t.Cleanup(func() { deviceHostnameWithSuffix = old })
	deviceHostnameWithSuffix = func() string { return "" }

	env, err := buildContainerBaseEnv("demo-app", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, kv := range env {
		if len(kv) >= len("WENDY_HOSTNAME=") && kv[:len("WENDY_HOSTNAME=")] == "WENDY_HOSTNAME=" {
			t.Errorf("env unexpectedly contains %q when device hostname is unresolvable", kv)
		}
	}
}

func TestBuildContainerBaseEnvIncludesAppID(t *testing.T) {
	old := deviceHostnameWithSuffix
	t.Cleanup(func() { deviceHostnameWithSuffix = old })
	deviceHostnameWithSuffix = func() string { return "" }

	env, err := buildContainerBaseEnv("demo-app", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := "WENDY_APP_ID=demo-app"
	for _, kv := range env {
		if kv == want {
			return
		}
	}
	t.Errorf("env missing %q; got %v", want, env)
}

func TestBuildContainerBaseEnvOmitsAppIDWhenEmpty(t *testing.T) {
	old := deviceHostnameWithSuffix
	t.Cleanup(func() { deviceHostnameWithSuffix = old })
	deviceHostnameWithSuffix = func() string { return "" }

	env, err := buildContainerBaseEnv("", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, kv := range env {
		if strings.HasPrefix(kv, "WENDY_APP_ID=") {
			t.Errorf("env unexpectedly contains %q when appID is empty", kv)
		}
	}
}

func TestBuildContainerBaseEnvMultiServiceHostname(t *testing.T) {
	old := deviceHostnameWithSuffix
	t.Cleanup(func() { deviceHostnameWithSuffix = old })
	deviceHostnameWithSuffix = func() string { return "wendyos-test-device.local" }

	env, err := buildContainerBaseEnv("com.example.app", "api")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	wantHostname := "WENDY_HOSTNAME=api.local"
	wantDeviceHostname := "WENDY_DEVICE_HOSTNAME=wendyos-test-device.local"
	wantGroup := "WENDY_APP_GROUP=com.example.app"
	wantAppID := "WENDY_APP_ID=com.example.app"
	foundHostname, foundDeviceHostname, foundGroup, foundAppID := false, false, false, false
	for _, kv := range env {
		switch kv {
		case wantHostname:
			foundHostname = true
		case wantDeviceHostname:
			foundDeviceHostname = true
		case wantGroup:
			foundGroup = true
		case wantAppID:
			foundAppID = true
		}
	}
	if !foundHostname {
		t.Errorf("env missing %q; got %v", wantHostname, env)
	}
	if !foundDeviceHostname {
		t.Errorf("env missing %q; got %v", wantDeviceHostname, env)
	}
	if !foundGroup {
		t.Errorf("env missing %q; got %v", wantGroup, env)
	}
	if !foundAppID {
		t.Errorf("env missing %q; got %v", wantAppID, env)
	}
}

func TestBuildContainerBaseEnvMultiServiceNoDeviceHostname(t *testing.T) {
	old := deviceHostnameWithSuffix
	t.Cleanup(func() { deviceHostnameWithSuffix = old })
	deviceHostnameWithSuffix = func() string { return "device.local" }

	// For multi-service containers the device hostname must not appear.
	env, err := buildContainerBaseEnv("com.example.app", "worker")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, kv := range env {
		if kv == "WENDY_HOSTNAME=device.local" {
			t.Errorf("multi-service env must not use device hostname; got %v", env)
		}
	}
}

// TestBuildContainerBaseEnvIdentityVars documents the full env-var contract
// for WENDY_HOSTNAME, WENDY_DEVICE_HOSTNAME, WENDY_APP_GROUP, and WENDY_APP_ID
// across both app types.
//
// Single-container app  (serviceName == ""):
//
//	WENDY_HOSTNAME        = device hostname (e.g. "wendyos-abc.local")
//	WENDY_DEVICE_HOSTNAME = device hostname (same as WENDY_HOSTNAME)
//	WENDY_APP_ID          = appID
//	(WENDY_APP_GROUP is not set)
//
// Multi-service app (serviceName != ""):
//
//	WENDY_HOSTNAME        = "{serviceName}.local"   ← distinct per service
//	WENDY_DEVICE_HOSTNAME = device hostname          ← always the host mDNS name
//	WENDY_APP_GROUP       = appID                   ← lets a service discover siblings
//	WENDY_APP_ID          = appID
func TestBuildContainerBaseEnvIdentityVars(t *testing.T) {
	const deviceHost = "wendyos-abc.local"
	old := deviceHostnameWithSuffix
	t.Cleanup(func() { deviceHostnameWithSuffix = old })
	deviceHostnameWithSuffix = func() string { return deviceHost }

	tests := []struct {
		name               string
		appID              string
		serviceName        string
		wantHostname       string
		wantDeviceHostname string
		wantGroup          string // "" means the var must NOT be present
		wantAppID          string
	}{
		{
			name:               "single-container: hostname is device, no app group",
			appID:              "com.example.myapp",
			serviceName:        "",
			wantHostname:       deviceHost,
			wantDeviceHostname: deviceHost,
			wantGroup:          "",
			wantAppID:          "com.example.myapp",
		},
		{
			name:               "multi-service api: hostname is serviceName.local, device hostname still set",
			appID:              "com.example.myapp",
			serviceName:        "api",
			wantHostname:       "api.local",
			wantDeviceHostname: deviceHost,
			wantGroup:          "com.example.myapp",
			wantAppID:          "com.example.myapp",
		},
		{
			name:               "multi-service worker: hostname is serviceName.local, device hostname still set",
			appID:              "com.example.myapp",
			serviceName:        "worker",
			wantHostname:       "worker.local",
			wantDeviceHostname: deviceHost,
			wantGroup:          "com.example.myapp",
			wantAppID:          "com.example.myapp",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env, err := buildContainerBaseEnv(tc.appID, tc.serviceName)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			envMap := make(map[string]string)
			for _, kv := range env {
				if i := strings.Index(kv, "="); i >= 0 {
					envMap[kv[:i]] = kv[i+1:]
				}
			}

			if got := envMap["WENDY_HOSTNAME"]; got != tc.wantHostname {
				t.Errorf("WENDY_HOSTNAME = %q, want %q", got, tc.wantHostname)
			}
			if got := envMap["WENDY_DEVICE_HOSTNAME"]; got != tc.wantDeviceHostname {
				t.Errorf("WENDY_DEVICE_HOSTNAME = %q, want %q", got, tc.wantDeviceHostname)
			}
			if tc.wantGroup == "" {
				if _, ok := envMap["WENDY_APP_GROUP"]; ok {
					t.Errorf("WENDY_APP_GROUP must not be set for single-container apps; got %q", envMap["WENDY_APP_GROUP"])
				}
			} else {
				if got := envMap["WENDY_APP_GROUP"]; got != tc.wantGroup {
					t.Errorf("WENDY_APP_GROUP = %q, want %q", got, tc.wantGroup)
				}
			}
			if got := envMap["WENDY_APP_ID"]; got != tc.wantAppID {
				t.Errorf("WENDY_APP_ID = %q, want %q", got, tc.wantAppID)
			}
		})
	}
}

// TestBuildContainerBaseEnvOmitsPathAndTerm guards against reintroducing
// generic PATH/TERM into the always-wins Wendy env: buildContainerBaseEnv is
// appended after the image env, so any PATH there clobbers image-defined
// values under OCI last-one-wins semantics (WDY-1825). PATH/TERM defaults
// belong in fallbackContainerEnv via appendFallbackEnv.
func TestBuildContainerBaseEnvOmitsPathAndTerm(t *testing.T) {
	old := deviceHostnameWithSuffix
	t.Cleanup(func() { deviceHostnameWithSuffix = old })
	deviceHostnameWithSuffix = func() string { return "device.local" }

	env, err := buildContainerBaseEnv("demo-app", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, kv := range env {
		if strings.HasPrefix(kv, "PATH=") || strings.HasPrefix(kv, "TERM=") {
			t.Errorf("base env must not contain generic defaults (WDY-1825); got %q in %v", kv, env)
		}
	}
}

func TestAppendFallbackEnvPreservesImagePATH(t *testing.T) {
	imagePATH := "PATH=/usr/local/nvidia/bin:/usr/local/cuda/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	env := appendFallbackEnv([]string{imagePATH, "NVIDIA_VISIBLE_DEVICES=all"})

	var paths []string
	for _, kv := range env {
		if strings.HasPrefix(kv, "PATH=") {
			paths = append(paths, kv)
		}
	}
	if len(paths) != 1 {
		t.Fatalf("want exactly one PATH entry, got %d: %v", len(paths), env)
	}
	if paths[0] != imagePATH {
		t.Errorf("PATH = %q, want image PATH %q preserved", paths[0], imagePATH)
	}
	// TERM was not set by the image, so the fallback must still be appended.
	if !envHasKey(env, "TERM") {
		t.Errorf("env missing TERM fallback; got %v", env)
	}
}

func TestAppendFallbackEnvAddsPATHWhenImageHasNone(t *testing.T) {
	env := appendFallbackEnv([]string{"FOO=bar"})

	wantPATH := "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	wantTERM := "TERM=xterm"
	foundPATH, foundTERM := false, false
	for _, kv := range env {
		switch kv {
		case wantPATH:
			foundPATH = true
		case wantTERM:
			foundTERM = true
		}
	}
	if !foundPATH {
		t.Errorf("env missing fallback %q; got %v", wantPATH, env)
	}
	if !foundTERM {
		t.Errorf("env missing fallback %q; got %v", wantTERM, env)
	}
}

func TestAppendFallbackEnvEmptyInput(t *testing.T) {
	env := appendFallbackEnv(nil)
	if !envHasKey(env, "PATH") || !envHasKey(env, "TERM") {
		t.Errorf("fallbacks missing on empty input; got %v", env)
	}
}

// TestAppendFallbackEnvKeyMatchIsExact ensures the presence check matches the
// whole key, not a prefix: an image setting PATHEXT (or TERMINFO) must not
// suppress the PATH (or TERM) fallback.
func TestAppendFallbackEnvKeyMatchIsExact(t *testing.T) {
	env := appendFallbackEnv([]string{"PATHEXT=.sh", "TERMINFO=/usr/share/terminfo"})
	if !envHasKey(env, "PATH") {
		t.Errorf("PATHEXT must not suppress the PATH fallback; got %v", env)
	}
	if !envHasKey(env, "TERM") {
		t.Errorf("TERMINFO must not suppress the TERM fallback; got %v", env)
	}
}

func hostNetworkCfg() *appconfig.AppConfig {
	return &appconfig.AppConfig{
		Entitlements: []appconfig.Entitlement{
			{Type: appconfig.EntitlementNetwork, Mode: "host"},
		},
	}
}

func TestInjectOTELEnvDefaultPort(t *testing.T) {
	t.Setenv("WENDY_OTEL_PORT", "")

	env := injectOTELEnvIfNeeded(nil, hostNetworkCfg(), "")

	want := "OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:4317"
	for _, kv := range env {
		if kv == want {
			return
		}
	}
	t.Errorf("env missing %q; got %v", want, env)
}

func TestInjectOTELEnvCustomPort(t *testing.T) {
	t.Setenv("WENDY_OTEL_PORT", "9999")

	env := injectOTELEnvIfNeeded(nil, hostNetworkCfg(), "")

	want := "OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:9999"
	for _, kv := range env {
		if kv == want {
			return
		}
	}
	t.Errorf("env missing %q; got %v", want, env)
}

func TestInjectOTELEnvSetsGRPCProtocol(t *testing.T) {
	env := injectOTELEnvIfNeeded(nil, hostNetworkCfg(), "")

	const want = "OTEL_EXPORTER_OTLP_PROTOCOL=grpc"
	for _, kv := range env {
		if kv == want {
			return
		}
	}
	t.Errorf("env missing %q; got %v", want, env)
}

func TestInjectOTELEnvSkipsWithoutHostNetworking(t *testing.T) {
	cfg := &appconfig.AppConfig{} // no network entitlement

	env := injectOTELEnvIfNeeded(nil, cfg, "")

	for _, kv := range env {
		if len(kv) > len("OTEL_EXPORTER_OTLP_ENDPOINT=") &&
			kv[:len("OTEL_EXPORTER_OTLP_ENDPOINT=")] == "OTEL_EXPORTER_OTLP_ENDPOINT=" {
			t.Errorf("unexpected OTEL var injected without host networking: %q", kv)
		}
	}
}

func TestInjectOTELEnvSkipsWhenEndpointAlreadySet(t *testing.T) {
	existing := []string{"OTEL_EXPORTER_OTLP_ENDPOINT=http://custom-collector:4317"}

	env := injectOTELEnvIfNeeded(existing, hostNetworkCfg(), "")

	count := 0
	for _, kv := range env {
		if len(kv) > len("OTEL_EXPORTER_OTLP_ENDPOINT=") &&
			kv[:len("OTEL_EXPORTER_OTLP_ENDPOINT=")] == "OTEL_EXPORTER_OTLP_ENDPOINT=" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 OTEL_EXPORTER_OTLP_ENDPOINT entry, got %d: %v", count, env)
	}
}

func TestInjectOTELEnvDoesNotOverrideExistingProtocol(t *testing.T) {
	existing := []string{"OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf"}

	env := injectOTELEnvIfNeeded(existing, hostNetworkCfg(), "")

	count := 0
	for _, kv := range env {
		if strings.HasPrefix(kv, "OTEL_EXPORTER_OTLP_PROTOCOL=") {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 OTEL_EXPORTER_OTLP_PROTOCOL entry, got %d: %v", count, env)
	}
	for _, kv := range env {
		if kv == "OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf" {
			return
		}
	}
	t.Errorf("image-set protocol was overridden; got %v", env)
}

func TestInjectOTELEnvInvalidPortFallsBackToDefault(t *testing.T) {
	t.Setenv("WENDY_OTEL_PORT", "notaport")

	env := injectOTELEnvIfNeeded(nil, hostNetworkCfg(), "")

	const want = "OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:4317"
	for _, kv := range env {
		if kv == want {
			return
		}
	}
	t.Errorf("expected fallback to default port; got %v", env)
}

func hostNetworkCfgWithID(appID string) *appconfig.AppConfig {
	cfg := hostNetworkCfg()
	cfg.AppID = appID
	return cfg
}

func TestInjectOTELEnvSetsServiceNameAndResourceAttrs(t *testing.T) {
	env := injectOTELEnvIfNeeded(nil, hostNetworkCfgWithID("my-app"), "my-app")

	wantService := false
	wantAttrs := false
	for _, kv := range env {
		switch kv {
		case "OTEL_SERVICE_NAME=my-app":
			wantService = true
		case "OTEL_RESOURCE_ATTRIBUTES=wendy.app.name=my-app":
			wantAttrs = true
		}
	}
	if !wantService {
		t.Errorf("env missing OTEL_SERVICE_NAME=my-app; got %v", env)
	}
	if !wantAttrs {
		t.Errorf("env missing OTEL_RESOURCE_ATTRIBUTES=wendy.app.name=my-app; got %v", env)
	}
}

func TestInjectOTELEnvUsesContainerNameForMultiServiceIdentity(t *testing.T) {
	cfg := hostNetworkCfgWithID("robot")
	cfg.ServiceName = "listener"

	env := injectOTELEnvIfNeeded(nil, cfg, "robot")

	if !slices.Contains(env, "OTEL_SERVICE_NAME=robot_listener") {
		t.Errorf("env missing multi-service OTEL_SERVICE_NAME; got %v", env)
	}
	if !slices.Contains(env, "OTEL_RESOURCE_ATTRIBUTES=wendy.app.name=robot") {
		t.Errorf("env missing owning app resource attribute; got %v", env)
	}
}

func TestInjectOTELEnvSetsIdentityWhenEndpointPreset(t *testing.T) {
	// An image that presets only the endpoint should still get identity vars so
	// its direct OTLP logs remain filterable by `wendy device logs --app <id>`.
	existing := []string{"OTEL_EXPORTER_OTLP_ENDPOINT=http://custom-collector:4317"}

	env := injectOTELEnvIfNeeded(existing, hostNetworkCfgWithID("my-app"), "my-app")

	endpointCount, wantService, wantAttrs := 0, false, false
	for _, kv := range env {
		switch {
		case strings.HasPrefix(kv, "OTEL_EXPORTER_OTLP_ENDPOINT="):
			endpointCount++
		case kv == "OTEL_SERVICE_NAME=my-app":
			wantService = true
		case kv == "OTEL_RESOURCE_ATTRIBUTES=wendy.app.name=my-app":
			wantAttrs = true
		}
	}
	if endpointCount != 1 {
		t.Errorf("expected image endpoint preserved (1 entry), got %d: %v", endpointCount, env)
	}
	if !wantService || !wantAttrs {
		t.Errorf("expected identity vars injected alongside preset endpoint; got %v", env)
	}
}

func TestInjectOTELEnvOmitsServiceNameWhenAppIDEmpty(t *testing.T) {
	env := injectOTELEnvIfNeeded(nil, hostNetworkCfgWithID(""), "")

	for _, kv := range env {
		if strings.HasPrefix(kv, "OTEL_SERVICE_NAME=") ||
			strings.HasPrefix(kv, "OTEL_RESOURCE_ATTRIBUTES=") {
			t.Errorf("env unexpectedly contains %q when appID is empty", kv)
		}
	}
}

func TestInjectOTELEnvDoesNotOverrideExistingServiceName(t *testing.T) {
	existing := []string{
		"OTEL_SERVICE_NAME=custom",
		"OTEL_RESOURCE_ATTRIBUTES=deployment.environment=prod",
	}

	env := injectOTELEnvIfNeeded(existing, hostNetworkCfgWithID("my-app"), "my-app")

	serviceCount, attrCount := 0, 0
	for _, kv := range env {
		if strings.HasPrefix(kv, "OTEL_SERVICE_NAME=") {
			serviceCount++
		}
		if strings.HasPrefix(kv, "OTEL_RESOURCE_ATTRIBUTES=") {
			attrCount++
		}
	}
	if serviceCount != 1 || attrCount != 1 {
		t.Errorf("expected image-set OTEL_SERVICE_NAME/OTEL_RESOURCE_ATTRIBUTES to be preserved; got %v", env)
	}
	for _, kv := range env {
		if kv == "OTEL_SERVICE_NAME=my-app" || kv == "OTEL_RESOURCE_ATTRIBUTES=wendy.app.name=my-app" {
			t.Errorf("image-set OTel resource values were overridden; got %v", env)
		}
	}
}

func TestHasHostNetworkEntitlementEmptyModeIsHost(t *testing.T) {
	cfg := &appconfig.AppConfig{
		Entitlements: []appconfig.Entitlement{
			{Type: appconfig.EntitlementNetwork, Mode: ""},
		},
	}
	if !hasHostNetworkEntitlement(cfg) {
		t.Error("empty mode should imply host networking")
	}
}

// TestHasHostNetworkEntitlementHostAdmin verifies the WDY-1094 opt-in mode is
// still recognised as host networking (it shares the host network stack; it
// just additionally carries CAP_NET_ADMIN), so host-network-dependent wiring
// like OTEL endpoint injection continues to apply.
func TestHasHostNetworkEntitlementHostAdmin(t *testing.T) {
	cfg := &appconfig.AppConfig{
		Entitlements: []appconfig.Entitlement{
			{Type: appconfig.EntitlementNetwork, Mode: "host-admin"},
		},
	}
	if !hasHostNetworkEntitlement(cfg) {
		t.Error("host-admin mode should imply host networking")
	}
}

// stubBoard pins the detected board kind for one test so needsNvidiaCDI never
// reads the host's real /etc/nv_tegra_release — the assertions below are about
// the entitlement/board decision, not about what the machine running the suite
// happens to be.
func stubBoard(t *testing.T, kind board.Kind) {
	t.Helper()
	prev := boardDetect
	boardDetect = func() board.Info { return board.Info{Kind: kind} }
	t.Cleanup(func() { boardDetect = prev })
}

// TestNeedsNvidiaCDIGPUOnly verifies the pre-existing trigger (gpu entitlement
// alone) still applies CDI, so this change is additive. Board-independent: a
// gpu entitlement asks for CDI/CSV provisioning on any host, and applyNvidiaCDI's
// warnings when there is none are the intended outcome.
func TestNeedsNvidiaCDIGPUOnly(t *testing.T) {
	cfg := &appconfig.AppConfig{
		Entitlements: []appconfig.Entitlement{
			{Type: appconfig.EntitlementGPU},
		},
	}
	for _, kind := range []board.Kind{board.Jetson, board.RaspberryPi, board.Generic} {
		stubBoard(t, kind)
		if !needsNvidiaCDI(cfg) {
			t.Errorf("gpu entitlement should trigger CDI application on board kind %v", kind)
		}
	}
}

// TestNeedsNvidiaCDIDisplayOnlyJetson covers the gap this change closes: a
// display entitlement with no explicit gpu entitlement must still get the
// NVIDIA CDI library/device mounts, matching applyDisplay's own doc comment
// that Jetson injects the EGL/GLES userspace via CDI. Before this fix, a
// display-only app got /dev/dri and the driver-capability env vars from
// applyDisplay but none of CDI's actual library mounts.
func TestNeedsNvidiaCDIDisplayOnlyJetson(t *testing.T) {
	stubBoard(t, board.Jetson)
	cfg := &appconfig.AppConfig{
		Entitlements: []appconfig.Entitlement{
			{Type: appconfig.EntitlementDisplay},
		},
	}
	if !needsNvidiaCDI(cfg) {
		t.Error("display entitlement alone should trigger CDI application on a Jetson")
	}
}

// TestNeedsNvidiaCDIDisplayOnlyNonJetson pins the other half of the gate: a
// display app on a board with no NVIDIA userspace must not enter applyNvidiaCDI,
// which would only log "no NVIDIA CDI spec" warnings at it. This mirrors
// applyDisplay, which likewise sets the NVIDIA driver-capability env vars only
// on a Jetson.
func TestNeedsNvidiaCDIDisplayOnlyNonJetson(t *testing.T) {
	cfg := &appconfig.AppConfig{
		Entitlements: []appconfig.Entitlement{
			{Type: appconfig.EntitlementDisplay},
		},
	}
	for _, kind := range []board.Kind{board.RaspberryPi, board.Generic} {
		stubBoard(t, kind)
		if needsNvidiaCDI(cfg) {
			t.Errorf("display entitlement should not trigger NVIDIA CDI on board kind %v", kind)
		}
	}
}

// TestNeedsNvidiaCDINeitherEntitlement verifies an app with neither
// entitlement gets no CDI mounts, preserving the default no-GPU sandbox for
// apps that never opted into GPU or display access — including on a Jetson,
// where the board check alone must not be enough.
func TestNeedsNvidiaCDINeitherEntitlement(t *testing.T) {
	stubBoard(t, board.Jetson)
	cfg := &appconfig.AppConfig{
		Entitlements: []appconfig.Entitlement{
			{Type: appconfig.EntitlementAudio},
		},
	}
	if needsNvidiaCDI(cfg) {
		t.Error("neither gpu nor display entitlement should trigger CDI application")
	}
}

func TestExpandAgentHook(t *testing.T) {
	t.Setenv("EXTRA_VALUE", "ok")

	got := expandAgentHook("echo ${WENDY_APP_ID} ${WENDY_HOSTNAME} ${EXTRA_VALUE}", "camera-app")
	want := "echo camera-app localhost ok"
	if got != want {
		t.Fatalf("expandAgentHook = %q; want %q", got, want)
	}
}

func TestExpandAgentHookMissingEnv(t *testing.T) {
	t.Setenv("MISSING_VALUE", "")

	got := expandAgentHook("echo ${MISSING_VALUE}", "app")
	if got != "echo " {
		t.Fatalf("expandAgentHook missing env = %q; want empty expansion", got)
	}
}

// TestWrapWithDebugpyBindsLoopback is the regression test for WDY-1010: the
// debugpy DAP listener must bind to loopback (127.0.0.1), never 0.0.0.0.
// Binding all interfaces exposed unauthenticated Python RCE to anyone on the
// device's network for the duration of a debug session. Remote attach now goes
// through a device-side tunnel to loopback.
func TestWrapWithDebugpyBindsLoopback(t *testing.T) {
	cases := map[string][]string{
		"python entrypoint":     {"python3", "app.py"},
		"non-python entrypoint": {"/app/server"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			got := wrapWithDebugpy(args)
			joined := strings.Join(got, " ")
			if strings.Contains(joined, "0.0.0.0:5678") {
				t.Fatalf("debugpy must not bind 0.0.0.0; got %q", joined)
			}
			if !slices.Contains(got, "127.0.0.1:5678") {
				t.Fatalf("debugpy must bind loopback 127.0.0.1:5678; got %q", joined)
			}
		})
	}
}

func TestStartPostStartAgentHookSkippedWhenEmpty(t *testing.T) {
	old := startPostStartHookCommand
	t.Cleanup(func() { startPostStartHookCommand = old })

	var calls int
	startPostStartHookCommand = func(_ []string) (func() error, error) {
		calls++
		return func() error { return nil }, nil
	}

	client := &Client{logger: zap.NewNop()}
	started := client.startPostStartAgentHook("", "camera-app")
	if started {
		t.Fatal("startPostStartAgentHook returned true without command")
	}
	if calls != 0 {
		t.Fatalf("hook runner called %d times; want 0", calls)
	}
}

func TestStartPostStartAgentHookRunsWhenPresent(t *testing.T) {
	t.Setenv("EXTRA_VALUE", "ok")
	old := startPostStartHookCommand
	t.Cleanup(func() { startPostStartHookCommand = old })

	var gotArgv []string
	startPostStartHookCommand = func(argv []string) (func() error, error) {
		gotArgv = argv
		return func() error { return nil }, nil
	}

	client := &Client{logger: zap.NewNop()}
	started := client.startPostStartAgentHook("echo ${WENDY_APP_ID} ${WENDY_HOSTNAME} ${EXTRA_VALUE}", "camera-app")
	if !started {
		t.Fatal("startPostStartAgentHook returned false with command")
	}
	wantArgv := []string{"echo", "camera-app", "localhost", "ok"}
	if !slices.Equal(gotArgv, wantArgv) {
		t.Fatalf("hook argv = %q; want %q", gotArgv, wantArgv)
	}
}

// TestStartPostStartAgentHookDoesNotInterpretShellMetacharacters is the
// regression test for WDY-1009: the hook command must be executed directly via
// argv, never handed to a shell. Shell metacharacters (;, &&, |, $(...)) must
// survive as inert literal arguments to argv[0] rather than spawning new
// commands.
func TestStartPostStartAgentHookDoesNotInterpretShellMetacharacters(t *testing.T) {
	old := startPostStartHookCommand
	t.Cleanup(func() { startPostStartHookCommand = old })

	var gotArgv []string
	startPostStartHookCommand = func(argv []string) (func() error, error) {
		gotArgv = argv
		return func() error { return nil }, nil
	}

	client := &Client{logger: zap.NewNop()}
	started := client.startPostStartAgentHook("/app/post-start.sh ; touch /tmp/pwned && rm -rf /", "camera-app")
	if !started {
		t.Fatal("startPostStartAgentHook returned false with command")
	}
	if len(gotArgv) == 0 {
		t.Fatal("hook argv is empty")
	}
	// The program executed is the first token only; the injected command tokens
	// must appear verbatim as arguments, proving no shell parsed them.
	for _, tok := range gotArgv {
		if tok == "sh" || tok == "-c" || tok == "cmd.exe" || tok == "/C" {
			t.Fatalf("hook argv must not invoke a shell, got %q", gotArgv)
		}
	}
	// The whole command must survive as argv[0] (the program) plus inert literal
	// tokens — the metacharacters are arguments, never separators.
	wantArgv := []string{"/app/post-start.sh", ";", "touch", "/tmp/pwned", "&&", "rm", "-rf", "/"}
	if !slices.Equal(gotArgv, wantArgv) {
		t.Fatalf("hook argv = %q; want %q", gotArgv, wantArgv)
	}
}

// TestStartPostStartAgentHookEmptyExpansionLogsConfiguredCommand asserts the
// "expanded to empty" warning is actionable: it carries the raw, pre-expansion
// command from wendy.json so an operator can tell which hook misfired. The raw
// command holds variable references (not their expanded values), so it is safe
// to log — unlike the expanded string, which may contain secrets.
func TestStartPostStartAgentHookEmptyExpansionLogsConfiguredCommand(t *testing.T) {
	t.Setenv("MISSING_VALUE", "")
	old := startPostStartHookCommand
	t.Cleanup(func() { startPostStartHookCommand = old })
	startPostStartHookCommand = func(_ []string) (func() error, error) {
		return func() error { return nil }, nil
	}

	core, observed := observer.New(zap.WarnLevel)
	client := &Client{logger: zap.New(core)}
	client.startPostStartAgentHook("${MISSING_VALUE}", "camera-app")

	logs := observed.FilterMessageSnippet("empty command")
	if logs.Len() != 1 {
		t.Fatalf("expected one empty-command warning; got %d", logs.Len())
	}
	var found bool
	for _, f := range logs.All()[0].Context {
		if f.Key == "configured_command" && f.String == "${MISSING_VALUE}" {
			found = true
		}
	}
	if !found {
		t.Fatalf("empty-command warning must include the raw configured_command field; got %+v", logs.All()[0].Context)
	}
}

// TestStartPostStartAgentHookWarnsOnQuotedArguments guards the silent
// behavioral regression from dropping `sh -c`: strings.Fields does not honor
// quotes, so a quoted argument is split on whitespace. The hook still runs
// (best-effort), but the operator must get a warning rather than silent
// mis-execution.
func TestStartPostStartAgentHookWarnsOnQuotedArguments(t *testing.T) {
	old := startPostStartHookCommand
	t.Cleanup(func() { startPostStartHookCommand = old })
	var calls int
	startPostStartHookCommand = func(_ []string) (func() error, error) {
		calls++
		return func() error { return nil }, nil
	}

	core, observed := observer.New(zap.WarnLevel)
	client := &Client{logger: zap.New(core)}
	started := client.startPostStartAgentHook(`/app/run --message "hello world"`, "camera-app")

	if !started {
		t.Fatal("hook with quoted args should still run (best-effort)")
	}
	if calls != 1 {
		t.Fatalf("hook runner called %d times; want 1", calls)
	}
	if observed.FilterMessageSnippet("quot").Len() == 0 {
		t.Fatal("expected a warning that quoting is not honored")
	}
}

// TestStartPostStartHookCommandRejectsEmptyArgv keeps the argv[0] indexing
// invariant local to the runner: an empty argv must return an error, never
// panic, even if a future caller forgets the length guard.
func TestStartPostStartHookCommandRejectsEmptyArgv(t *testing.T) {
	if _, err := startPostStartHookCommand(nil); err == nil {
		t.Fatal("expected error for empty argv, got nil")
	}
}

// TestStartPostStartAgentHookSkippedWhenExpansionEmpty guards the case where a
// command consists solely of an env reference that expands to nothing: there is
// no program to run, so the runner must not be invoked.
func TestStartPostStartAgentHookSkippedWhenExpansionEmpty(t *testing.T) {
	t.Setenv("MISSING_VALUE", "")
	old := startPostStartHookCommand
	t.Cleanup(func() { startPostStartHookCommand = old })

	var calls int
	startPostStartHookCommand = func(_ []string) (func() error, error) {
		calls++
		return func() error { return nil }, nil
	}

	client := &Client{logger: zap.NewNop()}
	started := client.startPostStartAgentHook("${MISSING_VALUE}", "camera-app")
	if started {
		t.Fatal("startPostStartAgentHook returned true for a command that expanded to nothing")
	}
	if calls != 0 {
		t.Fatalf("hook runner called %d times; want 0", calls)
	}
}

func TestStartPostStartAgentHookStartErrorDoesNotLogCommand(t *testing.T) {
	old := startPostStartHookCommand
	t.Cleanup(func() { startPostStartHookCommand = old })

	startPostStartHookCommand = func(_ []string) (func() error, error) {
		return nil, errors.New("start failed")
	}

	core, observed := observer.New(zap.WarnLevel)
	client := &Client{logger: zap.New(core)}
	started := client.startPostStartAgentHook("echo secret-token-value", "camera-app")
	if started {
		t.Fatal("startPostStartAgentHook returned true after start error")
	}

	logs := observed.FilterMessage("Failed to start postStart agent hook")
	if logs.Len() != 1 {
		t.Fatalf("warning log count = %d; want 1", logs.Len())
	}
	if observed.FilterMessageSnippet("secret-token-value").Len() != 0 {
		t.Fatal("hook command leaked into warning message")
	}
	for _, field := range logs.All()[0].Context {
		if field.Key == "command" {
			t.Fatal("hook command leaked into warning fields")
		}
	}
}

func TestLayerMediaType_Zstd(t *testing.T) {
	got := layerMediaType(agentpb.RunContainerLayerHeader_COMPRESSION_ZSTD, false)
	want := "application/vnd.oci.image.layer.v1.tar+zstd"
	if got != want {
		t.Errorf("layerMediaType(ZSTD, false) = %q; want %q", got, want)
	}
}

func TestLayerMediaType_ZstdIgnoresGzipBool(t *testing.T) {
	got := layerMediaType(agentpb.RunContainerLayerHeader_COMPRESSION_ZSTD, true)
	want := "application/vnd.oci.image.layer.v1.tar+zstd"
	if got != want {
		t.Errorf("layerMediaType(ZSTD, true) = %q; want %q", got, want)
	}
}

func TestLayerMediaType_None(t *testing.T) {
	got := layerMediaType(agentpb.RunContainerLayerHeader_COMPRESSION_NONE, true)
	want := "application/vnd.oci.image.layer.v1.tar"
	if got != want {
		t.Errorf("layerMediaType(NONE, true) = %q; want %q", got, want)
	}
}

func TestLayerMediaType_GzipDefault_GzipTrue(t *testing.T) {
	// Old CLI path: compression field absent (zero value = GZIP), gzip=true.
	got := layerMediaType(agentpb.RunContainerLayerHeader_COMPRESSION_GZIP, true)
	want := "application/vnd.oci.image.layer.v1.tar+gzip"
	if got != want {
		t.Errorf("layerMediaType(GZIP, true) = %q; want %q", got, want)
	}
}

func TestLayerMediaType_GzipDefault_GzipFalse(t *testing.T) {
	// Old CLI path: compression field absent (zero value = GZIP), gzip=false → uncompressed.
	got := layerMediaType(agentpb.RunContainerLayerHeader_COMPRESSION_GZIP, false)
	want := "application/vnd.oci.image.layer.v1.tar"
	if got != want {
		t.Errorf("layerMediaType(GZIP, false) = %q; want %q", got, want)
	}
}

func TestResolveStopOrder_ReversesTopoOrder(t *testing.T) {
	services := map[string]*appconfig.ServiceConfig{
		"db":  {},
		"api": {DependsOn: []string{"db"}},
	}
	order, err := appconfig.ServiceTopoOrder(services)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Reverse for stop order: dependents first, then dependencies.
	for i, j := 0, len(order)-1; i < j; i, j = i+1, j-1 {
		order[i], order[j] = order[j], order[i]
	}
	if len(order) != 2 || order[0] != "api" || order[1] != "db" {
		t.Errorf("expected [api db], got %v", order)
	}
}

func TestPrimaryPIDTracking(t *testing.T) {
	c := &Client{primaryPIDs: make(map[string]uint32)}
	c.setPrimaryPID("com.example.app", 12345)
	got, ok := c.getPrimaryPID("com.example.app")
	if !ok {
		t.Fatal("expected primary PID to be found")
	}
	if got != 12345 {
		t.Fatalf("got PID %d, want 12345", got)
	}
	c.clearPrimaryPID("com.example.app")
	_, ok = c.getPrimaryPID("com.example.app")
	if ok {
		t.Fatal("expected primary PID to be cleared")
	}
}

func envContains(env []string, want string) bool {
	for _, e := range env {
		if e == want {
			return true
		}
	}
	return false
}

func envValue(env []string, key string) (string, bool) {
	prefix := key + "="
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			return e[len(prefix):], true
		}
	}
	return "", false
}

func TestBuildROS2Env_WithConfig(t *testing.T) {
	domainID := 42
	cfg := &appconfig.AppConfig{
		Frameworks: &appconfig.FrameworksConfig{
			ROS2: &appconfig.ROS2Config{
				DomainID: &domainID,
				RMW:      "rmw_cyclonedds_cpp",
			},
		},
	}
	got := mustBuildROS2Env(t, cfg, "com.example.app", "")
	for _, want := range []string{
		"ROS_DOMAIN_ID=42",
		"RMW_IMPLEMENTATION=rmw_cyclonedds_cpp",
		"ROS_LOCALHOST_ONLY=1",
	} {
		if !envContains(got, want) {
			t.Errorf("expected %s in env, got %v", want, got)
		}
	}
	if uri, ok := envValue(got, "CYCLONEDDS_URI"); !ok || !strings.Contains(uri, "<SharedMemory>") {
		t.Errorf("expected inline CYCLONEDDS_URI with shared memory config, got %v", got)
	}
}

func TestBuildROS2Env_NoConfig(t *testing.T) {
	cfg := &appconfig.AppConfig{}
	got := mustBuildROS2Env(t, cfg, "com.example.app", "")
	if len(got) != 0 {
		t.Errorf("expected empty env for no ROS2 config, got %v", got)
	}
}

func TestBuildROS2Env_HostDiscovery(t *testing.T) {
	domainID := 30
	cfg := &appconfig.AppConfig{
		Frameworks: &appconfig.FrameworksConfig{
			ROS2: &appconfig.ROS2Config{
				DomainID:       &domainID,
				DiscoveryScope: appconfig.ROS2DiscoveryScopeHost,
			},
		},
	}
	got := mustBuildROS2Env(t, cfg, "sh.wendy.foxglovebridge", "")
	if !envContains(got, "ROS_LOCALHOST_ONLY=0") {
		t.Errorf("expected host discovery to disable localhost-only mode, got %v", got)
	}
	if envContains(got, "ROS_LOCALHOST_ONLY=1") {
		t.Errorf("host discovery must not enable localhost-only mode, got %v", got)
	}
}

func TestBuildROS2Env_AutoDomainID(t *testing.T) {
	cfg := &appconfig.AppConfig{
		Frameworks: &appconfig.FrameworksConfig{ROS2: &appconfig.ROS2Config{}},
	}
	got := mustBuildROS2Env(t, cfg, "com.example.app", "")
	val, ok := envValue(got, "ROS_DOMAIN_ID")
	if !ok {
		t.Fatalf("expected ROS_DOMAIN_ID in env, got %v", got)
	}
	id, err := strconv.Atoi(val)
	if err != nil || id < 0 || id > 232 {
		t.Errorf("auto domain ID = %q, want integer in [0,232]", val)
	}
	// Stable: a second call for the same appId must produce the same ID.
	again := mustBuildROS2Env(t, cfg, "com.example.app", "")
	if val2, _ := envValue(again, "ROS_DOMAIN_ID"); val2 != val {
		t.Errorf("auto domain ID not stable: %q vs %q", val, val2)
	}
	// Defaults: CycloneDDS RMW with inline config.
	if !envContains(got, "RMW_IMPLEMENTATION=rmw_cyclonedds_cpp") {
		t.Errorf("expected default RMW_IMPLEMENTATION=rmw_cyclonedds_cpp, got %v", got)
	}
}

func TestBuildROS2Env_InvalidDomainID(t *testing.T) {
	domainID := 500
	cfg := &appconfig.AppConfig{
		Frameworks: &appconfig.FrameworksConfig{
			ROS2: &appconfig.ROS2Config{DomainID: &domainID},
		},
	}
	// Previously this returned an empty env, which meant the container started
	// with no ROS_DOMAIN_ID at all and fell back to the global default domain 0 —
	// i.e. a validation gap produced maximum exposure. It must fail instead.
	got, err := buildROS2Env(cfg, "com.example.app", "")
	if err == nil {
		t.Fatalf("expected an error for out-of-range domain ID, got env %v", got)
	}
	if got != nil {
		t.Errorf("expected nil env alongside the error, got %v", got)
	}
	if !strings.Contains(err.Error(), "domain 0") {
		t.Errorf("error should explain the domain-0 fallback it is preventing, got: %v", err)
	}
}

func TestBuildROS2Env_NonCycloneRMWSkipsCycloneURI(t *testing.T) {
	cfg := &appconfig.AppConfig{
		Frameworks: &appconfig.FrameworksConfig{
			ROS2: &appconfig.ROS2Config{RMW: "fastrtps"},
		},
	}
	got := mustBuildROS2Env(t, cfg, "com.example.app", "")
	if !envContains(got, "RMW_IMPLEMENTATION=rmw_fastrtps_cpp") {
		t.Errorf("expected short rmw name to normalize to rmw_fastrtps_cpp, got %v", got)
	}
	if _, ok := envValue(got, "CYCLONEDDS_URI"); ok {
		t.Errorf("CYCLONEDDS_URI must not be injected for non-CycloneDDS RMW, got %v", got)
	}
}

func TestBuildROS2Env_ServiceOverride(t *testing.T) {
	groupID, svcID := 1, 7
	cfg := &appconfig.AppConfig{
		Frameworks: &appconfig.FrameworksConfig{ROS2: &appconfig.ROS2Config{DomainID: &groupID}},
		Services: map[string]*appconfig.ServiceConfig{
			"detector": {
				Frameworks: &appconfig.FrameworksConfig{ROS2: &appconfig.ROS2Config{DomainID: &svcID}},
			},
			"camera": {},
		},
	}
	got := mustBuildROS2Env(t, cfg, "com.example.app", "detector")
	if !envContains(got, "ROS_DOMAIN_ID=7") {
		t.Errorf("expected service-level override ROS_DOMAIN_ID=7, got %v", got)
	}
	got = mustBuildROS2Env(t, cfg, "com.example.app", "camera")
	if !envContains(got, "ROS_DOMAIN_ID=1") {
		t.Errorf("expected group-level ROS_DOMAIN_ID=1 for camera, got %v", got)
	}
}

// TestSharedSHMPath verifies the shared-ipc shm path is derived from a valid
// app ID and that an invalid app ID is rejected (the path is later stat'd to
// detect shared-ipc topology and bind-mounted into the ROS 2 sidecar, WDY-1555).
func TestSharedSHMPath(t *testing.T) {
	path, err := sharedSHMPath("sh.wendy.examples.so101")
	if err != nil {
		t.Fatalf("sharedSHMPath valid app ID: unexpected error %v", err)
	}
	if want := "/run/wendy/shm/sh.wendy.examples.so101"; path != want {
		t.Errorf("sharedSHMPath = %q, want %q", path, want)
	}
	if _, err := sharedSHMPath(""); err == nil {
		t.Error("sharedSHMPath(\"\") = nil error, want validation error")
	}
	if _, err := sharedSHMPath("../escape"); err == nil {
		t.Error("sharedSHMPath(\"../escape\") = nil error, want validation error")
	}
}

// TestRMWFromEnv verifies the ROS 2 sidecar reads back the anchor app's
// RMW_IMPLEMENTATION from its OCI spec env so it can match the app's DDS
// implementation (WDY-1593).
func TestRMWFromEnv(t *testing.T) {
	got := rmwFromEnv([]string{"ROS_DOMAIN_ID=42", "RMW_IMPLEMENTATION=rmw_cyclonedds_cpp", "ROS_LOCALHOST_ONLY=1"})
	if got != "rmw_cyclonedds_cpp" {
		t.Errorf("rmwFromEnv = %q, want rmw_cyclonedds_cpp", got)
	}
	if got := rmwFromEnv([]string{"ROS_DOMAIN_ID=42"}); got != "" {
		t.Errorf("rmwFromEnv (absent) = %q, want empty", got)
	}
	if got := rmwFromEnv(nil); got != "" {
		t.Errorf("rmwFromEnv(nil) = %q, want empty", got)
	}
}

func TestRebuildCachesFromLabels(t *testing.T) {
	labels := []map[string]string{
		// isolated single-service app
		{labelKeyAppID: "cam", labelKeyServiceName: "cam", labelKeyIsolation: "isolated"},
		// shared-namespace group: two services, one with a dependency
		{labelKeyAppID: "stack", labelKeyServiceName: "web", labelKeyIsolation: "shared-network", labelKeyDependsOn: "db"},
		{labelKeyAppID: "stack", labelKeyServiceName: "db", labelKeyIsolation: "shared-network"},
		// non-isolated app: no isolation label
		{labelKeyAppID: "plain", labelKeyServiceName: "plain"},
		// junk row with no appID is ignored
		{labelKeyServiceName: "orphan"},
	}

	isolation, services := rebuildCachesFromLabels(labels)

	if isolation["cam"] != "isolated" {
		t.Fatalf("cam isolation = %q, want isolated", isolation["cam"])
	}
	if isolation["stack"] != "shared-network" {
		t.Fatalf("stack isolation = %q, want shared-network", isolation["stack"])
	}
	if _, ok := isolation["plain"]; ok {
		t.Fatal("plain must not have an isolation entry")
	}
	if len(services["stack"]) != 2 {
		t.Fatalf("stack services = %d, want 2", len(services["stack"]))
	}
	web := services["stack"]["web"]
	if web == nil || len(web.DependsOn) != 1 || web.DependsOn[0] != "db" {
		t.Fatalf("stack/web dependsOn = %+v, want [db]", web)
	}
	if db := services["stack"]["db"]; db == nil || len(db.DependsOn) != 0 {
		t.Fatalf("stack/db dependsOn = %+v, want empty", db)
	}
	if _, ok := services[""]; ok {
		t.Fatal("orphan row (no appID) must be ignored")
	}
}

func TestEntitlementsUseHostNetwork(t *testing.T) {
	host := []appconfig.Entitlement{{Type: appconfig.EntitlementNetwork, Mode: "host"}}
	hostAdmin := []appconfig.Entitlement{{Type: appconfig.EntitlementNetwork, Mode: "host-admin"}}
	omitted := []appconfig.Entitlement{{Type: appconfig.EntitlementNetwork, Mode: ""}}
	mesh := []appconfig.Entitlement{{Type: appconfig.EntitlementNetwork, Mode: "mesh"}}
	none := []appconfig.Entitlement{{Type: appconfig.EntitlementNetwork, Mode: "none"}}
	noNet := []appconfig.Entitlement{{Type: appconfig.EntitlementGPU}}

	for _, tc := range []struct {
		name string
		ents []appconfig.Entitlement
		want bool
	}{
		{"host", host, true},
		{"host-admin", hostAdmin, true},
		{"omitted", omitted, true},
		{"mesh", mesh, false},
		{"none", none, false},
		{"no network entitlement", noNet, false},
	} {
		if got := entitlementsUseHostNetwork(tc.ents); got != tc.want {
			t.Errorf("%s: entitlementsUseHostNetwork = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestIsPubliclyBoundAddress(t *testing.T) {
	for _, tc := range []struct {
		addr string
		want bool
	}{
		{"0.0.0.0", true},      // IPv4 wildcard = all interfaces
		{"::", true},           // IPv6 wildcard
		{"192.168.1.10", true}, // specific non-loopback
		{"127.0.0.1", false},   // IPv4 loopback
		{"127.0.0.53", false},  // loopback range
		{"::1", false},         // IPv6 loopback
		{"", false},            // empty
		{"garbage", false},     // unparseable
	} {
		if got := isPubliclyBoundAddress(tc.addr); got != tc.want {
			t.Errorf("isPubliclyBoundAddress(%q) = %v, want %v", tc.addr, got, tc.want)
		}
	}
}

func TestCollectExposures(t *testing.T) {
	portsByApp := map[string][]*agentpb.PortEntry{
		"web": {
			{Protocol: "tcp", Port: 8080, Address: "0.0.0.0"},   // public
			{Protocol: "tcp", Port: 9000, Address: "127.0.0.1"}, // private, skipped
		},
		"api": {
			{Protocol: "tcp", Port: 443, Address: "192.168.1.5"}, // public
		},
	}
	got := collectExposures(portsByApp)
	if len(got) != 2 {
		t.Fatalf("expected 2 exposures, got %d: %v", len(got), got)
	}
	if _, ok := got[exposureKey(exposedPort{appID: "web", protocol: "tcp", port: 8080, address: "0.0.0.0"})]; !ok {
		t.Error("web:8080/0.0.0.0 should be an exposure")
	}
	if _, ok := got[exposureKey(exposedPort{appID: "api", protocol: "tcp", port: 443, address: "192.168.1.5"})]; !ok {
		t.Error("api:443/192.168.1.5 should be an exposure")
	}
	for k := range got {
		if strings.Contains(k, "9000") {
			t.Errorf("loopback port 9000 must not be an exposure (key %q)", k)
		}
	}
}

// fakeRestartSuppressor is a recording restartSuppressor for the F2
// replace/stop-suppression tests below: it captures every Suppress call and
// every subsequent resume so tests can assert both the call and its pairing
// without depending on the real container.ContainerMonitor.
type fakeRestartSuppressor struct {
	suppressed []string
	resumed    []string
}

func (f *fakeRestartSuppressor) Suppress(containerName string) func() {
	f.suppressed = append(f.suppressed, containerName)
	return func() {
		f.resumed = append(f.resumed, containerName)
	}
}

// TestSuppressRestartsNoopWhenNoMonitor verifies suppressRestarts always
// returns a callable resume func even when no monitor was wired in (e.g.
// containerd came up before/without a monitor, or a bare *Client in a unit
// test) — callers must be able to unconditionally `defer resume()`.
func TestSuppressRestartsNoopWhenNoMonitor(t *testing.T) {
	c := &Client{}
	resume := c.suppressRestarts("com.example.app")
	if resume == nil {
		t.Fatal("suppressRestarts returned a nil func with no monitor wired")
	}
	resume() // must not panic
}

func TestStartContainerRejectsStartWhileAppStopping(t *testing.T) {
	c := &Client{
		namespace:   "default",
		appStopping: map[string]bool{"com.example.app": true},
	}
	t.Run("without stdin", func(t *testing.T) {
		_, err := c.StartContainer(context.Background(), "com.example.app", "", nil)
		if !errors.Is(err, errAppStopping) {
			t.Fatalf("StartContainer error = %v; want errAppStopping", err)
		}
	})
	t.Run("with stdin", func(t *testing.T) {
		_, err := c.StartContainerWithStdin(context.Background(), "com.example.app", strings.NewReader("input"), "", nil)
		if !errors.Is(err, errAppStopping) {
			t.Fatalf("StartContainerWithStdin error = %v; want errAppStopping", err)
		}
	})
}

// TestSuppressRestartsDelegatesToMonitor verifies suppressRestarts forwards
// to the wired restartMonitor and returns its resume func unchanged.
func TestSuppressRestartsDelegatesToMonitor(t *testing.T) {
	fake := &fakeRestartSuppressor{}
	c := &Client{restartMonitor: fake}

	resume := c.suppressRestarts("com.example.app")
	if len(fake.suppressed) != 1 || fake.suppressed[0] != "com.example.app" {
		t.Fatalf("Suppress calls = %v; want [com.example.app]", fake.suppressed)
	}
	if len(fake.resumed) != 0 {
		t.Fatalf("resume calls before resume() = %v; want none", fake.resumed)
	}

	resume()
	if len(fake.resumed) != 1 || fake.resumed[0] != "com.example.app" {
		t.Fatalf("resume calls = %v; want [com.example.app]", fake.resumed)
	}
}

// TestSetRestartSuppressor verifies the setter wires the field suppressRestarts reads.
func TestSetRestartSuppressor(t *testing.T) {
	fake := &fakeRestartSuppressor{}
	c := &Client{}
	c.SetRestartSuppressor(fake)

	c.suppressRestarts("app")
	if len(fake.suppressed) != 1 || fake.suppressed[0] != "app" {
		t.Fatalf("Suppress calls after SetRestartSuppressor = %v; want [app]", fake.suppressed)
	}
}

// TestIsMissingRuncStateDir is the regression guard for the missing-runc-
// state-dir workaround (F2): forceDeleteTask must recognize the exact error
// runc produces when a task's state directory under
// /run/containerd/runc/<ns>/<id> is gone, and extract the directory path so
// the caller can recreate it. The first case uses the literal string observed
// live on hardware.
func TestIsMissingRuncStateDir(t *testing.T) {
	tests := []struct {
		name    string
		err     error
		wantDir string
		wantOK  bool
	}{
		{
			name:    "observed live error",
			err:     errors.New("cannot open directory `/run/containerd/runc/default/com.wendylabs.examples.mcp-example`: No such file or directory"),
			wantDir: "/run/containerd/runc/default/com.wendylabs.examples.mcp-example",
			wantOK:  true,
		},
		{
			name: "nil error",
			err:  nil,
		},
		{
			name: "unrelated error",
			err:  errors.New("failed precondition"),
		},
		{
			name: "cannot open directory but not a runc state path",
			err:  errors.New("cannot open directory `/some/other/path`: No such file or directory"),
		},
		{
			name: "cannot open directory with no closing backtick",
			err:  errors.New("cannot open directory `/run/containerd/runc/default/app: No such file or directory"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir, ok := isMissingRuncStateDir(tt.err)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v; want %v", ok, tt.wantOK)
			}
			if ok && dir != tt.wantDir {
				t.Errorf("dir = %q; want %q", dir, tt.wantDir)
			}
		})
	}
}

// TestRecoverMissingRuncStateDir_PassesThroughUnrelatedErrors verifies
// recoverMissingRuncStateDir (the F2 round-1-followup finding-3 shared
// helper, used by both forceDeleteTask and terminateTask's task.Delete step)
// never invokes retry for an error isMissingRuncStateDir doesn't recognize —
// including nil, the success case.
func TestRecoverMissingRuncStateDir_PassesThroughUnrelatedErrors(t *testing.T) {
	c := &Client{logger: zap.NewNop()}

	t.Run("nil error", func(t *testing.T) {
		called := false
		got := c.recoverMissingRuncStateDir("ctr", nil, func() error {
			called = true
			return nil
		})
		if got != nil {
			t.Errorf("got %v, want nil", got)
		}
		if called {
			t.Error("retry called for nil error")
		}
	})

	t.Run("unrelated error", func(t *testing.T) {
		want := errors.New("boom")
		called := false
		got := c.recoverMissingRuncStateDir("ctr", want, func() error {
			called = true
			return nil
		})
		if got != want {
			t.Errorf("got %v, want %v", got, want)
		}
		if called {
			t.Error("retry called for an error that isn't the missing-state-dir failure")
		}
	})
}

// TestRecreateRuncStateDirAndRetry_MkdirSucceeds is the decision-flow
// regression guard for finding 3 (the missing-runc-state-dir recovery was
// unreachable from the stop path, the actual live-observed wedge): given a
// directory that doesn't exist yet but whose parent is writable, MkdirAll
// must succeed, retry must be invoked exactly once, and its result (success
// or failure) must be returned unchanged. Uses t.TempDir() rather than a real
// /run/containerd/runc/ path (isMissingRuncStateDir's prefix requirement),
// since recreateRuncStateDirAndRetry is deliberately split out to be testable
// against an arbitrary directory without needing root on a real Linux host —
// see its doc comment.
func TestRecreateRuncStateDirAndRetry_MkdirSucceeds(t *testing.T) {
	c := &Client{logger: zap.NewNop()}
	dir := filepath.Join(t.TempDir(), "default", "com.example.app")
	origErr := errors.New("cannot open directory `" + dir + "`: No such file or directory")
	retryErr := errors.New("still failing after retry")

	retryCalls := 0
	got := c.recreateRuncStateDirAndRetry("ctr", dir, origErr, func() error {
		retryCalls++
		return retryErr
	})

	if retryCalls != 1 {
		t.Fatalf("retry called %d times, want 1", retryCalls)
	}
	if got != retryErr {
		t.Errorf("got %v, want retry's error %v returned unchanged", got, retryErr)
	}
	if info, statErr := os.Stat(dir); statErr != nil || !info.IsDir() {
		t.Errorf("expected %s to be recreated as a directory (stat err: %v)", dir, statErr)
	}
}

// TestRecreateRuncStateDirAndRetry_MkdirSucceeds_RetrySucceeds covers the
// success path end to end: mkdir succeeds, retry succeeds, the original
// error is fully swallowed.
func TestRecreateRuncStateDirAndRetry_MkdirSucceeds_RetrySucceeds(t *testing.T) {
	c := &Client{logger: zap.NewNop()}
	dir := filepath.Join(t.TempDir(), "default", "com.example.app")
	origErr := errors.New("cannot open directory `" + dir + "`: No such file or directory")

	got := c.recreateRuncStateDirAndRetry("ctr", dir, origErr, func() error {
		return nil
	})

	if got != nil {
		t.Errorf("got %v, want nil after a successful retry", got)
	}
}

// TestRecreateRuncStateDirAndRetry_MkdirFails verifies that when MkdirAll
// itself fails, retry is never called and the original error is preserved
// (not the mkdir error) — a file occupying a path component that MkdirAll
// needs to descend through makes it fail portably, without needing real
// /run/containerd/runc permissions.
func TestRecreateRuncStateDirAndRetry_MkdirFails(t *testing.T) {
	c := &Client{logger: zap.NewNop()}
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatalf("setup: writing blocker file: %v", err)
	}
	dir := filepath.Join(blocker, "default", "com.example.app")
	origErr := errors.New("cannot open directory `" + dir + "`: No such file or directory")

	retryCalls := 0
	got := c.recreateRuncStateDirAndRetry("ctr", dir, origErr, func() error {
		retryCalls++
		return nil
	})

	if retryCalls != 0 {
		t.Errorf("retry called %d times after mkdir failure, want 0", retryCalls)
	}
	if got != origErr {
		t.Errorf("got %v, want original error %v preserved", got, origErr)
	}
}

// The npu entitlement grants the FastRPC transport but no userspace to drive it, so
// an entitled app must also pull in the host's Qualcomm runtime — the counterpart of
// what needsNvidiaCDI does for a Jetson.
func TestNeedsQualcommNPURuntime(t *testing.T) {
	npu := &appconfig.AppConfig{
		Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementNPU}},
	}
	if !needsQualcommNPURuntime(npu) {
		t.Error("npu entitlement should pull in the Qualcomm runtime")
	}

	gpu := &appconfig.AppConfig{
		Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementGPU}},
	}
	if needsQualcommNPURuntime(gpu) {
		t.Error("a gpu-only app must not get the Qualcomm NPU runtime")
	}
}
