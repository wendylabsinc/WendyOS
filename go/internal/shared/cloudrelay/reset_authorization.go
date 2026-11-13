package cloudrelay

import (
	"bytes"
	"context"
	"crypto/mldsa"
	"encoding/json"
	"fmt"
	"strings"
)

// VerifyResetSignature authenticates only this purpose's ML-DSA-65 signature.
// Callers must additionally validate issuer, audience, freshness and all bindings.
// The returned key came from the configured HTTPS issuer, never the artifact.
func (v *Verifier) VerifyResetSignature(ctx context.Context, compact string) ([]byte, []byte, error) {
	if len(compact) == 0 || len(compact) > 65536 || !strings.HasPrefix(v.Issuer, "https://") {
		return nil, nil, fmt.Errorf("invalid reset authorization")
	}
	parts := strings.Split(compact, ".")
	if len(parts) != 3 {
		return nil, nil, fmt.Errorf("invalid reset authorization")
	}
	header, err := decode64(parts[0])
	if err != nil {
		return nil, nil, err
	}
	var h map[string]string
	if json.Unmarshal(header, &h) != nil || len(h) != 3 || h["alg"] != "ML-DSA-65" || h["typ"] != "asset-reset-authorization+jws" || !bounded(h["kid"], 128) {
		return nil, nil, fmt.Errorf("invalid reset authorization header")
	}
	canonical, _ := canonicalJSON(h)
	if !bytes.Equal(canonical, header) {
		return nil, nil, fmt.Errorf("noncanonical reset header")
	}
	key, err := v.key(ctx, h["kid"])
	if err != nil {
		return nil, nil, err
	}
	if key.Kty != "AKP" || key.Alg != "ML-DSA-65" || key.Use != "sig" {
		return nil, nil, fmt.Errorf("reset requires Cloud ML-DSA-65 signing key")
	}
	raw, err := decode64(key.Pub)
	if err != nil {
		return nil, nil, err
	}
	pk, err := mldsa.NewPublicKey(mldsa.MLDSA65(), raw)
	if err != nil {
		return nil, nil, err
	}
	sig, err := decode64(parts[2])
	if err != nil {
		return nil, nil, err
	}
	if err = mldsa.Verify(pk, []byte(parts[0]+"."+parts[1]), sig, nil); err != nil {
		return nil, nil, err
	}
	payload, err := decode64(parts[1])
	return payload, raw, err
}
