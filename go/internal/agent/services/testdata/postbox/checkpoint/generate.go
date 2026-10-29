//go:build ignore

// Generates conformance/postbox/checkpoint-vectors-v1.json (WDY-4015 item 2,
// WDY-2162): the pki-core postbox head checkpoint that PullChangesResponse
// .checkpoint carries. Mirrors pki-core's producer: postbox.EncodeBody and
// EncodeArtifact (internal/postbox/checkpoint.go), Engine.SignCheckpoint
// (internal/ca/checkpoint_signer.go) and Engine.IssueTimestamp
// (internal/ca/tsa.go), with a test hierarchy in place of the platform keys.
// Go standard library only (Go 1.27+). Every key is ML-DSA from
// SHA-256(<label>) and every signature is deterministic, so the output is
// reproducible byte for byte.
// Run: go run conformance/postbox/generate.go conformance/postbox
package main

import (
	"bytes"
	"crypto"
	"crypto/mldsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"time"
)

const (
	tenant      = "2558fd76-afc7-466e-9613-6b715296a526"
	otherTenant = "6a1f0c3e-8d2b-4f7a-9e5c-3b1d0a8f6e4c"
	notarizedAt = 1790000000        // 2026-09-21T14:13:20Z
	expiresAt   = notarizedAt + 900 // pki-core default postbox.checkpoint_ttl, 15m
	genTime     = notarizedAt + 1   // the TSA stamps after the engine's clock read
	verifyAt    = notarizedAt + 60  // the device's trusted clock
	maxID       = 42
)

var (
	b64  = base64.StdEncoding.EncodeToString
	b64u = base64.RawURLEncoding.EncodeToString

	oidCheckpointEKU = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 65441, 1, 8} // pki-core config.OIDCheckpointSigningEKU
	oidTimeStamping  = asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 7, 3, 8}
	oidEKU           = asn1.ObjectIdentifier{2, 5, 29, 37}
	oidTSAPolicy     = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 65441, 2, 1} // pki-core config.OIDTSAPolicyWendy
	oidSHA256        = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 2, 1}
	oidMLDSA65       = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 3, 18}
	oidMLDSA87       = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 3, 19}
	oidSignedData    = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 2}
	oidTSTInfo       = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 16, 1, 4}
	oidContentType   = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 3}
	oidMessageDigest = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 4}
	oidSigningTime   = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 5}
	oidSigningCertV2 = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 16, 2, 47}
)

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func seed(label string) []byte { s := sha256.Sum256([]byte(label)); return s[:] }

func deviceURI(t, id string) string { return "spiffe://wendy.sh/tenant/" + t + "/device/" + id }

// detSigner signs ML-DSA deterministically, so every output byte is reproducible.
type detSigner struct{ *mldsa.PrivateKey }

func (s detSigner) Sign(_ io.Reader, msg []byte, opts crypto.SignerOpts) ([]byte, error) {
	return s.SignDeterministic(msg, opts)
}

func key(p mldsa.Parameters, label string) detSigner {
	return detSigner{must(mldsa.NewPrivateKey(p, seed(label)))}
}

type cert struct {
	der    []byte
	parsed *x509.Certificate
	key    detSigner
}

// issue mints a CA (eku == nil) or a leaf carrying exactly one EKU, the way
// pki-core does: id-kp-timeStamping critical on the TSA signer, the
// checkpoint-signer EKU non-critical on the checkpoint signer.
func issue(serial int64, cn string, k detSigner, eku asn1.ObjectIdentifier, critical bool, parent *cert) *cert {
	t := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Unix(1767225600, 0).UTC(), // 2026-01-01
		NotAfter:     time.Unix(2082758400, 0).UTC(), // 2036-01-01
	}
	if eku == nil {
		t.IsCA, t.BasicConstraintsValid = true, true
		t.KeyUsage = x509.KeyUsageCertSign | x509.KeyUsageCRLSign
	} else {
		t.KeyUsage = x509.KeyUsageDigitalSignature
		t.ExtraExtensions = []pkix.Extension{{Id: oidEKU, Critical: critical, Value: must(asn1.Marshal([]asn1.ObjectIdentifier{eku}))}}
	}
	p, signer := t, k
	if parent != nil {
		p, signer = parent.parsed, parent.key
	}
	der := must(x509.CreateCertificate(rand.Reader, t, p, k.Public(), signer))
	return &cert{der: der, parsed: must(x509.ParseCertificate(der)), key: k}
}

