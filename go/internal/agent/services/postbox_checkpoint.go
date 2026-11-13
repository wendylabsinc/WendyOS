package services

import (
	"bytes"
	"crypto/mldsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/asn1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"slices"
	"strings"
	"time"
)

// The notarized postbox checkpoint (PullChangesResponse.checkpoint), verified
// in the order of service-protos conformance/postbox/README.md (65830a0a),
// which mirrors pki-core postbox.Verifier.Verify. Any refusal holds: nothing
// up to max_id is treated as complete on cloud's word.
//
// ponytail: ML-DSA signers only, as pki-core's default config and the vectors
// use; an ES256/ES384 checkpoint signer or TSA is refused (a hold), add the
// ECDSA branch when a deployment configures one.

const postboxCheckpointSkew = 5 * time.Second // genTime may trail notarized_at by this much

var (
	oidCheckpointSignerEKU = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 65441, 1, 8}
	oidCMSSignedData       = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 2}
	oidTSTInfo             = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 16, 1, 4}
	oidAttrContentType     = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 3}
	oidAttrMessageDigest   = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 4}
	oidSHA256              = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 2, 1}
	// ML-DSA by public key length: the CMS signature OID and the JWS alg.
	mldsaSigOIDs = map[int]asn1.ObjectIdentifier{
		1312: {2, 16, 840, 1, 101, 3, 4, 3, 17},
		1952: {2, 16, 840, 1, 101, 3, 4, 3, 18},
		2592: {2, 16, 840, 1, 101, 3, 4, 3, 19},
	}
	mldsaJWSAlgs = map[int]string{1312: "ML-DSA-44", 1952: "ML-DSA-65", 2592: "ML-DSA-87"}
)

// checkpointRefusal names the step that failed, by the vectors' refusal code.
type checkpointRefusal struct{ code, why string }

func (r *checkpointRefusal) Error() string { return "checkpoint " + r.code + ": " + r.why }

func cpRefuse(code, format string, a ...any) error {
	return &checkpointRefusal{code, fmt.Sprintf(format, a...)}
}

// postboxCheckpoint is what a verified checkpoint binds.
type postboxCheckpoint struct {
	maxID   int64
	head    []byte    // hash_of_head: the entry_hash at max_id
	genTime time.Time // the TSA's time: the device's trusted time (AAA D9)
	// tenantWide: the body's target is empty, the tenant's chain rather than
	// this device's.
	tenantWide bool
	// inter is the checkpoint x5c beyond its leaf: the platform intermediates
	// an entry's TSA token chains through (the token embeds only its signer).
	inter []*x509.Certificate
}

