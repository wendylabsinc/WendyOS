package services

import (
	"bytes"
	"context"
	"crypto/mldsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/registrycrypto"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
)

// The service-protos device-signer vectors (conformance/reqsig), vendored by
// registrycrypto. Their keys are SHA-256 of the generator's labels.
const deviceVectorsPath = "../../shared/registrycrypto/testdata/device-signed-request-vectors-v1.json"

type deviceVectors struct {
	Root     []byte `json:"root_cert_der_b64"`
	DeviceCA []byte `json:"device_ca_cert_der_b64"`
	Vectors  []struct {
		Name        string `json:"name"`
		Tenant      string `json:"expected_tenant"`
		SAN         string `json:"spiffe_san"`
		Leaf        []byte `json:"leaf_cert_der_b64"`
		Payload     []byte `json:"payload_b64"`
		PayloadType string `json:"payload_type"`
	} `json:"vectors"`
	RecipientKeys []struct {
		Name          string `json:"name"`
		ExpectedKeyID string `json:"expected_key_id"`
	} `json:"recipient_key_vectors"`
}

func vectorSeed(label string) []byte { s := sha256.Sum256([]byte(label)); return s[:] }

func pemBlock(typ string, der []byte) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}))
}

type fakeKeyDirectory struct {
	cloudpbv2.KeyDirectoryServiceClient
	mu      sync.Mutex
	calls   []*cloudpbv2.SignedRequest
	answers []error  // per call; past the end: success
	keyIDs  []string // per call when set: the key_id answered
}

func (f *fakeKeyDirectory) RegisterDeviceEncryptionKey(_ context.Context, in *cloudpbv2.SignedRequest, _ ...grpc.CallOption) (*cloudpbv2.RegisterEncryptionKeyResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := len(f.calls)
	f.calls = append(f.calls, in)
	if n < len(f.answers) && f.answers[n] != nil {
		return nil, f.answers[n]
	}
	if n < len(f.keyIDs) {
		return &cloudpbv2.RegisterEncryptionKeyResponse{KeyId: f.keyIDs[n]}, nil
	}
	var req cloudpbv2.RegisterEncryptionKeyRequest
	_ = proto.Unmarshal(in.GetPayload(), &req)
	return &cloudpbv2.RegisterEncryptionKeyResponse{KeyId: registrycrypto.KeyID(req.GetPublicKey())}, nil
}

func (f *fakeKeyDirectory) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func newTestEncryptionKey(t *testing.T, dir *fakeKeyDirectory) (*DeviceEncryptionKey, *observer.ObservedLogs) {
	t.Helper()
	core, logs := observer.New(zap.DebugLevel)
	k := NewDeviceEncryptionKey(zap.New(core), t.TempDir())
	k.retry = time.Millisecond
	k.dial = func(string, string, string, string) (cloudpbv2.KeyDirectoryServiceClient, io.Closer, error) {
		return dir, io.NopCloser(nil), nil
	}
	return k, logs
}

// leafFor self-issues a device leaf for uri over the key in keyPEM.
func leafFor(t *testing.T, keyPEM, uri string) string {
	t.Helper()
	signer, err := certs.ParseSigningPrivateKeyPEM([]byte(keyPEM))
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(uri)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(7), NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		URIs: []*url.URL{u},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, signer.Public(), signer)
	if err != nil {
		t.Fatal(err)
	}
	return pemBlock("CERTIFICATE", der)
}

// mldsaDevice issues a self-signed ML-DSA-65 device leaf for uri.
func mldsaDevice(t *testing.T, uri string) (certPEM, keyPEM string) {
	t.Helper()
	keyPEM, err := certs.GenerateMLDSAKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	return leafFor(t, keyPEM, uri), keyPEM
}

func TestDeviceEncryptionKeyPersistsPerPrincipal(t *testing.T) {
	dir := t.TempDir()
	a := "spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/device/7c1d3e5f-2a4b-4c6d-8e9f-0a1b2c3d4e5f"
	first, err := loadOrCreateDeviceEncryptionKey(zap.NewNop(), dir, a)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, deviceEncryptionKeyFile))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode = %v, %v; want 0600 like device-key.pem", info.Mode(), err)
	}
	// Cloud accepts exactly the 1216-byte ML-KEM-768+X25519 public key.
	if n := len(first.PublicKey().Bytes()); n != 1216 {
		t.Fatalf("public key is %d bytes, want 1216", n)
	}
	again, err := loadOrCreateDeviceEncryptionKey(zap.NewNop(), dir, a)
	if err != nil || !bytes.Equal(again.PublicKey().Bytes(), first.PublicKey().Bytes()) {
		t.Fatalf("reload returned another key (err %v); a renewal must keep the key", err)
	}
	// The stored key opens what was wrapped to its public half.
	w := registrycrypto.Wrap{
		OrganizationID: "11111111-1111-4111-8111-111111111111", Scope: registrycrypto.ScopeDeployment, ScopeID: "d",
		KeyID: registrycrypto.KeyID(make([]byte, 32)), RecipientKind: 1, Recipient: a,
		RecipientKeyID: registrycrypto.KeyID(first.PublicKey().Bytes()),
	}
	enc, ct, err := w.Seal(first.PublicKey().Bytes(), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Open(again, enc, ct); err != nil {
		t.Fatalf("reloaded key does not open a wrap to it: %v", err)
	}

	b := "spiffe://wendy.sh/tenant/22222222-2222-4222-8222-222222222222/device/3f8e2d1c-9b7a-4e6f-8d5c-4b3a2f1e0d9c"
	other, err := loadOrCreateDeviceEncryptionKey(zap.NewNop(), dir, b)
	if err != nil || bytes.Equal(other.PublicKey().Bytes(), first.PublicKey().Bytes()) {
		t.Fatalf("a new enrollment reused the previous enrollment's key (err %v)", err)
	}
}

