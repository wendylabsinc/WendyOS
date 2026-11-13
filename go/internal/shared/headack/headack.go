// Package headack verifies a device's last signed postbox head ack as cloud
// serves it to operators (PostboxAckService.GetLastHeadAck), per the client
// rule and vectors in service-protos conformance/reqsig ("Head-ack read:
// client rule", head_ack_read_vectors).
//
// The ack is device-signed and cloud only relays it: cloud can withhold an
// ack, never forge one. The console's staleness alarm therefore runs on the
// signed observed_at this package returns, never on cloud-asserted fields.
package headack

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/mldsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/protobuf/proto"
)

const (
	payloadType    = "wendycloud.v2.PostboxHeadAck"
	ackOperation   = "wendycloud.v2.DevicePostboxService/AckHead"
	brokerAudience = "https://cloud.wendy.sh/broker"
	maxJWS         = 32768
)

// Refusal names the client-rule check an ack failed. The names are the
// vectors' refusal labels plus the checks no vector covers; they are not wire
// values, and every refusal reads the same to the user.
type Refusal string

func (r Refusal) Error() string { return "head ack refused: " + string(r) }

const (
	NeverAcked            Refusal = "never_acked"
	WrongPayloadType      Refusal = "wrong_payload_type"
	Malformed             Refusal = "malformed"
	WrongTenant           Refusal = "wrong_tenant"
	WrongSigner           Refusal = "wrong_signer"
	ChainMissing          Refusal = "chain_missing"
	AlgorithmNotIssuer    Refusal = "algorithm_not_issuer"
	LiteUnsupported       Refusal = "lite_unsupported"
	BadSignature          Refusal = "bad_signature"
	ClaimsMismatch        Refusal = "claims_mismatch"
	BodyDigestMismatch    Refusal = "body_digest_mismatch"
	SignedInFuture        Refusal = "signed_in_future"
	LeafExpiredAtIat      Refusal = "leaf_expired_at_iat"
	UnpinnedRoot          Refusal = "unpinned_root"
	EntryHashMissing      Refusal = "entry_hash_missing"
	EntryHashMismatch     Refusal = "entry_hash_mismatch"
	HeadBelowAck          Refusal = "head_below_ack"
	HeadHashMismatch      Refusal = "head_hash_mismatch"
	EntryIDRegressed      Refusal = "entry_id_regressed"
	ObservedAtRegressed   Refusal = "observed_at_regressed"
	EntryHashEquivocation Refusal = "entry_hash_equivocation"
)

// Ack is the signed PostboxHeadAck a client accepted for one device. Verify
// returns it, and the caller keeps it per device and passes it back on the
// next read: the zero Ack is first contact.
type Ack struct {
	EntryID    int64     `json:"entryId"`
	EntryHash  []byte    `json:"entryHash"`
	ObservedAt time.Time `json:"observedAt"`
}

// Verify checks resp for device (its SPIFFE URI) in organizationID against
// the pinned platform root and the ack kept from the last accepted read, and
// returns the signed ack to show and keep. A client that lost kept is at first
// contact: an older ack then passes, and only the staleness alarm covers it.
// Any failure is a Refusal.
func Verify(resp *cloudpbv2.GetLastHeadAckResponse, root *x509.Certificate, device, organizationID string, kept Ack) (Ack, error) {
	return verify(resp, root, device, organizationID, kept, time.Now())
}

