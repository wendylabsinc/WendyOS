package containerd

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/internal/shared/certs"
	agentpb "github.com/wendylabsinc/wendy/proto/gen/agentpb"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// makeTestKeyAndCertPEM generates a throw-away EC P-256 key and self-signed cert
// for use in signature tests.
func makeTestKeyAndCertPEM(t *testing.T) (*ecdsa.PrivateKey, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	certPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	return key, certPEM
}

// signAnnotations adds sh.wendy/signature and sh.wendy/signature.cert to annotations.
func signAnnotations(t *testing.T, annotations map[string]string, key *ecdsa.PrivateKey, certPEM string) {
	t.Helper()
	payload := certs.EntitlementAnnotationPayload(annotations)
	sig, err := certs.SignBytes(payload, key)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	annotations[certs.AnnotationSignature] = sig
	annotations[certs.AnnotationSignatureCert] = certPEM
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

func TestBuildContainerBaseEnvIncludesWendyHostname(t *testing.T) {
	old := deviceHostnameWithSuffix
	t.Cleanup(func() { deviceHostnameWithSuffix = old })
	deviceHostnameWithSuffix = func() string { return "wendyos-test-device.local" }

	env := buildContainerBaseEnv()

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

	env := buildContainerBaseEnv()

	for _, kv := range env {
		if len(kv) >= len("WENDY_HOSTNAME=") && kv[:len("WENDY_HOSTNAME=")] == "WENDY_HOSTNAME=" {
			t.Errorf("env unexpectedly contains %q when device hostname is unresolvable", kv)
		}
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

	env := injectOTELEnvIfNeeded(nil, hostNetworkCfg())

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

	env := injectOTELEnvIfNeeded(nil, hostNetworkCfg())

	want := "OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:9999"
	for _, kv := range env {
		if kv == want {
			return
		}
	}
	t.Errorf("env missing %q; got %v", want, env)
}

func TestInjectOTELEnvSetsGRPCProtocol(t *testing.T) {
	env := injectOTELEnvIfNeeded(nil, hostNetworkCfg())

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

	env := injectOTELEnvIfNeeded(nil, cfg)

	for _, kv := range env {
		if len(kv) > len("OTEL_EXPORTER_OTLP_ENDPOINT=") &&
			kv[:len("OTEL_EXPORTER_OTLP_ENDPOINT=")] == "OTEL_EXPORTER_OTLP_ENDPOINT=" {
			t.Errorf("unexpected OTEL var injected without host networking: %q", kv)
		}
	}
}

func TestInjectOTELEnvSkipsWhenEndpointAlreadySet(t *testing.T) {
	existing := []string{"OTEL_EXPORTER_OTLP_ENDPOINT=http://custom-collector:4317"}

	env := injectOTELEnvIfNeeded(existing, hostNetworkCfg())

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

	env := injectOTELEnvIfNeeded(existing, hostNetworkCfg())

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

	env := injectOTELEnvIfNeeded(nil, hostNetworkCfg())

	const want = "OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:4317"
	for _, kv := range env {
		if kv == want {
			return
		}
	}
	t.Errorf("expected fallback to default port; got %v", env)
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

func TestStartPostStartAgentHookSkippedWhenEmpty(t *testing.T) {
	old := startPostStartHookCommand
	t.Cleanup(func() { startPostStartHookCommand = old })

	var calls int
	startPostStartHookCommand = func(_, _, _ string) (func() error, error) {
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

	var gotShell, gotFlag, gotCommand string
	startPostStartHookCommand = func(shell, flag, command string) (func() error, error) {
		gotShell = shell
		gotFlag = flag
		gotCommand = command
		return func() error { return nil }, nil
	}

	client := &Client{logger: zap.NewNop()}
	started := client.startPostStartAgentHook("echo ${WENDY_APP_ID} ${WENDY_HOSTNAME} ${EXTRA_VALUE}", "camera-app")
	if !started {
		t.Fatal("startPostStartAgentHook returned false with command")
	}
	if gotShell == "" || gotFlag == "" {
		t.Fatalf("shell command not populated: shell=%q flag=%q", gotShell, gotFlag)
	}
	wantCommand := "echo camera-app localhost ok"
	if gotCommand != wantCommand {
		t.Fatalf("hook command = %q; want %q", gotCommand, wantCommand)
	}
}

func TestStartPostStartAgentHookStartErrorDoesNotLogCommand(t *testing.T) {
	old := startPostStartHookCommand
	t.Cleanup(func() { startPostStartHookCommand = old })

	startPostStartHookCommand = func(_, _, _ string) (func() error, error) {
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

func TestCheckManifestSignature(t *testing.T) {
	key, certPEM := makeTestKeyAndCertPEM(t)
	otherKey, _ := makeTestKeyAndCertPEM(t) // different key, same cert issuer

	entitlementAnnotations := func() map[string]string {
		return map[string]string{
			"sh.wendy/entitlement.bluetooth": `{}`,
		}
	}

	tests := []struct {
		name     string
		annots   func() map[string]string
		enrolled bool
		wantErr  string
	}{
		{
			name:     "no annotations, not enrolled",
			annots:   func() map[string]string { return nil },
			enrolled: false,
		},
		{
			name:     "no annotations, enrolled",
			annots:   func() map[string]string { return nil },
			enrolled: true,
			wantErr:  "unsigned",
		},
		{
			name: "valid signature, not enrolled",
			annots: func() map[string]string {
				a := entitlementAnnotations()
				signAnnotations(t, a, key, certPEM)
				return a
			},
			enrolled: false,
		},
		{
			name: "valid signature, enrolled",
			annots: func() map[string]string {
				a := entitlementAnnotations()
				signAnnotations(t, a, key, certPEM)
				return a
			},
			enrolled: true,
		},
		{
			name: "valid signature on empty entitlements, enrolled",
			annots: func() map[string]string {
				a := map[string]string{}
				signAnnotations(t, a, key, certPEM)
				return a
			},
			enrolled: true,
		},
		{
			name: "corrupt signature, not enrolled",
			annots: func() map[string]string {
				a := entitlementAnnotations()
				signAnnotations(t, a, key, certPEM)
				a[certs.AnnotationSignature] = "AAAA" // corrupt
				return a
			},
			enrolled: false,
			wantErr:  "verification failed",
		},
		{
			name: "corrupt signature, enrolled",
			annots: func() map[string]string {
				a := entitlementAnnotations()
				signAnnotations(t, a, key, certPEM)
				a[certs.AnnotationSignature] = "AAAA" // corrupt
				return a
			},
			enrolled: true,
			wantErr:  "verification failed",
		},
		{
			name: "tampered entitlement after signing, enrolled",
			annots: func() map[string]string {
				a := entitlementAnnotations()
				signAnnotations(t, a, key, certPEM)
				a["sh.wendy/entitlement.bluetooth"] = `{"port":9999}` // tamper
				return a
			},
			enrolled: true,
			wantErr:  "verification failed",
		},
		{
			name: "signature by wrong key, enrolled",
			annots: func() map[string]string {
				a := entitlementAnnotations()
				signAnnotations(t, a, otherKey, certPEM) // signed by different key
				return a
			},
			enrolled: true,
			wantErr:  "verification failed",
		},
		{
			name: "sig present but cert missing, enrolled",
			annots: func() map[string]string {
				a := entitlementAnnotations()
				signAnnotations(t, a, key, certPEM)
				delete(a, certs.AnnotationSignatureCert) // strip cert
				return a
			},
			enrolled: true,
			wantErr:  "unsigned",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := checkManifestSignature(tc.annots(), tc.enrolled)
			if tc.wantErr == "" {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
			} else {
				if err == nil {
					t.Errorf("expected error containing %q, got nil", tc.wantErr)
				} else if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error %q does not contain %q", err.Error(), tc.wantErr)
				}
			}
		})
	}
}
