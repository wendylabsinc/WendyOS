package services

import (
	"context"
	"crypto"
	"crypto/mldsa"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/interceptor"
	"github.com/wendylabsinc/wendy/go/internal/agent/mtls"
	"github.com/wendylabsinc/wendy/go/internal/agent/trustedkeys"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

type accessDevice struct {
	dir      string
	keys     *trustedkeys.Store
	enrolled bool
	restarts int
}

func (d *accessDevice) svc() *AccessService {
	return NewAccessService(zap.NewNop(), d.keys, func() bool { return d.enrolled }, func() { d.restarts++ })
}

func serve(t *testing.T, opts ...grpc.ServerOption) (*grpc.Server, *bufconn.Listener) {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer(opts...)
	t.Cleanup(srv.Stop)
	return srv, lis
}

func dial(t *testing.T, lis *bufconn.Listener, creds credentials.TransportCredentials) agentpbv2.WendyAccessServiceClient {
	t.Helper()
	conn, err := grpc.NewClient("passthrough:///device",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(creds))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return agentpbv2.NewWendyAccessServiceClient(conn)
}

func newKey(t *testing.T) (crypto.Signer, []byte) {
	t.Helper()
	k, err := mldsa.GenerateKey(mldsa.MLDSA65())
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(k.Public())
	if err != nil {
		t.Fatal(err)
	}
	return k, der
}

// keyCreds proves key with a self-signed client certificate, as the CLI does.
func keyCreds(t *testing.T, key crypto.Signer) credentials.TransportCredentials {
	t.Helper()
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "cli"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	return credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
		// The CLI pins the device key; pinning is client-side and not under test.
		InsecureSkipVerify: true,
	})
}

func wantCode(t *testing.T, what string, err error, code codes.Code) {
	t.Helper()
	if status.Code(err) != code {
		t.Fatalf("%s: got %v, want %v", what, err, code)
	}
}

