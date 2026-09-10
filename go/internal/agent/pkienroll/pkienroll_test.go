package pkienroll

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
)

const testTenant = "022b7284-f7f3-4d86-b844-d105a7c06d9e"

// fakeFrontend mimics pki-core's CSR frontend closely enough to pin this
// client's wire shape: it checks the path, the bearer scheme and the body, and
// it issues a leaf whose SPIFFE SAN it stamps itself from the request's
// device_id — the behaviour that makes the CSR's own URI SANs irrelevant.
type fakeFrontend struct {
	t *testing.T

	caKey  *ecdsa.PrivateKey
	caCert *x509.Certificate
	caPEM  string

	// wantToken, when set, is the only token accepted; anything else is 401.
	wantToken string
	// wantDeviceID, when set, is the only device_id accepted; anything else
	// is 400, as pki-core answers a token/device_id mismatch.
	wantDeviceID string
	// tenant is stamped into the issued SAN. Deliberately settable so a test
	// can serve a leaf for the wrong tenant.
	tenant string
	// kind overrides the SPIFFE principal kind ("device" unless set), so a
	// test can serve a service principal the ingest surface would reject.
	kind string
	// sanCount is how many tenant SPIFFE SANs to stamp (1 unless set).
	sanCount int
	// notAfter overrides the issued validity.
	lifetime time.Duration
	// includeChain controls whether the issuer is appended after the leaf.
	includeChain bool

	// Captured for assertions.
	lastPath       string
	lastAuth       string
	lastDeviceID   string
	lastTier       string
	lastCSRSubject string
	lastCSRURIs    []string
	lastDeviceName string
	calls          int
}

func newFakeFrontend(t *testing.T) *fakeFrontend {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating test CA key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Test Device Identity CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour * 365),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("creating test CA cert: %v", err)
	}
	caCert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parsing test CA cert: %v", err)
	}
	return &fakeFrontend{
		t:            t,
		caKey:        caKey,
		caCert:       caCert,
		caPEM:        string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		tenant:       testTenant,
		sanCount:     1,
		lifetime:     30 * 24 * time.Hour,
		includeChain: true,
	}
}

func (f *fakeFrontend) serve() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(f.handle))
}

