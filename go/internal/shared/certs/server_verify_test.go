package certs_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
)

// selfSignedCert creates a minimal self-signed ECDSA cert with an optional SAN URI.
func selfSignedCert(t *testing.T, cn string, sanURI string) (*x509.Certificate, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	if sanURI != "" {
		u, uriErr := url.Parse(sanURI)
		if uriErr != nil {
			t.Fatalf("url.Parse(%q): %v", sanURI, uriErr)
		}
		tmpl.URIs = []*url.URL{u}
	}
	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("creating cert: %v", err)
	}
	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		t.Fatalf("parsing cert: %v", err)
	}
	chainPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	return cert, chainPEM
}

func TestBuildServerVerifyConnection_OrgMismatch(t *testing.T) {
	// Self-signed cert for org 7, expected org 5 → OrgMismatchError
	serverCert, chainPEM := selfSignedCert(t, "device", "urn:wendy:org:7:asset:42")

	verifyConn, err := certs.BuildServerVerifyConnection(certs.ServerVerifyOpts{
		ChainPEM:      string(chainPEM),
		ExpectedOrgID: 5,
	})
	if err != nil {
		t.Fatalf("BuildServerVerifyConnection: %v", err)
	}

	cs := tls.ConnectionState{PeerCertificates: []*x509.Certificate{serverCert}}
	err = verifyConn(cs)

	var mismatch *certs.OrgMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("expected OrgMismatchError, got %v", err)
	}
	if mismatch.Want != 5 || mismatch.Got != 7 {
		t.Errorf("OrgMismatchError = {%d, %d}, want {5, 7}", mismatch.Want, mismatch.Got)
	}
}

func TestBuildServerVerifyConnection_OrgMatch(t *testing.T) {
	serverCert, chainPEM := selfSignedCert(t, "device", "urn:wendy:org:7:asset:42")

	verifyConn, err := certs.BuildServerVerifyConnection(certs.ServerVerifyOpts{
		ChainPEM:      string(chainPEM),
		ExpectedOrgID: 7,
	})
	if err != nil {
		t.Fatalf("BuildServerVerifyConnection: %v", err)
	}

	cs := tls.ConnectionState{PeerCertificates: []*x509.Certificate{serverCert}}
	if err := verifyConn(cs); err != nil {
		t.Errorf("expected nil, got %v", err)
	}
}

func TestBuildServerVerifyConnection_ZeroOrgAcceptsAny(t *testing.T) {
	serverCert, chainPEM := selfSignedCert(t, "device", "urn:wendy:org:7:asset:42")

	verifyConn, err := certs.BuildServerVerifyConnection(certs.ServerVerifyOpts{
		ChainPEM:      string(chainPEM),
		ExpectedOrgID: 0, // accept any
	})
	if err != nil {
		t.Fatalf("BuildServerVerifyConnection: %v", err)
	}

	cs := tls.ConnectionState{PeerCertificates: []*x509.Certificate{serverCert}}
	if err := verifyConn(cs); err != nil {
		t.Errorf("expected nil, got %v", err)
	}
}

func TestBuildServerVerifyConnection_PinStoreCalledOnSuccess(t *testing.T) {
	serverCert, chainPEM := selfSignedCert(t, "device", "urn:wendy:org:7:asset:42")

	called := false
	pin := &fakePinChecker{onCheck: func(leaf *x509.Certificate, name string) error {
		called = true
		return nil
	}}

	verifyConn, err := certs.BuildServerVerifyConnection(certs.ServerVerifyOpts{
		ChainPEM:      string(chainPEM),
		ExpectedOrgID: 7,
		PinStore:      pin,
	})
	if err != nil {
		t.Fatalf("BuildServerVerifyConnection: %v", err)
	}

	cs := tls.ConnectionState{PeerCertificates: []*x509.Certificate{serverCert}}
	if err := verifyConn(cs); err != nil {
		t.Errorf("expected nil, got %v", err)
	}
	if !called {
		t.Error("PinStore.CheckAndUpdate was not called")
	}
}

type fakePinChecker struct {
	onCheck func(*x509.Certificate, string) error
}

func (f *fakePinChecker) CheckAndUpdate(leaf *x509.Certificate, displayName string) error {
	return f.onCheck(leaf, displayName)
}

// blockingPinError stands in for devicepin.PinMismatchError, which this package
// cannot import (that circular import is the reason PinChecker is an interface
// here). What it models is the only thing the verifier looks at: the error says
// its rejection is about the peer.
type blockingPinError struct{ msg string }

