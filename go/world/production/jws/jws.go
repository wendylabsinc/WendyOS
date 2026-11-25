// Package jws implements only the bounded Wendy World ML-DSA-65 compact proof
// format. It has no key lookup, certificate enrollment, trust discovery or clock.
package jws

import (
	"bytes"
	"crypto/mldsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/world/production/wendycert"
)

const (
	MaxCertificates     = 8
	MaxCertificateBytes = 64 << 10
	MaxProofBytes       = 128 << 10
	MaxPayloadBytes     = 64 << 10
	Algorithm           = "ML-DSA-65"
)

var worldType = regexp.MustCompile(`^wendy\.world\.[a-z][a-z0-9.-]{0,80}\.v[1-9][0-9]?$`)

type header struct {
	Algorithm string   `json:"alg"`
	Type      string   `json:"typ"`
	Chain     []string `json:"x5c"`
}

// Verify authenticates received protected-header and payload bytes without
// reserializing them. The caller supplies existing tenant trust, the exact type
// and one trusted time interval; this function never supplies default trust/time.
// Payload schema, application permission and replay admission remain with callers.
func Verify(compact, expectedTyp string, verifier *wendycert.Verifier, earliest, latest time.Time) ([]byte, wendycert.Identity, error) {
	var empty wendycert.Identity
	if verifier == nil || earliest.IsZero() || latest.IsZero() || latest.Before(earliest) || !worldType.MatchString(expectedTyp) || len(compact) == 0 || len(compact) > MaxProofBytes {
		return nil, empty, errors.New("invalid proof bounds, trust, type or time interval")
	}
	parts := strings.Split(compact, ".")
	if len(parts) != 3 {
		return nil, empty, errors.New("compact proof requires three segments")
	}
	protected, err := decodeURL(parts[0], MaxProofBytes)
	if err != nil {
		return nil, empty, err
	}
	var h header
	if decodeStrict(protected, &h, MaxProofBytes) != nil || h.Algorithm != Algorithm || h.Type != expectedTyp {
		return nil, empty, errors.New("unsupported protected header")
	}
	chain, err := decodeChain(h.Chain)
	if err != nil {
		return nil, empty, err
	}
	payload, err := decodeURL(parts[1], MaxPayloadBytes)
	if err != nil {
		return nil, empty, err
	}
	if err = validateJSON(payload, MaxPayloadBytes); err != nil {
		return nil, empty, err
	}
	signature, err := decodeURL(parts[2], mldsa.MLDSA65SignatureSize)
	if err != nil || len(signature) != mldsa.MLDSA65SignatureSize {
		return nil, empty, errors.New("invalid ML-DSA-65 signature encoding")
	}
	identity, err := verifier.Verify(chain, earliest, latest)
	if err != nil {
		return nil, empty, errors.New("signing certificate path or identity refused")
	}
	if identity.VerifySignature([]byte(parts[0]+"."+parts[1]), signature) != nil {
		return nil, empty, errors.New("proof signature refused")
	}
	return payload, identity, nil
}

// Sign uses only the explicitly supplied in-memory key and chain. It checks
// the leaf/key match but does not establish trust or authorization. It signs raw
// protected-header.payload bytes with the FIPS-204 empty context and no prehash.
func Sign(expectedTyp string, payload []byte, chain [][]byte, key *mldsa.PrivateKey) (string, error) {
	if !worldType.MatchString(expectedTyp) || key == nil || key.PublicKey().Parameters() != mldsa.MLDSA65() {
		return "", errors.New("World type and ML-DSA-65 key required")
	}
	if err := validateJSON(payload, MaxPayloadBytes); err != nil {
		return "", err
	}
	encoded, err := encodeChain(chain)
	if err != nil {
		return "", err
	}
	leaf, err := x509.ParseCertificate(chain[0])
	if err != nil {
		return "", errors.New("invalid signing leaf")
	}
	public, err := x509.MarshalPKIXPublicKey(key.Public())
	if err != nil || !bytes.Equal(public, leaf.RawSubjectPublicKeyInfo) {
		return "", errors.New("signing key does not match leaf")
	}
	protected, err := json.Marshal(header{Algorithm: Algorithm, Type: expectedTyp, Chain: encoded})
	if err != nil {
		return "", errors.New("cannot encode protected header")
	}
	signing := base64.RawURLEncoding.EncodeToString(protected) + "." + base64.RawURLEncoding.EncodeToString(payload)
	if len(signing)+1+base64.RawURLEncoding.EncodedLen(mldsa.MLDSA65SignatureSize) > MaxProofBytes {
		return "", errors.New("encoded proof exceeds bound")
	}
	signature, err := key.Sign(nil, []byte(signing), &mldsa.Options{})
	if err != nil || len(signature) != mldsa.MLDSA65SignatureSize {
		return "", errors.New("ML-DSA-65 signing failed")
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}
func decodeURL(s string, limit int) ([]byte, error) {
	if len(s) == 0 || len(s) > base64.RawURLEncoding.EncodedLen(limit) {
		return nil, errors.New("base64url segment exceeds bound")
	}
	b, err := base64.RawURLEncoding.Strict().DecodeString(s)
	if err != nil || len(b) > limit || base64.RawURLEncoding.EncodeToString(b) != s {
		return nil, errors.New("noncanonical base64url segment")
	}
	return b, nil
}
func decodeChain(encoded []string) ([][]byte, error) {
	if len(encoded) == 0 || len(encoded) > MaxCertificates {
		return nil, errors.New("certificate chain length exceeds bound")
	}
	chain := make([][]byte, 0, len(encoded))
	total := 0
	seen := map[string]bool{}
	for _, s := range encoded {
		if len(s) == 0 || len(s) > base64.StdEncoding.EncodedLen(MaxCertificateBytes-total) {
			return nil, errors.New("certificate bytes exceed bound")
		}
		der, err := base64.StdEncoding.Strict().DecodeString(s)
		if err != nil || len(der) == 0 || base64.StdEncoding.EncodeToString(der) != s {
			return nil, errors.New("noncanonical certificate encoding")
		}
		total += len(der)
		if total > MaxCertificateBytes || seen[string(der)] {
			return nil, errors.New("over-budget or duplicate certificate")
		}
		seen[string(der)] = true
		chain = append(chain, der)
	}
	return chain, nil
}
func encodeChain(chain [][]byte) ([]string, error) {
	if len(chain) == 0 || len(chain) > MaxCertificates {
		return nil, errors.New("certificate chain length exceeds bound")
	}
	total := 0
	seen := map[string]bool{}
	encoded := make([]string, 0, len(chain))
	for _, der := range chain {
		if len(der) == 0 || len(der) > MaxCertificateBytes-total || seen[string(der)] {
			return nil, errors.New("over-budget or duplicate certificate")
		}
		total += len(der)
		seen[string(der)] = true
		c, err := x509.ParseCertificate(der)
		if err != nil || !bytes.Equal(c.Raw, der) {
			return nil, errors.New("invalid certificate DER")
		}
		encoded = append(encoded, base64.StdEncoding.EncodeToString(der))
	}
	return encoded, nil
}
