package services

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/acmeenroll"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// This is a loopback ACME protocol peer, not NetworkManager or a physical boot.
// Neither the production directory wait nor Enroll is replaced. A delayed public
// directory response models the connectivity boundary, followed by real account,
// order, CSR and certificate requests and durable provisioning state.
func TestFirstBootDelayedTransport(t *testing.T) {
	const tenant = "11111111-1111-4111-8111-111111111111"
	dir := t.TempDir()
	cfg := acmeenroll.Config{DeviceID: "firstboot-fixture", EABKeyID: "fixture-only", EABHMACKey: strings.Repeat("ab", 32)}
	principal := "spiffe://wendy.sh/tenant/" + tenant + "/device/" + cfg.DeviceID
	var online atomic.Bool
	var mu sync.Mutex
	accounts, orders, finalizes := 0, 0, 0
	var chain []byte
	probes := make(chan struct{}, 16)
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "private firstboot fixture CA"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(nil)
	base := "http://" + server.Listener.Addr().String()
	cfg.DirectoryURL = base + "/" + tenant + "/acme/directory"
	decode := func(r *http.Request, value any) error {
		var jws struct {
			Payload string `json:"payload"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20)).Decode(&jws); err != nil {
			return err
		}
		raw, err := base64.RawURLEncoding.DecodeString(jws.Payload)
		if err != nil {
			return err
		}
		return json.Unmarshal(raw, value)
	}
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Replay-Nonce", "Zmlyc3Rib290LWZpeHR1cmU")
		reply := func(code int, v any) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			_ = json.NewEncoder(w).Encode(v)
		}
		fail := func(message string) { t.Error(message); http.Error(w, message, http.StatusBadRequest) }
		if r.URL.Path == "/"+tenant+"/acme/directory" {
			if r.Method != http.MethodGet || r.ContentLength > 0 {
				fail("public probe sent a credential/body")
				return
			}
			if !online.Load() {
				select {
				case probes <- struct{}{}:
				default:
				}
				http.Error(w, "fixture network unavailable", http.StatusServiceUnavailable)
				return
			}
			reply(200, map[string]any{"newNonce": base + "/nonce", "newAccount": base + "/account", "newOrder": base + "/order", "meta": map[string]bool{"externalAccountRequired": true}})
			return
		}
		if !online.Load() {
			fail("credential request preceded connectivity")
			return
		}
		switch r.URL.Path {
		case "/nonce":
			if r.Method != http.MethodHead {
				fail("unexpected nonce method")
				return
			}
			w.WriteHeader(200)
		case "/account":
			if r.Method != http.MethodPost {
				fail("account was not a POST")
				return
			}
			if _, err := os.Stat(filepath.Join(dir, "acme-first-boot-attempt.json")); err != nil {
				fail("redemption preceded durable attempt marker")
				return
			}
			var body struct {
				EAB struct{ Protected, Payload, Signature string } `json:"externalAccountBinding"`
			}
			if err := decode(r, &body); err != nil {
				fail("invalid account JWS")
				return
			}
			raw, err := base64.RawURLEncoding.DecodeString(body.EAB.Protected)
			if err != nil {
				fail("invalid EAB header")
				return
			}
			var header struct{ Alg, Kid, URL string }
			if json.Unmarshal(raw, &header) != nil || header.Kid != cfg.EABKeyID || header.Alg != "HS256" || header.URL != base+"/account" {
				fail("wrong EAB identity")
				return
			}
			key, _ := hex.DecodeString(cfg.EABHMACKey)
			mac := hmac.New(sha256.New, key)
			mac.Write([]byte(body.EAB.Protected + "." + body.EAB.Payload))
			sig, _ := base64.RawURLEncoding.DecodeString(body.EAB.Signature)
			if !hmac.Equal(sig, mac.Sum(nil)) {
				fail("EAB credential did not verify")
				return
			}
			accounts++
			w.Header().Set("Location", base+"/account/1")
			reply(201, map[string]string{"status": "valid"})
		case "/order":
			var body struct {
				Identifiers []struct{ Type, Value string }
			}
			if err := decode(r, &body); r.Method != http.MethodPost || err != nil || len(body.Identifiers) != 1 || body.Identifiers[0].Type != "permanent-identifier" || body.Identifiers[0].Value != cfg.DeviceID {
				fail("order changed seeded identity")
				return
			}
			orders++
			w.Header().Set("Location", base+"/order/1")
			reply(201, map[string]any{"status": "ready", "identifiers": body.Identifiers, "finalize": base + "/finalize"})
		case "/finalize":
			var body struct{ CSR string }
			if err := decode(r, &body); err != nil {
				fail("invalid CSR request")
				return
			}
			der, err := base64.RawURLEncoding.DecodeString(body.CSR)
			if err != nil {
				fail("invalid CSR encoding")
				return
			}
			csr, err := x509.ParseCertificateRequest(der)
			if err != nil || csr.CheckSignature() != nil {
				fail("invalid signed CSR")
				return
			}
			uri, _ := url.Parse(principal)
			leaf := &x509.Certificate{SerialNumber: big.NewInt(2), NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), URIs: []*url.URL{uri}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}}
			leafDER, err := x509.CreateCertificate(rand.Reader, leaf, ca, csr.PublicKey, caKey)
			if err != nil {
				fail(fmt.Sprint(err))
				return
			}
			chain = append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})...)
			finalizes++
			w.Header().Set("Location", base+"/order/1")
			reply(200, map[string]string{"status": "valid", "certificate": base + "/certificate"})
		case "/certificate":
			if len(chain) == 0 {
				fail("certificate requested before issuance")
				return
			}
			w.Header().Set("Content-Type", "application/pem-certificate-chain")
			_, _ = w.Write(chain)
		default:
			fail("unexpected ACME route")
		}
	})
	server.Start()
	defer server.Close()
	handoff := filepath.Join(t.TempDir(), "acme-enrollment.json")
	data, err := json.Marshal(struct {
		acmeenroll.Config
		CloudHost string `json:"cloudHost"`
	}{cfg, "fixture.invalid:443"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(handoff, data, 0600); err != nil {
		t.Fatal(err)
	}
	assertOffline := func() {
		t.Helper()
		for _, name := range []string{"acme-first-boot-attempt.json", "device-key.pem", "acme-account-key.pem.d", "provisioning.json"} {
			if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
				t.Fatalf("offline boot created %s", name)
			}
		}
		got, err := os.ReadFile(handoff)
		if err != nil || !bytes.Equal(got, data) {
			t.Fatal("offline boot changed the credential handoff")
		}
		mu.Lock()
		defer mu.Unlock()
		if accounts != 0 {
			t.Fatal("offline boot redeemed a credential")
		}
	}
	waitProbe := func(done <-chan struct{}) {
		t.Helper()
		select {
		case <-probes:
		case <-done:
			t.Fatal("boot enrollment returned before network became available")
		case <-time.After(15 * time.Second):
			t.Fatal("no real offline directory request")
		}
	}
	// First boot is interrupted while offline. It must not poison the later boot.
	firstCtx, cancelFirst := context.WithCancel(context.Background())
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		NewProvisioningServiceV2(NewProvisioningService(zap.NewNop(), dir)).ApplyACMEEnrollmentFile(firstCtx, handoff)
	}()
	defer func() {
		cancelFirst()
		select {
		case <-firstDone:
		case <-time.After(3 * time.Second):
			t.Error("offline boot did not drain")
		}
	}()
	waitProbe(firstDone)
	assertOffline()
	cancelFirst()
	select {
	case <-firstDone:
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled directory wait did not exit")
	}
	assertOffline()
	// The first HTTP client may have retried before cancellation. Discard those
	// observations so only a request from the new boot can release waitProbe.
	mu.Lock()
	for len(probes) > 0 {
		<-probes
	}
	mu.Unlock()
	// A newly constructed service uses the same files and the unchanged 5s retry.
	svc := NewProvisioningService(zap.NewNop(), dir)
	v2 := NewProvisioningServiceV2(svc)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	rpc := grpc.NewServer()
	pb.RegisterWendyProvisioningServiceServer(rpc, v2)
	rpcDone := make(chan struct{})
	go func() { defer close(rpcDone); _ = rpc.Serve(listener) }()
	defer func() {
		rpc.Stop()
		select {
		case <-rpcDone:
		case <-time.After(3 * time.Second):
			t.Error("fixture gRPC server did not drain")
		}
	}()
	conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := pb.NewWendyProvisioningServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); v2.ApplyACMEEnrollmentFile(ctx, handoff) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("enrollment did not drain")
		}
	}()
	waitProbe(done)
	assertOffline()
	probeCtx, probeCancel := context.WithTimeout(ctx, time.Second)
	state, err := client.IsProvisioned(probeCtx, &pb.IsProvisionedRequest{})
	probeCancel()
	if err != nil || state.GetNotProvisioned() == nil {
		t.Fatalf("setup status unavailable during offline enrollment: %v %v", state, err)
	}
	online.Store(true)
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("boot did not recover after directory became reachable")
	}
	state, err = client.IsProvisioned(ctx, &pb.IsProvisionedRequest{})
	if err != nil || state.GetProvisioned().GetPrincipalUri() != principal {
		t.Fatalf("real enrollment did not persist intended identity: %v %v", state, err)
	}
	if _, err := os.Stat(handoff); !os.IsNotExist(err) {
		t.Fatal("successful enrollment retained the credential")
	}
	key, err := os.ReadFile(filepath.Join(dir, "device-key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := os.ReadFile(filepath.Join(dir, "provisioning.json"))
	if err != nil || bytes.Contains(persisted, []byte(cfg.EABHMACKey)) {
		t.Fatal("missing state or credential leakage")
	}
	reloaded := NewProvisioningServiceV2(NewProvisioningService(zap.NewNop(), dir))
	reloaded.ApplyACMEEnrollmentFile(ctx, handoff)
	after, err := reloaded.IsProvisioned(ctx, &pb.IsProvisionedRequest{})
	if err != nil || after.GetProvisioned().GetPrincipalUri() != principal {
		t.Fatal("restart lost the enrolled identity")
	}
	again, err := os.ReadFile(filepath.Join(dir, "device-key.pem"))
	if err != nil || !bytes.Equal(again, key) {
		t.Fatal("restart replaced the device key")
	}
	mu.Lock()
	defer mu.Unlock()
	if accounts != 1 || orders != 1 || finalizes != 1 {
		t.Fatalf("enrollment repeated or incomplete: accounts=%d orders=%d finalizes=%d", accounts, orders, finalizes)
	}
	t.Log("offline cancellation retained handoff; delayed real ACME enrollment completed once; gRPC status and restart identity verified")
}