func (f *fakeFrontend) handle(w http.ResponseWriter, r *http.Request) {
	f.calls++
	f.lastPath = r.URL.Path
	f.lastAuth = r.Header.Get("Authorization")

	var body enrollRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"detail":"invalid request body"}`, http.StatusBadRequest)
		return
	}
	f.lastDeviceID = body.DeviceID
	f.lastTier = body.Tier

	if f.wantToken != "" && f.lastAuth != "Bearer "+f.wantToken {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"detail":"bearer token required"}`))
		return
	}
	if f.wantDeviceID != "" && body.DeviceID != f.wantDeviceID {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"detail":"ca: enrollment token device_id mismatch"}`))
		return
	}

	csrBlock, _ := pem.Decode([]byte(body.CSR))
	if csrBlock == nil {
		http.Error(w, `{"detail":"csr is not PEM"}`, http.StatusBadRequest)
		return
	}
	csr, err := x509.ParseCertificateRequest(csrBlock.Bytes)
	if err != nil {
		http.Error(w, `{"detail":"csr does not parse"}`, http.StatusBadRequest)
		return
	}
	f.lastCSRSubject = csr.Subject.CommonName
	f.lastCSRURIs = nil
	for _, u := range csr.URIs {
		f.lastCSRURIs = append(f.lastCSRURIs, u.String())
	}

	// On a renewal pki-core reads the device name off the PRESENTED
	// certificate's SAN, never off the renewal CSR, so the name carries
	// forward from the previous issuance. The fake reproduces that by reusing
	// the last name it minted rather than reading the CSR subject.
	deviceName := body.DeviceID
	if deviceName == "" {
		if strings.HasSuffix(r.URL.Path, "/renew") && f.lastDeviceName != "" {
			deviceName = f.lastDeviceName
		} else {
			deviceName = csr.Subject.CommonName
		}
	}
	f.lastDeviceName = deviceName
	kind := f.kind
	if kind == "" {
		kind = "device"
	}
	var uris []*url.URL
	for i := 0; i < f.sanCount; i++ {
		name := deviceName
		if i > 0 {
			name = deviceName + "-extra"
		}
		u, err := url.Parse("spiffe://wendy.sh/tenant/" + f.tenant + "/" + kind + "/" + name)
		if err != nil {
			f.t.Fatalf("building test SAN: %v", err)
		}
		uris = append(uris, u)
	}

	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(int64(f.calls + 100)),
		// pki-core derives the leaf DN from the minted identity, so the test
		// server does too: it must not echo the CSR subject.
		Subject:     pkix.Name{CommonName: kind + ":" + deviceName},
		NotBefore:   time.Now().Add(-time.Minute),
		NotAfter:    time.Now().Add(f.lifetime),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		URIs:        uris,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, f.caCert, csr.PublicKey, f.caKey)
	if err != nil {
		f.t.Fatalf("issuing test leaf: %v", err)
	}
	leafPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	bundle := leafPEM
	if f.includeChain {
		bundle += f.caPEM
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(certificateResponseBody{Certificate: bundle})
}

func testKey(t *testing.T) []byte {
	t.Helper()
	keyPEM, err := certs.GenerateKeyPair()
	if err != nil {
		t.Fatalf("generating test key: %v", err)
	}
	return []byte(keyPEM)
}

func TestEnrollSuccess(t *testing.T) {
	f := newFakeFrontend(t)
	f.wantToken = "tok-abc"
	f.wantDeviceID = "sh/wendy/2/408"
	srv := f.serve()
	defer srv.Close()

	result, err := Enroll(context.Background(), EnrollRequest{
		CSRFrontendURL:  srv.URL,
		TenantUUID:      testTenant,
		EnrollmentToken: "tok-abc",
		Key:             testKey(t),
		CommonName:      "sh/wendy/2/408",
	})
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}

	if got, want := f.lastPath, "/v1/"+testTenant+"/enroll"; got != want {
		t.Errorf("path = %q, want %q", got, want)
	}
	if f.lastAuth != "Bearer tok-abc" {
		t.Errorf("Authorization = %q", f.lastAuth)
	}
	// device_id defaults to the Common Name, which is what pki-core's own
	// minting convention pairs it with.
	if f.lastDeviceID != "sh/wendy/2/408" {
		t.Errorf("device_id = %q, want the Common Name", f.lastDeviceID)
	}
	if f.lastTier != DefaultTier {
		t.Errorf("tier = %q, want %q", f.lastTier, DefaultTier)
	}
	if f.lastCSRSubject != "sh/wendy/2/408" {
		t.Errorf("CSR CN = %q, want the CAS Common Name form", f.lastCSRSubject)
	}
	// The client asserts no identity of its own: pki-core discards CSR URI
	// SANs on a device profile, so sending one would be a claim it is not
	// entitled to make.
	if len(f.lastCSRURIs) != 0 {
		t.Errorf("CSR carried URI SANs %v, want none", f.lastCSRURIs)
	}

	wantURI := certs.DeviceSPIFFEURI(testTenant, "sh/wendy/2/408")
	if result.SPIFFEURI != wantURI {
		t.Errorf("SPIFFEURI = %q, want %q", result.SPIFFEURI, wantURI)
	}
	if result.DeviceName != "sh/wendy/2/408" {
		t.Errorf("DeviceName = %q", result.DeviceName)
	}
	if result.LeafPEM == "" || !strings.Contains(result.LeafPEM, "BEGIN CERTIFICATE") {
		t.Errorf("LeafPEM is not a certificate: %q", result.LeafPEM)
	}
	if result.ChainPEM == "" {
		t.Error("ChainPEM is empty, want the issuer below the leaf")
	}
	if strings.Contains(result.LeafPEM, result.ChainPEM) {
		t.Error("leaf and chain were not separated")
	}
	if result.NotAfter.Before(result.NotBefore) || result.NotAfter.IsZero() {
		t.Errorf("validity window is nonsense: %v to %v", result.NotBefore, result.NotAfter)
	}
	// The CSR requests both EKUs, matching what the agent asks CAS for.
	assertCSREKUs(t, f)
}

func assertCSREKUs(t *testing.T, f *fakeFrontend) {
	t.Helper()
	// Re-derive the CSR from a fresh build rather than re-reading the wire:
	// the assertion is about what buildCSR requests.
	csrPEM, err := buildCSR(testKey(t), "sh/wendy/2/408")
	if err != nil {
		t.Fatalf("buildCSR: %v", err)
	}
	block, _ := pem.Decode([]byte(csrPEM))
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		t.Fatalf("parsing CSR: %v", err)
	}
	var haveEKU bool
	for _, ext := range csr.Extensions {
		if ext.Id.String() == "2.5.29.37" {
			haveEKU = true
		}
	}
	if !haveEKU {
		t.Error("CSR carries no extendedKeyUsage extension")
	}
}

func TestEnrollUsesExplicitDeviceID(t *testing.T) {
	f := newFakeFrontend(t)
	f.wantDeviceID = "408"
	srv := f.serve()
	defer srv.Close()

	result, err := Enroll(context.Background(), EnrollRequest{
		CSRFrontendURL:  srv.URL,
		TenantUUID:      testTenant,
		EnrollmentToken: "tok",
		Key:             testKey(t),
		CommonName:      "sh/wendy/2/408",
		DeviceID:        "408",
	})
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	if result.DeviceName != "408" {
		t.Errorf("DeviceName = %q, want the token's device_id", result.DeviceName)
	}
}

func TestEnrollUnauthorized(t *testing.T) {
	f := newFakeFrontend(t)
	f.wantToken = "the-real-token"
	srv := f.serve()
	defer srv.Close()

	_, err := Enroll(context.Background(), EnrollRequest{
		CSRFrontendURL:  srv.URL,
		TenantUUID:      testTenant,
		EnrollmentToken: "wrong",
		Key:             testKey(t),
		CommonName:      "sh/wendy/2/408",
	})
	var statusErr *StatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("err = %v, want *StatusError", err)
	}
	if !statusErr.Unauthorized() {
		t.Errorf("StatusCode = %d, want 401", statusErr.StatusCode)
	}
	if !strings.Contains(statusErr.Detail, "bearer token required") {
		t.Errorf("Detail = %q, want the frontend's own detail", statusErr.Detail)
	}
}

func TestEnrollBadRequestOnDeviceIDMismatch(t *testing.T) {
	f := newFakeFrontend(t)
	f.wantDeviceID = "sh/wendy/2/408"
	srv := f.serve()
	defer srv.Close()

	_, err := Enroll(context.Background(), EnrollRequest{
		CSRFrontendURL:  srv.URL,
		TenantUUID:      testTenant,
		EnrollmentToken: "tok",
		Key:             testKey(t),
		CommonName:      "sh/wendy/2/408",
		DeviceID:        "sh/wendy/2/999",
	})
	var statusErr *StatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("err = %v, want *StatusError", err)
	}
	if statusErr.StatusCode != http.StatusBadRequest {
		t.Errorf("StatusCode = %d, want 400", statusErr.StatusCode)
	}
	if statusErr.Unauthorized() {
		t.Error("a 400 must not report as unauthorized: the remedies differ")
	}
	if !strings.Contains(statusErr.Detail, "device_id mismatch") {
		t.Errorf("Detail = %q", statusErr.Detail)
	}
}

func TestEnrollRefusesWrongTenant(t *testing.T) {
	f := newFakeFrontend(t)
	f.tenant = "11111111-2222-3333-4444-555555555555"
	srv := f.serve()
	defer srv.Close()

	_, err := Enroll(context.Background(), EnrollRequest{
		CSRFrontendURL:  srv.URL,
		TenantUUID:      testTenant,
		EnrollmentToken: "tok",
		Key:             testKey(t),
		CommonName:      "sh/wendy/2/408",
	})
	var identityErr *IdentityError
	if !errors.As(err, &identityErr) {
		t.Fatalf("err = %v, want *IdentityError", err)
	}
	if len(identityErr.Got) != 1 || !strings.Contains(identityErr.Got[0], "11111111") {
		t.Errorf("Got = %v, want the SAN actually issued recorded for logging", identityErr.Got)
	}
}

func TestEnrollRefusesNonDeviceKind(t *testing.T) {
	f := newFakeFrontend(t)
	f.kind = "service"
	srv := f.serve()
	defer srv.Close()

	_, err := Enroll(context.Background(), EnrollRequest{
		CSRFrontendURL:  srv.URL,
		TenantUUID:      testTenant,
		EnrollmentToken: "tok",
		Key:             testKey(t),
		CommonName:      "sh/wendy/2/408",
	})
	var identityErr *IdentityError
	if !errors.As(err, &identityErr) {
		t.Fatalf("err = %v, want *IdentityError for a service principal", err)
	}
}

func TestEnrollRefusesMultipleTenantSANs(t *testing.T) {
	f := newFakeFrontend(t)
	f.sanCount = 2
	srv := f.serve()
	defer srv.Close()

	_, err := Enroll(context.Background(), EnrollRequest{
		CSRFrontendURL:  srv.URL,
		TenantUUID:      testTenant,
		EnrollmentToken: "tok",
		Key:             testKey(t),
		CommonName:      "sh/wendy/2/408",
	})
	var identityErr *IdentityError
	if !errors.As(err, &identityErr) {
		t.Fatalf("err = %v, want *IdentityError", err)
	}
	if len(identityErr.Got) != 2 {
		t.Errorf("Got = %v, want both SANs recorded", identityErr.Got)
	}
}

func TestEnrollAcceptsLeafWithoutChain(t *testing.T) {
	f := newFakeFrontend(t)
	f.includeChain = false
	srv := f.serve()
	defer srv.Close()

	result, err := Enroll(context.Background(), EnrollRequest{
		CSRFrontendURL:  srv.URL,
		TenantUUID:      testTenant,
		EnrollmentToken: "tok",
		Key:             testKey(t),
		CommonName:      "sh/wendy/2/408",
	})
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	if result.ChainPEM != "" {
		t.Errorf("ChainPEM = %q, want empty", result.ChainPEM)
	}
}

func TestEnrollValidatesRequest(t *testing.T) {
	_, err := Enroll(context.Background(), EnrollRequest{TenantUUID: testTenant})
	if err == nil {
		t.Fatal("want an error naming the missing fields")
	}
	for _, want := range []string{"csr frontend url", "enrollment token", "common name", "private key"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

func TestRenewSuccess(t *testing.T) {
	f := newFakeFrontend(t)
	srv := f.serve()
	defer srv.Close()
	key := testKey(t)

	enrolled, err := Enroll(context.Background(), EnrollRequest{
		CSRFrontendURL:  srv.URL,
		TenantUUID:      testTenant,
		EnrollmentToken: "tok",
		Key:             key,
		CommonName:      "sh/wendy/2/408",
	})
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}

	// The renewal is driven over the plain test transport: pki-core's own
	// possession proof is the mTLS handshake, which is its contract to test,
	// not this client's. What is pinned here is that the renewal sends no
	// token, restates the current subject, and verifies the returned SAN.
	renewed, err := Renew(context.Background(), RenewRequest{
		CSRFrontendURL:  srv.URL,
		TenantUUID:      testTenant,
		CurrentLeafPEM:  enrolled.LeafPEM,
		CurrentChainPEM: enrolled.ChainPEM,
		Key:             key,
		HTTPClient:      srv.Client(),
	})
	if err != nil {
		t.Fatalf("Renew: %v", err)
	}
	if got, want := f.lastPath, "/v1/"+testTenant+"/renew"; got != want {
		t.Errorf("path = %q, want %q", got, want)
	}
	if f.lastAuth != "" {
		t.Errorf("renewal sent Authorization %q; possession of the leaf is the credential", f.lastAuth)
	}
	if renewed.SPIFFEURI != enrolled.SPIFFEURI {
		t.Errorf("renewed identity changed: %q -> %q", enrolled.SPIFFEURI, renewed.SPIFFEURI)
	}
	if renewed.LeafPEM == enrolled.LeafPEM {
		t.Error("renewal returned the same leaf")
	}
}

func TestRenewRequiresCurrentLeaf(t *testing.T) {
	_, err := Renew(context.Background(), RenewRequest{
		CSRFrontendURL: "https://csr.dev.pki.wendy.sh",
		TenantUUID:     testTenant,
		Key:            testKey(t),
	})
	if err == nil || !strings.Contains(err.Error(), "no current certificate") {
		t.Fatalf("err = %v, want a refusal naming the missing certificate", err)
	}
}

func TestFrontendEndpoint(t *testing.T) {
	tests := []struct {
		name, base, action, want string
	}{
		{"bare host", "csr.dev.pki.wendy.sh", "enroll", "https://csr.dev.pki.wendy.sh/v1/" + testTenant + "/enroll"},
		{"scheme and host", "https://csr.pki.wendy.sh", "renew", "https://csr.pki.wendy.sh/v1/" + testTenant + "/renew"},
		{"trailing slash", "https://csr.pki.wendy.sh/", "enroll", "https://csr.pki.wendy.sh/v1/" + testTenant + "/enroll"},
		{"host and port", "https://localhost:8451", "enroll", "https://localhost:8451/v1/" + testTenant + "/enroll"},
		// A fully specified endpoint is honoured, so an operator who pasted
		// one is not silently sent somewhere else.
		{"full path passthrough", "https://csr.pki.wendy.sh/v1/other/enroll", "renew", "https://csr.pki.wendy.sh/v1/other/enroll"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := frontendEndpoint(tc.base, testTenant, tc.action)
			if err != nil {
				t.Fatalf("frontendEndpoint: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCSRFrontendURL(t *testing.T) {
	t.Setenv(CSREndpointEnv, "")
	t.Setenv(EnvironmentEnv, "")

	if got, want := CSRFrontendURL("", EnvDev), "https://"+DevCSRFrontendHost; got != want {
		t.Errorf("dev = %q, want %q", got, want)
	}
	if got, want := CSRFrontendURL("", EnvProd), "https://"+ProdCSRFrontendHost; got != want {
		t.Errorf("prod = %q, want %q", got, want)
	}
	// An unrecognised environment must not be read as dev: sending a
	// production device to a dev certificate authority is the failure this
	// guards.
	if got, want := CSRFrontendURL("", "staging"), "https://"+ProdCSRFrontendHost; got != want {
		t.Errorf("unknown environment = %q, want %q", got, want)
	}
	if got, want := CSRFrontendURL("http://127.0.0.1:8451", EnvProd), "http://127.0.0.1:8451"; got != want {
		t.Errorf("override = %q, want %q", got, want)
	}

	t.Setenv(EnvironmentEnv, EnvDev)
	if got, want := CSRFrontendURL("", ""), "https://"+DevCSRFrontendHost; got != want {
		t.Errorf("environment from env = %q, want %q", got, want)
	}
	t.Setenv(CSREndpointEnv, "csr.internal:9443")
	if got, want := CSRFrontendURL("", ""), "https://csr.internal:9443"; got != want {
		t.Errorf("endpoint from env = %q, want %q", got, want)
	}
	if got, want := CSRFrontendURL("explicit.example", ""), "https://explicit.example"; got != want {
		t.Errorf("explicit override must beat the environment variable, got %q want %q", got, want)
	}
}

func TestSplitLeafAndChain(t *testing.T) {
	if _, _, err := SplitLeafAndChain("not pem at all"); err == nil {
		t.Error("want an error for a bundle with no certificate")
	}
	f := newFakeFrontend(t)
	leaf, chain, err := SplitLeafAndChain(f.caPEM + f.caPEM)
	if err != nil {
		t.Fatalf("SplitLeafAndChain: %v", err)
	}
	if leaf == "" || chain == "" {
		t.Errorf("leaf = %q chain = %q, want both populated", leaf, chain)
	}
}
