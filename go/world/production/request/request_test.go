package request

import (
	"bytes"
	"crypto/mldsa"
	"encoding/base64"
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/production/internal/testidentity"
	"github.com/wendylabsinc/wendy/go/world/production/jws"
	"github.com/wendylabsinc/wendy/go/world/production/wendycert"
)

var now = time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

func setup(t *testing.T) (*testidentity.Fixture, *wendycert.Verifier, Descriptor, Expected, []byte) {
	t.Helper()
	f := testidentity.New(t, now)
	v, e := wendycert.New(wendycert.Config{Tenant: f.Tenant, Roots: [][]byte{f.RootDER}})
	if e != nil {
		t.Fatal(e)
	}
	body := []byte(`{"kind":"asset","limit":20}`)
	digest, e := BodyDigest(body)
	if e != nil {
		t.Fatal(e)
	}
	d := Descriptor{Version: 1, Domain: "physical", Tenant: f.Tenant, Site: "commissioned-office", Audience: "wendy-world/gateway-1", Principal: f.Principal, Operation: OperationRead, Method: Method, Path: Path, BodySHA256: digest, PolicyRevision: 7, IssuedAtUnixMS: now.UnixMilli(), ExpiresAtUnixMS: now.Add(30 * time.Second).UnixMilli(), Nonce: base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0xa5}, 24))}
	expected := Expected{Tenant: d.Tenant, Site: d.Site, Audience: d.Audience, Operation: d.Operation, Method: d.Method, Path: d.Path, PolicyRevision: d.PolicyRevision}
	return f, v, d, expected, body
}
func signedUnchecked(t *testing.T, f *testidentity.Fixture, payload any) string {
	t.Helper()
	b, e := json.Marshal(payload)
	if e != nil {
		t.Fatal(e)
	}
	token, e := jws.Sign(Type, b, f.ChainDER, f.Key)
	if e != nil {
		t.Fatal(e)
	}
	return token
}
func TestBoundReadProofWithExplicitTrustedTime(t *testing.T) {
	f, v, d, e, body := setup(t)
	token, err := Sign(d, body, f.ChainDER, f.Key)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := Verify(token, e, body, v, now.Add(time.Second), now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if proof.Descriptor != d || proof.Identity.Principal != f.Principal || proof.Identity.Tenant != f.Tenant {
		t.Fatal("request or identity changed")
	}
	// Verification itself does not consume a nonce or claim application admission.
	if _, err = Verify(token, e, body, v, now.Add(time.Second), now.Add(2*time.Second)); err != nil {
		t.Fatal("request layer unexpectedly performs replay admission", err)
	}
}
func TestExpectedTargetAndBodyBinding(t *testing.T) {
	f, v, d, expected, body := setup(t)
	token, err := Sign(d, body, f.ChainDER, f.Key)
	if err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*Expected){
		"tenant": func(e *Expected) { e.Tenant = "other-tenant" }, "site": func(e *Expected) { e.Site = "other-site" }, "audience": func(e *Expected) { e.Audience = "other-gateway" }, "policy": func(e *Expected) { e.PolicyRevision++ }, "method": func(e *Expected) { e.Method = "GET" }, "query": func(e *Expected) { e.Path = Path + "?scope=all" }, "encoded-path": func(e *Expected) { e.Path = "/v1/world/%71uery" }, "operation": func(e *Expected) { e.Operation = "world.move" },
	} {
		t.Run(name, func(t *testing.T) {
			e := expected
			change(&e)
			if _, err := Verify(token, e, body, v, now, now); err == nil {
				t.Fatal("mismatched target accepted")
			}
		})
	}
	for _, b := range [][]byte{append(append([]byte(nil), body...), ' '), nil, bytes.Repeat([]byte{'x'}, MaxBodyBytes+1)} {
		if _, err := Verify(token, expected, b, v, now, now); err == nil {
			t.Fatal("changed raw body accepted")
		}
	}
}
func TestCryptographicallyValidButInvalidDescriptor(t *testing.T) {
	f, v, d, expected, body := setup(t)
	changes := map[string]func(*Descriptor){
		"version": func(d *Descriptor) { d.Version = 2 }, "simulation": func(d *Descriptor) { d.Domain = "simulation" }, "lab": func(d *Descriptor) { d.Domain = "lab" }, "principal": func(d *Descriptor) { d.Principal += "-other" }, "tenant": func(d *Descriptor) { d.Tenant = "other" }, "site": func(d *Descriptor) { d.Site = "elsewhere" }, "audience": func(d *Descriptor) { d.Audience = "cloud" }, "body": func(d *Descriptor) { d.BodySHA256 = strings.Repeat("0", 64) }, "digest-case": func(d *Descriptor) { d.BodySHA256 = strings.ToUpper(d.BodySHA256) }, "policy": func(d *Descriptor) { d.PolicyRevision = 0 }, "operation": func(d *Descriptor) { d.Operation = "cloud.read" }, "move": func(d *Descriptor) { d.Operation = "world.move" }, "method": func(d *Descriptor) { d.Method = "GET" }, "query": func(d *Descriptor) { d.Path = Path + "?x=1" }, "alternate-path": func(d *Descriptor) { d.Path = "/v1/world/./query" }, "encoded-path": func(d *Descriptor) { d.Path = "/v1/world/%71uery" }, "short-nonce": func(d *Descriptor) { d.Nonce = base64.RawURLEncoding.EncodeToString(make([]byte, 23)) }, "padded-nonce": func(d *Descriptor) { d.Nonce += "=" }, "nonce-newline": func(d *Descriptor) { d.Nonce += "\n" }, "lifetime": func(d *Descriptor) { d.ExpiresAtUnixMS = d.IssuedAtUnixMS + 60001 }, "negative-time": func(d *Descriptor) { d.IssuedAtUnixMS = -1 }, "overflow-time": func(d *Descriptor) { d.ExpiresAtUnixMS = math.MaxInt64 },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			altered := d
			change(&altered)
			token := signedUnchecked(t, f, altered)
			if _, err := Verify(token, expected, body, v, now, now); err == nil {
				t.Fatal("invalid descriptor accepted despite valid signature")
			}
		})
	}
}
func TestRequestValidityCoversWholeIntervalAndExclusiveExpiry(t *testing.T) {
	f, v, d, e, body := setup(t)
	token, err := Sign(d, body, f.ChainDER, f.Key)
	if err != nil {
		t.Fatal(err)
	}
	for _, window := range [][2]time.Time{{now.Add(-time.Nanosecond), now}, {now, now.Add(30 * time.Second)}, {now, now.Add(30*time.Second + time.Nanosecond)}, {now.Add(time.Second), now}, {time.Time{}, now}, {now, time.Time{}}} {
		if _, err := Verify(token, e, body, v, window[0], window[1]); err == nil {
			t.Fatal("request accepted outside full interval")
		}
	}
	if _, err := Verify(token, e, body, v, now, now.Add(30*time.Second-time.Nanosecond)); err != nil {
		t.Fatal("valid exact boundaries refused", err)
	}
	d.ExpiresAtUnixMS = now.Add(MaxLifetime).UnixMilli()
	token, err = Sign(d, body, f.ChainDER, f.Key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Verify(token, e, body, v, now, now.Add(MaxLifetime-time.Nanosecond)); err != nil {
		t.Fatal("60-second boundary refused", err)
	}
}
func TestUnknownDuplicateAndCaseAliasedRequestFields(t *testing.T) {
	f, v, d, e, body := setup(t)
	payload, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	unknown := append([]byte(`{"unknown":true,`), payload[1:]...)
	token, err := jws.Sign(Type, unknown, f.ChainDER, f.Key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Verify(token, e, body, v, now, now); err == nil {
		t.Fatal("unknown request field accepted")
	}
	aliases := bytes.Replace(payload, []byte(`"version"`), []byte(`"Version"`), 1)
	token, err = jws.Sign(Type, aliases, f.ChainDER, f.Key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Verify(token, e, body, v, now, now); err == nil {
		t.Fatal("case-aliased request field accepted")
	}
	// Raw signing deliberately bypasses the generic signer's duplicate-JSON check.
	duplicate := append([]byte(`{"version":1,`), payload[1:]...)
	token = raw(t, f, Type, duplicate)
	if _, err = Verify(token, e, body, v, now, now); err == nil {
		t.Fatal("duplicate request field accepted")
	}
}
func raw(t *testing.T, f *testidentity.Fixture, typ string, payload []byte) string {
	t.Helper()
	certs := make([]string, len(f.ChainDER))
	for i, c := range f.ChainDER {
		certs[i] = base64.StdEncoding.EncodeToString(c)
	}
	h, _ := json.Marshal(map[string]any{"alg": "ML-DSA-65", "typ": typ, "x5c": certs})
	input := base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(payload)
	sig, err := f.Key.Sign(nil, []byte(input), &mldsa.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return input + "." + base64.RawURLEncoding.EncodeToString(sig)
}
func TestCloudReleaseAndLabProofsDoNotAuthorizeWorldRead(t *testing.T) {
	f, v, d, e, body := setup(t)
	payload, _ := json.Marshal(d)
	for _, typ := range []string{"wendy.cloud.request.v1", "wendy.release.v1", "JWT", "wendy.world.site-policy.v1", "wendy.world.request.v2"} {
		if _, err := Verify(raw(t, f, typ, payload), e, body, v, now, now); err == nil {
			t.Fatal("cross-protocol proof accepted")
		}
	}
	release, err := f.Key.Sign(nil, []byte(d.BodySHA256), &mldsa.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Verify(base64.RawURLEncoding.EncodeToString(release), e, body, v, now, now); err == nil {
		t.Fatal("raw release signature accepted")
	}
	lab := `{"version":1,"grant":{"domain":"simulation"},"signature":"not-a-production-JWS"}`
	if _, err = Verify(lab, e, body, v, now, now); err == nil {
		t.Fatal("lab proof accepted")
	}
}
func TestSignerIsBoundedAndDoesNotInferBodyOrTarget(t *testing.T) {
	f, _, d, _, body := setup(t)
	altered := d
	altered.BodySHA256 = ""
	if _, err := Sign(altered, body, f.ChainDER, f.Key); err == nil {
		t.Fatal("missing supplied digest inferred")
	}
	if _, err := Sign(d, append(body, ' '), f.ChainDER, f.Key); err == nil {
		t.Fatal("wrong body signed")
	}
	if _, err := Sign(d, bytes.Repeat([]byte{'x'}, MaxBodyBytes+1), f.ChainDER, f.Key); err == nil {
		t.Fatal("oversized body signed")
	}
	if _, err := Sign(d, body, f.ChainDER, nil); err == nil {
		t.Fatal("missing key inferred")
	}
	altered = d
	altered.Domain = "simulation"
	if _, err := Sign(altered, body, f.ChainDER, f.Key); err == nil {
		t.Fatal("lab domain signed")
	}
}
