# Pre-Provision Wendy Agent at Imaging Time — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `wendy os install --pre-enroll` so a device is enrolled with Wendy Cloud and running mTLS from first boot, with no post-boot enrollment step.

**Architecture:** The CLI generates a key pair, gets an enrollment token, issues a certificate from the cloud, and serialises the result as `provisioning.json` into the config partition during imaging. On first boot, `configpartition.Apply()` reads that file, writes it to `/etc/wendy-agent/`, then deletes the source. `ProvisioningService.loadState()` picks it up naturally because `Apply()` runs before service init.

**Tech Stack:** Go, gRPC (`cloudpb.CertificateServiceClient`), `crypto/ecdsa`, `encoding/json`, `os`/`path/filepath`, Cobra, Bubble Tea TUI helpers.

---

### Task 1: Agent — `applyPreProvisioning` (TDD)

**Files:**
- Modify: `go/internal/agent/configpartition/apply.go`
- Modify: `go/internal/agent/configpartition/apply_test.go`
- Modify: `go/cmd/wendy-agent/main.go`

---

- [ ] **Step 1: Write failing tests for `applyPreProvisioning`**

Append to `go/internal/agent/configpartition/apply_test.go`:

```go
func TestApplyPreProvisioning_Success(t *testing.T) {
	cfgDir := t.TempDir()
	configPath := t.TempDir()

	state := `{"enrolled":true,"cloudHost":"cloud.wendy.sh","orgId":1,"assetId":42,"keyPem":"fake-key","certPem":"fake-cert","chainPem":"fake-chain"}`
	if err := os.WriteFile(filepath.Join(cfgDir, "provisioning.json"), []byte(state), 0o600); err != nil {
		t.Fatal(err)
	}

	logger, _ := zap.NewDevelopment()
	applyPreProvisioning(logger, cfgDir, configPath)

	if _, err := os.Stat(filepath.Join(cfgDir, "provisioning.json")); !os.IsNotExist(err) {
		t.Error("source provisioning.json should be deleted after apply")
	}

	got, err := os.ReadFile(filepath.Join(configPath, "provisioning.json"))
	if err != nil {
		t.Fatalf("provisioning.json not written to configPath: %v", err)
	}
	if string(got) != state {
		t.Errorf("provisioning.json content = %q; want %q", got, state)
	}

	for _, name := range []string{"device-key.pem", "device.pem", "ca.pem"} {
		if _, err := os.Stat(filepath.Join(configPath, name)); err != nil {
			t.Errorf("%s not written: %v", name, err)
		}
	}

	info, err := os.Stat(filepath.Join(configPath, "device-key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("device-key.pem mode = %o; want 0600", info.Mode().Perm())
	}

	if _, err := os.Stat(filepath.Join(configPath, ".provisioned")); err != nil {
		t.Error(".provisioned marker not written")
	}
}

func TestApplyPreProvisioning_NoFile(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	applyPreProvisioning(logger, t.TempDir(), t.TempDir()) // must not panic
}

func TestApplyPreProvisioning_MalformedJSON(t *testing.T) {
	cfgDir := t.TempDir()
	configPath := t.TempDir()
	srcPath := filepath.Join(cfgDir, "provisioning.json")
	if err := os.WriteFile(srcPath, []byte("not json {{"), 0o600); err != nil {
		t.Fatal(err)
	}
	logger, _ := zap.NewDevelopment()
	applyPreProvisioning(logger, cfgDir, configPath)

	if _, err := os.Stat(srcPath); !os.IsNotExist(err) {
		t.Error("malformed source should be deleted")
	}
	if _, err := os.Stat(filepath.Join(configPath, "provisioning.json")); !os.IsNotExist(err) {
		t.Error("provisioning.json must not be written for malformed input")
	}
}

func TestApplyPreProvisioning_IncompleteState(t *testing.T) {
	cfgDir := t.TempDir()
	configPath := t.TempDir()
	srcPath := filepath.Join(cfgDir, "provisioning.json")
	// Missing keyPem — should be rejected.
	if err := os.WriteFile(srcPath, []byte(`{"enrolled":true,"cloudHost":"cloud.wendy.sh","certPem":"cert"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	logger, _ := zap.NewDevelopment()
	applyPreProvisioning(logger, cfgDir, configPath)

	if _, err := os.Stat(srcPath); !os.IsNotExist(err) {
		t.Error("incomplete source should be deleted")
	}
	if _, err := os.Stat(filepath.Join(configPath, "provisioning.json")); !os.IsNotExist(err) {
		t.Error("provisioning.json must not be written for incomplete input")
	}
}

