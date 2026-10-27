package commands

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
)

// A release without a tag is refused before anything is sent: there is no
// implicit "latest". A malformed digest is refused the same way.
func TestAppReleaseRefusesImplicitLatest(t *testing.T) {
	// Run beside a valid wendy.json, so only the flag checks can refuse.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "wendy.json"), []byte(`{"appId":"com.example.cam"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	digest := "sha256:" + strings.Repeat("a", 64)
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"no tag":       {[]string{"--digest", digest}, "no implicit 'latest'"},
		"blank tag":    {[]string{"--tag", " ", "--digest", digest}, "no implicit 'latest'"},
		"no digest":    {[]string{"--tag", "v1"}, "--digest must be"},
		"short digest": {[]string{"--tag", "v1", "--digest", "sha256:abc"}, "--digest must be"},
	} {
		t.Run(name, func(t *testing.T) {
			cmd := newAppReleaseCmd()
			cmd.SetArgs(tc.args)
			cmd.SetOut(new(strings.Builder))
			cmd.SetErr(new(strings.Builder))
			if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want the refusal %q", err, tc.want)
			}
		})
	}
	if _, err := newDeployRelease("app", ""); err == nil || !strings.Contains(err.Error(), "latest") {
		t.Fatalf("--record-release with an empty tag: err = %v", err)
	}
}

type releaseRecorder struct {
	cloudpb.UnimplementedDeploymentServiceServer
	created *cloudpb.CreateAppReleaseRequest
	updated *cloudpb.UpdateAppReleaseRequest
}

func (r *releaseRecorder) CreateAppRelease(_ context.Context, env *cloudpb.SignedRequest) (*cloudpb.AppRelease, error) {
	r.created = new(cloudpb.CreateAppReleaseRequest)
	if err := proto.Unmarshal(env.GetPayload(), r.created); err != nil {
		return nil, err
	}
	return &cloudpb.AppRelease{Id: "0b6f2c1e-8a3d-4c5e-9f00-1234567890ab"}, nil
}

func (r *releaseRecorder) UpdateAppRelease(_ context.Context, env *cloudpb.SignedRequest) (*cloudpb.AppRelease, error) {
	r.updated = new(cloudpb.UpdateAppReleaseRequest)
	return &cloudpb.AppRelease{}, proto.Unmarshal(env.GetPayload(), r.updated)
}

// A recorded release is created under its tag and then given the digest the
// deploy reported; the update repeats the tag because it replaces the name.
func TestRecordedReleaseCarriesTagAndDigest(t *testing.T) {
	key := testKey(t)
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	principal := "spiffe://wendy.sh/tenant/" + deploymentTenant + "/operator/test"
	auth := &config.AuthConfig{Certificates: []config.CertificateInfo{{PrincipalURI: principal,
		PemCertificate: liteLeafPEM(t, key, principal, time.Now().Add(time.Hour), false),
		PemPrivateKey:  string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
	}}}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	rec := &releaseRecorder{}
	srv := grpc.NewServer()
	cloudpb.RegisterDeploymentServiceServer(srv, rec)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()

	ctx := context.Background()
	id, err := createAppRelease(ctx, conn, auth, deploymentTenant, "com.example.cam", "v1.4.0")
	if err != nil {
		t.Fatal(err)
	}
	if err := setAppReleaseDigest(ctx, conn, auth, id, "v1.4.0", "sha256:feed"); err != nil {
		t.Fatal(err)
	}
	wantCreate := &cloudpb.CreateAppReleaseRequest{AppId: "com.example.cam", OrganizationId: deploymentTenant, Name: "v1.4.0"}
	wantUpdate := &cloudpb.UpdateAppReleaseRequest{Id: id, Name: "v1.4.0", ImageDigest: "sha256:feed"}
	if !proto.Equal(rec.created, wantCreate) || !proto.Equal(rec.updated, wantUpdate) {
		t.Fatalf("create = %v, update = %v", rec.created, rec.updated)
	}
}