// Open -> Closed (anyone, key becomes owner) -> owner manages keys and the
// keyless switch -> Enrolled refuses changes -> owner reopens.
func TestAccessShapeTransitions(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	keys, err := trustedkeys.Open(filepath.Join(dir, trustedkeys.FileName))
	if err != nil {
		t.Fatal(err)
	}
	d := &accessDevice{dir: dir, keys: keys}

	// Open: today's plaintext listener, no role check.
	openSrv, openLis := serve(t)
	agentpbv2.RegisterWendyAccessServiceServer(openSrv, d.svc())
	go openSrv.Serve(openLis)
	anon := dial(t, openLis, insecure.NewCredentials())

	got, err := anon.GetAccess(ctx, &agentpbv2.GetAccessRequest{})
	if err != nil || got.GetShape() != agentpbv2.AccessShape_ACCESS_SHAPE_OPEN {
		t.Fatalf("open device: %v %v", got, err)
	}
	ownerKey, ownerSPKI := newKey(t)
	closed, err := anon.CloseDevice(ctx, &agentpbv2.CloseDeviceRequest{PublicKey: ownerSPKI, Label: "laptop"})
	if err != nil || closed.GetOwner().GetRole() != agentpbv2.TrustedKeyRole_TRUSTED_KEY_ROLE_OWNER || d.restarts != 1 {
		t.Fatalf("CloseDevice: %v %v restarts=%d", closed, err, d.restarts)
	}
	_, err = anon.CloseDevice(ctx, &agentpbv2.CloseDeviceRequest{PublicKey: ownerSPKI})
	wantCode(t, "closing twice", err, codes.FailedPrecondition)

	// Closed: the TLS listener with the role table, as main wires it.
	certPEM, keyPEM, err := mtls.ClosedServerIdentity(dir, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	tlsCfg, err := mtls.NewTrustedKeyTLSConfig(certPEM, keyPEM, keys, trustedkeys.RoleViewer)
	if err != nil {
		t.Fatal(err)
	}
	authz := &interceptor.ClosedAuthorizer{Logger: zap.NewNop(), Listener: interceptor.ClosedKeyListener, Keys: keys}
	closedSrv, closedLis := serve(t, grpc.Creds(credentials.NewTLS(tlsCfg)),
		grpc.ChainUnaryInterceptor(authz.Unary()), grpc.ChainStreamInterceptor(authz.Stream()))
	agentpbv2.RegisterWendyAccessServiceServer(closedSrv, d.svc())
	go closedSrv.Serve(closedLis)
	owner := dial(t, closedLis, keyCreds(t, ownerKey))

	got, err = owner.GetAccess(ctx, &agentpbv2.GetAccessRequest{})
	if err != nil || got.GetShape() != agentpbv2.AccessShape_ACCESS_SHAPE_CLOSED ||
		got.GetCallerRole() != agentpbv2.TrustedKeyRole_TRUSTED_KEY_ROLE_OWNER {
		t.Fatalf("closed device, owner: %v %v", got, err)
	}
	opKey, opSPKI := newKey(t)
	if _, err := owner.AddTrustedKey(ctx, &agentpbv2.AddTrustedKeyRequest{PublicKey: opSPKI, Role: agentpbv2.TrustedKeyRole_TRUSTED_KEY_ROLE_OPERATOR}); err != nil {
		t.Fatal(err)
	}
	operator := dial(t, closedLis, keyCreds(t, opKey))
	_, otherSPKI := newKey(t)
	_, err = operator.AddTrustedKey(ctx, &agentpbv2.AddTrustedKeyRequest{PublicKey: otherSPKI, Role: agentpbv2.TrustedKeyRole_TRUSTED_KEY_ROLE_VIEWER})
	wantCode(t, "operator adds a key", err, codes.PermissionDenied)
	_, err = operator.OpenDevice(ctx, &agentpbv2.OpenDeviceRequest{})
	wantCode(t, "operator opens the device", err, codes.PermissionDenied)
	_, err = operator.SetKeylessApps(ctx, &agentpbv2.SetKeylessAppsRequest{Enabled: true})
	wantCode(t, "operator opens the keyless set", err, codes.PermissionDenied)
	strangerKey, _ := newKey(t)
	_, err = dial(t, closedLis, keyCreds(t, strangerKey)).GetAccess(ctx, &agentpbv2.GetAccessRequest{})
	if err == nil {
		t.Fatal("untrusted key got through the TLS handshake")
	}

	if _, err := owner.SetKeylessApps(ctx, &agentpbv2.SetKeylessAppsRequest{Enabled: true}); err != nil || !keys.KeylessApps() {
		t.Fatalf("SetKeylessApps: %v", err)
	}
	list, err := owner.ListTrustedKeys(ctx, &agentpbv2.ListTrustedKeysRequest{})
	if err != nil || len(list.GetKeys()) != 2 {
		t.Fatalf("ListTrustedKeys: %v %v", list, err)
	}
	_, err = owner.RemoveTrustedKey(ctx, &agentpbv2.RemoveTrustedKeyRequest{Fingerprint: closed.GetOwner().GetFingerprint()})
	wantCode(t, "removing the last owner", err, codes.FailedPrecondition)

	// Enrolled: the file stays, nothing changes it, discovery says enrolled.
	d.enrolled = true
	got, err = owner.GetAccess(ctx, &agentpbv2.GetAccessRequest{})
	if err != nil || got.GetShape() != agentpbv2.AccessShape_ACCESS_SHAPE_ENROLLED {
		t.Fatalf("enrolled: %v %v", got, err)
	}
	_, err = owner.AddTrustedKey(ctx, &agentpbv2.AddTrustedKeyRequest{PublicKey: otherSPKI, Role: agentpbv2.TrustedKeyRole_TRUSTED_KEY_ROLE_VIEWER})
	wantCode(t, "adding a key while enrolled", err, codes.FailedPrecondition)
	_, err = anon.CloseDevice(ctx, &agentpbv2.CloseDeviceRequest{PublicKey: otherSPKI})
	wantCode(t, "closing an enrolled device", err, codes.FailedPrecondition)
	d.enrolled = false

	// Closed -> Open: owner only; every key goes.
	restarts := d.restarts
	if _, err := owner.OpenDevice(ctx, &agentpbv2.OpenDeviceRequest{}); err != nil || d.restarts != restarts+1 {
		t.Fatalf("OpenDevice: %v restarts=%d", err, d.restarts)
	}
	if _, err := os.Stat(filepath.Join(dir, trustedkeys.FileName)); !os.IsNotExist(err) {
		t.Fatalf("open device kept its keys file: %v", err)
	}
}

// Item 2: the access service, now served on Open and Enrolled listeners too,
// changes nothing there. Open: it reports Open with no role and no key call
// writes a file. Enrolled (straight from Open): it reports Enrolled, every
// change is refused, and the device never becomes Closed or restarts.
func TestOpenAndEnrolledUnchanged(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, trustedkeys.FileName)
	keys, err := trustedkeys.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	d := &accessDevice{dir: dir, keys: keys}
	srv, lis := serve(t)
	agentpbv2.RegisterWendyAccessServiceServer(srv, d.svc())
	go srv.Serve(lis)
	c := dial(t, lis, insecure.NewCredentials())
	_, spki := newKey(t)

	changes := func(shape string) {
		t.Helper()
		for name, call := range map[string]func() error{
			"AddTrustedKey": func() error {
				_, err := c.AddTrustedKey(ctx, &agentpbv2.AddTrustedKeyRequest{PublicKey: spki, Role: agentpbv2.TrustedKeyRole_TRUSTED_KEY_ROLE_OPERATOR})
				return err
			},
			"ListTrustedKeys": func() error { _, err := c.ListTrustedKeys(ctx, &agentpbv2.ListTrustedKeysRequest{}); return err },
			"RemoveTrustedKey": func() error {
				_, err := c.RemoveTrustedKey(ctx, &agentpbv2.RemoveTrustedKeyRequest{Fingerprint: trustedkeys.Fingerprint(spki)})
				return err
			},
			"SetKeylessApps": func() error {
				_, err := c.SetKeylessApps(ctx, &agentpbv2.SetKeylessAppsRequest{Enabled: true})
				return err
			},
			"OpenDevice": func() error { _, err := c.OpenDevice(ctx, &agentpbv2.OpenDeviceRequest{}); return err },
		} {
			if err := call(); err == nil {
				t.Errorf("%s: %s succeeded", shape, name)
			}
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("%s: a key call wrote the keys file: %v", shape, err)
		}
		if d.restarts != 0 {
			t.Fatalf("%s: restarted %d times", shape, d.restarts)
		}
	}

	got, err := c.GetAccess(ctx, &agentpbv2.GetAccessRequest{})
	if err != nil || got.GetShape() != agentpbv2.AccessShape_ACCESS_SHAPE_OPEN ||
		got.GetCallerRole() != agentpbv2.TrustedKeyRole_TRUSTED_KEY_ROLE_UNSPECIFIED || got.GetKeylessApps() {
		t.Fatalf("open: %v %v", got, err)
	}
	changes("open")

	d.enrolled = true
	got, err = c.GetAccess(ctx, &agentpbv2.GetAccessRequest{})
	if err != nil || got.GetShape() != agentpbv2.AccessShape_ACCESS_SHAPE_ENROLLED || got.GetCallerRole() != agentpbv2.TrustedKeyRole_TRUSTED_KEY_ROLE_UNSPECIFIED {
		t.Fatalf("enrolled: %v %v", got, err)
	}
	_, err = c.CloseDevice(ctx, &agentpbv2.CloseDeviceRequest{PublicKey: spki})
	wantCode(t, "closing an enrolled device", err, codes.FailedPrecondition)
	changes("enrolled")
	if keys.Closed() {
		t.Fatal("enrolled device became Closed")
	}
}