func TestApplyPreProvisioning_CreatesConfigDir(t *testing.T) {
	cfgDir := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "subdir", "wendy-agent")

	state := `{"enrolled":true,"cloudHost":"cloud.wendy.sh","orgId":1,"assetId":42,"keyPem":"k","certPem":"c","chainPem":"ch"}`
	if err := os.WriteFile(filepath.Join(cfgDir, "provisioning.json"), []byte(state), 0o600); err != nil {
		t.Fatal(err)
	}
	logger, _ := zap.NewDevelopment()
	applyPreProvisioning(logger, cfgDir, configPath)

	if _, err := os.Stat(filepath.Join(configPath, "provisioning.json")); err != nil {
		t.Errorf("configPath should be created automatically: %v", err)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

```bash
cd go && go test ./internal/agent/configpartition/... -run TestApplyPreProvisioning -v
```

Expected: compile error — `applyPreProvisioning` undefined.

- [ ] **Step 3: Add `preProvisionedState` and `applyPreProvisioning` to `apply.go`**

Add `"encoding/json"` and `"time"` to the import block in `go/internal/agent/configpartition/apply.go`.

Add after the `applyWendyConf` function:

```go
// preProvisionedState is the provisioning state written by the CLI during imaging.
// JSON tags must match provisioningState in internal/agent/services.
type preProvisionedState struct {
	Enrolled  bool   `json:"enrolled"`
	CloudHost string `json:"cloudHost,omitempty"`
	OrgID     int32  `json:"orgId,omitempty"`
	AssetID   int32  `json:"assetId,omitempty"`
	KeyPEM    string `json:"keyPem,omitempty"`
	CertPEM   string `json:"certPem,omitempty"`
	ChainPEM  string `json:"chainPem,omitempty"`
}

// applyPreProvisioning checks cfgDir for a provisioning.json written by the CLI
// at imaging time. If present and valid, it copies the state to configPath so
// ProvisioningService.loadState() picks it up on first boot, then deletes the source.
func applyPreProvisioning(logger *zap.Logger, cfgDir, configPath string) {
	srcPath := filepath.Join(cfgDir, "provisioning.json")
	data, err := os.ReadFile(srcPath)
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		logger.Error("Failed to read pre-provisioning state from config partition",
			zap.String("path", srcPath), zap.Error(err))
		os.Remove(srcPath) //nolint:errcheck
		return
	}

	var state preProvisionedState
	if err := json.Unmarshal(data, &state); err != nil {
		logger.Error("Failed to parse pre-provisioning state, removing",
			zap.String("path", srcPath), zap.Error(err))
		os.Remove(srcPath) //nolint:errcheck
		return
	}

	if !state.Enrolled || state.KeyPEM == "" || state.CertPEM == "" || state.CloudHost == "" {
		logger.Error("Pre-provisioning state is incomplete, removing",
			zap.String("path", srcPath))
		os.Remove(srcPath) //nolint:errcheck
		return
	}

	if err := os.MkdirAll(configPath, 0o700); err != nil {
		logger.Error("Failed to create config directory for pre-provisioning",
			zap.String("path", configPath), zap.Error(err))
		return
	}

	if err := os.WriteFile(filepath.Join(configPath, "provisioning.json"), data, 0o600); err != nil {
		logger.Error("Failed to write provisioning.json from config partition", zap.Error(err))
		return
	}

	pemFiles := []struct {
		name string
		data string
		mode os.FileMode
	}{
		{"device-key.pem", state.KeyPEM, 0o600},
		{"device.pem", state.CertPEM, 0o644},
		{"ca.pem", state.ChainPEM, 0o644},
	}
	for _, f := range pemFiles {
		if f.data == "" {
			continue
		}
		if err := os.WriteFile(filepath.Join(configPath, f.name), []byte(f.data), f.mode); err != nil {
			logger.Error("Failed to write PEM file from config partition",
				zap.String("name", f.name), zap.Error(err))
			return
		}
	}

	_ = os.WriteFile(filepath.Join(configPath, ".provisioned"),
		[]byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o644)

	if err := os.Remove(srcPath); err != nil {
		logger.Warn("Failed to remove pre-provisioning state from config partition",
			zap.String("path", srcPath), zap.Error(err))
	}

	logger.Info("Applied pre-provisioned state from config partition",
		zap.String("cloudHost", state.CloudHost),
		zap.Int32("orgId", state.OrgID),
		zap.Int32("assetId", state.AssetID),
	)
}
```

- [ ] **Step 4: Update `Apply()` to accept `configPath` and call `applyPreProvisioning`**

Replace the existing `Apply` function in `go/internal/agent/configpartition/apply.go`:

```go
// Apply checks the config partition for a pending agent binary, WiFi config, and
// pre-provisioning state, applying them in order. If a binary update is installed,
// the process exits so systemd can restart it with the new binary.
// configPath is the agent's configuration directory (e.g. /etc/wendy-agent).
func Apply(logger *zap.Logger, configPath string) {
	installPath := defaultInstallPath
	if exe, err := os.Executable(); err == nil {
		if real, err := filepath.EvalSymlinks(exe); err == nil {
			installPath = real
		}
	}
	if applyBinaryUpdate(logger, configDir, installPath) {
		os.Exit(0)
	}
	applyWendyConf(logger, configDir)
	applyPreProvisioning(logger, configDir, configPath)
}
```

- [ ] **Step 5: Update `main.go` to pass `configPath` to `Apply`**

In `go/cmd/wendy-agent/main.go`, replace:

```go
configpartition.Apply(logger)
```

with:

```go
configpartition.Apply(logger, configPath)
```

(This line is at line 65; `configPath` is resolved on lines 110–113 of the same file — move the `configPath` resolution block to before line 65.)

The updated block around line 65 in `main.go`:

```go
configPath := "/etc/wendy-agent"
if envPath := os.Getenv("WENDY_CONFIG_PATH"); envPath != "" {
    configPath = envPath
}

configpartition.Apply(logger, configPath)
services.CommitMenderUpdate(logger)
```

Then remove the duplicate `configPath` resolution that currently sits at line 110.

- [ ] **Step 6: Run all agent tests**

```bash
cd go && go test ./internal/agent/configpartition/... -v
```

Expected: all pass, including the four new `TestApplyPreProvisioning_*` tests.

- [ ] **Step 7: Build the agent to confirm no compile errors**

```bash
cd go && go build ./cmd/wendy-agent/...
```

Expected: exits 0, no output.

- [ ] **Step 8: Commit**

```bash
cd go && git add internal/agent/configpartition/apply.go internal/agent/configpartition/apply_test.go cmd/wendy-agent/main.go
git commit -m "feat(agent): apply pre-provisioned cert from config partition on boot"
```

---

### Task 2: CLI — `preEnrollDevice` (TDD)

**Files:**
- Modify: `go/internal/cli/commands/os_provision.go`
- Create: `go/internal/cli/commands/os_provision_test.go`

---

- [ ] **Step 1: Write failing tests in new file `os_provision_test.go`**

Create `go/internal/cli/commands/os_provision_test.go`:

```go
package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/wendylabsinc/wendy/internal/shared/config"
	cloudpb "github.com/wendylabsinc/wendy/proto/gen/cloudpb"
)

// fakePreEnrollCertService implements CertificateService for pre-enrollment tests.
type fakePreEnrollCertService struct {
	cloudpb.UnimplementedCertificateServiceServer
	orgID     int32
	assetID   int32
	token     string
	certPEM   string
	chainPEM  string
	tokenErr  error
	issueErr  error
	emptyCert bool
}

func (f *fakePreEnrollCertService) CreateAssetEnrollmentToken(_ context.Context, _ *cloudpb.CreateAssetEnrollmentTokenRequest) (*cloudpb.CreateAssetEnrollmentTokenResponse, error) {
	if f.tokenErr != nil {
		return nil, f.tokenErr
	}
	return &cloudpb.CreateAssetEnrollmentTokenResponse{
		OrganizationId:  f.orgID,
		AssetId:         f.assetID,
		EnrollmentToken: f.token,
	}, nil
}

func (f *fakePreEnrollCertService) IssueCertificate(_ context.Context, _ *cloudpb.IssueCertificateRequest) (*cloudpb.IssueCertificateResponse, error) {
	if f.issueErr != nil {
		return nil, f.issueErr
	}
	if f.emptyCert {
		return &cloudpb.IssueCertificateResponse{}, nil
	}
	return &cloudpb.IssueCertificateResponse{
		Certificate: &cloudpb.Certificate{
			PemCertificate:      f.certPEM,
			PemCertificateChain: f.chainPEM,
		},
	}, nil
}

// startPreEnrollFakeServer starts a gRPC server backed by svc and returns a
// PreEnrollDialer that connects to it, ignoring the addr/opt arguments.
func startPreEnrollFakeServer(t *testing.T, svc *fakePreEnrollCertService) PreEnrollDialer {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	cloudpb.RegisterCertificateServiceServer(srv, svc)
	go srv.Serve(lis)
	t.Cleanup(func() { srv.GracefulStop(); lis.Close() })

	addr := lis.Addr().String()
	return func(_ context.Context, _ string, _ grpc.DialOption) (*grpc.ClientConn, error) {
		return grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}
}

func fakeAuth() *config.AuthConfig {
	return &config.AuthConfig{
		CloudGRPC: "localhost:9999",
		Certificates: []config.CertificateInfo{
			{
				PemCertificate:      "fake-cert",
				PemCertificateChain: "fake-chain",
				PemPrivateKey:       "fake-key",
				OrganizationID:      7,
			},
		},
	}
}

func TestPreEnrollDevice_Success(t *testing.T) {
	dialer := startPreEnrollFakeServer(t, &fakePreEnrollCertService{
		orgID:    7,
		assetID:  42,
		token:    "tok",
		certPEM:  "device-cert",
		chainPEM: "ca-chain",
	})

	data, err := preEnrollDevice(context.Background(), fakeAuth(), "my-device", dialer)
	if err != nil {
		t.Fatalf("preEnrollDevice: %v", err)
	}

	var state preProvisionedState
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if !state.Enrolled {
		t.Error("enrolled should be true")
	}
	if state.OrgID != 7 {
		t.Errorf("orgId = %d; want 7", state.OrgID)
	}
	if state.AssetID != 42 {
		t.Errorf("assetId = %d; want 42", state.AssetID)
	}
	if state.CertPEM != "device-cert" {
		t.Errorf("certPem = %q; want device-cert", state.CertPEM)
	}
	if state.ChainPEM != "ca-chain" {
		t.Errorf("chainPem = %q; want ca-chain", state.ChainPEM)
	}
	if state.KeyPEM == "" {
		t.Error("keyPem must not be empty")
	}
	if state.CloudHost == "" {
		t.Error("cloudHost must not be empty")
	}
}

func TestPreEnrollDevice_WritesFile(t *testing.T) {
	dialer := startPreEnrollFakeServer(t, &fakePreEnrollCertService{
		orgID: 1, assetID: 1, token: "t", certPEM: "c", chainPEM: "ch",
	})

	data, err := preEnrollDevice(context.Background(), fakeAuth(), "", dialer)
	if err != nil {
		t.Fatalf("preEnrollDevice: %v", err)
	}

	// Caller writes the returned JSON — verify it is valid and has correct mode when written.
	dir := t.TempDir()
	path := filepath.Join(dir, "provisioning.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o; want 0600", info.Mode().Perm())
	}
}

func TestPreEnrollDevice_NoAuthCerts(t *testing.T) {
	auth := &config.AuthConfig{CloudGRPC: "localhost:9999", Certificates: nil}
	_, err := preEnrollDevice(context.Background(), auth, "", nil)
	if err == nil {
		t.Fatal("expected error with no auth certificates")
	}
}

func TestPreEnrollDevice_TokenError(t *testing.T) {
	dialer := startPreEnrollFakeServer(t, &fakePreEnrollCertService{
		tokenErr: fmt.Errorf("token denied"),
	})
	_, err := preEnrollDevice(context.Background(), fakeAuth(), "", dialer)
	if err == nil {
		t.Fatal("expected error when token creation fails")
	}
}

func TestPreEnrollDevice_IssueError(t *testing.T) {
	dialer := startPreEnrollFakeServer(t, &fakePreEnrollCertService{
		orgID: 1, assetID: 1, token: "t",
		issueErr: fmt.Errorf("issuance rejected"),
	})
	_, err := preEnrollDevice(context.Background(), fakeAuth(), "", dialer)
	if err == nil {
		t.Fatal("expected error when certificate issuance fails")
	}
}

func TestPreEnrollDevice_EmptyCert(t *testing.T) {
	dialer := startPreEnrollFakeServer(t, &fakePreEnrollCertService{
		orgID: 1, assetID: 1, token: "t",
		emptyCert: true,
	})
	_, err := preEnrollDevice(context.Background(), fakeAuth(), "", dialer)
	if err == nil {
		t.Fatal("expected error when cloud returns empty certificate")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

```bash
cd go && go test ./internal/cli/commands/... -run TestPreEnrollDevice -v
```

Expected: compile error — `preEnrollDevice`, `PreEnrollDialer`, `preProvisionedState` undefined.

- [ ] **Step 3: Add `preProvisionedState`, `PreEnrollDialer`, and `preEnrollDevice` to `os_provision.go`**

Replace the import block in `go/internal/cli/commands/os_provision.go` with:

```go
import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/wendylabsinc/wendy/internal/shared/certs"
	"github.com/wendylabsinc/wendy/internal/shared/config"
	"github.com/wendylabsinc/wendy/internal/shared/wendyconf"
	cloudpb "github.com/wendylabsinc/wendy/proto/gen/cloudpb"
)
```

Then add these declarations before `writeConfigFiles`:

```go
// preProvisionedState is written to the config partition during imaging.
// JSON tags must match provisioningState in internal/agent/services.
type preProvisionedState struct {
	Enrolled  bool   `json:"enrolled"`
	CloudHost string `json:"cloudHost,omitempty"`
	OrgID     int32  `json:"orgId,omitempty"`
	AssetID   int32  `json:"assetId,omitempty"`
	KeyPEM    string `json:"keyPem,omitempty"`
	CertPEM   string `json:"certPem,omitempty"`
	ChainPEM  string `json:"chainPem,omitempty"`
}

