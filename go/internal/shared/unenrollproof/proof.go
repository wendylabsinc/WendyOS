// Package unenrollproof verifies PKI evidence and authenticates public reset
// receipts. It performs no issuance, revocation, deletion or credential reset.
package unenrollproof

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/mldsa"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"golang.org/x/crypto/ocsp"
)

const MaxProofBytes = 256 * 1024

func Fingerprint(leaf *x509.Certificate) string {
	d := sha256.Sum256(leaf.Raw)
	return hex.EncodeToString(d[:])
}

func Issuer(leaf *x509.Certificate, chain []*x509.Certificate) (*x509.Certificate, error) {
	for _, candidate := range chain {
		if bytes.Equal(leaf.RawIssuer, candidate.RawSubject) && leaf.CheckSignatureFrom(candidate) == nil {
			return candidate, nil
		}
	}
	return nil, fmt.Errorf("installed certificate issuer unavailable")
}

type certID struct {
	Algorithm pkix.AlgorithmIdentifier
	NameHash  []byte
	KeyHash   []byte
	Serial    *big.Int
}
type responseData struct {
	Raw        asn1.RawContent
	Version    int `asn1:"optional,explicit,tag:0,default:0"`
	Responder  asn1.RawValue
	ProducedAt time.Time `asn1:"generalized"`
	Responses  []asn1.RawValue
	Extensions []pkix.Extension `asn1:"optional,explicit,tag:1"`
}
type basicResponse struct {
	Data         asn1.RawValue
	Algorithm    pkix.AlgorithmIdentifier
	Signature    asn1.BitString
	Certificates []asn1.RawValue `asn1:"optional,explicit,tag:0"`
}
type responseBytes struct {
	Type     asn1.ObjectIdentifier
	Response []byte
}
type wireResponse struct {
	Status asn1.Enumerated
	Bytes  responseBytes `asn1:"optional,explicit,tag:0"`
}

// VerifyRevocation accepts only an issuer-signed revoked answer for this leaf.
// The native Go verifier handles ML-DSA, which x/crypto/ocsp's algorithm table
// does not yet recognize. Delegated responders are deliberately not accepted.
func VerifyRevocation(raw []byte, leaf, issuer *x509.Certificate, now time.Time) error {
	if len(raw) == 0 || len(raw) > MaxProofBytes {
		return fmt.Errorf("missing or oversized revocation evidence")
	}
	if leaf == nil || issuer == nil || leaf.CheckSignatureFrom(issuer) != nil {
		return fmt.Errorf("invalid installed certificate issuer")
	}
	answer, err := ocsp.ParseResponseForCert(raw, leaf, nil)
	if err != nil {
		return fmt.Errorf("invalid OCSP evidence: %w", err)
	}
	if answer.Certificate != nil || answer.Status != ocsp.Revoked || answer.SerialNumber.Cmp(leaf.SerialNumber) != 0 {
		return fmt.Errorf("OCSP does not confirm installed-leaf revocation")
	}
	if answer.ThisUpdate.After(now) || answer.ProducedAt.After(now) || answer.RevokedAt.After(now) || answer.NextUpdate.IsZero() || !now.Before(answer.NextUpdate) || answer.RevokedAt.IsZero() {
		return fmt.Errorf("stale or future revocation evidence")
	}
	var outer wireResponse
	rest, err := asn1.Unmarshal(raw, &outer)
	if err != nil || len(rest) != 0 || outer.Status != 0 || !outer.Bytes.Type.Equal(asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 7, 48, 1, 1}) {
		return fmt.Errorf("invalid OCSP envelope")
	}
	var basic basicResponse
	rest, err = asn1.Unmarshal(outer.Bytes.Response, &basic)
	if err != nil || len(rest) != 0 || len(basic.Certificates) != 0 {
		return fmt.Errorf("unsupported OCSP signer")
	}
	alg := answer.SignatureAlgorithm
	switch basic.Algorithm.Algorithm.String() {
	case "2.16.840.1.101.3.4.3.17":
		alg = x509.MLDSA44
	case "2.16.840.1.101.3.4.3.18":
		alg = x509.MLDSA65
	case "2.16.840.1.101.3.4.3.19":
		alg = x509.MLDSA87
	}
	if err := issuer.CheckSignature(alg, answer.TBSResponseData, answer.Signature); err != nil {
		return fmt.Errorf("invalid PKI revocation signature: %w", err)
	}
	var data responseData
	rest, err = asn1.Unmarshal(answer.TBSResponseData, &data)
	if err != nil || len(rest) != 0 || len(data.Responses) != 1 {
		return fmt.Errorf("ambiguous OCSP response")
	}
	var id certID
	_, err = asn1.Unmarshal(data.Responses[0].Bytes, &id)
	if err != nil {
		return fmt.Errorf("invalid OCSP certificate identity")
	}
	request, err := ocsp.CreateRequest(leaf, issuer, &ocsp.RequestOptions{Hash: answer.IssuerHash})
	if err != nil {
		return err
	}
	wanted, err := ocsp.ParseRequest(request)
	if err != nil || !bytes.Equal(id.NameHash, wanted.IssuerNameHash) || !bytes.Equal(id.KeyHash, wanted.IssuerKeyHash) {
		return fmt.Errorf("OCSP issuer binding mismatch")
	}
	var spki struct {
		Algorithm pkix.AlgorithmIdentifier
		Key       asn1.BitString
	}
	if _, err := asn1.Unmarshal(issuer.RawSubjectPublicKeyInfo, &spki); err != nil {
		return err
	}
	keyHash := sha1.Sum(spki.Key.RightAlign())
	if len(answer.RawResponderName) != 0 {
		if !bytes.Equal(answer.RawResponderName, issuer.RawSubject) {
			return fmt.Errorf("wrong OCSP responder")
		}
	} else if !bytes.Equal(answer.ResponderKeyHash, keyHash[:]) {
		return fmt.Errorf("wrong OCSP responder key")
	}
	return nil
}

