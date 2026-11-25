package agentvmfixture

import (
	"bytes"
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
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/servicejournal"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
	"github.com/wendylabsinc/wendy/go/world/production/wendycert"
	"google.golang.org/protobuf/encoding/protojson"
)

func fixture(t *testing.T) (string, string, *authority) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir, state := filepath.Join(base, "fixture"), filepath.Join(base, "state")
	if _, err = Initialize(dir, state); err != nil {
		t.Fatal(err)
	}
	a, err := load(dir)
	if err != nil {
		t.Fatal(err)
	}
	return dir, state, a
}

type testACMEClient struct {
	t      *testing.T
	srv    *httptest.Server
	server *acmeServer
	key    *ecdsa.PrivateKey
	jwk    jwk
}

func client(t *testing.T, a *authority) *testACMEClient {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s := &acmeServer{authority: a, nonces: map[string]bool{}}
	h := httptest.NewUnstartedServer(s)
	s.base = "http://" + h.Listener.Addr().String()
	h.Start()
	t.Cleanup(h.Close)
	return &testACMEClient{t: t, srv: h, server: s, key: k, jwk: jwk{Kty: "EC", Crv: "P-256", X: base64.RawURLEncoding.EncodeToString(k.X.FillBytes(make([]byte, 32))), Y: base64.RawURLEncoding.EncodeToString(k.Y.FillBytes(make([]byte, 32)))}}
}
func b64(v any) string { b, _ := json.Marshal(v); return base64.RawURLEncoding.EncodeToString(b) }
func (c *testACMEClient) eab() envelope {
	h := header{Alg: "HS256", URL: c.srv.URL + "/acme/new-account", KID: c.server.authority.secret.EABKeyID}
	e := envelope{Protected: b64(h), Payload: b64(c.jwk)}
	mac := hmac.New(sha256.New, c.server.authority.secret.EABKey)
	mac.Write([]byte(e.Protected + "." + e.Payload))
	e.Signature = base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return e
}
func (c *testACMEClient) post(path string, payload any, newAccount bool) (int, []byte) {
	c.t.Helper()
	r, err := http.Head(c.srv.URL + "/acme/new-nonce")
	if err != nil {
		c.t.Fatal(err)
	}
	r.Body.Close()
	h := header{Alg: "ES256", Nonce: r.Header.Get("Replay-Nonce"), URL: c.srv.URL + path, KID: c.srv.URL + "/acme/account/1"}
	if newAccount {
		h.KID = ""
		h.JWK = &c.jwk
	}
	e := envelope{Protected: b64(h)}
	if payload != nil {
		e.Payload = b64(payload)
	}
	hash := sha256.Sum256([]byte(e.Protected + "." + e.Payload))
	x, y, err := ecdsa.Sign(rand.Reader, c.key, hash[:])
	if err != nil {
		c.t.Fatal(err)
	}
	e.Signature = base64.RawURLEncoding.EncodeToString(append(x.FillBytes(make([]byte, 32)), y.FillBytes(make([]byte, 32))...))
	b, _ := json.Marshal(e)
	r, err = http.Post(c.srv.URL+path, "application/jose+json", bytes.NewReader(b))
	if err != nil {
		c.t.Fatal(err)
	}
	defer r.Body.Close()
	out, err := io.ReadAll(r.Body)
	if err != nil {
		c.t.Fatal(err)
	}
	return r.StatusCode, out
}
func (c *testACMEClient) register() {
	c.t.Helper()
	status, _ := c.post("/acme/new-account", map[string]any{"externalAccountBinding": c.eab()}, true)
	if status != 201 {
		c.t.Fatalf("account status %d", status)
	}
}
func (c *testACMEClient) order() {
	c.t.Helper()
	status, _ := c.post("/acme/new-order", map[string]any{"identifiers": []map[string]string{{"type": "permanent-identifier", "value": c.server.authority.public.DeviceID}}}, false)
	if status != 201 {
		c.t.Fatalf("order status %d", status)
	}
}
func enroll(t *testing.T, a *authority) (*mldsa.PrivateKey, [][]byte) {
	t.Helper()
	c := client(t, a)
	c.register()
	c.order()
	key, err := mldsa.GenerateKey(mldsa.MLDSA65())
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.CertificateRequest{}
	if a.public.PairID != "" {
		template.Subject = pkix.Name{CommonName: a.public.DeviceID}
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, template, key)
	if err != nil {
		t.Fatal(err)
	}
	status, _ := c.post("/acme/finalize", map[string]string{"csr": base64.RawURLEncoding.EncodeToString(csr)}, false)
	if status != 200 {
		t.Fatalf("finalize status %d", status)
	}
	status, body := c.post("/"+a.public.Tenant+"/acme/cert/1", nil, false)
	if status != 200 {
		t.Fatalf("certificate status %d", status)
	}
	var chain [][]byte
	for len(body) > 0 {
		b, rest := pem.Decode(body)
		if b == nil {
			t.Fatal("bad cert PEM")
		}
		chain = append(chain, b.Bytes)
		body = rest
	}
	return key, chain
}

