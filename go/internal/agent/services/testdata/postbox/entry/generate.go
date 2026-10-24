//go:build ignore

// Generates conformance/postbox-entry/postbox-entry-vectors-v1.json (WDY-4015
// items 1 and 3): the device's check of a CA served by
// DevicePostboxService.GetOrgCAs, and the validity of a postbox-relayed
// instruction. Go standard library only (Go 1.27+: crypto/mldsa, ML-DSA in
// crypto/x509). Every key and signature is ML-DSA and deterministic, so a rerun
// is byte-identical. Run: go run conformance/postbox-entry/generate.go conformance
package main

import (
	"bytes"
	"crypto"
	"crypto/mldsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

const (
	tenant      = "2558fd76-afc7-466e-9613-6b715296a526"
	otherTenant = "6a1f0c3e-8d2b-4f7a-9e5c-3b1d0a8f6e4c"
	deviceID    = "7c1d3e5f-2a4b-4c6d-8e9f-0a1b2c3d4e5f"
	aud         = "https://cloud.wendy.sh/broker"
	iat         = 1790000000
	day         = 86400

	platformCap      = 365 * day
	defaultTenantCap = 60 * day

	controlContainer = "wendycloud.v2.DeploymentService/ControlContainer"
)

var (
	b64  = base64.StdEncoding.EncodeToString
	b64u = base64.RawURLEncoding.EncodeToString
)

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func seed(label string) []byte { s := sha256.Sum256([]byte(label)); return s[:] }

type detSigner struct{ *mldsa.PrivateKey }

func (s detSigner) Sign(_ io.Reader, msg []byte, opts crypto.SignerOpts) ([]byte, error) {
	return s.SignDeterministic(msg, opts)
}

func key(label string) detSigner {
	return detSigner{must(mldsa.NewPrivateKey(mldsa.MLDSA65(), seed("wendy-postbox-entry-vector-"+label+"-2026-10")))}
}

type cert struct {
	der    []byte
	parsed *x509.Certificate
	signer crypto.Signer
}

func issue(serial int64, cn, uri string, k detSigner, parent *cert) *cert {
	t := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Unix(1767225600, 0).UTC(), // 2026-01-01
		NotAfter:     time.Unix(2082758400, 0).UTC(), // 2036-01-01
	}
	if uri == "" {
		t.IsCA, t.BasicConstraintsValid = true, true
		t.KeyUsage = x509.KeyUsageCertSign | x509.KeyUsageCRLSign
	} else {
		t.KeyUsage = x509.KeyUsageDigitalSignature
		t.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
		t.URIs = []*url.URL{must(url.Parse(uri))}
	}
	p, s := t, crypto.Signer(k)
	if parent != nil {
		p, s = parent.parsed, parent.signer
	}
	der := must(x509.CreateCertificate(rand.Reader, t, p, k.Public(), s))
	return &cert{der, must(x509.ParseCertificate(der)), k}
}

