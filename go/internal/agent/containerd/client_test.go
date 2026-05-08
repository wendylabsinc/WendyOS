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

// makeTestCA generates a throw-away EC P-256 self-signed CA certificate.
func makeTestCA(t *testing.T, cn string) (*ecdsa.PrivateKey, *x509.Certificate, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create CA cert: %v", err)
	}
	cert, _ := x509.ParseCertificate(der)
	certPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	return key, cert, certPEM
}

// makeTestLeafCert issues a leaf cert signed by caKey/caCert with the given CN.
func makeTestLeafCert(t *testing.T, cn string, caKey *ecdsa.PrivateKey, caCert *x509.Certificate) (*ecdsa.PrivateKey, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate leaf key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create leaf cert: %v", err)
	}
	certPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	return key, certPEM
}

// makeTestLeafCertExpired issues an already-expired leaf cert signed by caKey/caCert.
// Returns the key, PEM, and the midpoint signing time (when it was valid).
func makeTestLeafCertExpired(t *testing.T, cn string, caKey *ecdsa.PrivateKey, caCert *x509.Certificate) (*ecdsa.PrivateKey, string, time.Time) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate leaf key: %v", err)
	}
	notBefore := time.Now().Add(-2 * time.Hour)
	notAfter := time.Now().Add(-30 * time.Minute)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(99),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create expired leaf cert: %v", err)
	}
	certPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	signingTime := notBefore.Add(time.Hour) // midpoint: cert was valid then
	return key, certPEM, signingTime
}

// makeTestKeyAndCertPEM generates a self-signed CA cert, usable as both a leaf
// (for signing) and a root (for pool construction) in simple tests.
func makeTestKeyAndCertPEM(t *testing.T) (*ecdsa.PrivateKey, string) {
	t.Helper()
	key, _, certPEM := makeTestCA(t, "test-ca")
	return key, certPEM
}

// poolFromPEM builds an x509.CertPool containing the given PEM cert.
func poolFromPEM(t *testing.T, certPEM string) *x509.CertPool {
	t.Helper()
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(certPEM)) {
		t.Fatal("failed to build cert pool from PEM")
	}
	return pool
}