func TestDeviceEncryptionKeyUnreadableFileIsNotReplaced(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, deviceEncryptionKeyFile)
	if err := os.Mkdir(path, 0o700); err != nil { // reads fail with EISDIR
		t.Fatal(err)
	}
	if _, err := loadOrCreateDeviceEncryptionKey(zap.NewNop(), dir, "spiffe://wendy.sh/tenant/t/device/d"); err == nil {
		t.Fatal("an unreadable key file was replaced")
	}
}

// The registration the agent sends is the conformance vector's request: the
// same payload bytes for the vector's KEM key, and a device-signed request
// that the operator-side verifier accepts against the vector's pinned root.
func TestDeviceEncryptionKeyRegistrationMatchesConformanceVector(t *testing.T) {
	raw, err := os.ReadFile(deviceVectorsPath)
	if err != nil {
		t.Fatal(err)
	}
	var v deviceVectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	var vec = v.Vectors[0]
	for _, x := range v.Vectors {
		if x.Name == "mldsa65-register-device-encryption-key-valid" {
			vec = x
		}
	}
	if vec.Name != "mldsa65-register-device-encryption-key-valid" {
		t.Fatal("register vector missing")
	}
	signing, err := mldsa.NewPrivateKey(mldsa.MLDSA65(), vectorSeed("wendy-device-request-vector-device-a-2026-10"))
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(signing)
	if err != nil {
		t.Fatal(err)
	}
	kem, err := registrycrypto.KEM.DeriveKeyPair(vectorSeed("wendy-registry-vector-device-kem-2026-10"))
	if err != nil {
		t.Fatal(err)
	}
	kemBytes, _ := kem.Bytes()

	fake := &fakeKeyDirectory{}
	k, _ := newTestEncryptionKey(t, fake)
	stored, _ := json.Marshal(storedEncryptionKey{Principal: vec.SAN, PrivateKey: kemBytes})
	if err := writeProvisioningFile(k.configPath, deviceEncryptionKeyFile, stored); err != nil {
		t.Fatal(err)
	}
	k.UpdateIdentity("cloud", pemBlock("CERTIFICATE", vec.Leaf), pemBlock("PRIVATE KEY", der),
		pemBlock("CERTIFICATE", v.DeviceCA)+pemBlock("CERTIFICATE", v.Root))
	if err := k.register(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fake.count() != 1 {
		t.Fatalf("%d registrations sent, want 1", fake.count())
	}
	sent := fake.calls[0]
	if !bytes.Equal(sent.GetPayload(), vec.Payload) || sent.GetPayloadType() != vec.PayloadType {
		t.Fatalf("payload %x (%s) differs from the vector's %x (%s)", sent.GetPayload(), sent.GetPayloadType(), vec.Payload, vec.PayloadType)
	}
	root, _ := x509.ParseCertificate(v.Root)
	pub, err := registrycrypto.VerifyRegistration(sent, [][]byte{v.DeviceCA}, root, vec.SAN, vec.Tenant)
	if err != nil {
		t.Fatalf("operator-side verification refused the agent's registration: %v", err)
	}
	for _, rk := range v.RecipientKeys {
		if rk.Name == "recipient-key-valid" && registrycrypto.KeyID(pub) != rk.ExpectedKeyID {
			t.Fatalf("key_id %s, vector expects %s", registrycrypto.KeyID(pub), rk.ExpectedKeyID)
		}
	}
}

// Cloud failing or acknowledging another key leaves the key unbound: the
// agent logs it, backs off and registers again until Cloud acknowledges this
// key, then stops. A new identity (enrollment, or the restart after a
// renewal) registers again.
func TestDeviceEncryptionKeyReregistersWhileUnbound(t *testing.T) {
	fake := &fakeKeyDirectory{
		answers: []error{status.Error(codes.Unavailable, "cloud down"), status.Error(codes.Internal, "boom"), nil},
		keyIDs:  []string{"", "", "0000"},
	}
	k, logs := newTestEncryptionKey(t, fake)
	uri := "spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/device/7c1d3e5f-2a4b-4c6d-8e9f-0a1b2c3d4e5f"
	certPEM, keyPEM := mldsaDevice(t, uri)
	k.UpdateIdentity("cloud", certPEM, keyPEM, "")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { k.Run(ctx); close(done) }()
	waitForEncryptionKey(t, func() bool { return fake.count() == 4 })
	time.Sleep(20 * time.Millisecond)
	if n := fake.count(); n != 4 {
		t.Fatalf("%d registrations after Cloud acknowledged the key, want 4", n)
	}
	if n := logs.FilterField(zap.String("event", "device_encryption_key_unbound")).Len(); n != 3 {
		t.Fatalf("%d unbound warnings, want 3", n)
	}
	if logs.FilterField(zap.String("event", "device_encryption_key_registered")).Len() != 1 {
		t.Fatal("registration not logged")
	}

	// The same identity again (a duplicate UpdateIdentity) sends nothing.
	k.UpdateIdentity("cloud", certPEM, keyPEM, "")
	time.Sleep(20 * time.Millisecond)
	if n := fake.count(); n != 4 {
		t.Fatalf("an unchanged identity registered again (%d calls)", n)
	}
	// A renewed leaf for the same device registers the same key again.
	renewedCert, renewedKey := mldsaDevice(t, uri)
	k.UpdateIdentity("cloud", renewedCert, renewedKey, "")
	waitForEncryptionKey(t, func() bool { return fake.count() == 5 })
	if !bytes.Equal(fake.calls[4].GetPayload(), fake.calls[3].GetPayload()) {
		t.Fatal("the renewed identity registered a different key")
	}
	cancel()
	<-done
}

// A device still on P-256 cannot sign the registration: it keeps its key,
// sends nothing and does not spin retrying.
func TestDeviceEncryptionKeyWaitsForMLDSADevice(t *testing.T) {
	fake := &fakeKeyDirectory{}
	k, logs := newTestEncryptionKey(t, fake)
	keyPEM, err := certs.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	certPEM := leafFor(t, keyPEM, "spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/device/7c1d3e5f-2a4b-4c6d-8e9f-0a1b2c3d4e5f")
	k.UpdateIdentity("cloud", certPEM, keyPEM, "")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { k.Run(ctx); close(done) }()
	time.Sleep(30 * time.Millisecond)
	cancel()
	<-done
	if fake.count() != 0 || logs.FilterField(zap.String("event", "device_encryption_key_unbound")).Len() != 0 {
		t.Fatalf("P-256 device sent %d registrations", fake.count())
	}
	if _, err := os.Stat(filepath.Join(k.configPath, deviceEncryptionKeyFile)); err != nil {
		t.Fatalf("no key generated at enrollment: %v", err)
	}
}

// A legacy (non-SPIFFE) certificate can never register: the agent logs it,
// writes no key and does not retry.
func TestDeviceEncryptionKeyIgnoresLegacyIdentity(t *testing.T) {
	fake := &fakeKeyDirectory{}
	k, logs := newTestEncryptionKey(t, fake)
	keyPEM, err := certs.GenerateMLDSAKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	k.UpdateIdentity("cloud", leafFor(t, keyPEM, "urn:wendy:legacy-asset:42"), keyPEM, "")
	if err := k.register(context.Background()); err != nil || fake.count() != 0 {
		t.Fatalf("legacy identity is retryable (%v) or registered (%d calls)", err, fake.count())
	}
	if n := logs.FilterField(zap.String("event", "device_encryption_key_unsupported_identity")).Len(); n != 1 {
		t.Fatalf("%d unsupported-identity warnings, want 1", n)
	}
	if _, err := os.Stat(filepath.Join(k.configPath, deviceEncryptionKeyFile)); !os.IsNotExist(err) {
		t.Fatalf("key generated for a legacy identity: %v", err)
	}
}

// Only a non-ML-DSA key waits silently for renewal; any other signer failure
// (here a certificate over another key) is an error, so it is retried.
func TestDeviceEncryptionKeySignerFailureIsRetryable(t *testing.T) {
	k, logs := newTestEncryptionKey(t, &fakeKeyDirectory{})
	uri := "spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/device/7c1d3e5f-2a4b-4c6d-8e9f-0a1b2c3d4e5f"
	certPEM, _ := mldsaDevice(t, uri)
	_, otherKey := mldsaDevice(t, uri)
	k.UpdateIdentity("cloud", certPEM, otherKey, "")
	if err := k.register(context.Background()); err == nil {
		t.Fatal("a certificate/key mismatch was treated as a P-256 device")
	}
	if logs.FilterField(zap.String("event", "device_encryption_key_unsignable")).Len() != 0 {
		t.Fatal("a certificate/key mismatch was logged as not ML-DSA")
	}
}

// A stored key that does not parse is replaced, with a warning: the wraps to it
// no longer open.
func TestDeviceEncryptionKeyWarnsWhenReplacingUnparsableKey(t *testing.T) {
	core, logs := observer.New(zap.DebugLevel)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, deviceEncryptionKeyFile), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadOrCreateDeviceEncryptionKey(zap.New(core), dir, "spiffe://wendy.sh/tenant/t/device/d"); err != nil {
		t.Fatal(err)
	}
	if logs.FilterField(zap.String("event", "device_encryption_key_replaced")).Len() != 1 {
		t.Fatal("replacing an unparsable key was not logged")
	}
}

func waitForEncryptionKey(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(time.Millisecond)
	}
}