func FetchRevocation(ctx context.Context, leaf, issuer *x509.Certificate) ([]byte, error) {
	request, err := ocsp.CreateRequest(leaf, issuer, nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 15 * time.Second}
	for _, endpoint := range leaf.OCSPServer {
		parsed, err := url.Parse(endpoint)
		if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || parsed.User != nil {
			continue
		}
		// PKI's existing RFC 6960 HTTP responder supports POST. No authentication
		// credentials are sent; the response signature, not HTTP, is the evidence.
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, parsed.String(), bytes.NewReader(request))
		if err != nil {
			continue
		}
		req.Header.Set("Content-Type", "application/ocsp-request")
		req.Header.Set("Accept", "application/ocsp-response")
		response, err := client.Do(req)
		if err != nil {
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, MaxProofBytes+1))
		response.Body.Close()
		if readErr == nil && response.StatusCode == http.StatusOK && VerifyRevocation(body, leaf, issuer, time.Now()) == nil {
			return body, nil
		}
	}
	return nil, fmt.Errorf("PKI has not supplied authenticated installed-leaf revocation evidence; keys retained")
}

// Completion is public, device-signed evidence, never a private key or bearer
// credential. CloudDeletion is the exact serialized typed binding evidence.
type Completion struct {
	Version       int    `json:"version"`
	Principal     string `json:"principal"`
	Cloud         string `json:"cloud"`
	AssetID       string `json:"assetId"`
	Fingerprint   string `json:"certificateSHA256"`
	AuthorizedAt  int64  `json:"authorizedAt"`
	Certificate   []byte `json:"certificate"`
	Chain         string `json:"chain"`
	Revocation    []byte `json:"revocation"`
	CloudDeletion []byte `json:"cloudDeletion"`
}
type envelope struct {
	Body      []byte `json:"body"`
	Signature []byte `json:"signature"`
}

func SignCompletion(record Completion, signer crypto.Signer) ([]byte, error) {
	record.Version = 1
	body, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}
	input, options := signingInput(body, signer.Public())
	signature, err := signer.Sign(rand.Reader, input, options)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope{body, signature})
}
func signingInput(body []byte, key crypto.PublicKey) ([]byte, crypto.SignerOpts) {
	if _, ok := key.(*mldsa.PublicKey); ok {
		return body, &mldsa.Options{}
	}
	digest := sha256.Sum256(body)
	return digest[:], crypto.SHA256
}

// ReadCompletion authenticates the receipt and its PKI evidence. Trust-chain
// anchoring is additionally required by the caller against its existing roots.
func ReadCompletion(raw []byte) (*Completion, *x509.Certificate, error) {
	if len(raw) == 0 || len(raw) > 4*MaxProofBytes {
		return nil, nil, fmt.Errorf("missing or oversized completion receipt")
	}
	var signed envelope
	if err := json.Unmarshal(raw, &signed); err != nil {
		return nil, nil, err
	}
	canonicalEnvelope, _ := json.Marshal(signed)
	if !bytes.Equal(canonicalEnvelope, raw) {
		return nil, nil, fmt.Errorf("noncanonical completion envelope")
	}
	var record Completion
	if err := json.Unmarshal(signed.Body, &record); err != nil {
		return nil, nil, err
	}
	canonical, _ := json.Marshal(record)
	if !bytes.Equal(canonical, signed.Body) || record.Version != 1 {
		return nil, nil, fmt.Errorf("noncanonical completion receipt")
	}
	leaf, err := x509.ParseCertificate(record.Certificate)
	if err != nil {
		return nil, nil, err
	}
	principal, ok := certs.TenantPrincipalFromCert(leaf)
	if !ok || principal != record.Principal || Fingerprint(leaf) != record.Fingerprint {
		return nil, nil, fmt.Errorf("completion identity mismatch")
	}
	input, _ := signingInput(signed.Body, leaf.PublicKey)
	switch key := leaf.PublicKey.(type) {
	case *mldsa.PublicKey:
		err = mldsa.Verify(key, signed.Body, signed.Signature, nil)
	case *ecdsa.PublicKey:
		if !ecdsa.VerifyASN1(key, input, signed.Signature) {
			err = fmt.Errorf("invalid completion signature")
		}
	case *rsa.PublicKey:
		err = rsa.VerifyPKCS1v15(key, crypto.SHA256, input, signed.Signature)
	default:
		err = fmt.Errorf("unsupported completion key")
	}
	if err != nil {
		return nil, nil, err
	}
	chain, _ := certs.ParseCertsFromPEM([]byte(record.Chain))
	issuer, err := Issuer(leaf, chain)
	if err != nil {
		return nil, nil, err
	}
	if err := VerifyRevocation(record.Revocation, leaf, issuer, time.Unix(record.AuthorizedAt, 0)); err != nil {
		return nil, nil, err
	}
	return &record, leaf, nil
}
