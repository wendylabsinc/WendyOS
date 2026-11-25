package certs

import (
	"bytes"
	"crypto/x509"
	"errors"
	"time"
)

// IssuerPathError marks a missing/unusable issuer path, rather than a known
// validity or policy rejection. It preserves the verifier's original error.
// TLS may retry the presentation; it must never relax certificate verification.
type IssuerPathError struct{ Err error }

func (e *IssuerPathError) Error() string { return e.Err.Error() }
func (e *IssuerPathError) Unwrap() error { return e.Err }

func IsIssuerPathError(err error) bool {
	var path *IssuerPathError
	return errors.As(err, &path)
}

// ClassifyIssuerFailure also handles Go's UnknownAuthorityError, which can hide
// an expired or otherwise ineligible parent. Conservatively exclude known
// problems in reachable issuer candidates; an unrelated expired CA does not
// count. This diagnoses an already rejected chain and never changes acceptance.
func ClassifyIssuerFailure(err error, leaf *x509.Certificate, peers, roots []*x509.Certificate, usage x509.ExtKeyUsage, now, leafNow time.Time) error {
	if err == nil {
		return nil
	}
	var unknown x509.UnknownAuthorityError
	if !IsIssuerPathError(err) && !errors.As(err, &unknown) {
		return err
	}
	// Unwrap our marker when an exclusion is found, retaining the original
	// verifier error and its diagnostic text.
	original := err
	if path := new(IssuerPathError); errors.As(err, &path) {
		original = path.Err
	}
	if leafNow.Before(leaf.NotBefore) || now.After(leaf.NotAfter) ||
		len(leaf.UnhandledCriticalExtensions) != 0 || !permitsUsage(leaf, usage) {
		return original
	}
	candidates := append(append([]*x509.Certificate(nil), roots...), peers...)
	seen := make(map[string]bool)
	var excluded func(*x509.Certificate, int) bool
	excluded = func(child *x509.Certificate, depth int) bool {
		if depth >= 8 || seen[string(child.Raw)] {
			return false
		}
		seen[string(child.Raw)] = true
		for _, parent := range candidates {
			if !bytes.Equal(child.RawIssuer, parent.RawSubject) ||
				(len(child.AuthorityKeyId) > 0 && len(parent.SubjectKeyId) > 0 && !bytes.Equal(child.AuthorityKeyId, parent.SubjectKeyId)) {
				continue
			}
			if checkPeerCA(parent, usage, now, depth) != nil || excluded(parent, depth+1) {
				return true
			}
		}
		return false
	}
	if excluded(leaf, 0) {
		return original
	}
	return &IssuerPathError{Err: original}
}
