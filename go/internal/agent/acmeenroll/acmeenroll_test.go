package acmeenroll

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
)

const (
	testTenantID = "13a72725-dfe3-4425-bd04-b253d2036089"
	testDeviceID = "dev-01"
	testEABKeyID = "6f1d0e2c-0000-4000-8000-000000000001"
	// testEABHMACKey is hex, as pki-core hands it out. The fake server MACs
	// with the decoded bytes, so a client that skips the decode fails here.
	testEABHMACKey = "0badc0ffee1234567890abcdef0badc0ffee1234567890abcdef0badc0ffee12"
)

// fakeACME is a canned RFC 8555 directory that answers just enough for Enroll.
// It does not verify the outer JWS (that is pki-core's job); it does verify the
// EAB MAC, which is the one thing this client can get wrong on its own.
type fakeACME struct {
	t          *testing.T
	leaf       []byte // DER
	issuer     []byte // DER
	orderState string
	accounts   int
	// Captured for assertions.
	eabKID        string
	eabMACOK      bool
	orderIDs      []map[string]string
	finalizeOK    bool
	finalizeCSR   *x509.CertificateRequest
	wrongKey      bool
	wrongIdentity bool
	getOnlyCert   bool
	certGets      int
}

func (f *fakeACME) handler(base string) http.Handler {
	mux := http.NewServeMux()
	nonce := func(w http.ResponseWriter) { w.Header().Set("Replay-Nonce", "bm9uY2U") }

	mux.HandleFunc("/"+testTenantID+"/acme/directory", func(w http.ResponseWriter, r *http.Request) {
		nonce(w)
		writeJSON(w, http.StatusOK, map[string]any{
			"newNonce":   base + "/acme/new-nonce",
			"newAccount": base + "/acme/new-account",
			"newOrder":   base + "/acme/new-order",
			"meta":       map[string]any{"externalAccountRequired": true},
		})
	})
	mux.HandleFunc("/acme/new-nonce", func(w http.ResponseWriter, r *http.Request) {
		nonce(w)
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/acme/new-account", func(w http.ResponseWriter, r *http.Request) {
		nonce(w)
		f.checkEAB(r)
		w.Header().Set("Location", base+"/acme/account/1")
		f.accounts++
		status := http.StatusCreated
		if f.accounts > 1 {
			// pki-core re-registers the same account key idempotently, which
			// is what keeps the single-use EAB from being burned twice.
			status = http.StatusOK
		}
		writeJSON(w, status, map[string]any{"status": "valid"})
	})
	mux.HandleFunc("/acme/new-order", func(w http.ResponseWriter, r *http.Request) {
		nonce(w)
		var body struct {
			Identifiers []map[string]string `json:"identifiers"`
		}
		decodePayload(f.t, r, &body)
		f.orderIDs = body.Identifiers
		w.Header().Set("Location", base+"/acme/order/1")
		writeJSON(w, http.StatusCreated, map[string]any{
			"status":      f.orderState,
			"identifiers": body.Identifiers,
			"finalize":    base + "/acme/finalize",
		})
	})
	mux.HandleFunc("/acme/finalize", func(w http.ResponseWriter, r *http.Request) {
		nonce(w)
		var body struct {
			CSR string `json:"csr"`
		}
		decodePayload(f.t, r, &body)
		der, err := base64.RawURLEncoding.DecodeString(body.CSR)
		if err != nil {
			f.t.Errorf("finalize CSR is not base64url: %v", err)
		}
		csr, err := x509.ParseCertificateRequest(der)
		if err != nil {
			f.t.Errorf("finalize CSR does not parse: %v", err)
			return
		}
		if !f.wrongKey {
			f.finalizeCSR = csr
			key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			if err != nil {
				f.t.Error(err)
				return
			}
			deviceID := testDeviceID
			if f.wrongIdentity {
				deviceID = "another-device"
			}
			principal, _ := url.Parse("spiffe://wendy.sh/tenant/" + testTenantID + "/device/" + deviceID)
			tmpl := &x509.Certificate{
				SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "leaf"},
				NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
				URIs: []*url.URL{principal},
			}
			f.leaf, err = x509.CreateCertificate(rand.Reader, tmpl, tmpl, csr.PublicKey, key)
			if err != nil {
				f.t.Error(err)
				return
			}
		}
		f.finalizeOK = true
		w.Header().Set("Location", base+"/acme/order/1")
		writeJSON(w, http.StatusOK, map[string]any{
			"status":      "valid",
			"certificate": base + "/" + testTenantID + "/acme/cert/1",
		})
	})
	mux.HandleFunc("/"+testTenantID+"/acme/cert/1", func(w http.ResponseWriter, r *http.Request) {
		if f.getOnlyCert && r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		if r.Method == http.MethodGet {
			f.certGets++
		}
		nonce(w)
		w.Header().Set("Content-Type", "application/pem-certificate-chain")
		w.Write(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.leaf}))
		w.Write(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.issuer}))
	})
	return mux
}

