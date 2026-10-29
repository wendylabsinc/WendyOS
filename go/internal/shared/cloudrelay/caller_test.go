package cloudrelay

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudflare/circl/sign/mldsa/mldsa65"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/relaypb"
	"google.golang.org/protobuf/proto"
)

func TestPrincipalSignerMLDSA(t *testing.T) {
	scheme := mldsa65.Scheme()
	seed := make([]byte, scheme.SeedSize())
	_, _ = rand.Read(seed)
	pub, _ := scheme.DeriveKey(seed)
	pubBytes, _ := pub.MarshalBinary()
	oid := asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 3, 18}
	algorithm := pkix.AlgorithmIdentifier{Algorithm: oid}
	// A parseable leaf carrying the request-signing key. Trust validation belongs
	// to pki-core; this test exercises the producer's algorithm/key binding.
	name := pkix.Name{CommonName: "operator"}.ToRDNSequence()
	tbs := struct {
		Version   int `asn1:"explicit,tag:0"`
		Serial    *big.Int
		Signature pkix.AlgorithmIdentifier
		Issuer    pkix.RDNSequence
		Validity  struct{ NotBefore, NotAfter time.Time }
		Subject   pkix.RDNSequence
		SPKI      struct {
			Algorithm pkix.AlgorithmIdentifier
			PublicKey asn1.BitString
		}
	}{Version: 2, Serial: big.NewInt(1), Signature: algorithm, Issuer: name, Subject: name}
	tbs.Validity.NotBefore = time.Now().Add(-time.Hour)
	tbs.Validity.NotAfter = time.Now().Add(time.Hour)
	tbs.SPKI.Algorithm = algorithm
	tbs.SPKI.PublicKey = asn1.BitString{Bytes: pubBytes, BitLength: len(pubBytes) * 8}
	rawTBS, e := asn1.Marshal(tbs)
	if e != nil {
		t.Fatal(e)
	}
	cert, e := asn1.Marshal(struct {
		TBS       asn1.RawValue
		Algorithm pkix.AlgorithmIdentifier
		Signature asn1.BitString
	}{asn1.RawValue{FullBytes: rawTBS}, algorithm, asn1.BitString{Bytes: []byte{1}, BitLength: 8}})
	if e != nil {
		t.Fatal(e)
	}
	seedDER, _ := asn1.Marshal(asn1.RawValue{Class: 2, Tag: 0, Bytes: seed})
	keyDER, e := asn1.Marshal(struct {
		Version    int
		Algorithm  pkix.AlgorithmIdentifier
		PrivateKey []byte
	}{0, algorithm, seedDER})
	if e != nil {
		t.Fatal(e)
	}
	signer, e := PrincipalSigner(string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert})), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
	if e != nil {
		t.Fatal(e)
	}
	payload := []byte(`{"operation":"create_tunnel"}`)
	compact, e := signer(payload)
	if e != nil {
		t.Fatal(e)
	}
	parts := strings.Split(string(compact), ".")
	h, _ := decode64(parts[0])
	var header map[string]json.RawMessage
	if json.Unmarshal(h, &header) != nil || len(header) != 3 || string(header["alg"]) != `"ML-DSA-65"` {
		t.Fatal("wrong request-signing header")
	}
	signature, _ := decode64(parts[2])
	if !scheme.Verify(pub, []byte(parts[0]+"."+parts[1]), signature, nil) {
		t.Fatal("invalid ML-DSA request signature")
	}
}
func TestPrincipalSignerRefusesECDSADowngrade(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, e := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if e != nil {
		t.Fatal(e)
	}
	raw, _ := x509.MarshalPKCS8PrivateKey(key)
	_, e = PrincipalSigner(string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: raw}))
	if e == nil || !strings.Contains(e.Error(), "ML-DSA operator request-signing certificate") {
		t.Fatalf("unexpected downgrade result: %v", e)
	}
}
func TestDeviceEndpoints(t *testing.T) {
	for _, tt := range []struct{ cloud, override, want string }{
		{"api.dev.wendy.sh:443", "", "devices.dev.wendy.sh:443"},
		{"https://api.wendy.sh", "", "devices.wendy.sh:443"},
		{"api.wendy.dev:443", "", "devices.wendy.dev:443"},
		{"https://api.wendy.dev", "", "devices.wendy.dev:443"},
		{"https://api.wendy.dev:9443", "", "devices.wendy.dev:9443"},
		{"custom.example:443", "", "custom.example:443"},
		{"localhost:8501", "localhost:9443", "localhost:9443"},
		{"api.dev.wendy.sh:443", "https://custom.example:9443", "custom.example:9443"},
		{"api.wendy.dev:443", "https://custom.example:9443", "custom.example:9443"},
	} {
		got, e := DeviceEndpoint(tt.cloud, tt.override)
		if e != nil || got != tt.want {
			t.Fatalf("endpoint(%q,%q)=%q,%v", tt.cloud, tt.override, got, e)
		}
	}
	for _, s := range []string{"http://example.com", "https://user:pass@example.com", "unix:///tmp/sock", "https://example.com/path"} {
		if _, e := DeviceEndpoint("api.dev.wendy.sh", s); e == nil {
			t.Fatalf("accepted unsafe target %q", s)
		}
	}
	for _, host := range []string{"api.dev.wendy.sh", "api.wendy.dev"} {
		if got, e := Issuer(host+":443", ""); e != nil || got != "https://"+host {
			t.Fatalf("issuer(%q)=%s,%v", host, got, e)
		}
	}
}

