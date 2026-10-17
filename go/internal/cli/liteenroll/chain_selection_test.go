package liteenroll

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/cloudrequest"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	litepb "github.com/wendylabsinc/wendy/go/proto/gen/litepb"
	"google.golang.org/protobuf/proto"
)

func TestEnrollmentUsesFirmwareChainSelection(t *testing.T) {
	classical, pq := testTrustPEM(t, true, time.Now().Add(time.Hour)), testMLDSATrustPEM(t)
	auth := authFixture(t)
	for _, tc := range []struct {
		name          string
		capability    *bool
		wantAlgorithm string
		wantRoots     []byte
		explicitRoots bool
	}{
		{"legacy", nil, cloudrequest.EnrollmentChainP256, classical, false},
		{"disabled", proto.Bool(false), cloudrequest.EnrollmentChainP256, classical, false},
		{"enabled", proto.Bool(true), "", pq, false},
		{"explicit compatible roots", nil, "", classical, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var discoveredAlgorithm string
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					discoveredAlgorithm = r.URL.Query().Get("chain_algorithm")
					if discoveredAlgorithm == cloudrequest.EnrollmentChainP256 {
						w.Header().Set(chainAlgorithmHeader, discoveredAlgorithm)
						w.Write(classical)
					} else {
						w.Write(pq)
					}
				} else if r.URL.Query().Has("chain_algorithm") {
					t.Error("chain selector leaked to HTTPS or TSA request")
				}
			}))
			defer srv.Close()
			challenge := &litepb.WendyComEnrollmentChallenge{UsbTrustSupported: true, MldsaVerificationSupported: tc.capability}
			profile := TrustProfileFromChallenge(challenge)
			cfg := &litepb.WendyConfEnrollment{DeviceId: "lite-test", TenantId: tenant, CsrUrl: srv.URL + "/csr", TimeUrl: DefaultTimeURL}
			if tc.explicitRoots {
				profile = profile.WithExistingDeviceIssuer()
				cfg.DeviceRoots = classical
			}
			if err := discoverTrustForProfile(context.Background(), defaultTSATestClient(t, srv, nil), cfg, srv.URL+"/cacerts", profile); err != nil {
				t.Fatal(err)
			}
			if discoveredAlgorithm != tc.wantAlgorithm || !bytes.Equal(cfg.DeviceRoots, tc.wantRoots) {
				t.Fatalf("discovered chain = %q, want %q with matching roots", discoveredAlgorithm, tc.wantAlgorithm)
			}
			if err := CheckTrustSupport(cfg, challenge); err != nil {
				t.Fatal(err)
			}
			cloud := &fakeEnrollment{reply: &cloudpb.EnrollDeviceResponse{CredentialKind: "enrollment_token", TokenValue: "test-token", AssetId: "42", ExpiresAt: time.Now().Add(time.Minute).Format(time.RFC3339)}}
			if _, err := Mint(context.Background(), cloud, auth, cfg, "lite-test", nil, profile); err != nil {
				t.Fatal(err)
			}
			parts := strings.Split(string(cloud.request.EnrollmentRequestJws), ".")
			payload, err := base64.RawURLEncoding.DecodeString(parts[1])
			if err != nil {
				t.Fatal(err)
			}
			var claims struct {
				ChainAlgorithm string `json:"chain_algorithm"`
			}
			if err := json.Unmarshal(payload, &claims); err != nil {
				t.Fatal(err)
			}
			if claims.ChainAlgorithm != discoveredAlgorithm {
				t.Fatalf("token requested chain %q, discovered %q", claims.ChainAlgorithm, discoveredAlgorithm)
			}
		})
	}
}

func TestExplicitRootsRetainFirmwareAlgorithmRestrictions(t *testing.T) {
	profile := TrustProfileFromChallenge(nil).WithExistingDeviceIssuer()
	if profile.chainAlgorithm() != "" {
		t.Fatal("explicit roots selected a different issuer")
	}
	if err := validateTrustBundleForProfile(testMLDSATrustPEM(t), profile); err == nil {
		t.Fatal("explicit roots bypassed firmware algorithm restrictions")
	}
}

func TestBuiltInRootsPreserveConfiguredIssuer(t *testing.T) {
	cfg := &litepb.WendyConfEnrollment{DeviceId: "lite-test", TenantId: tenant}
	cloud := &fakeEnrollment{reply: &cloudpb.EnrollDeviceResponse{CredentialKind: "enrollment_token", TokenValue: "test-token", AssetId: "42", ExpiresAt: time.Now().Add(time.Minute).Format(time.RFC3339)}}
	if _, err := Mint(context.Background(), cloud, authFixture(t), cfg, "lite-test", nil, TrustProfile{}); err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(string(cloud.request.EnrollmentRequestJws), ".")
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims map[string]json.RawMessage
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatal(err)
	}
	if _, selected := claims["chain_algorithm"]; selected {
		t.Fatal("selected a new authority without provisioning its roots")
	}
}

func TestDiscoveryRequiresP256ConfirmationBeforeMint(t *testing.T) {
	root := testTrustPEM(t, true, time.Now().Add(time.Hour))
	for _, headers := range [][]string{nil, {"ML_DSA_65"}, {"ECDSA_P256", "ECDSA_P256"}} {
		t.Run(strings.Join(headers, ","), func(t *testing.T) {
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Error("discovery continued after unconfirmed P-256 selection")
				}
				for _, value := range headers {
					w.Header().Add(chainAlgorithmHeader, value)
				}
				w.Write(root)
			}))
			defer srv.Close()
			cfg := &litepb.WendyConfEnrollment{CsrUrl: srv.URL + "/csr", TimeUrl: DefaultTimeURL}
			before := proto.Clone(cfg)
			if err := discoverTrust(context.Background(), srv.Client(), cfg, srv.URL+"/cacerts"); err == nil {
				t.Fatal("accepted an unconfirmed issuer selection")
			}
			if !proto.Equal(cfg, before) {
				t.Fatal("unconfirmed discovery published trust")
			}
			if _, err := discoverP256DeviceRoots(context.Background(), srv.Client(), srv.URL+"/cacerts"); err == nil {
				t.Fatal("relay trusted an unconfirmed P-256 authority")
			}
		})
	}
}

func TestDiscoveryRejectsConflictingChainSelection(t *testing.T) {
	client := &http.Client{Transport: discoveryRoundTripper(func(*http.Request) (*http.Response, error) {
		t.Fatal("conflicting chain selection reached the network")
		return nil, nil
	})}
	for _, query := range []string{"chain_algorithm=ML_DSA_65", "chain_algorithm=ECDSA_P256&chain_algorithm=ECDSA_P256", "chain_algorithm=", "broken=%zz"} {
		cfg := &litepb.WendyConfEnrollment{CsrUrl: "https://csr.example/v1/" + tenant, TimeUrl: DefaultTimeURL}
		before := proto.Clone(cfg)
		if err := discoverTrust(context.Background(), client, cfg, "https://est.example/cacerts?"+query); err == nil {
			t.Fatalf("accepted conflicting query %q", query)
		}
		if !proto.Equal(cfg, before) {
			t.Fatal("failed discovery changed enrollment configuration")
		}
	}
}
