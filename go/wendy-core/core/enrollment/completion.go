package enrollment

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/wendylabsinc/WendyOS/go/wendy-core/platform"
	"time"
)

// MaxProofBytes bounds a single public revocation proof.
const MaxProofBytes = 256 * 1024

// Completion is public device-signed reset evidence, never a credential.
type Completion struct {
	Version              int    `json:"version"`
	Principal            string `json:"principal"`
	Cloud                string `json:"cloud"`
	AssetID              string `json:"assetId"`
	Fingerprint          string `json:"certificateSHA256"`
	AuthorizedAt         int64  `json:"authorizedAt"`
	AuthorizedAtNanos    int64  `json:"authorizedAtNanos,omitempty"`
	Certificate          []byte `json:"certificate"`
	Chain                string `json:"chain"`
	Revocation           []byte `json:"revocation"`
	CloudDeletion        []byte `json:"cloudDeletion"`
	CloudAuthorization   []byte `json:"cloudAuthorization,omitempty"`
	CloudVerificationKey []byte `json:"cloudVerificationKey,omitempty"`
	ResetOperator        []byte `json:"resetOperator,omitempty"`
}
type envelope struct {
	Body      []byte `json:"body"`
	Signature []byte `json:"signature"`
}

// AuthorizationTime preserves the exact authenticated upper bound in v2.
func (record *Completion) AuthorizationTime() time.Time {
	if record.Version == 2 {
		return time.Unix(0, record.AuthorizedAtNanos)
	}
	return time.Unix(record.AuthorizedAt, 0)
}

// SignCompletion encodes the existing canonical receipt using the device's host
// signer. The host chooses the existing ML-DSA or SHA-256 signing primitive.
func SignCompletion(ctx context.Context, record Completion, signer platform.CompletionSigner) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if signer == nil {
		return nil, fmt.Errorf("host completion signer is required")
	}
	record.Version = 1
	if len(record.CloudAuthorization) != 0 {
		record.Version = 2
	}
	body, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}
	signature, err := signer.SignCompletion(ctx, body)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope{body, signature})
}

// VerifyRevocation requires issuer-signed revoked evidence for the exact leaf.
// Host verification checks certificate/issuer/responder and OCSP CertID binding;
// Core rejects delegated responders, non-revoked status and stale/future evidence.
func VerifyRevocation(ctx context.Context, verifier platform.ReceiptVerifier, raw, leaf, issuer []byte, now time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(raw) == 0 || len(raw) > MaxProofBytes {
		return fmt.Errorf("missing or oversized revocation evidence")
	}
	if verifier == nil {
		return fmt.Errorf("host receipt verifier is required")
	}
	proof, err := verifier.RevocationEvidence(ctx, raw, leaf, issuer)
	if err != nil {
		return err
	}
	if proof.Delegated || !proof.Revoked {
		return fmt.Errorf("OCSP does not confirm installed-leaf revocation")
	}
	if proof.ThisUpdate.After(now) || proof.ProducedAt.After(now) || proof.RevokedAt.After(now) || proof.NextUpdate.IsZero() || !now.Before(proof.NextUpdate) || proof.RevokedAt.IsZero() {
		return fmt.Errorf("stale or future revocation evidence")
	}
	return nil
}

// ReadCompletion verifies canonical encoding, device signature, identity binding,
// revocation and saved Cloud authority. It does not establish root/key trust:
// remote callers must independently anchor the device and reauthenticate Cloud.
func ReadCompletion(ctx context.Context, verifier platform.ReceiptVerifier, raw []byte) (*Completion, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if verifier == nil {
		return nil, fmt.Errorf("host receipt verifier is required")
	}
	if len(raw) == 0 || len(raw) > 4*MaxProofBytes {
		return nil, fmt.Errorf("missing or oversized completion receipt")
	}
	var signed envelope
	if err := json.Unmarshal(raw, &signed); err != nil {
		return nil, err
	}
	canonicalEnvelope, _ := json.Marshal(signed)
	if !bytes.Equal(canonicalEnvelope, raw) {
		return nil, fmt.Errorf("noncanonical completion envelope")
	}
	var record Completion
	if err := json.Unmarshal(signed.Body, &record); err != nil {
		return nil, err
	}
	canonical, _ := json.Marshal(record)
	if !bytes.Equal(canonical, signed.Body) || (record.Version != 1 && record.Version != 2) {
		return nil, fmt.Errorf("noncanonical completion receipt")
	}
	leaf, err := verifier.ParseProofCertificate(ctx, record.Certificate)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(record.Certificate)
	if leaf.Principal == "" || leaf.Principal != record.Principal || hex.EncodeToString(digest[:]) != record.Fingerprint {
		return nil, fmt.Errorf("completion identity mismatch")
	}
	if err := verifier.VerifyCompletionSignature(ctx, record.Certificate, signed.Body, signed.Signature); err != nil {
		return nil, err
	}
	issuer, err := verifier.CertificateIssuer(ctx, record.Certificate, record.Chain)
	if err != nil {
		return nil, err
	}
	at := record.AuthorizationTime()
	if record.Version == 2 && (record.AuthorizedAtNanos <= 0 || at.Unix() != record.AuthorizedAt) {
		return nil, fmt.Errorf("invalid authenticated receipt time")
	}
	if err := VerifyRevocation(ctx, verifier, record.Revocation, record.Certificate, issuer, at); err != nil {
		return nil, err
	}
	if record.Version == 2 {
		if err := VerifySavedCloudReset(ctx, verifier, record.CloudAuthorization, record.CloudVerificationKey, record.Cloud, record.Principal, record.Fingerprint, record.CloudDeletion, record.ResetOperator, at, at); err != nil {
			return nil, err
		}
	}
	return &record, nil
}