func verify(resp *cloudpbv2.GetLastHeadAckResponse, root *x509.Certificate, device, organizationID string, kept Ack, now time.Time) (Ack, error) {
	if root == nil {
		return Ack{}, errors.New("no pinned platform root")
	}
	if id, err := certs.ParseDevicePrincipal(device); err != nil || id.TenantUUID != organizationID {
		return Ack{}, errors.New("device is not a device of the organization")
	}
	// Step 1.
	req := resp.GetAck()
	if req == nil {
		return Ack{}, NeverAcked
	}
	// Step 2: the ack verifies exactly like a device's key registration.
	if req.GetPayloadType() != payloadType {
		return Ack{}, WrongPayloadType
	}
	compact := string(req.GetSignature())
	parts := strings.Split(compact, ".")
	if len(compact) > maxJWS || len(parts) != 3 {
		return Ack{}, Malformed
	}
	var header struct {
		Alg string   `json:"alg"`
		X5C []string `json:"x5c"`
	}
	// kid and crit are refused here.
	if err := strictJSON(parts[0], &header); err != nil || len(header.X5C) != 1 {
		return Ack{}, Malformed
	}
	leafDER, err := base64.StdEncoding.Strict().DecodeString(header.X5C[0])
	if err != nil {
		return Ack{}, Malformed
	}
	leaf, err := x509.ParseCertificate(leafDER)
	if err != nil {
		return Ack{}, Malformed
	}
	san, ok := certs.TenantPrincipalFromCert(leaf)
	if !ok {
		return Ack{}, WrongSigner
	}
	if id, err := certs.ParsePrincipal(san); err != nil || id.TenantUUID != organizationID {
		return Ack{}, WrongTenant
	}
	if san != device {
		return Ack{}, WrongSigner
	}

	// The issuing CA fixes the algorithm, never the header: ML-DSA-65 under
	// the ML-DSA Device Identity CA, ES256 under the P-256 Lite Device CA.
	// Lite (tier C) acks are refused until a tier C device calls AckHead
	// (service-protos todo es256-tier-c-valid).
	if len(resp.GetChainDer()) == 0 {
		return Ack{}, ChainMissing
	}
	chain := make([]*x509.Certificate, 0, len(resp.GetChainDer()))
	for _, der := range resp.GetChainDer() {
		c, err := x509.ParseCertificate(der)
		if err != nil {
			return Ack{}, Malformed
		}
		chain = append(chain, c)
	}
	if !bytes.Equal(chain[0].RawSubject, leaf.RawIssuer) {
		return Ack{}, UnpinnedRoot
	}
	pub, isMLDSA := leaf.PublicKey.(*mldsa.PublicKey)
	_, caIsMLDSA := chain[0].PublicKey.(*mldsa.PublicKey)
	switch {
	case isMLDSA && caIsMLDSA && header.Alg == "ML-DSA-65" && pub.Parameters() == mldsa.MLDSA65():
	case isP256(leaf.PublicKey) && isP256(chain[0].PublicKey) && header.Alg == "ES256":
		return Ack{}, LiteUnsupported
	default:
		return Ack{}, AlgorithmNotIssuer
	}
	sig, err := base64.RawURLEncoding.Strict().DecodeString(parts[2])
	if err != nil {
		return Ack{}, Malformed
	}
	if mldsa.Verify(pub, []byte(parts[0]+"."+parts[1]), sig, nil) != nil {
		return Ack{}, BadSignature
	}

	var claims struct {
		Aud         string `json:"aud"`
		Digest      string `json:"body_sha256"`
		Correlation string `json:"correlation_id"`
		Expiry      int64  `json:"expiry"`
		Iat         int64  `json:"iat"`
		Nonce       string `json:"nonce"`
		Operation   string `json:"operation"`
		Target      struct {
			Resource string `json:"resource"`
			Tenant   string `json:"tenant"`
		} `json:"target"`
	}
	if strictJSON(parts[1], &claims) != nil {
		return Ack{}, Malformed
	}
	if claims.Aud != brokerAudience || claims.Operation != ackOperation || claims.Target.Tenant != organizationID || claims.Target.Resource != device {
		return Ack{}, ClaimsMismatch
	}
	sum := sha256.Sum256(req.GetPayload())
	if claims.Digest != base64.RawURLEncoding.EncodeToString(sum[:]) {
		return Ack{}, BodyDigestMismatch
	}
	if claims.Iat > now.Add(time.Minute).Unix() {
		return Ack{}, SignedInFuture
	}
	at := time.Unix(claims.Iat, 0)
	if at.Before(leaf.NotBefore) || at.After(leaf.NotAfter) {
		return Ack{}, LeafExpiredAtIat
	}
	if certs.VerifyPeerCertificateChain(leaf, chain, []*x509.Certificate{root}, x509.ExtKeyUsageClientAuth, at, at) != nil {
		return Ack{}, UnpinnedRoot
	}
	ack, err := decodeAck(req.GetPayload())
	if err != nil {
		return Ack{}, Malformed
	}

	// Step 3. acked_entry_hash and postbox_head_entry_hash are cloud-asserted:
	// they catch cloud's store disagreeing with the device, not a lying cloud.
	// An ack without a signed hash names no entry: absent cloud hashes would
	// match it, and an empty kept hash would hide equivocation.
	if len(ack.EntryHash) == 0 {
		return Ack{}, EntryHashMissing
	}
	if !bytes.Equal(resp.AckedEntryHash, ack.EntryHash) {
		return Ack{}, EntryHashMismatch
	}
	// Step 4. A head above the ack is a display hint.
	head := resp.GetPostboxHeadEntryId()
	if head < ack.EntryID {
		return Ack{}, HeadBelowAck
	}
	if head == ack.EntryID && !bytes.Equal(resp.PostboxHeadEntryHash, ack.EntryHash) {
		return Ack{}, HeadHashMismatch
	}
	// Step 5. An equal entry_id with the kept hash is the same ack served again.
	if ack.EntryID < kept.EntryID {
		return Ack{}, EntryIDRegressed
	}
	if ack.ObservedAt.Before(kept.ObservedAt) {
		return Ack{}, ObservedAtRegressed
	}
	if ack.EntryID == kept.EntryID && kept.EntryHash != nil && !bytes.Equal(ack.EntryHash, kept.EntryHash) {
		return Ack{}, EntryHashEquivocation
	}
	return ack, nil
}

