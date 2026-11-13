// macenrollment is a loopback-only protocol peer for the macOS enrollment test.
// Its disposable certificates do not establish production PKI acceptance.
package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/mldsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const tenant = "11111111-1111-4111-8111-111111111111"

type peer struct {
	cloudpb.UnimplementedDeviceEnrollmentServiceServer
	cloudpb.UnimplementedOperatorSessionServiceServer
	mu                                    sync.Mutex
	ca                                    *x509.Certificate
	caKey                                 *ecdsa.PrivateKey
	caDER, operatorDER                    []byte
	operatorKey                           *mldsa.PublicKey
	base, device, chain, eab              string
	nonces                                map[string]bool
	account                               *ecdsa.PublicKey
	enrolled, accounts, orders, finalizes int
	errors                                []string
}

func die(err error) {
	if err != nil {
		panic(err)
	}
}
func b64(s string) []byte {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil
	}
	return b
}
func (p *peer) signed(in *cloudpb.SignedRequest, method string) error {
	if in.GetPayloadType() != "wendycloud.v2."+method+"Request" {
		return errors.New("wrong typed request")
	}
	parts := strings.Split(string(in.GetSignature()), ".")
	if len(parts) != 3 {
		return errors.New("invalid operator JWS")
	}
	var h struct {
		Alg, Kid string
		X5C      []string
	}
	if json.Unmarshal(b64(parts[0]), &h) != nil || h.Alg != "ML-DSA-65" {
		return errors.New("invalid operator algorithm")
	}
	sum := sha256.Sum256(p.operatorDER)
	if method == "RegisterOperatorLeaf" {
		if len(h.X5C) != 1 || h.X5C[0] != base64.StdEncoding.EncodeToString(p.operatorDER) || h.Kid != "" {
			return errors.New("unexpected operator leaf")
		}
	} else if h.Kid != base64.RawURLEncoding.EncodeToString(sum[:]) || len(h.X5C) != 0 {
		return errors.New("unexpected registered operator")
	}
	if err := mldsa.Verify(p.operatorKey, []byte(parts[0]+"."+parts[1]), b64(parts[2]), nil); err != nil {
		return err
	}
	var descriptor struct {
		Operation  string
		BodySHA256 string `json:"body_sha256"`
		Target     struct{ Tenant string }
	}
	service := "DeviceEnrollmentService"
	if method == "RegisterOperatorLeaf" {
		service = "OperatorSessionService"
	}
	body := sha256.Sum256(in.GetPayload())
	if json.Unmarshal(b64(parts[1]), &descriptor) != nil || descriptor.Operation != "wendycloud.v2."+service+"/"+method || descriptor.Target.Tenant != tenant || descriptor.BodySHA256 != base64.RawURLEncoding.EncodeToString(body[:]) {
		return errors.New("operator signature did not bind actual request bytes/scope")
	}
	return nil
}
func (p *peer) RegisterOperatorLeaf(_ context.Context, in *cloudpb.SignedRequest) (*cloudpb.RegisterOperatorLeafResponse, error) {
	if err := p.signed(in, "RegisterOperatorLeaf"); err != nil {
		return nil, status.Error(codes.PermissionDenied, err.Error())
	}
	sum := sha256.Sum256(p.operatorDER)
	return &cloudpb.RegisterOperatorLeafResponse{Kid: base64.RawURLEncoding.EncodeToString(sum[:])}, nil
}
func (p *peer) EnrollDevice(_ context.Context, in *cloudpb.SignedRequest) (*cloudpb.EnrollDeviceResponse, error) {
	if err := p.signed(in, "EnrollDevice"); err != nil {
		return nil, status.Error(codes.PermissionDenied, err.Error())
	}
	req := new(cloudpb.EnrollDeviceRequest)
	if proto.Unmarshal(in.GetPayload(), req) != nil || req.GetName() != "mac-enrollment-fixture" || req.GetDeviceClass() != cloudpb.DeviceClass_DEVICE_CLASS_B {
		return nil, status.Error(codes.InvalidArgument, "unexpected enrollment")
	}
	if _, err := uuid.Parse(req.DeviceId); err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid UUID")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.enrolled != 0 {
		return nil, status.Error(codes.AlreadyExists, "fixture permits one enrollment")
	}
	p.device = req.DeviceId
	p.enrolled++
	return &cloudpb.EnrollDeviceResponse{AssetId: p.device, CredentialKind: "eab", EabKeyId: "private-eab", EabHmacKey: p.eab}, nil
}
func (p *peer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	fail := func(err error) { p.errors = append(p.errors, err.Error()); http.Error(w, err.Error(), 400) }
	reply := func(code int, body any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(body)
	}
	if r.URL.Path == "/status" {
		reply(200, map[string]any{"enrolled": p.enrolled, "accounts": p.accounts, "orders": p.orders, "finalizes": p.finalizes, "device": p.device, "errors": p.errors})
		return
	}
	nonce := uuid.NewString()
	p.nonces[nonce] = true
	w.Header().Set("Replay-Nonce", nonce)
	if r.URL.Path == "/"+tenant+"/acme/directory" && r.Method == "GET" {
		reply(200, map[string]any{"newNonce": p.base + "/nonce", "newAccount": p.base + "/account", "newOrder": p.base + "/order"})
		return
	}
	if r.URL.Path == "/nonce" && r.Method == "HEAD" {
		w.WriteHeader(200)
		return
	}
	if r.Method != "POST" {
		fail(errors.New("unexpected ACME method"))
		return
	}
	var jws struct{ Protected, Payload, Signature string }
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&jws); err != nil {
		fail(err)
		return
	}
	var h struct {
		Alg, Kid, Nonce, URL string
		JWK                  *struct{ Kty, Crv, X, Y string }
	}
	if json.Unmarshal(b64(jws.Protected), &h) != nil || h.Alg != "ES256" || h.URL != p.base+r.URL.Path || !p.nonces[h.Nonce] {
		fail(errors.New("invalid ACME header/nonce"))
		return
	}
	delete(p.nonces, h.Nonce)
	key := p.account
	if h.JWK != nil {
		if r.URL.Path != "/account" || h.Kid != "" || h.JWK.Kty != "EC" || h.JWK.Crv != "P-256" {
			fail(errors.New("unexpected ACME account key"))
			return
		}
		key = &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(b64(h.JWK.X)), Y: new(big.Int).SetBytes(b64(h.JWK.Y))}
	} else if h.Kid != p.base+"/account/1" {
		fail(errors.New("unexpected ACME account ID"))
		return
	}
	signature := b64(jws.Signature)
	digest := sha256.Sum256([]byte(jws.Protected + "." + jws.Payload))
	if key == nil || !key.Curve.IsOnCurve(key.X, key.Y) || len(signature) != 64 || !ecdsa.Verify(key, digest[:], new(big.Int).SetBytes(signature[:32]), new(big.Int).SetBytes(signature[32:])) {
		fail(errors.New("invalid ACME signature"))
		return
	}
	payload := b64(jws.Payload)
	switch r.URL.Path {
	case "/account":
		var request struct {
			EAB struct{ Protected, Payload, Signature string } `json:"externalAccountBinding"`
		}
		if json.Unmarshal(payload, &request) != nil {
			fail(errors.New("invalid account"))
			return
		}
		var eh struct{ Alg, Kid, URL string }
		if json.Unmarshal(b64(request.EAB.Protected), &eh) != nil || eh.Alg != "HS256" || eh.Kid != "private-eab" || eh.URL != p.base+"/account" || p.enrolled != 1 || p.accounts != 0 {
			fail(errors.New("unexpected EAB redemption"))
			return
		}
		secret, _ := hex.DecodeString(p.eab)
		mac := hmac.New(sha256.New, secret)
		mac.Write([]byte(request.EAB.Protected + "." + request.EAB.Payload))
		var bound struct{ Kty, Crv, X, Y string }
		if !hmac.Equal(mac.Sum(nil), b64(request.EAB.Signature)) || json.Unmarshal(b64(request.EAB.Payload), &bound) != nil || h.JWK == nil || bound != *h.JWK {
			fail(errors.New("EAB did not bind actual account key"))
			return
		}
		p.account = key
		p.accounts++
		w.Header().Set("Location", p.base+"/account/1")
		reply(201, map[string]string{"status": "valid"})
	case "/order":
		var request struct {
			Identifiers []struct{ Type, Value string }
		}
		if json.Unmarshal(payload, &request) != nil || len(request.Identifiers) != 1 || request.Identifiers[0].Type != "permanent-identifier" || request.Identifiers[0].Value != p.device {
			fail(errors.New("order changed Cloud-minted UUID"))
			return
		}
		p.orders++
		w.Header().Set("Location", p.base+"/order/1")
		reply(201, map[string]string{"status": "ready", "finalize": p.base + "/finalize"})
	case "/finalize":
		var request struct{ CSR string }
		if json.Unmarshal(payload, &request) != nil {
			fail(errors.New("invalid finalize"))
			return
		}
		csr, err := x509.ParseCertificateRequest(b64(request.CSR))
		if err != nil {
			fail(err)
			return
		}
		if err = csr.CheckSignature(); err != nil {
			fail(err)
			return
		}
		uri, _ := url.Parse("spiffe://wendy.sh/tenant/" + tenant + "/device/" + p.device)
		leaf := &x509.Certificate{SerialNumber: big.NewInt(3), Subject: pkix.Name{CommonName: p.device}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(24 * time.Hour), URIs: []*url.URL{uri}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}}
		der, err := x509.CreateCertificate(rand.Reader, leaf, p.ca, csr.PublicKey, p.caKey)
		if err != nil {
			fail(err)
			return
		}
		p.chain = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})) + string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: p.caDER}))
		p.finalizes++
		reply(200, map[string]string{"status": "valid", "finalize": p.base + "/finalize", "certificate": p.base + "/certificate"})
	case "/certificate":
		if len(payload) != 0 || p.chain == "" {
			fail(errors.New("invalid certificate POST-as-GET"))
			return
		}
		w.Header().Set("Content-Type", "application/pem-certificate-chain")
		_, _ = w.Write([]byte(p.chain))
	default:
		fail(errors.New("unexpected ACME route"))
	}
}

