package providers

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/liteclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/ble"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
)

const localLiteTenant = "11111111-1111-4111-8111-111111111111"
const localLiteOperator = "spiffe://wendy.sh/tenant/" + localLiteTenant + "/operator/test"

func localLiteTestCertificate(t *testing.T, principal string, mldsaKey bool) config.CertificateInfo {
	t.Helper()
	var keyPEM string
	var err error
	if mldsaKey {
		keyPEM, err = certs.GenerateMLDSAKeyPair()
	} else {
		keyPEM, err = certs.GenerateKeyPair()
	}
	if err != nil {
		t.Fatal(err)
	}
	key, err := certs.ParseSigningPrivateKeyPEM([]byte(keyPEM))
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(principal)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: principal},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		URIs: []*url.URL{u},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	return config.CertificateInfo{
		PemCertificate: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		PemPrivateKey:  keyPEM, PrincipalURI: principal,
	}
}

func localLiteTestSessions(t *testing.T, sessions ...config.AuthConfig) {
	t.Helper()
	t.Setenv("WENDY_CONFIG_DIR", t.TempDir())
	t.Setenv("WENDY_SECRET_STORE", "file")
	if err := config.Save(&config.Config{Auth: sessions}); err != nil {
		t.Fatal(err)
	}
	previous := LiteIdentityResolver
	t.Cleanup(func() { LiteIdentityResolver = previous })
}

func TestLocalLiteIdentityAndTrust(t *testing.T) {
	for _, useOverride := range []bool{false, true} {
		name := "primary ML-DSA"
		if useOverride {
			name = "explicit P-256 override"
		}
		t.Run(name, func(t *testing.T) {
			primary := localLiteTestCertificate(t, localLiteOperator, true)
			deviceIssuer := localLiteTestCertificate(t, localLiteOperator+"-device-issuer", false)
			primary.PemCertificateChain = deviceIssuer.PemCertificate
			selected := primary
			if useOverride {
				selected = localLiteTestCertificate(t, localLiteOperator, false)
				issuer := localLiteTestCertificate(t, localLiteOperator+"-override-issuer", false)
				selected.PemCertificateChain = issuer.PemCertificate
			}
			localLiteTestSessions(t, config.AuthConfig{Name: "work", Certificates: []config.CertificateInfo{primary}})
			LiteIdentityResolver = func(ctx context.Context, auth *config.AuthConfig) (config.CertificateInfo, string, error) {
				if auth.Name != "work" || auth.Certificates[0].PemCertificate != primary.PemCertificate {
					t.Fatal("resolver did not receive the complete selected session")
				}
				return selected, auth.Certificates[0].PemCertificateChain, nil
			}
			connected := false
			err := connectWithCLIIdentities(context.Background(), "", func(cert tls.Certificate, trust string, _ liteclient.LiteDeviceCAChain) error {
				connected = true
				leaf, _ := pem.Decode([]byte(selected.PemCertificate))
				ca, _ := pem.Decode([]byte(selected.PemCertificateChain))
				if len(cert.Certificate) != 2 || !bytes.Equal(cert.Certificate[0], leaf.Bytes) || !bytes.Equal(cert.Certificate[1], ca.Bytes) {
					t.Fatal("connection did not preserve the selected certificate and its issuer chain")
				}
				if trust != primary.PemCertificateChain {
					t.Fatal("connection replaced device trust with the override's issuer chain")
				}
				parsed, err := x509.ParseCertificate(cert.Certificate[0])
				if err != nil {
					t.Fatal(err)
				}
				if !parsed.PublicKey.(interface{ Equal(crypto.PublicKey) bool }).Equal(cert.PrivateKey.(crypto.Signer).Public()) {
					t.Fatal("connection did not use the selected private key")
				}
				return nil
			})
			if err != nil || !connected {
				t.Fatalf("selected operator identity was not connected: %v", err)
			}
		})
	}
}