func (e *blockingPinError) Error() string         { return e.msg }
func (e *blockingPinError) BlockingPinRejection() {}

func TestBuildServerVerifyConnection_OnServerIdentityFiresOnSuccess(t *testing.T) {
	serverCert, chainPEM := selfSignedCert(t, "device", "urn:wendy:org:7:asset:42")

	var got certs.WendyIdentity
	var calls int
	verifyConn, err := certs.BuildServerVerifyConnection(certs.ServerVerifyOpts{
		ChainPEM:      string(chainPEM),
		ExpectedOrgID: 7,
		OnServerIdentity: func(id certs.WendyIdentity) {
			got = id
			calls++
		},
	})
	if err != nil {
		t.Fatalf("BuildServerVerifyConnection: %v", err)
	}

	cs := tls.ConnectionState{PeerCertificates: []*x509.Certificate{serverCert}}
	if err := verifyConn(cs); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("OnServerIdentity calls = %d, want 1", calls)
	}
	if got.OrgID != 7 {
		t.Errorf("captured OrgID = %d, want 7", got.OrgID)
	}
}

func TestBuildServerVerifyConnection_OnServerIdentityFiresOnMismatch(t *testing.T) {
	serverCert, chainPEM := selfSignedCert(t, "device", "urn:wendy:org:7:asset:42")

	var got certs.WendyIdentity
	verifyConn, err := certs.BuildServerVerifyConnection(certs.ServerVerifyOpts{
		ChainPEM:         string(chainPEM),
		ExpectedOrgID:    5,
		OnServerIdentity: func(id certs.WendyIdentity) { got = id },
	})
	if err != nil {
		t.Fatalf("BuildServerVerifyConnection: %v", err)
	}

	cs := tls.ConnectionState{PeerCertificates: []*x509.Certificate{serverCert}}
	err = verifyConn(cs)
	var mismatch *certs.OrgMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("expected OrgMismatchError, got %v", err)
	}
	if got.OrgID != 7 {
		t.Errorf("captured OrgID = %d, want 7 (must fire even though org check rejects)", got.OrgID)
	}
}

func TestBuildServerVerifyConnection_OnServerIdentityFiresOnChainFailure(t *testing.T) {
	// serverCert (org 9) verified against an UNRELATED CA chain → chain verify fails.
	serverCert, _ := selfSignedCert(t, "device", "urn:wendy:org:9:asset:1")
	_, unrelatedChain := selfSignedCert(t, "other-ca", "")

	var got certs.WendyIdentity
	var calls int
	verifyConn, err := certs.BuildServerVerifyConnection(certs.ServerVerifyOpts{
		ChainPEM:         string(unrelatedChain),
		ExpectedOrgID:    9,
		OnServerIdentity: func(id certs.WendyIdentity) { got = id; calls++ },
	})
	if err != nil {
		t.Fatalf("BuildServerVerifyConnection: %v", err)
	}

	cs := tls.ConnectionState{PeerCertificates: []*x509.Certificate{serverCert}}
	if err := verifyConn(cs); err == nil {
		t.Fatal("expected chain-verification error, got nil")
	}
	if calls != 1 || got.OrgID != 9 {
		t.Errorf("OnServerIdentity fired %d time(s) with OrgID %d, want 1 call with OrgID 9 (must fire before chain check)", calls, got.OrgID)
	}
}

// TestBuildServerVerifyConnection_OnVerifiedServerIdentityFiresOnSuccess covers
// the trust-grade sink: unlike OnServerIdentity (best-effort, fires before any
// verification so diagnostics work on failure), this one fires only once the
// chain and org checks have passed, so its identity is safe to pin against.
func TestBuildServerVerifyConnection_OnVerifiedServerIdentityFiresOnSuccess(t *testing.T) {
	serverCert, chainPEM := selfSignedCert(t, "device", "urn:wendy:org:7:asset:42")

	var got certs.WendyIdentity
	var calls int
	verifyConn, err := certs.BuildServerVerifyConnection(certs.ServerVerifyOpts{
		ChainPEM:                 string(chainPEM),
		ExpectedOrgID:            7,
		OnVerifiedServerIdentity: func(id certs.WendyIdentity) { got = id; calls++ },
	})
	if err != nil {
		t.Fatalf("BuildServerVerifyConnection: %v", err)
	}

	cs := tls.ConnectionState{PeerCertificates: []*x509.Certificate{serverCert}}
	if err := verifyConn(cs); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("OnVerifiedServerIdentity calls = %d, want 1", calls)
	}
	if got.OrgID != 7 || got.EntityType != "asset" || got.EntityID != "42" {
		t.Errorf("captured identity = %+v, want org 7 asset 42", got)
	}
}