// ---------- checkpoint body (postbox.EncodeBody) ----------

type body struct {
	v              int
	tenant, target string
	maxID          uint64
	nat, exp       int64
}

func (b body) jcs() []byte {
	head := sha256.Sum256([]byte("wendy-postbox-vector-head-42"))
	// encoding/json sorts map keys; every value is ASCII with nothing JSON must
	// escape and every number an integer, so this is RFC 8785 output.
	var buf bytes.Buffer
	e := json.NewEncoder(&buf)
	e.SetEscapeHTML(false)
	if err := e.Encode(map[string]any{
		"v":            b.v,
		"tenant":       b.tenant,
		"target":       b.target,
		"max_id":       b.maxID,
		"hash_of_head": b64u(head[:]),
		"notarized_at": time.Unix(b.nat, 0).UTC().Format(time.RFC3339),
		"expires_at":   time.Unix(b.exp, 0).UTC().Format(time.RFC3339),
	}); err != nil {
		panic(err)
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
}

// ---------- signature (Engine.SignCheckpoint) ----------

func signJWS(alg string, x5c []*cert, payload []byte) string {
	chain := make([][]byte, len(x5c))
	for i, c := range x5c {
		chain[i] = c.der
	}
	input := b64u(must(json.Marshal(map[string]any{"alg": alg, "x5c": chain}))) + "." + b64u(payload)
	return input + "." + b64u(must(x5c[0].key.SignDeterministic([]byte(input), nil)))
}

// ---------- timestamp token (Engine.IssueTimestamp) ----------

type algID struct {
	Algorithm  asn1.ObjectIdentifier
	Parameters asn1.RawValue `asn1:"optional"`
}

type tstInfo struct {
	Version        int
	Policy         asn1.ObjectIdentifier
	MessageImprint struct {
		HashAlgorithm algID
		HashedMessage []byte
	}
	SerialNumber *big.Int
	GenTime      time.Time `asn1:"generalized"`
}

type attribute struct {
	Type  asn1.ObjectIdentifier
	Value asn1.RawValue `asn1:"set"`
}

type signerInfo struct {
	Version int
	SID     struct {
		Issuer       asn1.RawValue
		SerialNumber *big.Int
	}
	DigestAlgorithm    algID
	SignedAttrs        asn1.RawValue `asn1:"optional,tag:0"`
	SignatureAlgorithm algID
	Signature          []byte
}

type signedData struct {
	Version          int
	DigestAlgorithms []algID `asn1:"set"`
	EncapContentInfo struct {
		EContentType asn1.ObjectIdentifier
		EContent     []byte `asn1:"explicit,tag:0,optional"`
	}
	Certificates asn1.RawValue `asn1:"optional,tag:0"`
	SignerInfos  []signerInfo  `asn1:"set"`
}

func wrap(tag byte, b []byte) []byte {
	n := len(b)
	switch {
	case n < 0x80:
		return append([]byte{tag, byte(n)}, b...)
	case n < 0x100:
		return append([]byte{tag, 0x81, byte(n)}, b...)
	case n < 0x10000:
		return append([]byte{tag, 0x82, byte(n >> 8), byte(n)}, b...)
	}
	return append([]byte{tag, 0x83, byte(n >> 16), byte(n >> 8), byte(n)}, b...)
}

func attr(t asn1.ObjectIdentifier, v any) []byte {
	return must(asn1.Marshal(attribute{t, asn1.RawValue{FullBytes: wrap(0x31, must(asn1.Marshal(v)))}}))
}

// timestamp returns a granted TimeStampResp over imprint (SHA-256 of the
// stamped bytes), signed by tsa, with the signer certificate embedded
// (certReq=true, as NotarizePostboxHead requests). corrupt flips one bit of
// the CMS signature.
func timestamp(tsa *cert, sigOID asn1.ObjectIdentifier, imprint []byte, gen int64, serial int64, corrupt bool) []byte {
	var tst tstInfo
	tst.Version, tst.Policy = 1, oidTSAPolicy
	tst.MessageImprint.HashAlgorithm = algID{Algorithm: oidSHA256}
	tst.MessageImprint.HashedMessage = imprint
	tst.SerialNumber, tst.GenTime = big.NewInt(serial), time.Unix(gen, 0).UTC()
	tstDER := must(asn1.Marshal(tst))

	md := sha256.Sum256(tstDER)
	ch := sha256.Sum256(tsa.der)
	ess := struct {
		Certs []struct {
			HashAlgorithm algID
			CertHash      []byte
		}
	}{Certs: []struct {
		HashAlgorithm algID
		CertHash      []byte
	}{{algID{Algorithm: oidSHA256}, ch[:]}}}
	attrs := [][]byte{
		attr(oidContentType, oidTSTInfo),
		attr(oidMessageDigest, md[:]),
		attr(oidSigningTime, time.Unix(gen, 0).UTC()),
		attr(oidSigningCertV2, ess),
	}
	sort.Slice(attrs, func(i, j int) bool { return bytes.Compare(attrs[i], attrs[j]) < 0 })
	set := bytes.Join(attrs, nil)
	sig := must(tsa.key.SignDeterministic(wrap(0x31, set), nil))
	if corrupt {
		sig[len(sig)/2] ^= 0x01
	}

	var si signerInfo
	si.Version = 1
	si.SID.Issuer = asn1.RawValue{FullBytes: tsa.parsed.RawIssuer}
	si.SID.SerialNumber = tsa.parsed.SerialNumber
	si.DigestAlgorithm = algID{Algorithm: oidSHA256}
	si.SignedAttrs = asn1.RawValue{FullBytes: wrap(0xA0, set)}
	si.SignatureAlgorithm = algID{Algorithm: sigOID}
	si.Signature = sig

	var sd signedData
	sd.Version = 3
	sd.DigestAlgorithms = []algID{{Algorithm: oidSHA256}}
	sd.EncapContentInfo.EContentType, sd.EncapContentInfo.EContent = oidTSTInfo, tstDER
	sd.Certificates = asn1.RawValue{FullBytes: wrap(0xA0, tsa.der)}
	sd.SignerInfos = []signerInfo{si}

	ci := must(asn1.Marshal(struct {
		ContentType asn1.ObjectIdentifier
		Content     asn1.RawValue
	}{oidSignedData, asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 0, IsCompound: true, Bytes: must(asn1.Marshal(sd))}}))
	return must(asn1.Marshal(struct {
		Status         struct{ Status int }
		TimeStampToken asn1.RawValue
	}{TimeStampToken: asn1.RawValue{FullBytes: ci}}))
}