// checkEAB re-computes the HS256 MAC over the inner JWS with the decoded key
// bytes. A client that passes the hex string through unchanged produces a
// well-formed binding whose MAC does not match — exactly the failure this
// guards.
func (f *fakeACME) checkEAB(r *http.Request) {
	var acct struct {
		EAB *struct {
			Protected string `json:"protected"`
			Payload   string `json:"payload"`
			Signature string `json:"signature"`
		} `json:"externalAccountBinding"`
	}
	decodePayload(f.t, r, &acct)
	if acct.EAB == nil {
		f.t.Error("new-account carried no externalAccountBinding")
		return
	}

	var protected struct {
		Alg string `json:"alg"`
		KID string `json:"kid"`
	}
	raw, err := base64.RawURLEncoding.DecodeString(acct.EAB.Protected)
	if err != nil {
		f.t.Errorf("EAB protected header is not base64url: %v", err)
		return
	}
	if err := json.Unmarshal(raw, &protected); err != nil {
		f.t.Errorf("EAB protected header is not JSON: %v", err)
		return
	}
	f.eabKID = protected.KID
	if protected.Alg != "HS256" {
		f.t.Errorf("EAB alg = %q, want HS256", protected.Alg)
	}

	key, err := hex.DecodeString(testEABHMACKey)
	if err != nil {
		f.t.Fatalf("test EAB key: %v", err)
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(acct.EAB.Protected + "." + acct.EAB.Payload))
	got, err := base64.RawURLEncoding.DecodeString(acct.EAB.Signature)
	if err != nil {
		f.t.Errorf("EAB signature is not base64url: %v", err)
		return
	}
	f.eabMACOK = hmac.Equal(got, mac.Sum(nil))
}