// Old-protocol peer exposes version and v1 state but deliberately no v2 service.
type legacy struct {
	agentpb.UnimplementedWendyAgentServiceServer
	agentpb.UnimplementedWendyProvisioningServiceServer
}

func (legacy) GetAgentVersion(context.Context, *agentpb.GetAgentVersionRequest) (*agentpb.GetAgentVersionResponse, error) {
	return &agentpb.GetAgentVersionResponse{Version: "enrollment-legacy-fixture"}, nil
}
func (legacy) IsProvisioned(context.Context, *agentpb.IsProvisionedRequest) (*agentpb.IsProvisionedResponse, error) {
	return &agentpb.IsProvisionedResponse{Response: &agentpb.IsProvisionedResponse_NotProvisioned{NotProvisioned: &agentpb.NotProvisionedResponse{}}}, nil
}
func main() {
	if len(os.Args) != 2 {
		panic("usage: macenrollment NEW_PRIVATE_DIRECTORY")
	}
	root, err := filepath.Abs(os.Args[1])
	die(err)
	die(os.Mkdir(root, 0700))
	if os.Getenv("WENDY_CONFIG_DIR") != filepath.Join(root, "cli") || os.Getenv("WENDY_SECRET_STORE") != "file" {
		panic("private file-backed CLI config required")
	}
	secret := make([]byte, 32)
	_, err = rand.Read(secret)
	die(err)
	p := &peer{nonces: map[string]bool{}, eab: hex.EncodeToString(secret)}
	p.caKey, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	die(err)
	p.ca = &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "disposable mac enrollment CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(30 * 24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	p.caDER, err = x509.CreateCertificate(rand.Reader, p.ca, p.ca, &p.caKey.PublicKey, p.caKey)
	die(err)
	p.ca, err = x509.ParseCertificate(p.caDER)
	die(err)
	keyPEM, err := certs.GenerateMLDSAKeyPair()
	die(err)
	key, err := certs.ParseSigningPrivateKeyPEM([]byte(keyPEM))
	die(err)
	p.operatorKey = key.Public().(*mldsa.PublicKey)
	principal := "spiffe://wendy.sh/tenant/" + tenant + "/operator/private-fixture"
	uri, err := url.Parse(principal)
	die(err)
	operator := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "private-fixture"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(20 * 24 * time.Hour), URIs: []*url.URL{uri}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	p.operatorDER, err = x509.CreateCertificate(rand.Reader, operator, p.ca, p.operatorKey, p.caKey)
	die(err)
	httpListener, err := net.Listen("tcp", "127.0.0.1:0")
	die(err)
	p.base = "http://" + httpListener.Addr().String()
	cloudListener, err := net.Listen("tcp", "127.0.0.1:0")
	die(err)
	legacyListener, err := net.Listen("tcp", "127.0.0.1:0")
	die(err)
	server := grpc.NewServer()
	cloudpb.RegisterDeviceEnrollmentServiceServer(server, p)
	cloudpb.RegisterOperatorSessionServiceServer(server, p)
	oldServer := grpc.NewServer()
	agentpb.RegisterWendyAgentServiceServer(oldServer, legacy{})
	agentpb.RegisterWendyProvisioningServiceServer(oldServer, legacy{})
	httpServer := &http.Server{Handler: p, ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second, IdleTimeout: 5 * time.Second}
	done := make(chan error, 3)
	go func() { done <- server.Serve(cloudListener) }()
	go func() { done <- oldServer.Serve(legacyListener) }()
	go func() { done <- httpServer.Serve(httpListener) }()
	auth := config.AuthConfig{Name: "fixture", CloudGRPC: cloudListener.Addr().String(), OAuthIssuer: p.base + "/unused-issuer", PKIEndpoint: p.base + "/unused-identity", OAuthExpiresAt: time.Now().Add(time.Hour).Format(time.RFC3339), APIKey: uuid.NewString(), Certificates: []config.CertificateInfo{{PemCertificate: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: p.operatorDER})), PemCertificateChain: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: p.caDER})), PemPrivateKey: keyPEM, PrincipalURI: principal}}}
	die(config.Save(&config.Config{Auth: []config.AuthConfig{auth}}))
	ready, _ := json.Marshal(map[string]string{"directory": p.base + "/" + tenant + "/acme/directory", "status": p.base + "/status", "cloud": cloudListener.Addr().String(), "legacy": legacyListener.Addr().String(), "ca": auth.Certificates[0].PemCertificateChain, "operator": auth.Certificates[0].PemCertificate, "key": keyPEM})
	die(os.WriteFile(filepath.Join(root, "ready.tmp"), ready, 0600))
	die(os.Rename(filepath.Join(root, "ready.tmp"), filepath.Join(root, "ready.json")))
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	select {
	case <-ctx.Done():
	case err = <-done:
		panic(fmt.Sprintf("fixture server stopped: %v", err))
	case <-time.After(10 * time.Minute):
		panic("fixture lifetime exceeded")
	}
	server.Stop()
	oldServer.Stop()
	die(httpServer.Close())
	for i := 0; i < 3; i++ {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			panic("fixture server did not drain")
		}
	}
}