// artifact is postbox.EncodeArtifact: body and token unpadded base64url, the
// JWS verbatim. An empty member is left out, which pki-core never emits.
func artifact(bodyBytes []byte, jws string, token []byte) []byte {
	m := map[string]string{"body": b64u(bodyBytes)}
	if jws != "" {
		m["signature"] = jws
	}
	if token != nil {
		m["timestamp_token"] = b64u(token)
	}
	return must(json.Marshal(m))
}

func main() {
	out := os.Args[1]

	root := issue(1, "Wendy Checkpoint Vector Root", key(mldsa.MLDSA87(), "wendy-checkpoint-vector-root-2026-10"), nil, false, nil)
	tsaCA := issue(2, "Vector Platform TSA Intermediate", key(mldsa.MLDSA87(), "wendy-checkpoint-vector-tsa-ca-2026-10"), nil, false, root)
	tsaSigner := issue(3, "Vector Platform TSA Signer", key(mldsa.MLDSA87(), "wendy-checkpoint-vector-tsa-signer-2026-10"), oidTimeStamping, true, tsaCA)
	cpSigner := issue(4, "Vector Platform Checkpoint Signer", key(mldsa.MLDSA65(), "wendy-checkpoint-vector-checkpoint-signer-2026-10"), oidCheckpointEKU, false, tsaCA)
	foreignRoot := issue(1, "Foreign Checkpoint Vector Root", key(mldsa.MLDSA65(), "wendy-checkpoint-vector-foreign-root-2026-10"), nil, false, nil)
	foreignCP := issue(5, "Foreign Checkpoint Signer", key(mldsa.MLDSA65(), "wendy-checkpoint-vector-foreign-checkpoint-signer-2026-10"), oidCheckpointEKU, false, foreignRoot)
	foreignTSA := issue(6, "Foreign TSA Signer", key(mldsa.MLDSA65(), "wendy-checkpoint-vector-foreign-tsa-signer-2026-10"), oidTimeStamping, true, foreignRoot)

	device := deviceURI(tenant, "7c1d3e5f-2a4b-4c6d-8e9f-0a1b2c3d4e5f")
	otherDevice := deviceURI(tenant, "3f8e2d1c-9b7a-4e6f-8d5c-4b3a2f1e0d9c")
	base := body{1, tenant, device, maxID, notarizedAt, expiresAt}

	serial := int64(1000)
	stamp := func(b []byte, gen int64) []byte {
		serial++
		h := sha256.Sum256(b)
		return timestamp(tsaSigner, oidMLDSA87, h[:], gen, serial, false)
	}
	// good: signed by the checkpoint signer, x5c = [leaf, TSA Intermediate]
	// (what pki-core's startup loader configures), and stamped at genTime.
	good := func(b body, gen int64) (bodyBytes []byte, jws string, token []byte) {
		bodyBytes = b.jcs()
		return bodyBytes, signJWS("ML-DSA-65", []*cert{cpSigner, tsaCA}, bodyBytes), stamp(bodyBytes, gen)
	}

	type opts struct {
		hwm     uint64
		at      int64
		refusal string
	}
	var vectors []map[string]any
	add := func(name, why string, cp []byte, o opts) {
		v := map[string]any{
			"name":            name,
			"description":     why,
			"expected_tenant": tenant,
			"expected_target": device,
			"high_water_mark": o.hwm,
			"verify_at":       o.at,
			"checkpoint_b64":  b64(cp),
			"expect":          "valid",
		}
		if o.refusal != "" {
			v["expect"], v["refusal"] = "invalid", o.refusal
		}
		vectors = append(vectors, v)
	}
	ok := opts{hwm: maxID, at: verifyAt}
	with := func(f func(*opts)) opts { o := ok; f(&o); return o }
	refuse := func(code string) opts { return with(func(o *opts) { o.refusal = code }) }

	// The valid vector, broken out.
	vb, vj, vt := good(base, genTime)
	add("checkpoint-valid", "device-scoped checkpoint; high_water_mark equal to max_id is accepted", artifact(vb, vj, vt), ok)
	imprint := sha256.Sum256(vb)
	vectors[0]["breakdown"] = map[string]any{
		"body_jcs":                       string(vb),
		"body_sha256_hex":                hex.EncodeToString(imprint[:]),
		"signature_jws":                  vj,
		"timestamp_token_b64":            b64(vt),
		"gen_time":                       genTime,
		"checkpoint_signer_cert_der_b64": b64(cpSigner.der),
		"tsa_signer_cert_der_b64":        b64(tsaSigner.der),
	}

	wb, wj, wt := good(body{1, tenant, "", maxID, notarizedAt, expiresAt}, genTime)
	add("tenant-wide-valid", "target \"\" covers every device of the tenant; verify_at == expires_at is not yet stale", artifact(wb, wj, wt), with(func(o *opts) { o.at = expiresAt }))

	bb, bj, bt := good(base, notarizedAt-5)
	add("skew-boundary-valid", "genTime exactly 5 s before notarized_at is still inside the skew", artifact(bb, bj, bt), ok)

	add("missing-token-invalid", "no timestamp_token member", artifact(vb, vj, nil), refuse("malformed"))
	add("missing-signature-invalid", "no signature member", artifact(vb, "", vt), refuse("malformed"))

	sigBytes := must(base64.RawURLEncoding.DecodeString(vj[bytes.LastIndexByte([]byte(vj), '.')+1:]))
	sigBytes[len(sigBytes)/2] ^= 0x01
	badSig := vj[:bytes.LastIndexByte([]byte(vj), '.')+1] + b64u(sigBytes)
	add("bad-signature-invalid", "one bit of the JWS signature flipped", artifact(vb, badSig, vt), refuse("signature"))

	add("untrusted-notary-invalid", "signed by a checkpoint-EKU leaf under a root the device does not pin", artifact(vb, signJWS("ML-DSA-65", []*cert{foreignCP, foreignRoot}, vb), vt), refuse("signature"))

	add("wrong-notary-role-invalid", "signed by the platform TSA signer: chain and signature valid, but no checkpoint-signer EKU", artifact(vb, signJWS("ML-DSA-87", []*cert{tsaSigner, tsaCA}, vb), vt), refuse("signer_role"))

	other := base
	other.maxID = maxID + 1
	ob := other.jcs()
	add("signature-body-mismatch-invalid", "valid JWS over another body (max_id 43) stapled onto this body", artifact(vb, signJWS("ML-DSA-65", []*cert{cpSigner, tsaCA}, ob), vt), refuse("signature_body_mismatch"))

	add("token-imprint-mismatch-invalid", "genuine TSA token over the SHA-256 of another body (max_id 43)", artifact(vb, vj, stamp(ob, genTime)), refuse("imprint_mismatch"))

	serial++
	add("token-bad-signature-invalid", "one bit of the TSA's CMS signature flipped", artifact(vb, vj, timestamp(tsaSigner, oidMLDSA87, imprint[:], genTime, serial, true)), refuse("timestamp"))

	serial++
	add("token-untrusted-tsa-invalid", "token over the right imprint, signed by a timestamping leaf under a foreign root", artifact(vb, vj, timestamp(foreignTSA, oidMLDSA65, imprint[:], genTime, serial, false)), refuse("timestamp"))

	sb, sj, st := good(base, notarizedAt-6)
	add("token-skew-invalid", "genTime 6 s before notarized_at: the body claims a time the TSA does not attest", artifact(sb, sj, st), refuse("skew"))

	v2 := base
	v2.v = 2
	ub, uj, ut := good(v2, genTime)
	add("unknown-version-invalid", "body v = 2, signed and stamped", artifact(ub, uj, ut), refuse("body"))

	ot := base
	ot.tenant, ot.target = otherTenant, deviceURI(otherTenant, "7c1d3e5f-2a4b-4c6d-8e9f-0a1b2c3d4e5f")
	tb, tj, tt := good(ot, genTime)
	add("tenant-mismatch-invalid", "checkpoint of another tenant", artifact(tb, tj, tt), refuse("tenant_mismatch"))

	od := base
	od.target = otherDevice
	db, dj, dt := good(od, genTime)
	add("target-mismatch-invalid", "checkpoint scoped to another device of the same tenant", artifact(db, dj, dt), refuse("target_mismatch"))

	add("rollback-invalid", "max_id below the device's high-water mark", artifact(vb, vj, vt), with(func(o *opts) { o.hwm = maxID + 1; o.refusal = "rollback" }))
	add("stale-invalid", "verify_at one second after expires_at", artifact(vb, vj, vt), with(func(o *opts) { o.at = expiresAt + 1; o.refusal = "stale" }))

	doc := map[string]any{
		"version":                       1,
		"contract":                      "conformance/postbox/README.md; producer of record pki-core internal/postbox/checkpoint.go, internal/ca/checkpoint_signer.go, internal/ca/tsa.go",
		"root_cert_der_b64":             b64(root.der),
		"tsa_intermediate_cert_der_b64": b64(tsaCA.der),
		"checkpoint_signer_eku":         oidCheckpointEKU.String(),
		"max_notarize_skew_seconds":     5,
		"generation": map[string]any{
			"tool":          "go run conformance/postbox/generate.go conformance/postbox (Go 1.27, standard library only)",
			"deterministic": "ML-DSA keys from seed = SHA-256(label); certificates, JWS and CMS signed deterministically. Reproducible byte for byte.",
			"warning":       "TEST KEYS ONLY: the seed labels in the generator ARE the private keys. Real checkpoints are signed by pki-core's platform checkpoint signer and TSA signer, hedged.",
		},
		"vectors": vectors,
	}
	var buf bytes.Buffer
	e := json.NewEncoder(&buf)
	e.SetEscapeHTML(false)
	e.SetIndent("", "  ")
	if err := e.Encode(doc); err != nil {
		panic(err)
	}
	if err := os.WriteFile(filepath.Join(out, "checkpoint-vectors-v1.json"), buf.Bytes(), 0o644); err != nil {
		panic(err)
	}
}