func decodePayload(t *testing.T, r *http.Request, v any) {
	t.Helper()
	var jws struct {
		Payload string `json:"payload"`
	}
	if err := json.NewDecoder(r.Body).Decode(&jws); err != nil {
		t.Fatalf("request body is not a JWS: %v", err)
	}
	payload, err := base64.RawURLEncoding.DecodeString(jws.Payload)
	if err != nil {
		t.Fatalf("JWS payload is not base64url: %v", err)
	}
	if err := json.Unmarshal(payload, v); err != nil {
		t.Fatalf("JWS payload is not JSON: %v", err)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func selfSigned(t *testing.T, cn string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating test key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("creating test certificate: %v", err)
	}
	return der
}

func startFake(t *testing.T, orderState string) (*fakeACME, string) {
	t.Helper()
	f := &fakeACME{
		t:          t,
		leaf:       selfSigned(t, "leaf"),
		issuer:     selfSigned(t, "issuer"),
		orderState: orderState,
	}
	// The handler needs its own base URL to build absolute directory links, so
	// the server is wired up after the listener has an address but before it
	// serves anything.
	srv := httptest.NewUnstartedServer(nil)
	base := "http://" + srv.Listener.Addr().String()
	srv.Config.Handler = f.handler(base)
	srv.Start()
	t.Cleanup(srv.Close)
	return f, base + "/" + testTenantID + "/acme/directory"
}

func testConfig(directoryURL string) Config {
	return Config{
		DirectoryURL: directoryURL,
		DeviceID:     testDeviceID,
		EABKeyID:     testEABKeyID,
		EABHMACKey:   testEABHMACKey,
	}
}

func TestEnroll(t *testing.T) {
	f, dirURL := startFake(t, "ready")
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "acme-account-key.pem")

	deviceKey, err := certs.GenerateKeyPair()
	if err != nil {
		t.Fatalf("generating device key: %v", err)
	}

	certPEM, chainPEM, err := Enroll(context.Background(), testConfig(dirURL), keyPath, []byte(deviceKey))
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}

	if !f.eabMACOK {
		t.Error("EAB MAC did not verify: the HMAC key must be hex-decoded before use")
	}
	if f.eabKID != testEABKeyID {
		t.Errorf("EAB kid = %q, want %q", f.eabKID, testEABKeyID)
	}
	want := []map[string]string{{"type": "permanent-identifier", "value": testDeviceID}}
	if len(f.orderIDs) != 1 || f.orderIDs[0]["type"] != want[0]["type"] || f.orderIDs[0]["value"] != want[0]["value"] {
		t.Errorf("newOrder identifiers = %v, want %v", f.orderIDs, want)
	}
	if !f.finalizeOK {
		t.Error("finalize was never reached")
	}
	if f.finalizeCSR == nil {
		t.Fatal("finalize did not receive a CSR")
	}
	if err := f.finalizeCSR.CheckSignature(); err != nil {
		t.Fatalf("finalize CSR signature: %v", err)
	}
	var usages []asn1.ObjectIdentifier
	for _, ext := range f.finalizeCSR.Extensions {
		if ext.Id.Equal(asn1.ObjectIdentifier{2, 5, 29, 37}) {
			if rest, err := asn1.Unmarshal(ext.Value, &usages); err != nil || len(rest) != 0 {
				t.Fatalf("decoding requested key usages: %v", err)
			}
		}
	}
	if len(usages) != 2 || !usages[0].Equal(asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 7, 3, 2}) || !usages[1].Equal(asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 7, 3, 1}) {
		t.Fatalf("ACME CSR key usages = %v, want clientAuth and serverAuth", usages)
	}

	if got := countPEM(certPEM); got != 1 {
		t.Errorf("certPEM holds %d certificates, want the leaf alone", got)
	}
	if got := countPEM(chainPEM); got != 1 {
		t.Errorf("chainPEM holds %d certificates, want the issuer alone", got)
	}
	leaf, err := certs.ParseCertsFromPEM([]byte(certPEM))
	if err != nil {
		t.Fatalf("parsing returned leaf: %v", err)
	}
	if leaf[0].Subject.CommonName != "leaf" {
		t.Errorf("leaf CN = %q, want %q", leaf[0].Subject.CommonName, "leaf")
	}

	// The account key must survive so the single-use EAB is not needed again.
	stored, err := loadOrCreateAccountKey(keyPath)
	if err != nil {
		t.Fatalf("re-loading account key: %v", err)
	}
	if _, _, err := Enroll(context.Background(), testConfig(dirURL), keyPath, []byte(deviceKey)); err != nil {
		t.Fatalf("second Enroll (account already exists): %v", err)
	}
	again, err := loadOrCreateAccountKey(keyPath)
	if err != nil {
		t.Fatalf("re-loading account key: %v", err)
	}
	if !stored.Equal(again) {
		t.Error("account key changed between enrollments; the EAB cannot be reused to register a new one")
	}
}

func TestEnrollRejectsOrderNeedingAttestation(t *testing.T) {
	_, dirURL := startFake(t, "pending")
	deviceKey, err := certs.GenerateKeyPair()
	if err != nil {
		t.Fatalf("generating device key: %v", err)
	}
	_, _, err = Enroll(context.Background(), testConfig(dirURL),
		filepath.Join(t.TempDir(), "acme-account-key.pem"), []byte(deviceKey))
	if err == nil || !strings.Contains(err.Error(), "attestation") {
		t.Fatalf("Enroll error = %v, want an attestation-challenge error", err)
	}
}