// signAnnotations adds sh.wendy/signature and sh.wendy/signature.cert to annotations.
// contentDigests may be nil for tests that do not exercise content binding.
// repo, when non-empty, sets sh.wendy/signed.repo in annotations before signing.
// signingTime, when non-zero, sets sh.wendy/signed.at in annotations before signing.
func signAnnotations(t *testing.T, annotations map[string]string, contentDigests []string, key *ecdsa.PrivateKey, certPEM string, repo string, signingTime time.Time) {
	t.Helper()
	if repo != "" {
		annotations[certs.AnnotationSignedRepo] = repo
	}
	if !signingTime.IsZero() {
		annotations[certs.AnnotationSignedAt] = signingTime.UTC().Format(time.RFC3339)
	}
	payload := certs.SigningPayload(contentDigests, annotations)
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
	// Per-org CAs: each org has its own CA that signs its certs.
	org1CAKey, org1CACert, org1CAPEM := makeTestCA(t, "org-1-ca")
	org2CAKey, org2CACert, org2CAPEM := makeTestCA(t, "org-2-ca")
	org1Pool := poolFromPEM(t, org1CAPEM)
	org2Pool := poolFromPEM(t, org2CAPEM)

	// Leaf certs issued by each org's CA with Wendy CN format.
	org1Key, org1CertPEM := makeTestLeafCert(t, "wendy/1/42", org1CAKey, org1CACert)
	org2Key, org2CertPEM := makeTestLeafCert(t, "wendy/2/99", org2CAKey, org2CACert)

	// Shared CA: both orgs' certs signed by the same root (simulates flat PKI).
	sharedCAKey, sharedCACert, sharedCAPEM := makeTestCA(t, "shared-root-ca")
	sharedPool := poolFromPEM(t, sharedCAPEM)
	sharedOrg1Key, sharedOrg1CertPEM := makeTestLeafCert(t, "wendy/1/42", sharedCAKey, sharedCACert)
	sharedOrg2Key, sharedOrg2CertPEM := makeTestLeafCert(t, "wendy/2/99", sharedCAKey, sharedCACert)

	// User cert: CN has no numeric org ID.
	userKey, userCertPEM := makeTestLeafCert(t, "wendy/user/alice", org1CAKey, org1CACert)

	entitlementAnnotations := func() map[string]string {
		return map[string]string{"sh.wendy/entitlement.bluetooth": ""}
	}

	// Expired leaf cert and its midpoint signing time (used for cert expiry tests).
	expiredKey, expiredCertPEM, validSigningTime := makeTestLeafCertExpired(t, "wendy/1/42", org1CAKey, org1CACert)

	tests := []struct {
		name           string
		annots         func() map[string]string
		contentDigests []string       // digests used when signing and verifying
		checkDigests   []string       // if non-nil, overrides contentDigests at verify time
		expectedRepo   string         // empty = skip repo check
		trustedPool    *x509.CertPool // nil = unenrolled
		orgID          int32
		wantErr        string
	}{
		// ── Basic enrollment checks ──────────────────────────────────────────────
		{
			name:        "no annotations, not enrolled",
			annots:      func() map[string]string { return nil },
			trustedPool: nil,
		},
		{
			name:        "no annotations, enrolled",
			annots:      func() map[string]string { return nil },
			trustedPool: org1Pool,
			orgID:       1,
			wantErr:     "unsigned",
		},
		{
			name: "valid signature, not enrolled",
			annots: func() map[string]string {
				a := entitlementAnnotations()
				signAnnotations(t, a, nil, org1Key, org1CertPEM, "", time.Time{})
				return a
			},
			trustedPool: nil,
		},
		{
			name: "valid signature on empty entitlements, enrolled",
			annots: func() map[string]string {
				a := map[string]string{}
				signAnnotations(t, a, nil, org1Key, org1CertPEM, "", time.Time{})
				return a
			},
			trustedPool: org1Pool,
			orgID:       1,
		},
		{
			name: "sig present but cert missing, enrolled",
			annots: func() map[string]string {
				a := entitlementAnnotations()
				signAnnotations(t, a, nil, org1Key, org1CertPEM, "", time.Time{})
				delete(a, certs.AnnotationSignatureCert)
				return a
			},
			trustedPool: org1Pool,
			orgID:       1,
			wantErr:     "unsigned",
		},

		// ── Signature integrity ──────────────────────────────────────────────────
		{
			name: "corrupt signature",
			annots: func() map[string]string {
				a := entitlementAnnotations()
				signAnnotations(t, a, nil, org1Key, org1CertPEM, "", time.Time{})
				a[certs.AnnotationSignature] = "AAAA"
				return a
			},
			trustedPool: org1Pool,
			orgID:       1,
			wantErr:     "verification failed",
		},
		{
			name: "tampered entitlement after signing",
			annots: func() map[string]string {
				a := entitlementAnnotations()
				signAnnotations(t, a, nil, org1Key, org1CertPEM, "", time.Time{})
				a["sh.wendy/entitlement.bluetooth"] = `{"port":9999}`
				return a
			},
			trustedPool: org1Pool,
			orgID:       1,
			wantErr:     "verification failed",
		},

		// ── Content digest binding ───────────────────────────────────────────────
		{
			name: "valid signature with content digests",
			annots: func() map[string]string {
				a := entitlementAnnotations()
				signAnnotations(t, a, []string{"sha256:aaa", "sha256:bbb"}, org1Key, org1CertPEM, "", time.Time{})
				return a
			},
			contentDigests: []string{"sha256:aaa", "sha256:bbb"},
			trustedPool:    org1Pool,
			orgID:          1,
		},
		{
			name: "swapped layers (different digests) detected",
			annots: func() map[string]string {
				a := entitlementAnnotations()
				signAnnotations(t, a, []string{"sha256:aaa", "sha256:bbb"}, org1Key, org1CertPEM, "", time.Time{})
				return a
			},
			contentDigests: []string{"sha256:aaa", "sha256:bbb"},
			checkDigests:   []string{"sha256:aaa", "sha256:evil"}, // layer replaced
			trustedPool:    org1Pool,
			orgID:          1,
			wantErr:        "verification failed",
		},
		{
			// An image signed before content-digest binding was introduced (nil
			// digests at sign time) must be rejected by an agent that extracts
			// real layer/config digests from the manifest. Payloads differ → fail.
			name: "old-format signature (no content digests) rejected when agent has manifest digests",
			annots: func() map[string]string {
				a := entitlementAnnotations()
				signAnnotations(t, a, nil, org1Key, org1CertPEM, "", time.Time{}) // old format
				return a
			},
			contentDigests: []string{"sha256:config", "sha256:layer0"}, // agent extracts these
			trustedPool:    org1Pool,
			orgID:          1,
			wantErr:        "verification failed",
		},
		{
			// If the agent somehow ends up with no content digests (e.g. manifest
			// parsing returned nothing) but the image was signed with digests, the
			// payloads differ and verification must fail.
			name: "signature with content digests rejected when agent has no manifest digests",
			annots: func() map[string]string {
				a := entitlementAnnotations()
				signAnnotations(t, a, []string{"sha256:aaa"}, org1Key, org1CertPEM, "", time.Time{})
				return a
			},
			contentDigests: nil, // agent extracted nothing
			trustedPool:    org1Pool,
			orgID:          1,
			wantErr:        "verification failed",
		},

		// ── Per-org CA isolation (structural PKI isolation) ──────────────────────
		{
			name: "per-org CAs: org1 cert accepted by org1 device",
			annots: func() map[string]string {
				a := entitlementAnnotations()
				signAnnotations(t, a, nil, org1Key, org1CertPEM, "", time.Time{})
				return a
			},
			trustedPool: org1Pool,
			orgID:       1,
		},
		{
			name: "per-org CAs: org2 cert rejected by org1 device (chain mismatch)",
			annots: func() map[string]string {
				a := entitlementAnnotations()
				signAnnotations(t, a, nil, org2Key, org2CertPEM, "", time.Time{})
				return a
			},
			trustedPool: org1Pool, // org2 cert does not chain to org1 CA
			orgID:       1,
			wantErr:     "not trusted",
		},

		// ── Shared root CA — the cross-org gap ───────────────────────────────────
		// When all orgs are signed by the same CA, chain validation alone is
		// insufficient: org2's cert chains to the same root as org1's pool.
		{
			name: "shared CA, no orgID check: org2 cert passes org1 chain (gap demonstrated)",
			annots: func() map[string]string {
				a := entitlementAnnotations()
				signAnnotations(t, a, nil, sharedOrg2Key, sharedOrg2CertPEM, "", time.Time{})
				return a
			},
			trustedPool: sharedPool,
			orgID:       0, // no org ID check → cross-org accepted (the gap)
			// wantErr intentionally empty to show the gap exists
		},
		{
			name: "shared CA, with orgID check: org2 cert rejected by org1 device",
			annots: func() map[string]string {
				a := entitlementAnnotations()
				signAnnotations(t, a, nil, sharedOrg2Key, sharedOrg2CertPEM, "", time.Time{})
				return a
			},
			trustedPool: sharedPool,
			orgID:       1, // CN says org 2, device expects org 1 → rejected
			wantErr:     "org 2",
		},
		{
			name: "shared CA, org1 cert accepted by org1 device with orgID check",
			annots: func() map[string]string {
				a := entitlementAnnotations()
				signAnnotations(t, a, nil, sharedOrg1Key, sharedOrg1CertPEM, "", time.Time{})
				return a
			},
			trustedPool: sharedPool,
			orgID:       1,
		},

		// ── User certs (no org ID in CN) ─────────────────────────────────────────
		// User certs (CN "wendy/user/<id>") carry no numeric org ID; the CN
		// check is skipped. Cross-org isolation for user certs relies entirely
		// on per-org intermediate CAs in the PKI.
		{
			name: "user cert accepted when chain matches and no org ID in CN",
			annots: func() map[string]string {
				a := entitlementAnnotations()
				signAnnotations(t, a, nil, userKey, userCertPEM, "", time.Time{})
				return a
			},
			trustedPool: org1Pool,
			orgID:       1, // CN "wendy/user/alice" → certOrgID returns 0 → check skipped
		},
		{
			name: "user cert from different org's CA rejected by chain check",
			annots: func() map[string]string {
				a := entitlementAnnotations()
				signAnnotations(t, a, nil, userKey, userCertPEM, "", time.Time{}) // signed by org1 CA
				return a
			},
			trustedPool: org2Pool, // org2 device trusts org2 CA only
			orgID:       2,
			wantErr:     "not trusted",
		},

		// ── Docker Hub / cosign compatibility note ────────────────────────────────
		// Docker Content Trust (Notary) and cosign store signatures as separate
		// OCI artefacts / in a Notary server — not in manifest annotations. This
		// system is independent of and not compatible with those mechanisms.
		// A Docker Hub-signed image with no sh.wendy/signature annotation is
		// treated as unsigned by an enrolled device and rejected.
		{
			name: "docker-hub-signed image (no wendy annotation) rejected on enrolled device",
			annots: func() map[string]string {
				// Simulate a Docker Hub image: has standard OCI annotations
				// but no sh.wendy/signature annotation.
				return map[string]string{
					"org.opencontainers.image.created": "2024-01-01T00:00:00Z",
				}
			},
			trustedPool: org1Pool,
			orgID:       1,
			wantErr:     "unsigned",
		},

		// ── Repository binding (Fix #1) ──────────────────────────────────────────
		{
			name: "correct repo binding accepted",
			annots: func() map[string]string {
				a := entitlementAnnotations()
				signAnnotations(t, a, nil, org1Key, org1CertPEM, "myapp", time.Time{})
				return a
			},
			expectedRepo: "myapp",
			trustedPool:  org1Pool,
			orgID:        1,
		},
		{
			name: "repo mismatch: signed for app-a, deployed as app-b",
			annots: func() map[string]string {
				a := entitlementAnnotations()
				signAnnotations(t, a, nil, org1Key, org1CertPEM, "app-a", time.Time{})
				return a
			},
			expectedRepo: "app-b",
			trustedPool:  org1Pool,
			orgID:        1,
			wantErr:      "deployed as",
		},
		{
			name: "missing signed.repo annotation when repo check expected",
			annots: func() map[string]string {
				a := entitlementAnnotations()
				signAnnotations(t, a, nil, org1Key, org1CertPEM, "", time.Time{})
				return a
			},
			expectedRepo: "myapp",
			trustedPool:  org1Pool,
			orgID:        1,
			wantErr:      "signed.repo missing",
		},

		// ── Signing timestamp / cert expiry (Fix #4 & #7) ───────────────────────
		{
			name: "expired cert accepted when signing time annotation is present",
			annots: func() map[string]string {
				a := entitlementAnnotations()
				signAnnotations(t, a, nil, expiredKey, expiredCertPEM, "", validSigningTime)
				return a
			},
			trustedPool: org1Pool,
			orgID:       1,
		},
		{
			name: "expired cert rejected when signing time annotation is absent",
			annots: func() map[string]string {
				a := entitlementAnnotations()
				signAnnotations(t, a, nil, expiredKey, expiredCertPEM, "", time.Time{})
				return a
			},
			trustedPool: org1Pool,
			orgID:       1,
			wantErr:     "not trusted",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			verifyDigests := tc.contentDigests
			if tc.checkDigests != nil {
				verifyDigests = tc.checkDigests
			}
			err := checkManifestSignature(tc.annots(), verifyDigests, tc.expectedRepo, tc.trustedPool, tc.orgID)
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

func TestImageRepo(t *testing.T) {
	tests := []struct {
		name string
		want string
	}{
		{"myapp", "myapp"},
		{"myapp:latest", "myapp"},
		{"192.168.1.1:5000/myapp:latest", "myapp"},
		{"localhost:5000/org/app:v1", "org/app"},
		{"registry.example.com/org/app", "org/app"},
		{"org/app:v1", "org/app"},
	}
	for _, tc := range tests {
		if got := imageRepo(tc.name); got != tc.want {
			t.Errorf("imageRepo(%q) = %q; want %q", tc.name, got, tc.want)
		}
	}
}

func TestCertOrgID(t *testing.T) {
	tests := []struct {
		cn   string
		want int32
	}{
		{"wendy/1/42", 1},
		{"wendy/2/99", 2},
		{"sh/wendy/7/3", 7},
		{"wendy/user/alice", 0}, // user cert — no numeric org
		{"wendy/user/123", 0},   // user ID happens to be numeric but prefix is "user"
		{"unrelated-cn", 0},
		{"", 0},
	}
	for _, tc := range tests {
		if got := certOrgID(tc.cn); got != tc.want {
			t.Errorf("certOrgID(%q) = %d; want %d", tc.cn, got, tc.want)
		}
	}
}