// Minimal protobuf encoding (field order = field number, as protoc emits).
func tag(f, wt int) []byte { return binary.AppendUvarint(nil, uint64(f<<3|wt)) }
func pbBytes(f int, v []byte) []byte {
	return append(append(tag(f, 2), binary.AppendUvarint(nil, uint64(len(v)))...), v...)
}
func pbVarint(f int, v uint64) []byte { return append(tag(f, 0), binary.AppendUvarint(nil, v)...) }
func cat(parts ...[]byte) (out []byte) {
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// sign returns the compact JWS; encoding/json sorts keys and every value is
// ASCII or an integer, so the claims are RFC 8785 JCS.
func sign(k detSigner, leaf *cert, claims map[string]any) string {
	h := must(json.Marshal(map[string]any{"alg": "ML-DSA-65", "x5c": []string{b64(leaf.der)}}))
	input := b64u(h) + "." + b64u(must(json.Marshal(claims)))
	return input + "." + b64u(must(k.SignDeterministic([]byte(input), nil)))
}

func writeJSON(path string, v any) {
	var buf bytes.Buffer
	e := json.NewEncoder(&buf)
	e.SetEscapeHTML(false)
	e.SetIndent("", "  ")
	if err := e.Encode(v); err != nil {
		panic(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		panic(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		panic(err)
	}
}

func main() {
	out := os.Args[1]
	type vec = map[string]any

	root := issue(1, "Postbox Entry Vector Root", "", key("root"), nil)
	orgCA := issue(2, "Vector Tenant CA", "", key("tenant-ca"), root)
	operatorCA := issue(3, "Vector Operator Authority CA", "", key("operator-ca"), orgCA)
	otherOrgCA := issue(4, "Vector Tenant CA (other tenant)", "", key("other-tenant-ca"), root)
	otherOperatorCA := issue(5, "Vector Operator Authority CA (other tenant)", "", key("other-operator-ca"), otherOrgCA)
	opKey := key("operator")
	opURI := "spiffe://wendy.sh/tenant/" + tenant + "/operator/0e4f8a2c-6b1d-4c3e-9a7f-5d2b8e1c4a63"
	opLeaf := issue(10, "vector operator", opURI, opKey, operatorCA)
	device := "spiffe://wendy.sh/tenant/" + tenant + "/device/" + deviceID

	// ---------- item 1: GetOrgCAs ----------
	resp := func(cas ...*cert) string {
		var m []byte
		for _, c := range cas {
			m = append(m, pbBytes(1, c.der)...)
		}
		return b64(m)
	}
	unknown := must(x509.ParseCertificate(issue(6, "Vector CA cloud does not hold", "", key("unknown-ca"), orgCA).der)).RawSubject
	caVectors := []vec{
		{"name": "operator-ca-valid", "request_b64": b64(pbBytes(1, opLeaf.parsed.RawIssuer)), "response_b64": resp(operatorCA),
			"expect": "valid", "expected_cached_subject_cn": "Vector Operator Authority CA"},
		{"name": "other-tenant-ca-invalid", "request_b64": b64(pbBytes(1, opLeaf.parsed.RawIssuer)), "response_b64": resp(otherOperatorCA),
			"expect": "invalid", "refusal": "not_under_org_ca",
			"invalid_reason": "the served CA is another tenant's Operator Authority CA: valid, under the same platform root, but not issued by this device's tenant CA"},
		{"name": "not-a-ca-invalid", "request_b64": b64(pbBytes(1, opLeaf.parsed.RawIssuer)), "response_b64": resp(issue(7, "vector leaf", device, key("leaf-under-tenant-ca"), orgCA)),
			"expect": "invalid", "refusal": "not_a_ca",
			"invalid_reason": "the served certificate is issued directly by this tenant's CA and valid, but it is a leaf, not a CA"},
		{"name": "unknown-issuer-not-found-hold", "request_b64": b64(pbBytes(1, unknown)), "status": "NOT_FOUND",
			"expect": "hold", "invalid_reason": "cloud holds no CA with this subject: NOT_FOUND; the device holds the entry (not applied, not acked, not consumed) and asks again on its next pull"},
	}

	// ---------- item 3: instruction validity ----------
	// ControlContainerRequest: asset_id 1, organization_id 2, container_name 3,
	// action 4 (START = 1), valid_until 5 (Timestamp: seconds 1).
	payload := func(validUntil int64) []byte {
		m := cat(pbBytes(1, []byte(deviceID)), pbBytes(2, []byte(tenant)), pbBytes(3, []byte("myapp")), pbVarint(4, 1))
		if validUntil != 0 {
			m = append(m, pbBytes(5, pbVarint(1, uint64(validUntil)))...)
		}
		return m
	}
	request := func(validUntil int64, nonce string) string {
		p := payload(validUntil)
		body := sha256.Sum256(p)
		j := sign(opKey, opLeaf, map[string]any{
			"aud": aud, "body_sha256": b64u(body[:]), "correlation_id": "3c8e1f5a-9b2d-4e7c-a6f0-2d4b8e1a5c97",
			"expiry": iat + 30, "iat": iat, "nonce": nonce, "operation": controlContainer,
			"target": map[string]any{"tenant": tenant, "resource": device},
		})
		return b64(cat(pbBytes(1, p), pbBytes(2, []byte("wendycloud.v2.ControlContainerRequest")), pbBytes(3, []byte(j))))
	}
	twoMonths := request(iat+defaultTenantCap, "a1c5e9f3-2b6d-4a8e-9c1f-7d3b5e9a1c42")
	pastPlatform := request(iat+platformCap+1, "b7d3f1a9-5c2e-4b6d-8f0a-3e9c1d7b5f28")
	onDevice := func(name, sr string, now int64, expect, refusal, reason string) vec {
		v := vec{"name": name, "stage": "device", "signed_request_b64": sr, "trusted_now": now, "expect": expect}
		if refusal != "" {
			v["refusal"], v["invalid_reason"] = refusal, reason
		}
		return v
	}
	atAccept := func(name, sr string, tenantCap int64, expect, refusal, reason string) vec {
		v := vec{"name": name, "stage": "accept", "signed_request_b64": sr, "tenant_cap_seconds": tenantCap, "expect": expect}
		if refusal != "" {
			v["refusal"], v["invalid_reason"] = refusal, reason
		}
		return v
	}
	validityVectors := []vec{
		onDevice("control-container-valid", twoMonths, iat+10, "valid", "", ""),
		onDevice("call-expiry-ignored-valid", twoMonths, iat+14*day, "valid", "", ""),
		onDevice("past-valid-until-invalid", twoMonths, iat+defaultTenantCap, "invalid", "stale",
			"trusted now equals valid_until (iat + 60 days); valid_until is strict, like expiry"),
		onDevice("valid-until-missing-invalid", request(0, "c2e6a0b4-8d3f-4c7a-9e1b-5f2d6a0c4e83"), iat+10, "invalid", "validity_missing",
			"ControlContainerRequest without valid_until"),
		onDevice("above-platform-cap-device-invalid", pastPlatform, iat+10, "invalid", "above_platform_cap",
			"valid_until = iat + 365 days + 1 s: above the platform cap, which the device checks too (cloud should have refused it at accept)"),
		atAccept("tenant-cap-boundary-accept-valid", twoMonths, defaultTenantCap, "valid", "", ""),
		atAccept("above-tenant-cap-accept-invalid", request(iat+defaultTenantCap+day, "d8f4b2c6-0a5e-4d9b-8c3f-6e1a7b2d9f54"), defaultTenantCap, "invalid", "above_tenant_cap",
			"valid_until = iat + 61 days; the tenant cap is the 60-day default"),
		atAccept("above-platform-cap-accept-invalid", pastPlatform, platformCap, "invalid", "above_platform_cap",
			"valid_until = iat + 365 days + 1 s; the tenant cap is set to the platform cap, which it can never exceed"),
	}

	writeJSON(filepath.Join(out, "postbox-entry", "postbox-entry-vectors-v1.json"), map[string]any{
		"version":  "postbox-entry-vectors-v1",
		"contract": "conformance/postbox-entry/README.md; wendycloud/v2/postbox.proto (GetOrgCAs); valid_until on the postbox-relayed requests",
		"generation": map[string]any{
			"tool":          "go run conformance/postbox-entry/generate.go conformance (Go 1.27, standard library only)",
			"deterministic": "ML-DSA-65 keys from seed = SHA-256(label); certificates and JWS signed deterministically; a rerun is byte-identical",
			"warning":       "TEST KEYS ONLY: the seed labels in the generator ARE the private keys.",
		},
		"organization_id":            tenant,
		"device":                     device,
		"aud":                        aud,
		"org_ca_cert_der_b64":        b64(orgCA.der),
		"verify_at":                  iat,
		"org_ca_vectors":             caVectors,
		"operator_ca_cert_der_b64":   b64(operatorCA.der),
		"platform_cap_seconds":       platformCap,
		"default_tenant_cap_seconds": defaultTenantCap,
		"validity_vectors":           validityVectors,
	})
}
