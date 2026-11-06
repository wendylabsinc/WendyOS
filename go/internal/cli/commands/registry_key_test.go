package commands

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/registrycrypto"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
)

// A generated registry key is stored as base64 of the HPKE private key, comes
// back as the same key from the saved session, and starts unbound.
func TestRegistryKeyStoreRoundTrip(t *testing.T) {
	t.Setenv("WENDY_CONFIG_DIR", t.TempDir())
	session := tenantOperatorSession(t, "cloud.example:443")
	session.RegistryKeyID = "stale"
	priv, err := sessionRegistryKey(&session)
	if err != nil {
		t.Fatal(err)
	}
	if session.RegistryPrivateKey == "" || session.RegistryKeyID != "" {
		t.Fatalf("new key: stored %t, bound marker %q", session.RegistryPrivateKey != "", session.RegistryKeyID)
	}
	if err := config.Save(&config.Config{Auth: []config.AuthConfig{tenantOperatorSession(t, "cloud.example:443")}}); err != nil {
		t.Fatal(err)
	}
	session.RegistryKeyID = registrycrypto.KeyID(priv.PublicKey().Bytes())
	if err := persistRegistryKey(&session); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := sessionRegistryKey(&cfg.Auth[0])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(loaded.PublicKey().Bytes(), priv.PublicKey().Bytes()) || cfg.Auth[0].RegistryKeyID != session.RegistryKeyID {
		t.Fatal("stored registry key or bound marker did not round-trip")
	}
}

type keyDirectory struct {
	cloudpb.UnimplementedKeyDirectoryServiceServer
	calls  int
	got    *cloudpb.SignedRequest
	ack    string // key_id to acknowledge; empty acknowledges the registered key
	refuse error  // refuses every registration
	served []*cloudpb.RecipientKey
}

func (k *keyDirectory) RegisterUserEncryptionKey(_ context.Context, in *cloudpb.SignedRequest) (*cloudpb.RegisterEncryptionKeyResponse, error) {
	k.calls++
	k.got = in
	if k.refuse != nil {
		return nil, k.refuse
	}
	var req cloudpb.RegisterEncryptionKeyRequest
	if err := proto.Unmarshal(in.GetPayload(), &req); err != nil {
		return nil, err
	}
	id := k.ack
	if id == "" {
		id = registrycrypto.KeyID(req.GetPublicKey())
	}
	k.served = append(k.served, &cloudpb.RecipientKey{Principal: "spiffe://wendy.sh/tenant/" + testOperatorTenant + "/operator/" + testOperatorSubject, Registration: in})
	return &cloudpb.RegisterEncryptionKeyResponse{KeyId: id}, nil
}

func (k *keyDirectory) GetRecipientKeys(context.Context, *cloudpb.GetRecipientKeysRequest) (*cloudpb.GetRecipientKeysResponse, error) {
	return &cloudpb.GetRecipientKeysResponse{Keys: k.served}, nil
}

// serveKeyDirectory serves dir on a local plaintext listener and returns its
// address.
func serveKeyDirectory(t *testing.T, dir *keyDirectory) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	cloudpb.RegisterKeyDirectoryServiceServer(srv, dir)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis.Addr().String()
}

// savedRegistryKeyID is the bound marker of the one stored session.
func savedRegistryKeyID(t *testing.T) string {
	t.Helper()
	cfg, err := config.Load()
	if err != nil || len(cfg.Auth) != 1 {
		t.Fatalf("stored sessions: %v", err)
	}
	return cfg.Auth[0].RegistryKeyID
}

