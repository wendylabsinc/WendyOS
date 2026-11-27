package jws

import (
	"bytes"
	"crypto/mldsa"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/production/internal/testidentity"
	"github.com/wendylabsinc/wendy/go/world/production/wendycert"
)

var fixtureTime = time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

const testType = "wendy.world.test.v1"

func verifier(t *testing.T, f *testidentity.Fixture) *wendycert.Verifier {
	t.Helper()
	v, e := wendycert.New(wendycert.Config{Tenant: f.Tenant, Roots: [][]byte{f.RootDER}})
	if e != nil {
		t.Fatal(e)
	}
	return v
}
func rawProof(t *testing.T, f *testidentity.Fixture, protected, payload []byte) string {
	t.Helper()
	input := base64.RawURLEncoding.EncodeToString(protected) + "." + base64.RawURLEncoding.EncodeToString(payload)
	sig, e := f.Key.Sign(nil, []byte(input), &mldsa.Options{})
	if e != nil {
		t.Fatal(e)
	}
	return input + "." + base64.RawURLEncoding.EncodeToString(sig)
}
func protected(t *testing.T, f *testidentity.Fixture) []byte {
	t.Helper()
	chain, e := encodeChain(f.ChainDER)
	if e != nil {
		t.Fatal(e)
	}
	b, e := json.Marshal(header{Algorithm: Algorithm, Type: testType, Chain: chain})
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func TestActualMLDSA65PreservesReceivedPayloadBytes(t *testing.T) {
	f := testidentity.New(t, fixtureTime)
	v := verifier(t, f)
	payload := []byte("{ \"message\" : \"hello <&> 東京\", \"n\" : 1 }\n")
	token, e := Sign(testType, payload, f.ChainDER, f.Key)
	if e != nil {
		t.Fatal(e)
	}
	got, id, e := Verify(token, testType, v, fixtureTime, fixtureTime.Add(time.Second))
	if e != nil {
		t.Fatal(e)
	}
	if !bytes.Equal(got, payload) || id.Principal != f.Principal || id.Tenant != f.Tenant || len(id.SigningKeySHA256) != 64 || len(id.CertificateSHA256) != 64 {
		t.Fatal("wrong received bytes or verified identity")
	}
	parts := strings.Split(token, ".")
	sig, e := base64.RawURLEncoding.DecodeString(parts[2])
	if e != nil {
		t.Fatal(e)
	}
	if e = mldsa.Verify(f.Key.PublicKey(), []byte(parts[0]+"."+parts[1]), sig, nil); e != nil {
		t.Fatal("signature is not raw empty-context MLDSA65", e)
	}
	if mldsa.Verify(f.Key.PublicKey(), []byte(parts[0]+"."+parts[1]), sig, &mldsa.Options{Context: "other"}) == nil {
		t.Fatal("context domain unexpectedly interchangeable")
	}
}
func TestStrictHeaderRejectsAlgorithmRemoteKeysAndAliases(t *testing.T) {
	f := testidentity.New(t, fixtureTime)
	v := verifier(t, f)
	base := protected(t, f)
	cases := map[string][]byte{
		"cloud-type":        bytes.Replace(base, []byte(testType), []byte("wendy.cloud.request.v1"), 1),
		"release-type":      bytes.Replace(base, []byte(testType), []byte("wendy.release.v1"), 1),
		"alg-none":          bytes.Replace(base, []byte(Algorithm), []byte("none"), 1),
		"lab-eddsa":         bytes.Replace(base, []byte(Algorithm), []byte("EdDSA"), 1),
		"uppercase-alias":   bytes.Replace(base, []byte(`"alg"`), []byte(`"ALG"`), 1),
		"duplicate-alg":     append([]byte(`{"alg":"ML-DSA-65",`), base[1:]...),
		"escaped-duplicate": append([]byte(`{"\u0061lg":"ML-DSA-65",`), base[1:]...),
		"kid":               append([]byte(`{"kid":"https://example.invalid/key",`), base[1:]...),
		"jku":               append([]byte(`{"jku":"https://example.invalid/key",`), base[1:]...),
		"critical":          append([]byte(`{"crit":[],`), base[1:]...),
		"unencoded-payload": append([]byte(`{"b64":false,`), base[1:]...),
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			token := rawProof(t, f, h, []byte(`{"value":1}`))
			if _, _, e := Verify(token, testType, v, fixtureTime, fixtureTime); e == nil {
				t.Fatal("invalid protected header accepted")
			}
		})
	}
}
func TestTamperingAndCanonicalEncoding(t *testing.T) {
	f := testidentity.New(t, fixtureTime)
	v := verifier(t, f)
	token, e := Sign(testType, []byte(`{"value":1}`), f.ChainDER, f.Key)
	if e != nil {
		t.Fatal(e)
	}
	parts := strings.Split(token, ".")
	mutations := []string{parts[0] + "." + base64.RawURLEncoding.EncodeToString([]byte(`{"value":2}`)) + "." + parts[2], parts[0] + "." + parts[1] + "." + parts[2][:len(parts[2])-1], parts[0] + "=." + parts[1] + "." + parts[2], parts[0] + ".\n" + parts[1] + "." + parts[2], token + ".", strings.Repeat("x", MaxProofBytes+1)}
	for _, bad := range mutations {
		if _, _, e := Verify(bad, testType, v, fixtureTime, fixtureTime); e == nil {
			t.Fatal("modified/noncanonical compact proof accepted")
		}
	}
	other := testidentity.New(t, fixtureTime)
	if _, _, e = Verify(token, testType, verifier(t, other), fixtureTime, fixtureTime); e == nil {
		t.Fatal("unconfigured chain root accepted")
	}
	for _, bounds := range [][2]time.Time{{{}, fixtureTime}, {fixtureTime, {}}, {fixtureTime, fixtureTime.Add(-time.Second)}, {f.NotBefore.Add(-time.Nanosecond), fixtureTime}, {fixtureTime, f.NotAfter.Add(time.Nanosecond)}} {
		if _, _, e = Verify(token, testType, v, bounds[0], bounds[1]); e == nil {
			t.Fatal("invalid time interval accepted")
		}
	}
	if _, _, e = Verify(token, testType, nil, fixtureTime, fixtureTime); e == nil {
		t.Fatal("nil trust accepted")
	}
}
func TestCertificateAndPayloadBounds(t *testing.T) {
	f := testidentity.New(t, fixtureTime)
	v := verifier(t, f)
	for _, chain := range [][][]byte{nil, {f.ChainDER[0], f.ChainDER[0]}, make([][]byte, MaxCertificates+1), {make([]byte, MaxCertificateBytes+1)}} {
		if _, e := Sign(testType, []byte(`{}`), chain, f.Key); e == nil {
			t.Fatal("bad signer certificate bound accepted")
		}
	}
	oversizedHeader, _ := json.Marshal(header{Algorithm: Algorithm, Type: testType, Chain: []string{base64.StdEncoding.EncodeToString(make([]byte, MaxCertificateBytes+1))}})
	if _, _, e := Verify(rawProof(t, f, oversizedHeader, []byte(`{}`)), testType, v, fixtureTime, fixtureTime); e == nil {
		t.Fatal("oversized decoded certificates accepted")
	}
	h := protected(t, f)
	var parsed header
	_ = json.Unmarshal(h, &parsed)
	parsed.Chain = append(parsed.Chain, parsed.Chain[0])
	h, _ = json.Marshal(parsed)
	if _, _, e := Verify(rawProof(t, f, h, []byte(`{}`)), testType, v, fixtureTime, fixtureTime); e == nil {
		t.Fatal("duplicate presented certificate accepted")
	}
	for _, payload := range [][]byte{[]byte(`{"n":1,"n":2}`), []byte(`{"nested":{"x":1,"x":2}}`), []byte(`null`), []byte(`{} {}`), []byte(`{"bad":"\ud800"}`), []byte(`{"bad":"\udfff"}`), append([]byte(`{"bad":"`), 0xff, '"', '}'), []byte(`{"x":"` + strings.Repeat("a", MaxPayloadBytes) + `"}`)} {
		if _, e := Sign(testType, payload, f.ChainDER, f.Key); e == nil {
			t.Fatal("invalid payload accepted by signer")
		}
		if _, _, e := Verify(rawProof(t, f, protected(t, f), payload), testType, v, fixtureTime, fixtureTime); e == nil {
			t.Fatal("invalid payload accepted by verifier")
		}
	}
	other, _ := mldsa.GenerateKey(mldsa.MLDSA65())
	if _, e := Sign(testType, []byte(`{}`), f.ChainDER, other); e == nil {
		t.Fatal("wrong leaf key accepted")
	}
	wrongAlgorithm, _ := mldsa.GenerateKey(mldsa.MLDSA44())
	if _, e := Sign(testType, []byte(`{}`), f.ChainDER, wrongAlgorithm); e == nil {
		t.Fatal("wrong MLDSA parameters accepted")
	}
}
func TestDecodeStrictExactFieldNamesUnicodeAndDepth(t *testing.T) {
	type nested struct {
		Name string `json:"name"`
	}
	type document struct {
		Child nested   `json:"child"`
		Items []nested `json:"items"`
	}
	for _, input := range []string{`{"child":{"Name":"x"}}`, `{"Child":{"name":"x"}}`, `{"items":[{"NAME":"x"}]}`, `{"child":{"name":"x","\u006eame":"y"}}`, `{"child":null}`, `{"child":{"name":"\ud800"}}`, `{"child":{"name":"x"},"extra":1}`} {
		var d document
		if DecodeStrict([]byte(input), &d) == nil {
			t.Fatalf("accepted %s", input)
		}
	}
	var d document
	if e := DecodeStrict([]byte(`{"child":{"name":"\ud83d\udccd 東京"},"items":[]}`), &d); e != nil || d.Child.Name != "📍 東京" {
		t.Fatal("valid Unicode rejected", e)
	}
	nestedJSON := `{"x":` + strings.Repeat(`[`, 34) + `0` + strings.Repeat(`]`, 34) + `}`
	var anyValue map[string]any
	if DecodeStrict([]byte(nestedJSON), &anyValue) == nil {
		t.Fatal("overdeep JSON accepted")
	}
}