func TestInitIsExclusiveAndPrivate(t *testing.T) {
	dir, state, a := fixture(t)
	if _, err := Initialize(dir, state); err == nil {
		t.Fatal("reused fixture")
	}
	for _, name := range []string{"world.json", "simclock.json", "fixture-public.json", ".vm-fixture/authority.json"} {
		st, err := os.Stat(filepath.Join(dir, name))
		if err != nil || st.Mode().Perm() != 0600 {
			t.Fatalf("private file %s: %v", name, err)
		}
	}
	for _, name := range []string{"device-key.pem", "provisioning.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatal("fixture manufactured device provisioning")
		}
	}
	if a.public.Principal == "" || len(a.secret.IssuerKey) == 0 {
		t.Fatal("fixture incomplete")
	}
}
func TestNormalACMEIssuesVerifiedDeviceCSRAndRealChain(t *testing.T) {
	_, _, a := fixture(t)
	key, chain := enroll(t, a)
	if len(chain) != 3 {
		t.Fatalf("chain %d", len(chain))
	}
	v, err := wendycert.NewDevice(wendycert.Config{Tenant: a.public.Tenant, Roots: [][]byte{a.public.RootDER}})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	id, err := v.Verify(chain, now.Add(-time.Second), now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if id.Principal != a.public.Principal {
		t.Fatal("issuer identity changed")
	}
	leaf, err := x509.ParseCertificate(chain[0])
	if err != nil {
		t.Fatal(err)
	}
	if !key.PublicKey().Equal(leaf.PublicKey) || len(leaf.ExtKeyUsage) != 2 || leaf.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth || leaf.ExtKeyUsage[1] != x509.ExtKeyUsageServerAuth {
		t.Fatal("wrong CSR key or EKUs")
	}
	var receipt IssuedReceipt
	b, err := os.ReadFile(receiptPath(a.directory, "issued.json"))
	if err != nil {
		t.Fatal(err)
	}
	if json.Unmarshal(b, &receipt) != nil || !receipt.CSRVerified || receipt.LeafSHA256 != digest(chain[0]) {
		t.Fatal("missing public issuance proof")
	}
}
func TestACMERefusesWrongEABIdentifierAndCSR(t *testing.T) {
	_, _, a := fixture(t)
	c := client(t, a)
	accountKey := c.key
	otherKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	c.key = otherKey
	if s, _ := c.post("/acme/new-account", map[string]any{"externalAccountBinding": c.eab()}, true); s == 201 {
		t.Fatal("forged account JWS accepted")
	}
	c.key = accountKey
	e := c.eab()
	e.Signature = base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	if s, _ := c.post("/acme/new-account", map[string]any{"externalAccountBinding": e}, true); s == 201 {
		t.Fatal("wrong EAB accepted")
	}
	c.register()
	if s, _ := c.post("/acme/new-order", map[string]any{"identifiers": []map[string]string{{"type": "permanent-identifier", "value": "another-device"}}}, false); s == 201 {
		t.Fatal("wrong identity accepted")
	}
	c.order()
	weakCSR, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, accountKey)
	if err != nil {
		t.Fatal(err)
	}
	if s, _ := c.post("/acme/finalize", map[string]string{"csr": base64.RawURLEncoding.EncodeToString(weakCSR)}, false); s == 200 {
		t.Fatal("non-ML-DSA device key accepted")
	}
	key, _ := mldsa.GenerateKey(mldsa.MLDSA65())
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	if err != nil {
		t.Fatal(err)
	}
	csr[len(csr)-1] ^= 1
	if s, _ := c.post("/acme/finalize", map[string]string{"csr": base64.RawURLEncoding.EncodeToString(csr)}, false); s == 200 {
		t.Fatal("forged CSR accepted")
	}
	if _, err := os.Stat(receiptPath(a.directory, "issued.json")); !os.IsNotExist(err) {
		t.Fatal("failed request wrote issuance")
	}
}
func TestServerRefusesExternalBindingAndReissue(t *testing.T) {
	dir, _, a := fixture(t)
	if err := Serve(context.Background(), dir, "0.0.0.0:0", filepath.Join(dir, "acme-enrollment.json"), time.Second, nil); err == nil {
		t.Fatal("public listener accepted")
	}
	enroll(t, a)
	if err := Serve(context.Background(), dir, "127.0.0.1:0", filepath.Join(dir, "acme-enrollment.json"), time.Second, nil); err == nil {
		t.Fatal("issued fixture reused")
	}
}

type fixedClock struct{ now time.Time }

func (c fixedClock) Now() (trustedtime.Interval, error) {
	return trustedtime.Interval{Earliest: c.now, Latest: c.now, BootID: "fixture-unit-test", Monotonic: time.Second, Domain: "physical", Authenticated: true, Source: "unit-test-only"}, nil
}