// TestBuildServerVerifyConnection_OnVerifiedServerIdentitySilentOnChainFailure
// is the whole point of a separate sink: an unverified certificate must never
// reach a pin decision, or an impostor could rewrite the pin by presenting an
// unsigned cert.
func TestBuildServerVerifyConnection_OnVerifiedServerIdentitySilentOnChainFailure(t *testing.T) {
	serverCert, _ := selfSignedCert(t, "device", "urn:wendy:org:9:asset:1")
	_, unrelatedChain := selfSignedCert(t, "other-ca", "")

	var calls int
	verifyConn, err := certs.BuildServerVerifyConnection(certs.ServerVerifyOpts{
		ChainPEM:                 string(unrelatedChain),
		ExpectedOrgID:            9,
		OnVerifiedServerIdentity: func(id certs.WendyIdentity) { calls++ },
	})
	if err != nil {
		t.Fatalf("BuildServerVerifyConnection: %v", err)
	}

	cs := tls.ConnectionState{PeerCertificates: []*x509.Certificate{serverCert}}
	if err := verifyConn(cs); err == nil {
		t.Fatal("expected chain-verification error, got nil")
	}
	if calls != 0 {
		t.Errorf("OnVerifiedServerIdentity calls = %d, want 0 (chain never verified)", calls)
	}
}

// TestBuildServerVerifyConnection_OnVerifiedServerIdentitySilentOnPinRejection
// locks in that a rejected SPKI pin (devicepin.PinMismatchError) also keeps the
// trust-grade sink from firing. Device pinning is a security control that
// trusts OnVerifiedServerIdentity's output, so a pin REJECTION must behave
// exactly like a chain or org failure here: reject the connection and never
// reach this sink.
//
// The fake used to return a bare errors.New, back when the verifier failed on
// any PinChecker error at all. It now returns a blocking one — the same
// assertions, narrowed to the case they were always about. A pin store that
// merely failed to WRITE is not a rejection of the device, and is covered by
// TestBuildServerVerifyConnection_PinPersistenceFailureDoesNotBlock below.
func TestBuildServerVerifyConnection_OnVerifiedServerIdentitySilentOnPinRejection(t *testing.T) {
	serverCert, chainPEM := selfSignedCert(t, "device", "urn:wendy:org:7:asset:42")

	pinErr := error(&blockingPinError{msg: "simulated pin mismatch"})
	pin := &fakePinChecker{onCheck: func(leaf *x509.Certificate, name string) error {
		return pinErr
	}}

	var calls int
	verifyConn, err := certs.BuildServerVerifyConnection(certs.ServerVerifyOpts{
		ChainPEM:                 string(chainPEM),
		ExpectedOrgID:            7,
		PinStore:                 pin,
		OnVerifiedServerIdentity: func(id certs.WendyIdentity) { calls++ },
	})
	if err != nil {
		t.Fatalf("BuildServerVerifyConnection: %v", err)
	}

	cs := tls.ConnectionState{PeerCertificates: []*x509.Certificate{serverCert}}
	if err := verifyConn(cs); !errors.Is(err, pinErr) {
		t.Fatalf("verifyConn error = %v, want the pin error to propagate", err)
	}
	if calls != 0 {
		t.Errorf("OnVerifiedServerIdentity calls = %d, want 0 (pin check rejected)", calls)
	}
}

// TestBuildServerVerifyConnection_PinPersistenceFailureDoesNotBlock is the
// other half of the rule, and the one with the operational teeth: a pin store
// that cannot WRITE must not cost the user a device.
//
// The verifier used to return whatever CheckAndUpdate returned, so a read-only
// config directory, a full disk, or a stale lock aborted every mTLS connection
// the CLI made — a total loss of access with no security question anywhere
// behind it. The store is bookkeeping; the certificate already verified. So the
// connection must be accepted and the trust-grade sink must still fire, because
// the identity it reports was verified by exactly the same checks as always.
func TestBuildServerVerifyConnection_PinPersistenceFailureDoesNotBlock(t *testing.T) {
	serverCert, chainPEM := selfSignedCert(t, "device", "urn:wendy:org:7:asset:42")

	called := false
	pin := &fakePinChecker{onCheck: func(leaf *x509.Certificate, name string) error {
		called = true
		return errors.New("writing pin store: open /ro/known_devices.json: read-only file system")
	}}

	var calls int
	verifyConn, err := certs.BuildServerVerifyConnection(certs.ServerVerifyOpts{
		ChainPEM:                 string(chainPEM),
		ExpectedOrgID:            7,
		PinStore:                 pin,
		OnVerifiedServerIdentity: func(id certs.WendyIdentity) { calls++ },
	})
	if err != nil {
		t.Fatalf("BuildServerVerifyConnection: %v", err)
	}

	cs := tls.ConnectionState{PeerCertificates: []*x509.Certificate{serverCert}}
	if err := verifyConn(cs); err != nil {
		t.Fatalf("verifyConn = %v, want nil: a pin-store WRITE failure must never abort a certificate that verified", err)
	}
	if !called {
		t.Error("PinStore.CheckAndUpdate was not called")
	}
	if calls != 1 {
		t.Errorf("OnVerifiedServerIdentity calls = %d, want 1 (the cert verified; only the bookkeeping failed)", calls)
	}
}