// Closed -> Enrolled -> unenroll returns to Closed with the same keys: the
// unenroll cleanup removes enrollment files only.
func TestUnenrollKeepsClosedAccess(t *testing.T) {
	svc, dir := newTestProvisioningService(t)
	keys, err := trustedkeys.Open(filepath.Join(dir, trustedkeys.FileName))
	if err != nil {
		t.Fatal(err)
	}
	_, spki := newKey(t)
	if _, err := keys.Close(spki, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := mtls.ClosedServerIdentity(dir, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.StartProvisioning(context.Background(), &agentpb.StartProvisioningRequest{
		OrganizationId: 7, CloudHost: "unprov.wendy.io", AssetId: 70,
	}); err != nil {
		t.Fatalf("StartProvisioning: %v", err)
	}
	if _, err := svc.Unprovision(context.Background(), &agentpb.UnprovisionRequest{}); err != nil {
		t.Fatalf("Unprovision: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "device-key.pem")); !os.IsNotExist(err) {
		t.Fatalf("unenroll left the device key: %v", err)
	}
	after, err := trustedkeys.Open(filepath.Join(dir, trustedkeys.FileName))
	if err != nil || !after.Closed() {
		t.Fatalf("after unenroll: closed=%v err=%v, want Closed", after != nil && after.Closed(), err)
	}
	if role, _ := after.Lookup(spki); role != trustedkeys.RoleOwner {
		t.Fatalf("owner key after unenroll: %v", role)
	}
	if _, err := os.Stat(filepath.Join(dir, mtls.ClosedServerKeyFile)); err != nil {
		t.Fatalf("unenroll removed the closed-device TLS key: %v", err)
	}
}