func isP256(key any) bool {
	k, ok := key.(*ecdsa.PublicKey)
	return ok && k.Curve == elliptic.P256()
}

// decodeAck decodes the signed PostboxHeadAck and refuses any encoding other
// than the canonical one, so no two parsers can read different acks from it.
func decodeAck(payload []byte) (Ack, error) {
	var m cloudpbv2.PostboxHeadAck
	if err := proto.Unmarshal(payload, &m); err != nil {
		return Ack{}, err
	}
	if len(m.ProtoReflect().GetUnknown()) != 0 || m.GetObservedAt() == nil || !m.GetObservedAt().IsValid() {
		return Ack{}, errors.New("not a PostboxHeadAck")
	}
	if again, err := (proto.MarshalOptions{Deterministic: true}).Marshal(&m); err != nil || !bytes.Equal(again, payload) {
		return Ack{}, errors.New("PostboxHeadAck is not canonically encoded")
	}
	return Ack{EntryID: m.GetEntryId(), EntryHash: m.GetEntryHash(), ObservedAt: m.GetObservedAt().AsTime()}, nil
}

// strictJSON decodes the base64url segment into v and refuses it unless it is
// exactly v's canonical encoding: sorted keys, no whitespace, every field
// present, keys matched case-sensitively, no duplicate or unknown keys.
func strictJSON(segment string, v any) error {
	raw, err := base64.RawURLEncoding.Strict().DecodeString(segment)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	again, err := json.Marshal(v)
	if err != nil {
		return err
	}
	var object map[string]any
	dec = json.NewDecoder(bytes.NewReader(again))
	dec.UseNumber()
	if err := dec.Decode(&object); err != nil {
		return err
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(object); err != nil {
		return err
	}
	if !bytes.Equal(bytes.TrimSuffix(b.Bytes(), []byte{'\n'}), raw) {
		return errors.New("JSON is not in canonical form")
	}
	return nil
}