func TestLocalLiteSkipsIneligibleSessionsAndTriesOtherContexts(t *testing.T) {
	primary := localLiteTestCertificate(t, localLiteOperator, true)
	legacy := localLiteTestCertificate(t, "urn:wendy:org:2:user:42", false)
	// Stored metadata cannot make a legacy certificate into an operator login.
	legacy.PrincipalURI = localLiteOperator
	service := localLiteTestCertificate(t, strings.Replace(localLiteOperator, "/operator/", "/service/", 1), false)
	localLiteTestSessions(t,
		config.AuthConfig{Name: "empty"},
		config.AuthConfig{Name: "legacy", Certificates: []config.CertificateInfo{legacy}},
		config.AuthConfig{Name: "service", Certificates: []config.CertificateInfo{service}},
		config.AuthConfig{Name: "expired", Certificates: []config.CertificateInfo{primary}},
		config.AuthConfig{Name: "wrong-issuer", Certificates: []config.CertificateInfo{primary}},
		config.AuthConfig{Name: "work", Certificates: []config.CertificateInfo{primary}},
	)
	var resolved, connected []string
	LiteIdentityResolver = func(ctx context.Context, auth *config.AuthConfig) (config.CertificateInfo, string, error) {
		resolved = append(resolved, auth.Name)
		if auth.Name == "expired" {
			return config.CertificateInfo{}, "", errors.New("operator certificate expired")
		}
		return primary, auth.Name, nil
	}
	err := connectWithCLIIdentities(context.Background(), "", func(cert tls.Certificate, trust string, _ liteclient.LiteDeviceCAChain) error {
		connected = append(connected, trust)
		if trust == "wrong-issuer" {
			return errors.New("untrusted issuer")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(resolved, []string{"expired", "wrong-issuer", "work"}) || !slices.Equal(connected, []string{"wrong-issuer", "work"}) {
		t.Fatalf("unexpected attempts: resolved=%v connected=%v", resolved, connected)
	}
}

func TestLocalLiteErrorsNameContexts(t *testing.T) {
	primary := localLiteTestCertificate(t, localLiteOperator, true)
	localLiteTestSessions(t,
		config.AuthConfig{Name: "old", Certificates: []config.CertificateInfo{primary}},
		config.AuthConfig{Name: "work", Certificates: []config.CertificateInfo{primary}},
	)
	LiteIdentityResolver = func(ctx context.Context, auth *config.AuthConfig) (config.CertificateInfo, string, error) {
		if auth.Name == "old" {
			return config.CertificateInfo{}, "", errors.New("operator certificate expired")
		}
		return primary, "device trust", nil
	}
	err := connectWithCLIIdentities(context.Background(), "", func(tls.Certificate, string, liteclient.LiteDeviceCAChain) error {
		return errors.New("connection reset by peer")
	})
	if err == nil || !strings.Contains(err.Error(), `context "old": operator certificate expired`) || !strings.Contains(err.Error(), `context "work": connection reset by peer`) || strings.Contains(err.Error(), "identity 1") {
		t.Fatalf("missing context diagnostics: %v", err)
	}
}

func TestLocalLiteTenantSelection(t *testing.T) {
	for _, tc := range []struct {
		name, tenant string
		want         []string
		wantError    bool
	}{
		{name: "missing hint keeps fallback", want: []string{"other"}},
		{name: "matching tenant", tenant: localLiteTenant, want: []string{"work"}},
		{name: "unmatched tenant", tenant: "33333333-3333-4333-8333-333333333333", wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			primary := localLiteTestCertificate(t, localLiteOperator, true)
			otherURI := strings.Replace(localLiteOperator, localLiteTenant, "22222222-2222-4222-8222-222222222222", 1)
			other := localLiteTestCertificate(t, otherURI, true)
			// The advertised tenant must match the leaf SAN, not stored metadata.
			other.PrincipalURI = localLiteOperator
			localLiteTestSessions(t,
				config.AuthConfig{Name: "other", Certificates: []config.CertificateInfo{other}},
				config.AuthConfig{Name: "work", Certificates: []config.CertificateInfo{primary}},
			)
			var resolved []string
			LiteIdentityResolver = func(ctx context.Context, auth *config.AuthConfig) (config.CertificateInfo, string, error) {
				resolved = append(resolved, auth.Name)
				return auth.Certificates[0], "device trust", nil
			}
			err := connectWithCLIIdentities(context.Background(), tc.tenant, func(tls.Certificate, string, liteclient.LiteDeviceCAChain) error { return nil })
			if (err != nil) != tc.wantError || !slices.Equal(resolved, tc.want) {
				t.Fatalf("tenant selection: resolved=%v want=%v err=%v", resolved, tc.want, err)
			}
			if tc.wantError && !strings.Contains(err.Error(), "advertised tenant") {
				t.Fatalf("missing tenant mismatch diagnostic: %v", err)
			}
		})
	}
}

func TestLocalLiteCancellationStopsIdentityFallback(t *testing.T) {
	for _, phase := range []string{"before resolution", "during resolution", "during connection"} {
		t.Run(phase, func(t *testing.T) {
			primary := localLiteTestCertificate(t, localLiteOperator, true)
			localLiteTestSessions(t,
				config.AuthConfig{Name: "first", Certificates: []config.CertificateInfo{primary}},
				config.AuthConfig{Name: "second", Certificates: []config.CertificateInfo{primary}},
			)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if phase == "before resolution" {
				cancel()
			}
			resolved, connected := 0, 0
			LiteIdentityResolver = func(gotCtx context.Context, auth *config.AuthConfig) (config.CertificateInfo, string, error) {
				resolved++
				if gotCtx != ctx {
					t.Fatal("resolver lost the caller's context")
				}
				if phase == "during resolution" {
					cancel()
				}
				return primary, "device trust", nil
			}
			err := connectWithCLIIdentities(ctx, "", func(tls.Certificate, string, liteclient.LiteDeviceCAChain) error {
				connected++
				cancel()
				return errors.New("transport closed")
			})
			if !errors.Is(err, context.Canceled) || resolved > 1 || connected > 1 {
				t.Fatalf("cancellation did not stop attempts: resolved=%d connected=%d err=%v", resolved, connected, err)
			}
			if phase == "before resolution" && resolved != 0 || phase != "during connection" && connected != 0 {
				t.Fatalf("continued after cancellation: resolved=%d connected=%d", resolved, connected)
			}
		})
	}
}

func TestLocalLiteRequiresIdentityResolver(t *testing.T) {
	primary := localLiteTestCertificate(t, localLiteOperator, true)
	localLiteTestSessions(t, config.AuthConfig{Name: "work", Certificates: []config.CertificateInfo{primary}})
	LiteIdentityResolver = nil
	err := connectWithCLIIdentities(context.Background(), "", func(tls.Certificate, string, liteclient.LiteDeviceCAChain) error {
		t.Fatal("connection bypassed the configured identity resolver")
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "identity resolver is not available") {
		t.Fatalf("unexpected missing-resolver error: %v", err)
	}
}

func TestLocalLiteRequiresTenantOperatorSession(t *testing.T) {
	legacy := localLiteTestCertificate(t, "urn:wendy:org:2:user:42", false)
	localLiteTestSessions(t, config.AuthConfig{Name: "legacy", Certificates: []config.CertificateInfo{legacy}})
	LiteIdentityResolver = func(context.Context, *config.AuthConfig) (config.CertificateInfo, string, error) {
		t.Fatal("attempted operator refresh for a legacy login")
		return config.CertificateInfo{}, "", nil
	}
	err := connectWithCLIIdentities(context.Background(), "", func(tls.Certificate, string, liteclient.LiteDeviceCAChain) error {
		t.Fatal("attempted a connection without an operator login")
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "run 'wendy auth login'") {
		t.Fatalf("missing operator login remedy: %v", err)
	}
}

func TestLocalLiteNetworkAuth(t *testing.T) {
	primary := localLiteTestCertificate(t, localLiteOperator, true)
	otherURI := strings.Replace(localLiteOperator, localLiteTenant, "22222222-2222-4222-8222-222222222222", 1)
	other := localLiteTestCertificate(t, otherURI, true)
	localLiteTestSessions(t,
		config.AuthConfig{Name: "other", Certificates: []config.CertificateInfo{other}},
		config.AuthConfig{Name: "work", Certificates: []config.CertificateInfo{primary}},
	)
	for _, transport := range []string{"LAN", "BLE"} {
		t.Run(transport, func(t *testing.T) {
			resolved := false
			LiteIdentityResolver = func(ctx context.Context, auth *config.AuthConfig) (config.CertificateInfo, string, error) {
				if auth.Name != "work" {
					t.Fatal("network transport did not filter by the advertised tenant before renewal")
				}
				resolved = true
				return config.CertificateInfo{}, "", errors.New("operator refresh refused")
			}
			client, err := (&MicroWendyProvider{}).connectClient(context.Background(), models.ExternalDevice{
				ConnectionInfo: map[string]string{
					"type": transport, "mtls": "true", "ip": "invalid", "port": "0", "address": "invalid", "deviceId": "test", "enrolledDeviceId": "00000000-0000-4000-8000-441bf6804ff8", "tenantId": localLiteTenant,
				},
			})
			if client != nil {
				client.Close()
			}
			if !resolved || err == nil || !strings.Contains(err.Error(), "operator refresh refused") {
				t.Fatalf("%s did not resolve an operator identity before dialing: %v", transport, err)
			}
		})
	}
}

func TestLiteLANPinsTheAdvertisedEnrolledDeviceID(t *testing.T) {
	const device = "00000000-0000-4000-8000-441bf6804ff8"
	for _, tc := range []struct{ name, principal, want string }{
		{"enrolled board", "spiffe://wendy.sh/tenant/" + localLiteTenant + "/device/" + device, device},
		{"unenrolled board", "", ""},
		{"other tenant", "spiffe://wendy.sh/tenant/22222222-2222-4222-8222-222222222222/device/" + device, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := (&MicroWendyProvider{}).mdnsExternalDevice(discovery.MDNSService{
				Hostname: "board.local", IPAddress: "192.0.2.1", Port: 5054,
				TXTRecords: map[string]string{"id": "441bf6804ff8", "mtls": "true", "tenant": localLiteTenant, "principal": tc.principal},
			})
			if row.ConnectionInfo["enrolledDeviceId"] != tc.want {
				t.Fatalf("enrolledDeviceId = %q, want %q", row.ConnectionInfo["enrolledDeviceId"], tc.want)
			}
			if row.ConnectionInfo["deviceId"] != "441bf6804ff8" {
				t.Fatal("the hardware id no longer keys the picker row")
			}
		})
	}
}

func TestLiteBLEPinsTheAdvertisedPrincipal(t *testing.T) {
	const device = "00000000-0000-4000-8000-441bf6804ff8"
	for _, tc := range []struct{ name, principal, tenant, want string }{
		{"enrolled board", "spiffe://wendy.sh/tenant/" + localLiteTenant + "/device/" + device, localLiteTenant, device},
		{"older firmware or unenrolled", "", "", ""},
		{"operator principal", localLiteOperator, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := (&MicroWendyProvider{}).bleExternalDevice(discovery.BLELiteDevice{
				Address: "invalid",
				Info:    ble.LiteInfo{PSM: 129, DeviceID: "441bf6804ff8", MTLSEnabled: true, Principal: tc.principal},
			})
			if row.ConnectionInfo["enrolledDeviceId"] != tc.want || row.ConnectionInfo["tenantId"] != tc.tenant {
				t.Fatalf("pin = %q/%q, want %q/%q", row.ConnectionInfo["tenantId"], row.ConnectionInfo["enrolledDeviceId"], tc.tenant, tc.want)
			}
			if row.ConnectionInfo["deviceId"] != "441bf6804ff8" {
				t.Fatal("the hardware id no longer keys the picker row")
			}
		})
	}
}

func TestLiteBLEResolvesTheEnrolledDeviceIDFromCloud(t *testing.T) {
	const enrolled = "00000000-0000-4000-8000-441bf6804ff8"
	primary := localLiteTestCertificate(t, localLiteOperator, true)
	other := localLiteTestCertificate(t, strings.Replace(localLiteOperator, localLiteTenant, "22222222-2222-4222-8222-222222222222", 1), true)
	localLiteTestSessions(t,
		config.AuthConfig{Name: "other", Certificates: []config.CertificateInfo{other}},
		config.AuthConfig{Name: "work", Certificates: []config.CertificateInfo{primary}},
	)
	previous := LiteBoardIdentityResolver
	t.Cleanup(func() { LiteBoardIdentityResolver = previous })
	for _, tc := range []struct {
		name, advertised string
		tenant           string
		lookupErr        error
		wantLookup       bool
		want             string
	}{
		{name: "cloud match", wantLookup: true, want: "operator refresh refused"},
		{name: "cloud refusal", lookupErr: errors.New("no enrollment carries its hardware id"), wantLookup: true, want: "no enrollment carries its hardware id"},
		{name: "advertised id preferred", advertised: enrolled, want: "operator refresh refused"},
		// No session holds the principal's tenant: refuse without asking Cloud.
		{name: "advertised foreign tenant", advertised: enrolled, tenant: "33333333-3333-4333-8333-333333333333", want: "no operator login matches the device's advertised tenant"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			looked := false
			LiteBoardIdentityResolver = func(_ context.Context, hardwareID string) (string, string, error) {
				looked = true
				if hardwareID != "441bf6804ff8" {
					t.Fatalf("looked up %q, not the advertised hardware id", hardwareID)
				}
				return localLiteTenant, enrolled, tc.lookupErr
			}
			LiteIdentityResolver = func(_ context.Context, auth *config.AuthConfig) (config.CertificateInfo, string, error) {
				if tc.lookupErr != nil || tc.tenant != "" {
					t.Fatal("resolved an operator identity for a board it cannot pin")
				}
				if auth.Name != "work" {
					t.Fatal("the session was not selected by the resolved tenant")
				}
				return config.CertificateInfo{}, "", errors.New("operator refresh refused")
			}
			tenant := tc.tenant
			if tc.advertised != "" && tenant == "" {
				tenant = localLiteTenant
			}
			_, err := (&MicroWendyProvider{}).connectClient(context.Background(), models.ExternalDevice{
				ConnectionInfo: map[string]string{"type": "BLE", "mtls": "true", "address": "invalid", "deviceId": "441bf6804ff8", "enrolledDeviceId": tc.advertised, "tenantId": tenant},
			})
			if looked != tc.wantLookup || err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("lookup=%v err=%v", looked, err)
			}
		})
	}
}