// 'registry key show' asks Cloud instead of trusting the bound marker: a key
// Cloud still serves is left alone, a dropped one is registered again, and a
// refused one is left unbound so the next refresh retries it.
func TestRegistryKeyShowChecksWithCloud(t *testing.T) {
	t.Setenv("WENDY_CONFIG_DIR", t.TempDir())
	origFresh := ensureFreshCertificateFn
	ensureFreshCertificateFn = func(context.Context, *config.AuthConfig) error { return nil }
	t.Cleanup(func() { ensureFreshCertificateFn = origFresh })
	dir := &keyDirectory{}
	addr := serveKeyDirectory(t, dir)
	session := tenantOperatorSession(t, addr)
	if err := config.Save(&config.Config{Auth: []config.AuthConfig{session}}); err != nil {
		t.Fatal(err)
	}
	show := func() (string, error) {
		var err error
		out := captureStderr(t, func() { err = runRegistryKey(context.Background(), addr, false) })
		return out, err
	}

	if out, err := show(); err != nil || dir.calls != 1 || !strings.Contains(out, ": registered with Cloud") {
		t.Fatalf("first show: %v, %d calls, %q", err, dir.calls, out)
	}
	keyID := savedRegistryKeyID(t)
	if out, err := show(); err != nil || dir.calls != 1 || !strings.Contains(out, ": registered with Cloud") {
		t.Fatalf("show of a served key: %v, %d calls, %q", err, dir.calls, out)
	}
	dir.served = nil // Cloud dropped the registration
	if out, err := show(); err != nil || dir.calls != 2 || !strings.Contains(out, ": re-registered with Cloud") || savedRegistryKeyID(t) != keyID {
		t.Fatalf("show of a dropped key: %v, %d calls, %q", err, dir.calls, out)
	}
	dir.served, dir.refuse = nil, status.Error(codes.PermissionDenied, "no")
	if out, err := show(); err == nil || !strings.Contains(out, ": unregistered (cloud refused: ") || savedRegistryKeyID(t) != "" {
		t.Fatalf("show of a refused key: %v, %q", err, out)
	}
}

// The automatic pre-flight renewal is a certificate refresh: it registers an
// unbound key. Without a renewal nothing is sent.
func TestPreflightRenewalRetriesRegistryKey(t *testing.T) {
	t.Setenv("WENDY_CONFIG_DIR", t.TempDir())
	dir := &keyDirectory{}
	addr := serveKeyDirectory(t, dir)
	session := tenantOperatorSession(t, addr)
	session.OAuthIssuer = "https://auth.example/realms/acme"
	session.OAuthExpiresAt = time.Now().Add(time.Hour).Format(time.RFC3339)
	if err := config.Save(&config.Config{Auth: []config.AuthConfig{session}}); err != nil {
		t.Fatal(err)
	}
	renewed := false
	origFresh := ensureFreshCertificateFn
	ensureFreshCertificateFn = func(_ context.Context, auth *config.AuthConfig) error {
		if renewed {
			key, err := certs.ParseSigningPrivateKeyPEM([]byte(auth.Certificates[0].PemPrivateKey))
			if err != nil {
				t.Fatal(err)
			}
			auth.Certificates[0].PemCertificate = testLeafPEM(t, key)
		}
		return nil
	}
	t.Cleanup(func() { ensureFreshCertificateFn = origFresh })

	if _, err := pickAuthEntry(addr); err != nil || dir.calls != 0 {
		t.Fatalf("no renewal: %v, %d calls", err, dir.calls)
	}
	renewed = true
	if _, err := pickAuthEntry(addr); err != nil || dir.calls != 1 || savedRegistryKeyID(t) == "" {
		t.Fatalf("renewal did not register the key: %v, %d calls", err, dir.calls)
	}
}