func TestProofPreservesRequestAndJournalReadsExactOriginal(t *testing.T) {
	dir, state, a := fixture(t)
	key, chain := enroll(t, a)
	clock := fixedClock{time.Now().UTC().Truncate(time.Millisecond)}
	certs, err := wendycert.NewDevice(wendycert.Config{Tenant: a.public.Tenant, Roots: [][]byte{a.public.RootDER}})
	if err != nil {
		t.Fatal(err)
	}
	scope := serviceorigin.Scope{Tenant: a.public.Tenant, Site: "office", Device: a.public.Principal, App: App, Service: "gas", DeploymentGeneration: 42, ExecutionDomain: "simulation", Publish: []serviceorigin.PublishScope{{ComponentType: "wendy.sensor.scalar.v1", Stream: "gas"}}}
	v, err := serviceorigin.NewVerifier(serviceorigin.VerifierConfig{Tenant: a.public.Tenant, Site: "office", ExecutionDomain: "simulation", Certificates: certs, Clock: clock, Authorize: func(context.Context, serviceorigin.DelegationClaims, wendycert.DeviceIdentity) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	j, err := servicejournal.Create(servicejournal.Config{Path: filepath.Join(state, "journal", "journal.db"), Replica: world.Config{Tenant: a.public.Tenant, Site: "office", ClockDomain: ClockDomain, Now: func() time.Time { return SampleTime }, MaxEntries: 256, AllowSynthetic: true, Frames: []world.FrameRef{{ID: "office-floor", Revision: "vm-fixture-1"}}}, Verifier: v, PhysicalClock: clock, AdmitSchema: func(context.Context, serviceorigin.VerifiedRecord) error { return nil }, MaxProofBytes: 32 << 20, MaxEntries: 256})
	if err != nil {
		t.Fatal(err)
	}
	epoch, err := j.ReserveEpoch(context.Background(), scope.Tenant, scope.Device, scope.App, scope.Service)
	if err != nil {
		t.Fatal(err)
	}
	p, err := serviceorigin.NewPublisher(serviceorigin.PublisherConfig{Scope: scope, Epoch: epoch, Lifetime: 30 * time.Minute, Clock: clock, Certificates: certs, ChainDER: chain, DeviceKey: key})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	wire := &appv1.PublishRequest{Entity: "fixture-gas", ComponentType: "wendy.sensor.scalar.v1", Stream: "gas", Frame: &appv1.FrameRef{Id: "office-floor", Revision: "vm-fixture-1"}, Timing: &appv1.Timing{SampleUnixNs: SampleTime.UnixNano(), ClockDomain: ClockDomain, ErrorKnown: true}, TtlNs: uint64(time.Minute), Provenance: &appv1.Provenance{Origin: appv1.Origin_ORIGIN_SIMULATED, AppVersion: "1", Simulation: &appv1.SimulationContext{RunId: "agent-vm-fixture", ScenarioVersion: "private-agent-v1", Seed: 1, ClockDomain: ClockDomain}}, PayloadJson: []byte(`{ "value": 4, "unit":"ppm", "sensor_type":"gas", "calibration_revision":"vm-1", "uncertainty":0.2 }`)}
	req, err := appv1.DecodePublish(wire)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := p.Publish(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = j.Admit(context.Background(), signed); err != nil {
		t.Fatal(err)
	}
	proof, request := filepath.Join(dir, "proof.json"), filepath.Join(dir, "request.json")
	raw, _ := json.Marshal(signed)
	if err = writeNew(proof, raw); err != nil {
		t.Fatal(err)
	}
	requestRaw, _ := protojson.Marshal(wire)
	if err = writeNew(request, requestRaw); err != nil {
		t.Fatal(err)
	}
	receipt, err := verify(context.Background(), dir, proof, request, "gas", 42, epoch, clock)
	if err != nil {
		t.Fatal(err)
	}
	if !receipt.OriginalRequestPreserved || receipt.PayloadSHA256 != digest(wire.PayloadJson) {
		t.Fatal("request was not preserved")
	}
	if _, err = verify(context.Background(), dir, proof, request, "temperature", 42, epoch, clock); err == nil {
		t.Fatal("wrong service accepted")
	}
	if _, err = verify(context.Background(), dir, proof, request, "gas", 43, epoch, clock); err == nil {
		t.Fatal("wrong generation accepted")
	}
	if err = j.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(state, "journal", "journal.db"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := CompareJournal(dir, proof, "gas", 42, epoch)
	if err != nil {
		t.Fatal(err)
	}
	if !r.ReadOnly || !r.ExactEnvelopeRetained || !r.CheckpointMatches {
		t.Fatal("journal evidence incomplete")
	}
	after, err := os.ReadFile(filepath.Join(state, "journal", "journal.db"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("diagnostic mutated journal")
	}
	if _, err = CompareJournal(dir, proof, "gas", 42, epoch+1); err == nil {
		t.Fatal("wrong historical epoch accepted")
	}
	wire.PayloadJson = []byte(`{"value":5,"unit":"ppm","sensor_type":"gas","calibration_revision":"vm-1","uncertainty":0.2}`)
	changed, _ := protojson.Marshal(wire)
	if err = os.WriteFile(request, changed, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = verify(context.Background(), dir, proof, request, "gas", 42, epoch, clock); err == nil {
		t.Fatal("changed original payload accepted")
	}
}
