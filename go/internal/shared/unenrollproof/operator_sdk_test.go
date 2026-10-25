package unenrollproof

import (
	"context"
	"crypto/mldsa"
	"crypto/x509"
	"fmt"
	"time"

	wendy "github.com/wendylabsinc/wendy-go"
	"github.com/wendylabsinc/wendy-go/enrollment"
)

type fixtureOperatorVerifier struct {
	wendy.GoProofVerifier
	leaf *x509.Certificate
}

func (v fixtureOperatorVerifier) VerifyMLDSA(ctx context.Context, key, message, signature []byte, algorithm string, certificate bool) error {
	if !certificate {
		return v.GoProofVerifier.VerifyMLDSA(ctx, key, message, signature, algorithm, false)
	}
	public, ok := v.leaf.PublicKey.(*mldsa.PublicKey)
	params := mldsa.MLDSA65()
	if algorithm == "ML-DSA-87" {
		params = mldsa.MLDSA87()
	}
	if !ok || public.Parameters() != params {
		return fmt.Errorf("operator ML-DSA key must match the signature algorithm")
	}
	return mldsa.Verify(public, message, signature, nil)
}
func verifyOperatorReset(compact string, request []byte, asset, tenant, correlation string, leaf *x509.Certificate, earliest, latest time.Time) error {
	return enrollment.VerifyOperatorReset(context.Background(), fixtureOperatorVerifier{leaf: leaf}, compact, request, asset, tenant, correlation, leaf.Raw, earliest, latest)
}