// RegisterUserEncryptionKey is operator-signed with x5c over
// org/<tenant>/encryption-key, carries the shared Registration payload, and
// binds only the key Cloud acknowledged; a bound key is not sent again.
func TestRegistryKeyRegistrationShape(t *testing.T) {
	session := tenantOperatorSession(t, "unreachable.invalid:443")
	auth := &session
	priv, err := sessionRegistryKey(auth)
	if err != nil {
		t.Fatal(err)
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dir := &keyDirectory{ack: "another-key"}
	srv := grpc.NewServer()
	cloudpb.RegisterKeyDirectoryServiceServer(srv, dir)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()

	ctx := context.Background()
	if err := bindRegistryKey(ctx, conn, auth, priv); err == nil || auth.RegistryKeyID != "" {
		t.Fatalf("a key Cloud did not acknowledge was bound: %v", err)
	}
	dir.ack = ""
	if err := bindRegistryKey(ctx, conn, auth, priv); err != nil {
		t.Fatal(err)
	}
	keyID := registrycrypto.KeyID(priv.PublicKey().Bytes())
	if auth.RegistryKeyID != keyID {
		t.Fatalf("bound marker %q, want %q", auth.RegistryKeyID, keyID)
	}

	want, _ := proto.Marshal(registrycrypto.Registration(testOperatorTenant, priv.PublicKey()))
	if dir.got.GetPayloadType() != "wendycloud.v2.RegisterEncryptionKeyRequest" || !bytes.Equal(dir.got.GetPayload(), want) {
		t.Fatalf("payload %q %x", dir.got.GetPayloadType(), dir.got.GetPayload())
	}
	parts := strings.Split(string(dir.got.GetSignature()), ".")
	var header struct {
		X5C []string `json:"x5c"`
		Kid string   `json:"kid"`
	}
	var claims struct {
		Operation string `json:"operation"`
		Target    struct {
			Resource string `json:"resource"`
			Tenant   string `json:"tenant"`
		} `json:"target"`
	}
	for i, v := range []any{&header, &claims} {
		raw, _ := base64.RawURLEncoding.DecodeString(parts[i])
		if err := json.Unmarshal(raw, v); err != nil {
			t.Fatal(err)
		}
	}
	if len(header.X5C) != 1 || header.Kid != "" {
		t.Fatalf("header x5c %d, kid %q: want the leaf by x5c only", len(header.X5C), header.Kid)
	}
	if claims.Operation != "wendycloud.v2.KeyDirectoryService/RegisterUserEncryptionKey" ||
		claims.Target.Resource != "org/"+testOperatorTenant+"/encryption-key" || claims.Target.Tenant != testOperatorTenant {
		t.Fatalf("claims %+v", claims)
	}

	// Bound: nothing is dialed (the session's endpoint does not resolve).
	if err := registerRegistryKey(ctx, auth); err != nil || dir.calls != 2 {
		t.Fatalf("bound key registered again: %v, %d calls", err, dir.calls)
	}
}

// A re-login by the same operator on the same session keeps the registry key
// and its bound marker; a different operator on that session gets a new key.
func TestRegistryKeyKeptOnlyForSamePrincipal(t *testing.T) {
	prior := tenantOperatorSession(t, "cloud.example:443")
	priv, err := sessionRegistryKey(&prior)
	if err != nil {
		t.Fatal(err)
	}
	prior.RegistryKeyID = registrycrypto.KeyID(priv.PublicKey().Bytes())
	cfg := &config.Config{Auth: []config.AuthConfig{prior}}

	same := tenantOperatorSession(t, "cloud.example:443")
	keepRegistryKey(cfg, &same)
	if same.RegistryPrivateKey != prior.RegistryPrivateKey || same.RegistryKeyID != prior.RegistryKeyID {
		t.Fatal("same operator lost its registry key or bound marker")
	}

	other := tenantOperatorSession(t, "cloud.example:443")
	other.Certificates[0].PrincipalURI = "spiffe://wendy.sh/tenant/" + testOperatorTenant + "/operator/someone-else"
	keepRegistryKey(cfg, &other)
	if other.RegistryKeyID != "" {
		t.Fatalf("other operator inherited bound marker %q", other.RegistryKeyID)
	}
	fresh, err := sessionRegistryKey(&other)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(fresh.PublicKey().Bytes(), priv.PublicKey().Bytes()) {
		t.Fatal("other operator reused the prior operator's registry key")
	}
}
