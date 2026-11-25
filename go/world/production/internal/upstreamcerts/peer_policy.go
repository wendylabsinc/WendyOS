package upstreamcerts

import (
	"crypto/x509"
	"errors"
	"time"
)

// VerifyPeerInterval adapts Wendy mtls's uncached peer policy to an interval.
// No wall clock, NotBefore floor, network lookup, or peer trust promotion is used.
// See third_party/README.md for exact upstream sources and intentional changes.
func VerifyPeerInterval(leaf *x509.Certificate, peers, anchors []*x509.Certificate, earliest, latest time.Time) (time.Time, error) {
	if earliest.IsZero() || latest.IsZero() || latest.Before(earliest) || leaf == nil || leaf.IsCA {
		return time.Time{}, errors.New("invalid time interval or CA peer leaf")
	}
	clientAuth := false
	for _, usage := range leaf.ExtKeyUsage {
		if usage == x509.ExtKeyUsageClientAuth {
			clientAuth = true
		}
	}
	if !clientAuth || leaf.NotAfter.Sub(leaf.NotBefore) > 732*24*time.Hour {
		return time.Time{}, errors.New("peer requires clientAuth and bounded certificate lifetime")
	}
	valid := func(c *x509.Certificate) bool { return !earliest.Before(c.NotBefore) && !latest.After(c.NotAfter) }
	if !valid(leaf) {
		return time.Time{}, errors.New("peer leaf is not valid throughout trusted interval")
	}
	// Filter BEFORE selecting a path. Two alternate paths valid at opposite
	// endpoints cannot jointly establish one path valid throughout the interval.
	pool, intermediates := x509.NewCertPool(), x509.NewCertPool()
	var roots, eligiblePeers []*x509.Certificate
	for _, c := range anchors {
		if valid(c) {
			pool.AddCert(c)
			roots = append(roots, c)
		}
	}
	for _, c := range peers {
		if valid(c) {
			intermediates.AddCert(c)
			eligiblePeers = append(eligiblePeers, c)
		}
	}
	if len(roots) == 0 {
		return time.Time{}, errors.New("no pinned anchor valid throughout trusted interval")
	}
	paths, stdErr := leaf.Verify(x509.VerifyOptions{Roots: pool, Intermediates: intermediates, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, CurrentTime: earliest})
	if stdErr == nil {
		var best time.Time
		for _, path := range paths {
			until := leaf.NotAfter
			for _, cert := range path {
				if cert.NotAfter.Before(until) {
					until = cert.NotAfter
				}
			}
			if until.After(best) {
				best = until
			}
		}
		return best, nil
	}
	stdErr = ClassifyIssuerFailure(stdErr, leaf, eligiblePeers, roots, x509.ExtKeyUsageClientAuth, earliest, earliest)
	// This is Wendy's fallback gate: only ML-DSA-signed leaves use the
	// supplementary verifier; ordinary x509 policy failures never bypass it.
	oid, err := mldsaCertSigAlgOID(leaf)
	if err != nil {
		return time.Time{}, stdErr
	}
	if _, err = mldsaScheme(oid); err != nil {
		return time.Time{}, stdErr
	}
	return VerifyPeerCertificateChainExpiry(leaf, eligiblePeers, roots, x509.ExtKeyUsageClientAuth, earliest, earliest)
}