func TestBuildServerVerifyConnection_OnVerifiedServerIdentitySilentOnOrgMismatch(t *testing.T) {
	serverCert, chainPEM := selfSignedCert(t, "device", "urn:wendy:org:7:asset:42")

	var calls int
	verifyConn, err := certs.BuildServerVerifyConnection(certs.ServerVerifyOpts{
		ChainPEM:                 string(chainPEM),
		ExpectedOrgID:            5,
		OnVerifiedServerIdentity: func(id certs.WendyIdentity) { calls++ },
	})
	if err != nil {
		t.Fatalf("BuildServerVerifyConnection: %v", err)
	}

	cs := tls.ConnectionState{PeerCertificates: []*x509.Certificate{serverCert}}
	if err := verifyConn(cs); err == nil {
		t.Fatal("expected OrgMismatchError, got nil")
	}
	if calls != 0 {
		t.Errorf("OnVerifiedServerIdentity calls = %d, want 0 (org check rejected)", calls)
	}
}

func TestBuildServerVerifyConnection_OnServerIdentitySilentWhenNoIdentity(t *testing.T) {
	// CN carries no Wendy identity → sink must not be called.
	serverCert, chainPEM := selfSignedCert(t, "plain-cn", "")

	var calls int
	verifyConn, err := certs.BuildServerVerifyConnection(certs.ServerVerifyOpts{
		ChainPEM:         string(chainPEM),
		ExpectedOrgID:    0,
		OnServerIdentity: func(id certs.WendyIdentity) { calls++ },
	})
	if err != nil {
		t.Fatalf("BuildServerVerifyConnection: %v", err)
	}

	cs := tls.ConnectionState{PeerCertificates: []*x509.Certificate{serverCert}}
	if err := verifyConn(cs); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if calls != 0 {
		t.Errorf("OnServerIdentity calls = %d, want 0 (no Wendy identity in cert)", calls)
	}
}

func TestBuildServerVerifyConnection_ExpectedIdentity(t *testing.T) {
	want := certs.WendyIdentity{OrgID: 7, EntityType: "asset", EntityID: "42"}

	cases := []struct {
		name        string
		sanURI      string
		wantErr     bool
		wantGotAsst string
	}{
		{name: "exact match", sanURI: "urn:wendy:org:7:asset:42"},
		{name: "different asset, same org", sanURI: "urn:wendy:org:7:asset:43", wantErr: true, wantGotAsst: "43"},
		{name: "same asset, different org", sanURI: "urn:wendy:org:9:asset:42", wantErr: true, wantGotAsst: "42"},
		{name: "user URN is not an asset", sanURI: "urn:wendy:org:7:user:42", wantErr: true},
		{name: "no wendy identity at all", sanURI: "", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			serverCert, chainPEM := selfSignedCert(t, "device", tc.sanURI)
			expected := want
			verifyConn, err := certs.BuildServerVerifyConnection(certs.ServerVerifyOpts{
				ChainPEM:         string(chainPEM),
				ExpectedIdentity: &expected,
			})
			if err != nil {
				t.Fatalf("BuildServerVerifyConnection: %v", err)
			}

			err = verifyConn(tls.ConnectionState{PeerCertificates: []*x509.Certificate{serverCert}})

			var mismatch *certs.IdentityMismatchError
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("want accepted, got %v", err)
				}
				return
			}
			if !errors.As(err, &mismatch) {
				t.Fatalf("want IdentityMismatchError, got %v", err)
			}
			if mismatch.WantOrg != 7 || mismatch.WantAsset != "42" {
				t.Errorf("want side = org %d asset %q, want org 7 asset \"42\"", mismatch.WantOrg, mismatch.WantAsset)
			}
			if mismatch.GotAsset != tc.wantGotAsst {
				t.Errorf("GotAsset = %q, want %q", mismatch.GotAsset, tc.wantGotAsst)
			}
			wantMessage := "device presented no wendy asset identity, expected asset 42 in org 7"
			if tc.wantGotAsst != "" {
				wantMessage = fmt.Sprintf("device presented asset %s in org %d, expected asset 42 in org 7", tc.wantGotAsst, mismatch.GotOrg)
			}
			if err.Error() != wantMessage {
				t.Errorf("legacy error = %q, want %q", err, wantMessage)
			}
		})
	}
}