// PreEnrollDialer creates a gRPC connection for pre-enrollment.
// Tests replace this with a dialer that connects to a local fake server.
type PreEnrollDialer func(ctx context.Context, addr string, opt grpc.DialOption) (*grpc.ClientConn, error)

func defaultPreEnrollDialer(_ context.Context, addr string, opt grpc.DialOption) (*grpc.ClientConn, error) {
	return grpc.NewClient(addr, opt)
}

// preEnrollDevice generates a device key pair, gets an enrollment token from
// Wendy Cloud, issues a certificate, and returns the provisioning state as JSON
// to be written to the config partition. deviceName is optional (used as the
// cloud asset name). Pass nil for dialer to use the default.
func preEnrollDevice(ctx context.Context, auth *config.AuthConfig, deviceName string, dialer PreEnrollDialer) ([]byte, error) {
	if dialer == nil {
		dialer = defaultPreEnrollDialer
	}

	if len(auth.Certificates) == 0 {
		return nil, fmt.Errorf("auth entry has no certificates; re-run 'wendy auth login'")
	}
	cert := auth.Certificates[0]

	tlsCfg, err := certs.LoadTLSConfig(cert.PemCertificate, cert.PemCertificateChain, cert.PemPrivateKey, "")
	if err != nil {
		return nil, fmt.Errorf("loading TLS config: %w", err)
	}
	var transportOpt grpc.DialOption
	if strings.HasSuffix(auth.CloudGRPC, ":443") {
		transportOpt = grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg))
	} else {
		transportOpt = grpc.WithTransportCredentials(insecure.NewCredentials())
	}

	cloudConn, err := dialer(ctx, auth.CloudGRPC, transportOpt)
	if err != nil {
		return nil, fmt.Errorf("connecting to cloud: %w", err)
	}
	defer cloudConn.Close()

	certClient := cloudpb.NewCertificateServiceClient(cloudConn)

	tokenResp, err := certClient.CreateAssetEnrollmentToken(ctx, &cloudpb.CreateAssetEnrollmentTokenRequest{
		OrganizationId: int32(cert.OrganizationID),
		Name:           deviceName,
	})
	if err != nil {
		return nil, fmt.Errorf("creating enrollment token: %w", err)
	}
	orgID := tokenResp.GetOrganizationId()
	assetID := tokenResp.GetAssetId()

	// Generate key pair in memory only — never written to the local machine's disk.
	keyPEM, err := certs.GenerateKeyPair()
	if err != nil {
		return nil, fmt.Errorf("generating key pair: %w", err)
	}

	csrPEM, err := certs.GenerateCSR(keyPEM, fmt.Sprintf("sh/wendy/%d/%d", orgID, assetID))
	if err != nil {
		return nil, fmt.Errorf("generating CSR: %w", err)
	}

	issueResp, err := certClient.IssueCertificate(ctx, &cloudpb.IssueCertificateRequest{
		PemCsr:          csrPEM,
		EnrollmentToken: tokenResp.GetEnrollmentToken(),
	})
	if err != nil {
		return nil, fmt.Errorf("issuing certificate: %w", err)
	}
	if issueResp.GetError() != nil {
		return nil, fmt.Errorf("certificate issuance failed: %s", issueResp.GetError().GetMessage())
	}
	certObj := issueResp.GetCertificate()
	if certObj == nil {
		return nil, fmt.Errorf("cloud returned empty certificate")
	}

	cloudHost := auth.CloudGRPC
	if h, _, splitErr := net.SplitHostPort(cloudHost); splitErr == nil {
		cloudHost = h
	}

	state := preProvisionedState{
		Enrolled:  true,
		CloudHost: cloudHost,
		OrgID:     orgID,
		AssetID:   assetID,
		KeyPEM:    keyPEM,
		CertPEM:   certObj.GetPemCertificate(),
		ChainPEM:  certObj.GetPemCertificateChain(),
	}
	return json.Marshal(state)
}
```

- [ ] **Step 4: Run tests to verify they pass**

```bash
cd go && go test ./internal/cli/commands/... -run TestPreEnrollDevice -v
```

Expected: all 6 `TestPreEnrollDevice_*` tests pass.

- [ ] **Step 5: Commit**

```bash
cd go && git add internal/cli/commands/os_provision.go internal/cli/commands/os_provision_test.go
git commit -m "feat(cli): add preEnrollDevice to issue device cert at imaging time"
```

---

### Task 3: Thread `provisioningJSON` through the config-partition write chain

**Files:**
- Modify: `go/internal/cli/commands/os_provision.go` — `writeConfigFiles`
- Modify: `go/internal/cli/commands/os_provision_darwin.go` — `writeConfigPartition`
- Modify: `go/internal/cli/commands/os_provision_linux.go` — `writeConfigPartition`
- Modify: `go/internal/cli/commands/os_provision_windows.go` — `writeConfigPartition`
- Modify: `go/internal/cli/commands/os_install.go` — `provisionConfigPartition`

---

- [ ] **Step 1: Add `provisioningJSON []byte` parameter to `writeConfigFiles`**

Replace `writeConfigFiles` in `go/internal/cli/commands/os_provision.go`:

```go
// writeConfigFiles writes the agent binary, optional wendy.conf, and optional
// provisioning.json to mountPoint.
func writeConfigFiles(mountPoint string, agentBinary []byte, creds []wendyconf.WifiCredential, deviceName string, provisioningJSON []byte) error {
	binPath := filepath.Join(mountPoint, "wendy-agent")
	if err := os.WriteFile(binPath, agentBinary, 0o755); err != nil {
		return fmt.Errorf("writing wendy-agent to config partition: %w", err)
	}

	if len(creds) > 0 || deviceName != "" {
		for _, c := range creds {
			if strings.ContainsAny(c.SSID, "\n\r") || strings.ContainsAny(c.Password, "\n\r") {
				return fmt.Errorf("WiFi SSID and password must not contain newline characters")
			}
		}
		if strings.ContainsAny(deviceName, "\n\r") {
			return fmt.Errorf("device name must not contain newline characters")
		}

		var conf []byte
		if len(creds) > 0 {
			conf = wendyconf.Marshal(creds)
		}
		if deviceName != "" {
			if len(conf) > 0 {
				conf = append(conf, '\n')
			}
			conf = append(conf, []byte(fmt.Sprintf("[device]\nname = %s\n", deviceName))...)
		}

		confPath := filepath.Join(mountPoint, "wendy.conf")
		if err := os.WriteFile(confPath, conf, 0o644); err != nil {
			return fmt.Errorf("writing wendy.conf to config partition: %w", err)
		}
	}

	if len(provisioningJSON) > 0 {
		provPath := filepath.Join(mountPoint, "provisioning.json")
		if err := os.WriteFile(provPath, provisioningJSON, 0o600); err != nil {
			return fmt.Errorf("writing provisioning.json to config partition: %w", err)
		}
	}

	return nil
}
```

- [ ] **Step 2: Update `writeConfigPartition` in `os_provision_darwin.go`**

Replace the function:

```go
func writeConfigPartition(d drive, agentBinary []byte, creds []wendyconf.WifiCredential, deviceName string, provisioningJSON []byte) error {
	partDev, err := findConfigPartition(d.DevicePath)
	if err != nil {
		return fmt.Errorf("locating config partition on %s: %w", d.DevicePath, err)
	}

	mountPoint, err := mountConfigPartition(partDev)
	if err != nil {
		return fmt.Errorf("mounting config partition %s: %w", partDev, err)
	}
	defer exec.Command("diskutil", "unmount", partDev).Run() //nolint:errcheck

	return writeConfigFiles(mountPoint, agentBinary, creds, deviceName, provisioningJSON)
}
```

- [ ] **Step 3: Update `writeConfigPartition` in `os_provision_linux.go`**

Replace the function:

```go
func writeConfigPartition(d drive, agentBinary []byte, creds []wendyconf.WifiCredential, deviceName string, provisioningJSON []byte) error {
	exec.Command("sudo", "partprobe", d.DevicePath).Run() //nolint:errcheck
	time.Sleep(500 * time.Millisecond)

	partDev, err := findConfigPartition(d.DevicePath)
	if err != nil {
		return fmt.Errorf("locating config partition on %s: %w", d.DevicePath, err)
	}

	tmpDir, err := os.MkdirTemp("", "wendyos-config-*")
	if err != nil {
		return fmt.Errorf("creating temp mount dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	mountCmd := exec.Command("sudo", "mount", "-t", "vfat", partDev, tmpDir)
	if out, err := mountCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("mounting config partition %s: %s: %w", partDev, strings.TrimSpace(string(out)), err)
	}
	defer exec.Command("sudo", "umount", tmpDir).Run() //nolint:errcheck

	return writeConfigFiles(tmpDir, agentBinary, creds, deviceName, provisioningJSON)
}
```

- [ ] **Step 4: Update `writeConfigPartition` stub in `os_provision_windows.go`**

Replace the function:

```go
func writeConfigPartition(d drive, agentBinary []byte, creds []wendyconf.WifiCredential, deviceName string, _ []byte) error {
	return fmt.Errorf("config partition provisioning is not supported on Windows")
}
```

- [ ] **Step 5: Update `provisionConfigPartition` in `os_install.go`**

Replace the function:

```go
// provisionConfigPartition downloads the latest stable arm64 wendy-agent binary
// and writes it (along with WiFi credentials, an optional device name, and
// optional pre-provisioned certificate state) to the config partition on d.
func provisionConfigPartition(d drive, creds []wendyconf.WifiCredential, deviceName string, provisioningJSON []byte) error {
	release, err := fetchAgentRelease(false)
	if err != nil {
		return fmt.Errorf("fetching latest agent release: %w", err)
	}

	const assetPrefix = "wendy-agent-linux-arm64-"
	var matched *githubReleaseAsset
	for i := range release.Assets {
		a := &release.Assets[i]
		if strings.HasPrefix(a.Name, assetPrefix) && strings.HasSuffix(a.Name, ".tar.gz") {
			matched = a
			break
		}
	}
	if matched == nil {
		return fmt.Errorf("no arm64 asset found in release %s", release.TagName)
	}

	fmt.Printf("Downloading wendy-agent %s for device...\n", release.TagName)
	agentBinary, err := downloadAgentBinary(*matched)
	if err != nil {
		return fmt.Errorf("downloading agent binary: %w", err)
	}

	return writeConfigPartition(d, agentBinary, creds, deviceName, provisioningJSON)
}
```

- [ ] **Step 6: Build to confirm no compile errors**

```bash
cd go && go build ./internal/cli/commands/... && go build ./cmd/wendy/...
```

Expected: exits 0.

- [ ] **Step 7: Run full test suite for the commands package**

```bash
cd go && go test ./internal/cli/commands/... -v 2>&1 | tail -20
```

Expected: all pass (the `TestPreEnrollDevice_*` tests still pass).

- [ ] **Step 8: Commit**

```bash
cd go && git add internal/cli/commands/os_provision.go internal/cli/commands/os_provision_darwin.go internal/cli/commands/os_provision_linux.go internal/cli/commands/os_provision_windows.go internal/cli/commands/os_install.go
git commit -m "refactor(cli): thread provisioningJSON through config partition write chain"
```

---

### Task 4: Wire `--pre-enroll` into `wendy os install`

**Files:**
- Modify: `go/internal/cli/commands/os_install.go`

---

- [ ] **Step 1: Add `preEnrollMode` type and constants**

Add near the top of `go/internal/cli/commands/os_install.go` (after the imports):

```go
type preEnrollMode int

const (
	preEnrollAuto   preEnrollMode = iota // prompt if interactive + auth session exists
	preEnrollForced                      // --pre-enroll explicitly set to true
	preEnrollSkip                        // --pre-enroll explicitly set to false
)
```

- [ ] **Step 2: Add `--pre-enroll` flag to `newOSInstallCmd`**

In `newOSInstallCmd`, add the flag declaration alongside the other flags:

```go
var preEnroll bool
```

In the `RunE` function, resolve the mode before calling `runOSInstall`:

```go
mode := preEnrollAuto
if cmd.Flags().Changed("pre-enroll") {
    if preEnroll {
        mode = preEnrollForced
    } else {
        mode = preEnrollSkip
    }
}
```

Update the call to `runOSInstall` to pass `mode`:

```go
return runOSInstall(cmd.Context(), nightly, deviceType, versionFlag, driveFlag, force, opts, deviceName, mode)
```

Add the flag registration (alongside the others, after the `cmd` definition):

```go
cmd.Flags().BoolVar(&preEnroll, "pre-enroll", false, "Pre-enroll this device with Wendy Cloud during imaging (requires 'wendy auth login')")
```

- [ ] **Step 3: Thread `mode` through `runOSInstall` and `installLinuxImage`**

Update `runOSInstall` signature:

```go
func runOSInstall(ctx context.Context, nightly bool, flagDeviceType, flagVersion, flagDrive string, force bool, wifi wifiCLIOptions, deviceName string, mode preEnrollMode) error {
```

Update the `installLinuxImage` call at the bottom of `runOSInstall`:

```go
return installLinuxImage(ctx, selected, device, nightly, flagVersion, flagDrive, force, wifi, deviceName, mode)
```

Update `installLinuxImage` signature:

```go
func installLinuxImage(ctx context.Context, deviceKey string, device pickerDevice, nightly bool, flagVersion, flagDrive string, force bool, wifi wifiCLIOptions, deviceName string, mode preEnrollMode) error {
```

- [ ] **Step 4: Add pre-enrollment resolution in `installLinuxImage`**

Add this block in `installLinuxImage`, after `provDeviceName` is resolved (after the `resolveDeviceName` call) and **before** the image resolution step:

```go
// Resolve pre-enrollment — must happen before provisionConfigPartition since the
// config partition is mounted and unmounted inside that call.
var provisioningJSON []byte
switch mode {
case preEnrollForced:
    // Validate auth before flashing so we fail fast.
    auth, err := pickAuthEntry("")
    if err != nil {
        return fmt.Errorf("--pre-enroll: %w", err)
    }
    fmt.Printf("Pre-enrolling device with Wendy Cloud (org: %d)...\n", auth.Certificates[0].OrganizationID)
    js, err := preEnrollDevice(ctx, auth, provDeviceName, nil)
    if err != nil {
        fmt.Printf("Warning: pre-enrollment failed: %v\n", err)
        fmt.Println("The device will boot unenrolled. Run 'wendy device enroll' after first boot.")
    } else {
        provisioningJSON = js
        fmt.Println("Device pre-enrolled. It will be secure from first boot.")
    }
case preEnrollAuto:
    if isInteractiveTerminal() {
        cfg, loadErr := config.Load()
        if loadErr == nil && len(cfg.Auth) > 0 {
            ok, _ := tui.ConfirmDefaultYes("Pre-enroll this device with Wendy Cloud?")
            if ok {
                auth, err := pickAuthEntry("")
                if err != nil {
                    fmt.Printf("Warning: could not resolve auth for pre-enrollment: %v\n", err)
                } else {
                    fmt.Printf("Pre-enrolling device with Wendy Cloud (org: %d)...\n", auth.Certificates[0].OrganizationID)
                    js, err := preEnrollDevice(ctx, auth, provDeviceName, nil)
                    if err != nil {
                        fmt.Printf("Warning: pre-enrollment failed: %v\n", err)
                        fmt.Println("The device will boot unenrolled. Run 'wendy device enroll' after first boot.")
                    } else {
                        provisioningJSON = js
                        fmt.Printf("Device pre-enrolled. It will be secure from first boot.\n")
                    }
                }
            }
        }
    }
}
```

- [ ] **Step 5: Update `provisionConfigPartition` call to pass `provisioningJSON`**

Find the existing call in `installLinuxImage`:

```go
if err := provisionConfigPartition(targetDrive, provCreds, provDeviceName); err != nil {
```

Replace with:

```go
if err := provisionConfigPartition(targetDrive, provCreds, provDeviceName, provisioningJSON); err != nil {
```

- [ ] **Step 6: Build to confirm no compile errors**

```bash
cd go && go build ./cmd/wendy/...
```

Expected: exits 0.

- [ ] **Step 7: Run all tests**

```bash
cd go && go test ./... 2>&1 | grep -E "FAIL|ok|---"
```

Expected: no FAIL lines.

- [ ] **Step 8: Smoke test (manual — requires a macOS host with auth session)**

```bash
# Verify the flag appears in help
./wendy os install --help | grep pre-enroll

# Dry-run with explicit flag and no auth to confirm fast-fail before flashing
./wendy os install --device-type raspberry-pi-5 --drive /dev/null --force --pre-enroll 2>&1 | grep "pre-enroll"
```

Expected output of second command: error message containing `--pre-enroll: not logged in` (or similar), with no image write attempted.

- [ ] **Step 9: Commit**

```bash
cd go && git add internal/cli/commands/os_install.go
git commit -m "feat(cli): add --pre-enroll flag to wendy os install"
```

---

## Self-Review Notes

- **Spec coverage:** All spec sections covered: `--pre-enroll` flag ✓, interactive prompt ✓, `preEnrollDevice` ✓, `applyPreProvisioning` ✓, `Apply()` signature ✓, `main.go` update ✓, all test cases ✓, device name passed as asset name ✓, non-fatal failure ✓, fast-fail before flashing ✓.
- **Type consistency:** `preProvisionedState` defined once in each package (agent + CLI) with matching JSON tags. `PreEnrollDialer` used in tests and production. `preEnrollMode` used across Tasks 3 and 4.
- **Key never on disk:** `preEnrollDevice` generates the key in memory and returns JSON bytes; the caller writes to the config partition only.