// Dev Cloud also answers on the mirror name api.dev.wendy.dev, but it has one
// device listener and signs with one issuer. A login or enrollment through the
// mirror must derive exactly what the primary host derives (WDY-3939 #20).
func TestDevMirrorAPIHostDerivesPrimaryStack(t *testing.T) {
	for _, tt := range []struct{ cloud, endpoint, issuer string }{
		{"api.dev.wendy.dev", "devices.dev.wendy.sh:443", "https://api.dev.wendy.sh"},
		{"api.dev.wendy.dev:443", "devices.dev.wendy.sh:443", "https://api.dev.wendy.sh"},
		{"https://api.dev.wendy.dev", "devices.dev.wendy.sh:443", "https://api.dev.wendy.sh"},
		{"https://api.dev.wendy.dev:443/", "devices.dev.wendy.sh:443", "https://api.dev.wendy.sh"},
		{"api.dev.wendy.dev:9443", "devices.dev.wendy.sh:9443", "https://api.dev.wendy.sh:9443"},
		{"api.dev.wendy.sh", "devices.dev.wendy.sh:443", "https://api.dev.wendy.sh"},
		{"api.dev.wendy.sh:443", "devices.dev.wendy.sh:443", "https://api.dev.wendy.sh"},
		{"api.dev.wendy.sh:9443", "devices.dev.wendy.sh:9443", "https://api.dev.wendy.sh:9443"},
		// Unchanged: prod, the device listeners themselves, and other Clouds.
		{"api.wendy.dev:443", "devices.wendy.dev:443", "https://api.wendy.dev"},
		{"api.wendy.sh:443", "devices.wendy.sh:443", "https://api.wendy.sh"},
		{"devices.dev.wendy.dev:443", "devices.dev.wendy.dev:443", "https://devices.dev.wendy.dev"},
		{"dev.wendy.dev:443", "dev.wendy.dev:443", "https://dev.wendy.dev"},
		{"custom.example:8443", "custom.example:8443", "https://custom.example:8443"},
		{"localhost:8501", "localhost:8501", "https://localhost:8501"},
	} {
		if got, e := DeviceEndpoint(tt.cloud, ""); e != nil || got != tt.endpoint {
			t.Errorf("DeviceEndpoint(%q) = %q, %v; want %q", tt.cloud, got, e, tt.endpoint)
		}
		if got, e := Issuer(tt.cloud, ""); e != nil || got != tt.issuer {
			t.Errorf("Issuer(%q) = %q, %v; want %q", tt.cloud, got, e, tt.issuer)
		}
	}
	// Explicit device overrides still win over the mirror mapping.
	if got, e := DeviceEndpoint("api.dev.wendy.dev:443", "https://custom.example:9443"); e != nil || got != "custom.example:9443" {
		t.Errorf("DeviceEndpoint override = %q, %v", got, e)
	}
	if got, e := Issuer("api.dev.wendy.dev:443", "https://issuer.example/"); e != nil || got != "https://issuer.example" {
		t.Errorf("Issuer override = %q, %v", got, e)
	}
}
func TestRelayKeysPersistDistinctAndPrivate(t *testing.T) {
	dir := t.TempDir()
	a := &Agent{StateDir: dir}
	if e := a.keys(); e != nil {
		t.Fatal(e)
	}
	if string(publicDER(a.signing)) == string(publicDER(a.encryption)) {
		t.Fatal("reused key pair")
	}
	b := &Agent{StateDir: dir}
	if e := b.keys(); e != nil {
		t.Fatal(e)
	}
	if string(publicDER(a.signing)) != string(publicDER(b.signing)) || string(publicDER(a.encryption)) != string(publicDER(b.encryption)) {
		t.Fatal("keys changed on restart")
	}
	info, e := os.Stat(filepath.Join(dir, "keys.json"))
	if e != nil || info.Mode().Perm() != 0600 {
		t.Fatal("private relay state permissions")
	}
}
func TestLeaseReuseChecksKeyBinding(t *testing.T) {
	v := loadVectors(t)
	a := &Agent{Verifier: fixtureVerifier(t, v), signing: scalarKey(t, v.Keys["agent_signing"].Scalar), encryption: scalarKey(t, v.Keys["agent_key_agreement"].Scalar)}
	// Expired conformance leases must never be used after a process restart.
	l := &pb.IssuePresenceLeaseResponse{PresenceLeaseJws: v.Artifacts.Presence.JWS, Broker: &pb.BrokerInstance{Audience: v.Artifacts.Presence.Claims.Aud, Endpoint: "relay.example:443"}}
	if _, e := a.validateLease(context.Background(), l); e == nil {
		t.Fatal("reused expired persisted lease")
	}
	copied := proto.Clone(l).(*pb.IssuePresenceLeaseResponse)
	copied.Broker.Endpoint = "http://relay.example"
	if _, e := a.validateLease(context.Background(), copied); e == nil {
		t.Fatal("accepted non-TLS broker")
	}
}