// verifyCheckpoint runs README steps 1-10 against the pinned platform roots,
// this device's tenant and SPIFFE URI, the checkpoint mark and trusted now.
func verifyCheckpoint(raw []byte, roots *x509.CertPool, tenant, target string, mark int64, now time.Time) (postboxCheckpoint, error) {
	// 1. All three members present and non-empty.
	var a struct {
		Body           string `json:"body"`
		Signature      string `json:"signature"`
		TimestampToken string `json:"timestamp_token"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return postboxCheckpoint{}, cpRefuse("malformed", "artifact JSON: %v", err)
	}
	if a.Body == "" || a.Signature == "" || a.TimestampToken == "" {
		return postboxCheckpoint{}, cpRefuse("malformed", "artifact requires body, signature and timestamp_token")
	}
	body, err := b64any(a.Body)
	if err != nil {
		return postboxCheckpoint{}, cpRefuse("malformed", "body: %v", err)
	}
	token, err := b64any(a.TimestampToken)
	if err != nil {
		return postboxCheckpoint{}, cpRefuse("malformed", "timestamp_token: %v", err)
	}

	// 2, 2b. Authorship.
	payload, chain, err := verifyCheckpointJWS(a.Signature, roots, now)
	if err != nil {
		return postboxCheckpoint{}, err
	}
	// 3. The signed payload is the body, byte for byte.
	if !bytes.Equal(payload, body) {
		return postboxCheckpoint{}, cpRefuse("signature_body_mismatch", "JWS payload is not the artifact body")
	}
	// 4, 4b. The RFC 3161 token over SHA-256(body as received).
	genTime, err := verifyTimestampToken(token, body, chain[1:], roots)
	if err != nil {
		return postboxCheckpoint{}, err
	}

	// 5. Exactly the seven members, v = 1.
	if err := exactJSONMembers(body, "expires_at", "hash_of_head", "max_id", "notarized_at", "target", "tenant", "v"); err != nil {
		return postboxCheckpoint{}, cpRefuse("body", "%v", err)
	}
	var b struct {
		V           int    `json:"v"`
		Tenant      string `json:"tenant"`
		Target      string `json:"target"`
		MaxID       int64  `json:"max_id"`
		HashOfHead  string `json:"hash_of_head"`
		NotarizedAt string `json:"notarized_at"`
		ExpiresAt   string `json:"expires_at"`
	}
	if err := strictJSON(body, &b); err != nil {
		return postboxCheckpoint{}, cpRefuse("body", "decode: %v", err)
	}
	if b.V != 1 {
		return postboxCheckpoint{}, cpRefuse("body", "unsupported version %d", b.V)
	}
	head, err := base64.RawURLEncoding.DecodeString(b.HashOfHead)
	notarized, err1 := time.Parse(time.RFC3339, b.NotarizedAt)
	expires, err2 := time.Parse(time.RFC3339, b.ExpiresAt)
	if err != nil || len(head) != sha256.Size || err1 != nil || err2 != nil || b.Tenant == "" || b.MaxID < 0 || !expires.After(notarized) {
		return postboxCheckpoint{}, cpRefuse("body", "missing or malformed member")
	}
	// 6, 7.
	if b.Tenant != tenant {
		return postboxCheckpoint{}, cpRefuse("tenant_mismatch", "tenant %s", b.Tenant)
	}
	if b.Target != target && b.Target != "" {
		return postboxCheckpoint{}, cpRefuse("target_mismatch", "target %s", b.Target)
	}
	// 8. genTime bounds the body's own claim (no upper limit: late is fail-safe).
	if genTime.Before(notarized.Add(-postboxCheckpointSkew)) {
		return postboxCheckpoint{}, cpRefuse("skew", "genTime %s before notarized_at %s", genTime, notarized)
	}
	// 9, 10. A tenant-wide checkpoint (empty target) is on the tenant's chain,
	// not this device's: its max_id is never compared with this device's mark
	// (WDY-4015 item 9).
	if b.Target != "" && b.MaxID < mark {
		return postboxCheckpoint{}, cpRefuse("rollback", "max_id %d below the checkpoint mark %d", b.MaxID, mark)
	}
	if now.After(expires) {
		return postboxCheckpoint{}, cpRefuse("stale", "expired at %s", expires)
	}
	return postboxCheckpoint{maxID: b.MaxID, head: head, genTime: genTime, inter: chain[1:], tenantWide: b.Target == ""}, nil
}

// verifyCheckpointJWS returns the payload and the x5c chain (leaf first).
func verifyCheckpointJWS(compact string, roots *x509.CertPool, now time.Time) ([]byte, []*x509.Certificate, error) {
	seg := strings.Split(compact, ".")
	if len(seg) != 3 {
		return nil, nil, cpRefuse("signature", "not a compact JWS")
	}
	hb, err := b64url(seg[0])
	if err != nil {
		return nil, nil, cpRefuse("signature", "header: %v", err)
	}
	// {alg, x5c} only: crit, kid and anything else are refused.
	var h struct {
		Alg string   `json:"alg"`
		X5C []string `json:"x5c"`
	}
	if err := strictJSON(hb, &h); err != nil || len(h.X5C) == 0 {
		return nil, nil, cpRefuse("signature", "protected header is not {alg, x5c}: %v", err)
	}
	chain := make([]*x509.Certificate, len(h.X5C))
	inter := x509.NewCertPool()
	for i, s := range h.X5C {
		der, err := base64.StdEncoding.DecodeString(s)
		if err == nil {
			chain[i], err = x509.ParseCertificate(der)
		}
		if err != nil {
			return nil, nil, cpRefuse("signature", "x5c[%d]: %v", i, err)
		}
		if i > 0 {
			inter.AddCert(chain[i])
		}
	}
	leaf := chain[0]
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, Intermediates: inter, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}}); err != nil {
		return nil, nil, cpRefuse("signature", "chain to the platform root: %v", err)
	}
	if !slices.ContainsFunc(leaf.UnknownExtKeyUsage, oidCheckpointSignerEKU.Equal) {
		return nil, nil, cpRefuse("signer_role", "leaf %q lacks the checkpoint-signer EKU", leaf.Subject.CommonName)
	}
	pk, ok := leaf.PublicKey.(*mldsa.PublicKey)
	if !ok || mldsaJWSAlgs[len(pk.Bytes())] != h.Alg {
		return nil, nil, cpRefuse("signature", "alg %q does not match the leaf key", h.Alg)
	}
	sig, err := b64url(seg[2])
	if err != nil || mldsa.Verify(pk, []byte(seg[0]+"."+seg[1]), sig, nil) != nil {
		return nil, nil, cpRefuse("signature", "signature does not verify")
	}
	payload, err := b64url(seg[1])
	if err != nil {
		return nil, nil, cpRefuse("signature", "payload: %v", err)
	}
	return payload, chain, nil
}

type cmsAlgID struct {
	Algorithm  asn1.ObjectIdentifier
	Parameters asn1.RawValue `asn1:"optional"`
}

type cmsSignerInfo struct {
	Version int
	SID     struct {
		Issuer       asn1.RawValue
		SerialNumber *big.Int
	}
	DigestAlgorithm    cmsAlgID
	SignedAttrs        asn1.RawValue `asn1:"optional,tag:0"`
	SignatureAlgorithm cmsAlgID
	Signature          []byte
	UnsignedAttrs      asn1.RawValue `asn1:"optional,tag:1"`
}

type cmsSignedData struct {
	Version          int
	DigestAlgorithms asn1.RawValue
	EncapContentInfo struct {
		EContentType asn1.ObjectIdentifier
		EContent     []byte `asn1:"explicit,tag:0,optional"`
	}
	Certificates asn1.RawValue   `asn1:"optional,tag:0"`
	CRLs         asn1.RawValue   `asn1:"optional,tag:1"`
	SignerInfos  []cmsSignerInfo `asn1:"set"`
}

type tstInfo struct {
	Version        int
	Policy         asn1.ObjectIdentifier
	MessageImprint struct {
		HashAlgorithm cmsAlgID
		HashedMessage []byte
	}
	SerialNumber *big.Int
	GenTime      time.Time     `asn1:"generalized"`
	Rest         asn1.RawValue `asn1:"optional"`
}

// verifyTimestampToken checks an RFC 3161 TimeStampResp over SHA-256(stamped)
// (a checkpoint body or an entry's request_jws) and returns its genTime. The
// token embeds only the TSA signer leaf; candidate intermediates are inter
// (the checkpoint x5c beyond its leaf) and the token's other certificates,
// and each must still chain to the pinned root.
func verifyTimestampToken(der, stamped []byte, inter []*x509.Certificate, roots *x509.CertPool) (time.Time, error) {
	var resp struct {
		Status struct {
			Status int
			Rest   asn1.RawValue `asn1:"optional"`
		}
		TimeStampToken asn1.RawValue `asn1:"optional"`
	}
	if _, err := asn1.Unmarshal(der, &resp); err != nil || resp.Status.Status != 0 || len(resp.TimeStampToken.FullBytes) == 0 {
		return time.Time{}, cpRefuse("timestamp", "not a granted TimeStampResp")
	}
	var ci struct {
		ContentType asn1.ObjectIdentifier
		Content     asn1.RawValue `asn1:"explicit,tag:0"`
	}
	if _, err := asn1.Unmarshal(resp.TimeStampToken.FullBytes, &ci); err != nil || !ci.ContentType.Equal(oidCMSSignedData) {
		return time.Time{}, cpRefuse("timestamp", "token is not SignedData")
	}
	var sd cmsSignedData
	if _, err := asn1.Unmarshal(ci.Content.Bytes, &sd); err != nil {
		return time.Time{}, cpRefuse("timestamp", "SignedData: %v", err)
	}
	if len(sd.SignerInfos) != 1 || !sd.EncapContentInfo.EContentType.Equal(oidTSTInfo) || len(sd.EncapContentInfo.EContent) == 0 {
		return time.Time{}, cpRefuse("timestamp", "want one SignerInfo over an attached TSTInfo")
	}
	si := sd.SignerInfos[0]

	// The signer is the embedded certificate the SignerInfo names, never a
	// position in the list.
	var signer *x509.Certificate
	pool := x509.NewCertPool()
	for _, c := range inter {
		pool.AddCert(c)
	}
	for rest := sd.Certificates.Bytes; len(rest) > 0; {
		var raw asn1.RawValue
		var err error
		if rest, err = asn1.Unmarshal(rest, &raw); err != nil {
			return time.Time{}, cpRefuse("timestamp", "certificates: %v", err)
		}
		c, err := x509.ParseCertificate(raw.FullBytes)
		if err != nil {
			return time.Time{}, cpRefuse("timestamp", "certificates: %v", err)
		}
		if bytes.Equal(c.RawIssuer, si.SID.Issuer.FullBytes) && si.SID.SerialNumber != nil && c.SerialNumber.Cmp(si.SID.SerialNumber) == 0 {
			if signer != nil {
				return time.Time{}, cpRefuse("timestamp", "signer is ambiguous")
			}
			signer = c
		} else {
			pool.AddCert(c)
		}
	}
	if signer == nil {
		return time.Time{}, cpRefuse("timestamp", "signer certificate not embedded")
	}
	pk, ok := signer.PublicKey.(*mldsa.PublicKey)
	if !ok || !si.SignatureAlgorithm.Algorithm.Equal(mldsaSigOIDs[len(pk.Bytes())]) {
		return time.Time{}, cpRefuse("timestamp", "signature algorithm does not match the signer key")
	}

	// Signed attributes: content-type TSTInfo, message-digest = SHA-256(TSTInfo).
	if !si.DigestAlgorithm.Algorithm.Equal(oidSHA256) || len(si.SignedAttrs.FullBytes) == 0 {
		return time.Time{}, cpRefuse("timestamp", "want SHA-256 signed attributes")
	}
	var ct asn1.ObjectIdentifier
	var md []byte
	for rest := si.SignedAttrs.Bytes; len(rest) > 0; {
		var at struct {
			Type  asn1.ObjectIdentifier
			Value asn1.RawValue `asn1:"set"`
		}
		var err error
		if rest, err = asn1.Unmarshal(rest, &at); err != nil {
			return time.Time{}, cpRefuse("timestamp", "signed attribute: %v", err)
		}
		switch {
		case at.Type.Equal(oidAttrContentType):
			_, _ = asn1.Unmarshal(at.Value.Bytes, &ct)
		case at.Type.Equal(oidAttrMessageDigest):
			_, _ = asn1.Unmarshal(at.Value.Bytes, &md)
		}
	}
	sum := sha256.Sum256(sd.EncapContentInfo.EContent)
	if !ct.Equal(oidTSTInfo) || !bytes.Equal(md, sum[:]) {
		return time.Time{}, cpRefuse("timestamp", "signed attributes do not bind the TSTInfo")
	}
	// Pure ML-DSA over the signed attributes re-tagged as a SET (RFC 5652 5.4).
	set := append([]byte{0x31}, si.SignedAttrs.FullBytes[1:]...)
	if mldsa.Verify(pk, set, si.Signature, nil) != nil {
		return time.Time{}, cpRefuse("timestamp", "CMS signature does not verify")
	}

	var tst tstInfo
	if _, err := asn1.Unmarshal(sd.EncapContentInfo.EContent, &tst); err != nil {
		return time.Time{}, cpRefuse("timestamp", "TSTInfo: %v", err)
	}
	// The TSA chain is checked at genTime, now that the signature vouches for it.
	if _, err := signer.Verify(x509.VerifyOptions{Roots: roots, Intermediates: pool, CurrentTime: tst.GenTime, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageTimeStamping}}); err != nil {
		return time.Time{}, cpRefuse("timestamp", "TSA chain: %v", err)
	}
	h := sha256.Sum256(stamped)
	if !tst.MessageImprint.HashAlgorithm.Algorithm.Equal(oidSHA256) || !bytes.Equal(tst.MessageImprint.HashedMessage, h[:]) {
		return time.Time{}, cpRefuse("imprint_mismatch", "token imprint is not SHA-256 of the stamped bytes")
	}
	return tst.GenTime, nil
}

// exactJSONMembers refuses a JSON object whose member names are not exactly
// names: an unknown, duplicate, missing or case-folded member (encoding/json
// matches names case-insensitively, so this runs before decoding).
func exactJSONMembers(data []byte, names ...string) error {
	d := json.NewDecoder(bytes.NewReader(data))
	if t, err := d.Token(); err != nil || t != json.Delim('{') {
		return fmt.Errorf("not a JSON object")
	}
	seen := map[string]bool{}
	for d.More() {
		t, err := d.Token()
		if err != nil {
			return err
		}
		k, _ := t.(string)
		if !slices.Contains(names, k) || seen[k] {
			return fmt.Errorf("unknown or duplicate member %q", k)
		}
		seen[k] = true
		var skip json.RawMessage
		if err := d.Decode(&skip); err != nil {
			return err
		}
	}
	if len(seen) != len(names) {
		return fmt.Errorf("want members %v", names)
	}
	return nil
}

// b64any accepts standard or URL-safe base64, padded or not (pki-core
// postbox.DecodeArtifact).
func b64any(s string) ([]byte, error) {
	for _, e := range []*base64.Encoding{base64.RawURLEncoding, base64.URLEncoding, base64.StdEncoding, base64.RawStdEncoding} {
		if b, err := e.DecodeString(s); err == nil {
			return b, nil
		}
	}
	return nil, fmt.Errorf("not base64")
}