// A recorded deploy says what it left behind: nothing on a path that cannot
// record, the release id when the digest never arrived, silence when complete.
func TestDeployReleaseOutcome(t *testing.T) {
	r, err := newDeployRelease("app", "v1")
	if err != nil {
		t.Fatal(err)
	}
	// No release was begun: a digest from the agent is dropped.
	noteImageDigest(withDeployRelease(context.Background(), r), "sha256:feed")
	if err := r.outcome(nil); err == nil || !strings.Contains(err.Error(), "nothing was recorded") {
		t.Fatalf("unsupported path: %v", err)
	}
	if err := r.outcome(context.Canceled); err != nil {
		t.Fatalf("failed deploy before any release: %v", err)
	}
	r.id = "0b6f2c1e-8a3d-4c5e-9f00-1234567890ab"
	if err := r.outcome(nil); err == nil || !strings.Contains(err.Error(), r.id) {
		t.Fatalf("release without digest: %v", err)
	}
	// A deploy that failed before the device answered is not an older agent.
	if err := r.outcome(errors.New("creating container: refused")); err == nil || !strings.Contains(err.Error(), r.id) || strings.Contains(err.Error(), "older agent") {
		t.Fatalf("release left by a failed deploy: %v", err)
	}
	r.stored = true
	if err := r.outcome(context.Canceled); err != nil {
		t.Fatalf("completed release after Ctrl-C: %v", err)
	}
}

// --record-release records one container deployed to one WendyOS device. Any
// other deploy is refused before a device is contacted or an image built.
func TestRecordReleaseRefusedBeforeBuild(t *testing.T) {
	compose := writeProject(t, map[string]string{"docker-compose.yml": "services:\n  web:\n    image: nginx\n"})
	if err := runCommand(context.Background(), runOptions{prefix: compose, yes: true, recordRelease: "v1"}); !IsUsageError(err) || !strings.Contains(err.Error(), "Compose") {
		t.Fatalf("compose: err = %v", err)
	}
	docker := writeProject(t, map[string]string{"Dockerfile": "FROM scratch\n", "wendy.json": `{"appId":"a","version":"1.0.0"}`})
	previous := deviceFlag
	t.Cleanup(func() { deviceFlag = previous })
	deviceFlag = "ccr1,ccr2"
	if err := runCommand(context.Background(), runOptions{prefix: docker, yes: true, detach: true, buildHost: "spark", recordRelease: "v1"}); !IsUsageError(err) || !strings.Contains(err.Error(), "several devices") {
		t.Fatalf("fleet: err = %v", err)
	}
	if err := runWithProvider(context.Background(), nil, models.ExternalDevice{}, docker, "a", nil, runOptions{recordRelease: "v1"}); !IsUsageError(err) || !strings.Contains(err.Error(), "provider") {
		t.Fatalf("provider: err = %v", err)
	}
	// Refused before the device is asked about enrollment: conn is nil.
	single := &appconfig.AppConfig{AppID: "a"}
	services := &appconfig.AppConfig{AppID: "a", Services: map[string]*appconfig.ServiceConfig{"api": {}}}
	for name, tc := range map[string]struct {
		cfg  *appconfig.AppConfig
		opts runOptions
		want string
	}{
		"multi-service": {services, runOptions{recordRelease: "v1"}, "multi-service"},
		"watch":         {single, runOptions{recordRelease: "v1", watchState: &watchDeployState{}}, "--watch"},
		"offline":       {single, runOptions{recordRelease: "v1", skipCloudRegistration: true}, "--skip-cloud-registration"},
	} {
		if err := runWithAgent(context.Background(), nil, docker, tc.cfg, tc.opts); !IsUsageError(err) || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
}

// A recorded deploy finds the device's organization and operator session
// before anything is built, so a device that cannot record is refused then.
func TestDeployReleasePrepareRefusesBeforeBuild(t *testing.T) {
	r, err := newDeployRelease("app", "v1")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	unenrolled := deploymentProvisioningV2Stub{response: &agentpbv2.IsProvisionedResponse{ResponseType: &agentpbv2.IsProvisionedResponse_NotProvisioned{
		NotProvisioned: &agentpbv2.NotProvisionedResponse{},
	}}}
	if err := r.prepare(ctx, unenrolled, config.Load); err == nil || !strings.Contains(err.Error(), "enrolled with Wendy Cloud") {
		t.Fatalf("unenrolled device: %v", err)
	}
	numeric := func() (*config.Config, error) { return registrationConfig(), nil }
	if err := r.prepare(ctx, registrationProvisioned(registrationDevice()), numeric); err == nil || !strings.Contains(err.Error(), "Cloud v2") {
		t.Fatalf("device enrolled with numeric ids: %v", err)
	}
	session := tenantOperatorSession(t, "cloud.example:443")
	tenant := func() (*config.Config, error) { return &config.Config{Auth: []config.AuthConfig{session}}, nil }
	if err := r.prepare(ctx, registrationProvisioned(registrationTenantDevice()), tenant); err != nil {
		t.Fatal(err)
	}
	if r.tenant != testOperatorTenant || r.auth == nil || r.id != "" {
		t.Fatalf("prepared tenant %q, auth %v, id %q: want the device's tenant, a session and no release yet", r.tenant, r.auth, r.id)
	}
}