func TestLiteLANRefusesPKIWithoutAdvertisedPrincipal(t *testing.T) {
	previous := LiteIdentityResolver
	t.Cleanup(func() { LiteIdentityResolver = previous })
	LiteIdentityResolver = func(context.Context, *config.AuthConfig) (config.CertificateInfo, string, error) {
		t.Fatal("resolved an operator identity for a board it cannot pin")
		return config.CertificateInfo{}, "", nil
	}
	_, err := (&MicroWendyProvider{}).connectClient(context.Background(), models.ExternalDevice{
		ConnectionInfo: map[string]string{"type": "LAN", "mtls": "true", "ip": "192.0.2.1", "port": "5054", "deviceId": "441bf6804ff8", "tenantId": localLiteTenant},
	})
	if err == nil || !strings.Contains(err.Error(), "mDNS principal=") {
		t.Fatalf("LAN PKI without principal=: %v", err)
	}
}

// A pin is a canonical device UUID whatever its source: a crafted mDNS or BLE
// principal, or a pre-UUID asset found through Cloud, is refused before dialing.
func TestLitePinRefusesANonUUIDDeviceIdentity(t *testing.T) {
	const legacy = "lite-441bf6804ff8"
	principal := "spiffe://wendy.sh/tenant/" + localLiteTenant + "/device/" + legacy
	primary := localLiteTestCertificate(t, localLiteOperator, false)
	localLiteTestSessions(t, config.AuthConfig{Name: "work", Certificates: []config.CertificateInfo{primary}})
	LiteIdentityResolver = func(context.Context, *config.AuthConfig) (config.CertificateInfo, string, error) {
		return primary, primary.PemCertificate, nil
	}
	previous := LiteBoardIdentityResolver
	t.Cleanup(func() { LiteBoardIdentityResolver = previous })
	LiteBoardIdentityResolver = func(context.Context, string) (string, string, error) { return localLiteTenant, legacy, nil }
	p := &MicroWendyProvider{}
	lan := p.mdnsExternalDevice(discovery.MDNSService{
		Hostname: "board.local", IPAddress: "192.0.2.1", Port: 5054,
		TXTRecords: map[string]string{"id": "441bf6804ff8", "mtls": "true", "tenant": localLiteTenant, "principal": principal},
	})
	bleAdvertised := p.bleExternalDevice(discovery.BLELiteDevice{Address: "invalid", Info: ble.LiteInfo{PSM: 129, DeviceID: "441bf6804ff8", MTLSEnabled: true, Principal: principal}})
	bleCloud := p.bleExternalDevice(discovery.BLELiteDevice{Address: "invalid", Info: ble.LiteInfo{PSM: 129, DeviceID: "441bf6804ff8", MTLSEnabled: true}})
	for name, device := range map[string]models.ExternalDevice{"LAN principal": lan, "BLE principal": bleAdvertised, "BLE cloud lookup": bleCloud} {
		t.Run(name, func(t *testing.T) {
			client, err := p.connectClient(context.Background(), device)
			if client != nil {
				client.Close()
			}
			if err == nil || !strings.Contains(err.Error(), "is not a device UUID") {
				t.Fatalf("non-UUID pin %q not refused: %v", legacy, err)
			}
		})
	}
}
