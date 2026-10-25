package enrollment

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	cloudpb "github.com/wendylabsinc/WendyOS/go/wendy-core/core/enrollment/internal/cloudpb"
	"github.com/wendylabsinc/WendyOS/go/wendy-core/platform"
	"google.golang.org/protobuf/proto"
)

type resetClaims struct {
	Iss         string `json:"iss"`
	Aud         string `json:"aud"`
	Iat         int64  `json:"iat"`
	Exp         int64  `json:"exp"`
	JTI         string `json:"jti"`
	Correlation string `json:"correlation_id"`
	Deletion    []byte `json:"deletion"`
	Request     []byte `json:"request_body"`
	OperatorJWS string `json:"request_jws"`
}

// ResetDeletion extracts untrusted, bounded bytes for signature checking.
// Extraction alone never authorizes a reset.
func ResetDeletion(token []byte) ([]byte, error) {
	if len(token) == 0 || len(token) > 65536 {
		return nil, fmt.Errorf("missing or oversized Cloud reset authorization")
	}
	parts := strings.Split(string(token), ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("Cloud reset authorization required")
	}
	body, err := base64.RawURLEncoding.Strict().DecodeString(parts[1])
	if err != nil {
		return nil, err
	}
	var c resetClaims
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	if d.Decode(&c) != nil || !canonicalObject(body) || len(c.Deletion) == 0 || len(c.Deletion) > 4096 {
		return nil, fmt.Errorf("invalid Cloud deletion claims")
	}
	return c.Deletion, nil
}