func TestBuildServerVerifyConnection_ExpectedPrincipal(t *testing.T) {
	const tenant = "11111111-1111-4111-8111-111111111111"
	const otherTenant = "22222222-2222-4222-8222-222222222222"
	const deviceID = "lite-441bf6804824"
	principal := certs.DeviceSPIFFEURI(tenant, deviceID)
	expected, err := certs.ParsePrincipal(principal)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name         string
		sanURI       string
		wantAccepted bool
		wantGotAsset string
	}{
		{name: "same tenant and device", sanURI: principal, wantAccepted: true},
		{name: "same device in another tenant", sanURI: certs.DeviceSPIFFEURI(otherTenant, deviceID), wantGotAsset: deviceID},
		{name: "another device in same tenant", sanURI: certs.DeviceSPIFFEURI(tenant, "lite-another"), wantGotAsset: "lite-another"},
		{name: "same tenant and asset name with different principal kind", sanURI: "spiffe://wendy.sh/tenant/" + tenant + "/service/asset-" + deviceID, wantGotAsset: deviceID},
		{name: "operator is not a device", sanURI: "spiffe://wendy.sh/tenant/" + tenant + "/operator/" + deviceID},
		{name: "missing identity"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			serverCert, chainPEM := selfSignedCert(t, "device", tc.sanURI)
			verifyConn, err := certs.BuildServerVerifyConnection(certs.ServerVerifyOpts{
				ChainPEM: string(chainPEM), ExpectedIdentity: &expected,
			})
			if err != nil {
				t.Fatalf("BuildServerVerifyConnection: %v", err)
			}
			err = verifyConn(tls.ConnectionState{PeerCertificates: []*x509.Certificate{serverCert}})
			if tc.wantAccepted {
				if err != nil {
					t.Fatalf("matching principal rejected: %v", err)
				}
				return
			}
			var mismatch *certs.IdentityMismatchError
			if !errors.As(err, &mismatch) {
				t.Fatalf("want IdentityMismatchError, got %v", err)
			}
			if mismatch.WantPrincipal != principal || mismatch.GotPrincipal != tc.sanURI {
				t.Errorf("principals = want %q, got %q; expected want %q, got %q", mismatch.WantPrincipal, mismatch.GotPrincipal, principal, tc.sanURI)
			}
			if mismatch.WantAsset != deviceID || mismatch.GotAsset != tc.wantGotAsset || mismatch.WantOrg != 0 || mismatch.GotOrg != 0 {
				t.Errorf("legacy fields = %+v, want asset %q, got asset %q, both orgs 0", *mismatch, deviceID, tc.wantGotAsset)
			}
			presented := "no wendy asset identity"
			if tc.sanURI != "" {
				presented = "principal " + tc.sanURI
			}
			wantMessage := "device presented " + presented + ", expected principal " + principal
			if err.Error() != wantMessage {
				t.Errorf("error = %q, want %q", err, wantMessage)
			}
		})
	}
}

// TestBuildServerVerifyConnection_ExpectedIdentityNil locks in that
// the new field is opt-in: with it unset, a no-URN cert still passes (grace
// mode), which is what keeps unpinned legacy devices working.
func TestBuildServerVerifyConnection_ExpectedIdentityNil(t *testing.T) {
	serverCert, chainPEM := selfSignedCert(t, "device", "")
	verifyConn, err := certs.BuildServerVerifyConnection(certs.ServerVerifyOpts{
		ChainPEM:      string(chainPEM),
		ExpectedOrgID: 7,
	})
	if err != nil {
		t.Fatalf("BuildServerVerifyConnection: %v", err)
	}
	if err := verifyConn(tls.ConnectionState{PeerCertificates: []*x509.Certificate{serverCert}}); err != nil {
		t.Fatalf("grace mode should accept a no-URN cert, got %v", err)
	}
}