func TestEnrollRejectsCertificateForAnotherDevice(t *testing.T) {
	for _, wrongKey := range []bool{false, true} {
		t.Run(fmt.Sprint("wrongKey=", wrongKey), func(t *testing.T) {
			f, dirURL := startFake(t, "ready")
			f.wrongKey = wrongKey
			f.wrongIdentity = !wrongKey
			deviceKey, err := certs.GenerateKeyPair()
			if err != nil {
				t.Fatal(err)
			}
			leaf, chain, err := Enroll(context.Background(), testConfig(dirURL), filepath.Join(t.TempDir(), "account.pem"), []byte(deviceKey))
			if err == nil || leaf != "" || chain != "" {
				t.Fatalf("mismatched certificate accepted: error=%v", err)
			}
		})
	}
}

func TestPrincipalURIRejectsInvalidDirectory(t *testing.T) {
	for _, directory := range []string{
		"http://pki.example/" + testTenantID + "/acme/directory",
		"https://pki.example/not-a-tenant/acme/directory",
		"https://pki.example/" + testTenantID + "/acme/directory?tenant=other",
		"https://user:secret@pki.example/" + testTenantID + "/acme/directory",
	} {
		if _, err := testConfig(directory).PrincipalURI(); err == nil {
			t.Errorf("accepted directory %q", directory)
		}
	}
}

func TestConfigValidateNamesEveryMissingField(t *testing.T) {
	err := Config{}.validate()
	if err == nil {
		t.Fatal("empty Config validated")
	}
	for _, f := range []string{"directoryURL", "deviceID", "eabKeyID", "eabHMACKey"} {
		if !strings.Contains(err.Error(), f) {
			t.Errorf("validate error %q does not name %q", err, f)
		}
	}
}

func countPEM(s string) int {
	n := 0
	for rest := []byte(s); ; {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return n
		}
		n++
	}
}

func TestPrincipalURIDeviceNames(t *testing.T) {
	for _, name := range []string{"sim", "fleet-a/box-01", strings.Repeat("a", 64)} {
		cfg := testConfig("https://acme.example/" + testTenantID + "/acme/directory")
		cfg.DeviceID = name
		got, err := cfg.PrincipalURI()
		if err != nil || got != "spiffe://wendy.sh/tenant/"+testTenantID+"/device/"+name {
			t.Fatalf("name=%q, principal=%q, error=%v", name, got, err)
		}
	}
	for _, name := range []string{"", "/sim", "sim/", "fleet//sim", ".", "..", "fleet/../sim", "sim box", "sim?", "sim#", "sim%2fbox", "sím", strings.Repeat("a", 65)} {
		cfg := testConfig("https://acme.example/" + testTenantID + "/acme/directory")
		cfg.DeviceID = name
		if _, err := cfg.PrincipalURI(); err == nil {
			t.Errorf("accepted invalid name %q", name)
		}
	}
}

func TestEnrollDownloadsPKIGetOnlyCertificate(t *testing.T) {
	f, dirURL := startFake(t, "ready")
	f.getOnlyCert = true
	key, err := certs.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	leaf, _, err := Enroll(context.Background(), testConfig(dirURL), filepath.Join(t.TempDir(), "account.pem"), []byte(key))
	if err != nil {
		t.Fatal(err)
	}
	if leaf == "" || !f.finalizeOK || f.certGets != 1 {
		t.Fatal("GET certificate download did not complete after finalization")
	}
}

func TestPKICertificateGETRestrictsDestination(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; http.Error(w, "no", http.StatusNotFound) }))
	defer srv.Close()
	directory := srv.URL + "/" + testTenantID + "/acme/directory"
	for _, certURL := range []string{srv.URL + "/other/acme/cert/1", srv.URL + "/" + testTenantID + "/acme/order/1", srv.URL + "/" + testTenantID + "/acme/cert/1?secret=value", "https://other.example/" + testTenantID + "/acme/cert/1"} {
		if _, err := fetchPKICertificate(context.Background(), directory, certURL); err == nil {
			t.Fatal("unexpected certificate destination accepted")
		}
	}
	if calls != 0 {
		t.Fatal("GET reached an untrusted certificate endpoint")
	}
	_, err := fetchPKICertificate(context.Background(), directory, srv.URL+"/"+testTenantID+"/acme/cert/1")
	if err == nil || !strings.Contains(err.Error(), "HTTP 404") || calls != 1 {
		t.Fatalf("missing certificate download error: %v", err)
	}
}
