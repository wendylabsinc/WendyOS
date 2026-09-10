package cloudrelay

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudflare/circl/sign/mldsa/mldsa65"
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
	for _, tt := range []struct{ cloud, override, want string }{{"api.dev.wendy.sh:443", "", "devices.dev.wendy.sh:443"}, {"https://api.wendy.sh", "", "devices.wendy.sh:443"}, {"localhost:8501", "localhost:9443", "localhost:9443"}, {"api.dev.wendy.sh:443", "https://custom.example:9443", "custom.example:9443"}} {
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
	if got, e := Issuer("api.dev.wendy.sh:443", ""); e != nil || got != "https://api.dev.wendy.sh" {
		t.Fatalf("issuer=%s,%v", got, e)
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