// VerifySavedCloudReset uses a key authenticated at ORIGINAL reset admission.
// Only protected local recovery may trust that saved key. Remote CLI receipts
// must re-establish Cloud key trust independently before trusting this proof.
func VerifySavedCloudReset(ctx context.Context, verifier platform.ProofVerifier, token, key []byte, cloud, principal, fingerprint string, deletion, operatorDER []byte, earliest, latest time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if earliest.IsZero() || latest.Before(earliest) || len(operatorDER) == 0 || verifier == nil || len(token) > 65536 || len(deletion) > 4096 {
		return fmt.Errorf("invalid reset verification bounds")
	}
	issuer, err := ResetIssuer(cloud)
	if err != nil {
		return err
	}
	parts := strings.Split(string(token), ".")
	if len(parts) != 3 {
		return fmt.Errorf("Cloud reset authorization required")
	}
	header, err := base64.RawURLEncoding.Strict().DecodeString(parts[0])
	if err != nil {
		return err
	}
	var h map[string]string
	if json.Unmarshal(header, &h) != nil || len(h) != 3 || h["alg"] != "ML-DSA-65" || h["typ"] != "asset-reset-authorization+jws" || h["kid"] == "" {
		return fmt.Errorf("wrong reset signature purpose")
	}
	if !canonicalObject(header) {
		return fmt.Errorf("noncanonical reset header")
	}
	sig, err := base64.RawURLEncoding.Strict().DecodeString(parts[2])
	if err != nil {
		return err
	}
	if err = verifier.VerifyMLDSA(ctx, key, []byte(parts[0]+"."+parts[1]), sig, "ML-DSA-65", false); err != nil {
		return err
	}
	body, err := base64.RawURLEncoding.Strict().DecodeString(parts[1])
	if err != nil {
		return err
	}
	var c resetClaims
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	if d.Decode(&c) != nil || !canonicalObject(body) {
		return fmt.Errorf("invalid reset claims")
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil || len(fields) != 9 {
		return fmt.Errorf("missing reset claims")
	}
	if c.Iss != issuer || c.Aud != "wendy-agent/unprovision" || c.Iat < 0 || c.Exp <= c.Iat || c.Exp-c.Iat > 30 || c.Iat > latest.Unix() || c.Exp <= latest.Unix() {
		return fmt.Errorf("reset issuer, audience or freshness mismatch")
	}
	if !canonicalUUID(c.JTI) || c.JTI == "00000000-0000-0000-0000-000000000000" {
		return fmt.Errorf("invalid reset replay identity")
	}
	if !bytes.Equal(c.Deletion, deletion) {
		return fmt.Errorf("Cloud deletion bytes differ from signed authorization")
	}
	var deleted cloudpb.DeletedAsset
	if err = proto.Unmarshal(deletion, &deleted); err != nil {
		return err
	}
	identity, err := ParsePrincipal(principal)
	if err != nil {
		return err
	}
	if deleted.GetId() == "" || deleted.GetOrganizationId() != identity.TenantUUID || deleted.GetDeviceId() != identity.EntityID || deleted.GetDeletedAt() == nil || deleted.GetDeletedAt().CheckValid() != nil || deleted.GetDeletedAt().AsTime().After(earliest) {
		return fmt.Errorf("Cloud deletion binding mismatch")
	}
	var request cloudpb.DeleteAssetRequest
	if len(c.Request) > 4096 || proto.Unmarshal(c.Request, &request) != nil || request.GetId() != deleted.GetId() || request.GetExpectedDeviceId() != identity.EntityID || request.GetResetCertificateSha256() != fingerprint {
		return fmt.Errorf("operator reset target mismatch")
	}
	operator, err := verifier.ParseProofCertificate(ctx, operatorDER)
	if err != nil {
		return err
	}
	if earliest.Before(operator.NotBefore) || latest.After(operator.NotAfter) {
		return fmt.Errorf("operator validity mismatch")
	}
	actor, ok := operator.Principal, operator.Principal != ""
	parsed, e := ParsePrincipal(actor)
	if !ok || e != nil || parsed.EntityType != EntityUser || parsed.TenantUUID != identity.TenantUUID {
		return fmt.Errorf("wrong reset operator")
	}
	return VerifyOperatorReset(ctx, verifier, c.OperatorJWS, c.Request, request.GetId(), identity.TenantUUID, c.Correlation, operatorDER, earliest, latest)
}

func canonicalObject(raw []byte) bool {
	var object map[string]any
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if d.Decode(&object) != nil {
		return false
	}
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	if e.Encode(object) != nil {
		return false
	}
	return bytes.Equal(bytes.TrimSuffix(b.Bytes(), []byte{'\n'}), raw)
}

// VerifyOperatorReset verifies a host-checked ML-DSA signature and binds its
// canonical descriptor to the exact request bytes, identity and time interval.
func VerifyOperatorReset(ctx context.Context, verifier platform.ProofVerifier, compact string, request []byte, asset, tenant, correlation string, leafDER []byte, earliest, latest time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if verifier == nil {
		return fmt.Errorf("host proof verifier is required")
	}
	parts := strings.Split(compact, ".")
	if len(parts) != 3 || len(compact) > 32768 {
		return fmt.Errorf("exact operator reset signature required")
	}
	header, err := base64.RawURLEncoding.Strict().DecodeString(parts[0])
	if err != nil {
		return err
	}
	var h struct {
		Alg string   `json:"alg"`
		Kid string   `json:"kid,omitempty"`
		X5C []string `json:"x5c,omitempty"`
	}
	d := json.NewDecoder(bytes.NewReader(header))
	d.DisallowUnknownFields()
	if d.Decode(&h) != nil || !canonicalObject(header) || (h.Alg != "ML-DSA-65" && h.Alg != "ML-DSA-87") {
		return fmt.Errorf("invalid operator header")
	}
	hash := sha256.Sum256(leafDER)
	if (h.Kid == "") == (len(h.X5C) == 0) {
		return fmt.Errorf("operator key reference required")
	}
	if h.Kid != "" && h.Kid != base64.RawURLEncoding.EncodeToString(hash[:]) {
		return fmt.Errorf("operator key mismatch")
	}
	if len(h.X5C) > 0 {
		der, e := base64.StdEncoding.Strict().DecodeString(h.X5C[0])
		if e != nil || !bytes.Equal(der, leafDER) {
			return fmt.Errorf("operator leaf mismatch")
		}
	}
	sig, err := base64.RawURLEncoding.Strict().DecodeString(parts[2])
	if err != nil {
		return err
	}
	if err = verifier.VerifyMLDSA(ctx, leafDER, []byte(parts[0]+"."+parts[1]), sig, h.Alg, true); err != nil {
		return err
	}
	body, err := base64.RawURLEncoding.Strict().DecodeString(parts[1])
	if err != nil {
		return err
	}
	var descriptor struct {
		Audience    string `json:"aud"`
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
	d = json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	if d.Decode(&descriptor) != nil || !canonicalObject(body) {
		return fmt.Errorf("invalid operator descriptor")
	}
	digest := sha256.Sum256(request)
	if descriptor.Audience != "https://cloud.wendy.sh/broker" || descriptor.Operation != "wendycloud.v2.AssetService/DeleteAsset" || descriptor.Target.Tenant != tenant || descriptor.Target.Resource != "asset/"+asset || descriptor.Digest != base64.RawURLEncoding.EncodeToString(digest[:]) || descriptor.Correlation != correlation || correlation == "" || len(correlation) > 64 || descriptor.Iat < 0 || descriptor.Iat > latest.Unix() || descriptor.Expiry <= latest.Unix() || descriptor.Expiry <= descriptor.Iat || descriptor.Expiry-descriptor.Iat > 30 {
		return fmt.Errorf("operator descriptor binding or freshness mismatch")
	}
	nonce, e := base64.RawURLEncoding.Strict().DecodeString(descriptor.Nonce)
	if e != nil || len(nonce) != 32 {
		return fmt.Errorf("invalid operator replay nonce")
	}
	return nil
}
