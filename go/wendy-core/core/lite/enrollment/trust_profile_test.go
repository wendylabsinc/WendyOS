//go:build go1.27

package liteenroll

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/mldsa"
	"crypto/rand"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	litepb "github.com/wendylabsinc/WendyOS/go/wendy-core/core/lite/pb"
	"google.golang.org/protobuf/proto"
)

func TestTrustProfileNegotiatesMLDSAAlgorithms(t *testing.T) {
	ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	for _, params := range []mldsa.Parameters{mldsa.MLDSA44(), mldsa.MLDSA65(), mldsa.MLDSA87()} {
		key, err := mldsa.GenerateKey(params)
		if err != nil {
			t.Fatal(err)
		}
		for name, root := range map[string][]byte{
			"self-signed": testAlgorithmTrustPEM(t, key.Public(), key, x509.UnknownSignatureAlgorithm),
			"EC issuer":   testAlgorithmTrustPEM(t, key.Public(), ec, x509.UnknownSignatureAlgorithm),
			"EC key":      testAlgorithmTrustPEM(t, ec.Public(), key, x509.UnknownSignatureAlgorithm),
		} {
			t.Run(params.String()+"/"+name, func(t *testing.T) {
				for _, tc := range []struct {
					name      string
					challenge *litepb.WendyComEnrollmentChallenge
					wantOK    bool
				}{
					{"no challenge", nil, false},
					{"legacy", &litepb.WendyComEnrollmentChallenge{}, false},
					{"disabled", &litepb.WendyComEnrollmentChallenge{MldsaVerificationSupported: proto.Bool(false)}, false},
					{"enabled", &litepb.WendyComEnrollmentChallenge{MldsaVerificationSupported: proto.Bool(true)}, true},
				} {
					t.Run(tc.name, func(t *testing.T) {
						err := validateTrustBundleForProfile(root, testTrustProfile(tc.challenge))
						if (err == nil) != tc.wantOK {
							t.Fatalf("compatibility result: %v", err)
						}
					})
				}
			})
		}
	}
}

func TestMLDSAProfileStillValidatesTrust(t *testing.T) {
	_, ed, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	profile := testTrustProfile(&litepb.WendyComEnrollmentChallenge{MldsaVerificationSupported: proto.Bool(true)})
	valid := testTrustPEM(t, true, time.Now().Add(time.Hour))
	for name, data := range map[string][]byte{
		"empty":        nil,
		"malformed":    []byte("-----BEGIN CERTIFICATE-----\nbad\n-----END CERTIFICATE-----"),
		"leaf":         testTrustPEM(t, false, time.Now().Add(time.Hour)),
		"expired":      testTrustPEM(t, true, time.Now().Add(-time.Minute)),
		"Ed25519":      testAlgorithmTrustPEM(t, ed.Public(), ed, x509.PureEd25519),
		"too many CAs": bytes.Repeat(valid, 9),
		"trailing":     append(bytes.Clone(valid), 'x'),
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateTrustBundleForProfile(data, profile); err == nil {
				t.Fatal("ML-DSA capability bypassed trust validation")
			}
		})
	}
	if err := validateTrustBundleForProfile(valid, profile); err != nil {
		t.Fatal("ML-DSA capability rejected a classical CA", err)
	}
}

func TestProvisionTrustUsesNegotiatedProfileForEveryBundle(t *testing.T) {
	classical, pq := testTrustPEM(t, true, time.Now().Add(time.Hour)), testMLDSATrustPEM(t)
	classicalPath, pqPath := filepath.Join(t.TempDir(), "classical.pem"), filepath.Join(t.TempDir(), "pq.pem")
	for path, data := range map[string][]byte{classicalPath: classical, pqPath: pq} {
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	for i, name := range []string{"device", "TSA", "HTTPS"} {
		t.Run(name, func(t *testing.T) {
			paths := []string{classicalPath, classicalPath, classicalPath}
			paths[i] = pqPath
			for _, enabled := range []bool{false, true} {
				cfg := &litepb.WendyConfEnrollment{TimeUrl: "https://tsa.example/time"}
				before := proto.Clone(cfg)
				challenge := &litepb.WendyComEnrollmentChallenge{UsbTrustSupported: true, MldsaVerificationSupported: proto.Bool(enabled)}
				client, err := ProvisionTrustForProfile(cfg, paths[0], paths[1], paths[2], testTrustProfile(challenge))
				if !enabled {
					if err == nil || client != nil || !proto.Equal(cfg, before) {
						t.Fatal("disabled verifier accepted ML-DSA or changed configuration")
					}
					continue
				}
				if err != nil {
					t.Fatal(err)
				}
				client.CloseIdleConnections()
				if !cfg.ProvisionTrust || !bytes.Equal([][]byte{cfg.DeviceRoots, cfg.TsaRoots, cfg.HttpsRoots}[i], pq) {
					t.Fatal("enabled verifier did not provision the selected ML-DSA bundle")
				}
				if err := checkTrustSupport(cfg, challenge); err != nil {
					t.Fatal(err)
				}
				challenge.MldsaVerificationSupported = nil
				if err := checkTrustSupport(cfg, challenge); err == nil || !strings.Contains(err.Error(), "no cloud asset was reserved") {
					t.Fatalf("fresh legacy challenge did not reject previously accepted ML-DSA before mint: %v", err)
				}
			}
		})
	}
}

func TestDiscoverTrustUsesNegotiatedProfile(t *testing.T) {
	root := testMLDSATrustPEM(t)
	for _, capability := range []*bool{nil, proto.Bool(false), proto.Bool(true)} {
		name := "legacy"
		if capability != nil {
			name = "disabled"
			if *capability {
				name = "enabled"
			}
		}
		t.Run(name, func(t *testing.T) {
			requests := 0
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if r.Method == http.MethodGet {
					w.Write(root)
				}
			}))
			defer srv.Close()
			cfg := &litepb.WendyConfEnrollment{CsrUrl: srv.URL + "/csr", TimeUrl: DefaultTimeURL}
			before := proto.Clone(cfg)
			challenge := &litepb.WendyComEnrollmentChallenge{UsbTrustSupported: true, MldsaVerificationSupported: capability}
			err := discoverTrustForProfile(context.Background(), defaultTSATestClient(t, srv, nil), cfg, srv.URL+"/cacerts", testTrustProfile(challenge))
			if capability == nil || !*capability {
				if err == nil || requests != 1 || !proto.Equal(cfg, before) {
					t.Fatal("legacy/disabled firmware received incompatible discovered configuration")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if requests != 3 || !cfg.ProvisionTrust || !bytes.Equal(cfg.DeviceRoots, root) {
				t.Fatal("enabled verifier did not receive verified discovery roots")
			}
			if err := checkTrustSupport(cfg, challenge); err != nil {
				t.Fatal(err)
			}
		})
	}
}