func TestLitePrincipalSignerP256(t *testing.T) {
	for _, curve := range []elliptic.Curve{elliptic.P256(), elliptic.P384()} {
		key, err := ecdsa.GenerateKey(curve, rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		template := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
		der, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		certificate := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
		signer, err := LitePrincipalSigner(certificate, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: raw}))
		if curve != elliptic.P256() {
			if err == nil {
				t.Fatal("accepted P384")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		jws, err := signer([]byte("descriptor"))
		if err != nil {
			t.Fatal(err)
		}
		parts := strings.Split(string(jws), ".")
		if len(parts) != 3 {
			t.Fatal("bad JWS")
		}
		signature, err := base64.RawURLEncoding.DecodeString(parts[2])
		if err != nil || len(signature) != 64 {
			t.Fatal("bad signature")
		}
		digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
		if !ecdsa.Verify(&key.PublicKey, digest[:], new(big.Int).SetBytes(signature[:32]), new(big.Int).SetBytes(signature[32:])) {
			t.Fatal("signature failed")
		}
		header, err := base64.RawURLEncoding.DecodeString(parts[0])
		if err != nil {
			t.Fatal(err)
		}
		var h map[string]any
		if err = json.Unmarshal(header, &h); err != nil || h["alg"] != "ES256" {
			t.Fatal("wrong algorithm")
		}
		wrong, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		raw, _ = x509.MarshalPKCS8PrivateKey(wrong)
		if _, err = LitePrincipalSigner(certificate, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: raw})); err == nil {
			t.Fatal("accepted mismatched key")
		}
	}
}

func TestLitePrincipalRequestBindsOperatorKey(t *testing.T) {
	const assetID = "7ebe087d-d2c2-49fd-9e3b-421a11b1a070"
	identity, leaf, key := liteRequestIdentity(t, elliptic.P256())
	signer, err := LitePrincipalSigner(identity.PemCertificate, []byte(identity.PemPrivateKey))
	if err != nil {
		t.Fatal(err)
	}
	request, err := LitePrincipalRequest(assetID, identity, signer)
	if err != nil {
		t.Fatal(err)
	}
	body := request.RequestBody
	if body.TargetAssetId != assetID || body.Service != "wendycom" || len(body.RequestId) != 16 {
		t.Fatalf("wrong Lite request body: %v", body)
	}
	if !bytes.Equal(body.CallerSigningPublicKeySpkiDer, leaf.RawSubjectPublicKeyInfo) {
		t.Fatal("Lite caller key differs from the operator's mTLS key")
	}
	if request.PrincipalRequest.Kind != "tunnel-principal-request+jws" {
		t.Fatal("wrong Lite principal artifact kind")
	}
	parts := strings.Split(string(request.PrincipalRequest.Value), ".")
	if len(parts) != 3 {
		t.Fatal("invalid compact JWS")
	}
	headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatal(err)
	}
	var header struct {
		Algorithm string   `json:"alg"`
		Type      string   `json:"typ"`
		Chain     [][]byte `json:"x5c"`
	}
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		t.Fatal(err)
	}
	if header.Algorithm != "ES256" || header.Type != "tunnel-principal-request+jws" || len(header.Chain) != 1 || !bytes.Equal(header.Chain[0], leaf.Raw) {
		t.Fatal("Lite proof does not identify the selected operator leaf")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var descriptor struct {
		Operation string `json:"operation"`
		Audience  string `json:"aud"`
		BodyHash  string `json:"body_sha256"`
		Target    struct {
			AssetID string `json:"asset_id"`
			Service string `json:"service"`
		} `json:"target"`
	}
	if err := json.Unmarshal(payload, &descriptor); err != nil {
		t.Fatal(err)
	}
	rawBody, err := proto.MarshalOptions{Deterministic: true}.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if descriptor.Operation != "create_lite_tunnel" || descriptor.Audience != "pki-core/tunnel-attestation" || descriptor.BodyHash != binding(rawBody) || descriptor.Target.AssetID != assetID || descriptor.Target.Service != "wendycom" {
		t.Fatalf("wrong Lite descriptor: %s", payload)
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(signature) != 64 {
		t.Fatal("invalid ES256 signature encoding")
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(&key.PublicKey, digest[:], new(big.Int).SetBytes(signature[:32]), new(big.Int).SetBytes(signature[32:])) {
		t.Fatal("Lite request signature does not verify with the selected leaf")
	}
	second, err := LitePrincipalRequest(assetID, identity, signer)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(second.RequestBody.CallerSigningPublicKeySpkiDer, body.CallerSigningPublicKeySpkiDer) || bytes.Equal(second.RequestBody.RequestId, body.RequestId) {
		t.Fatal("Lite must retain the operator key and generate a fresh request ID")
	}
}

func TestLitePrincipalRequestRejectsInvalidInputs(t *testing.T) {
	const assetID = "7ebe087d-d2c2-49fd-9e3b-421a11b1a070"
	identity, _, _ := liteRequestIdentity(t, elliptic.P256())
	wrongCurve, _, _ := liteRequestIdentity(t, elliptic.P384())
	sign := func(payload []byte) ([]byte, error) { return payload, nil }
	for _, tc := range []struct {
		name     string
		assetID  string
		identity config.CertificateInfo
		sign     func([]byte) ([]byte, error)
	}{
		{"missing signer", assetID, identity, nil},
		{"invalid asset", "not-a-uuid", identity, sign},
		{"missing certificate", assetID, config.CertificateInfo{}, sign},
		{"multiple certificates", assetID, config.CertificateInfo{PemCertificate: identity.PemCertificate + identity.PemCertificate}, sign},
		{"wrong curve", assetID, wrongCurve, sign},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := LitePrincipalRequest(tc.assetID, tc.identity, tc.sign); err == nil {
				t.Fatal("invalid Lite request input accepted")
			}
		})
	}
}

func TestPrincipalRequestKeepsTunnelV2EphemeralKey(t *testing.T) {
	const assetID = "7ebe087d-d2c2-49fd-9e3b-421a11b1a070"
	sign := func(payload []byte) ([]byte, error) { return payload, nil }
	request, key, descriptor, err := PrincipalRequest(assetID, "wendy-agent", sign)
	if err != nil {
		t.Fatal(err)
	}
	var claims map[string]any
	if err := json.Unmarshal(descriptor, &claims); err != nil {
		t.Fatal(err)
	}
	if claims["operation"] != "create_tunnel" || request.RequestBody.Service != "wendy-agent" || !bytes.Equal(request.RequestBody.CallerSigningPublicKeySpkiDer, publicDER(key)) {
		t.Fatal("tunnel-v2 request profile or caller key changed")
	}
	rawBody, err := proto.MarshalOptions{Deterministic: true}.Marshal(request.RequestBody)
	if err != nil {
		t.Fatal(err)
	}
	if claims["body_sha256"] != binding(rawBody) || !bytes.Equal(request.PrincipalRequest.Value, descriptor) {
		t.Fatal("tunnel-v2 proof no longer binds its request body")
	}
	_, secondKey, _, err := PrincipalRequest(assetID, "wendy-agent", sign)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(publicDER(key), publicDER(secondKey)) {
		t.Fatal("tunnel-v2 must generate a fresh caller key")
	}
}

func liteRequestIdentity(t *testing.T, curve elliptic.Curve) (config.CertificateInfo, *x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(curve, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		ExtKeyUsage:        []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		UnknownExtKeyUsage: []asn1.ObjectIdentifier{{1, 3, 6, 1, 4, 1, 65441, 1, 9}},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return config.CertificateInfo{
		PemCertificate: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		PemPrivateKey:  string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})),
	}, leaf, key
}
