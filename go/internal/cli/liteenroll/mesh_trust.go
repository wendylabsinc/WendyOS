package liteenroll

import (
	"bytes"
	"context"
	"crypto/x509"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/estcacerts"
)

func discoverMeshTrust(ctx context.Context, client *http.Client, endpoint *url.URL,
	tenant string, profile TrustProfile, discovered []*x509.Certificate, roots []byte) ([]byte, []byte, error) {
	regularURL := *endpoint
	query := regularURL.Query()
	query.Del("chain_algorithm")
	query.Del("full_chain")
	regularURL.RawQuery = query.Encode()
	liteURL := regularURL
	query.Set("chain_algorithm", estcacerts.Classical)
	liteURL.RawQuery = query.Encode()
	regular := discovered
	classical := profile.chainAlgorithm() == estcacerts.Classical
	if classical {
		var err error
		regular, _, err = estcacerts.Fetch(ctx, client, &regularURL)
		if err != nil {
			return nil, nil, err
		}
	}
	query.Set("full_chain", "true")
	liteURL.RawQuery = query.Encode()
	signed, algorithm, err := estcacerts.Fetch(ctx, client, &liteURL)
	if err != nil {
		return nil, nil, err
	}
	if algorithm != estcacerts.Classical {
		return nil, nil, fmt.Errorf("EST did not confirm the parent-signed Lite CA")
	}
	now := time.Now()
	path, _, err := estcacerts.LitePath(signed, estcacerts.PEM(regular), tenant, now)
	if err != nil {
		return nil, nil, err
	}
	if classical {
		pins, err := meshTrustBundle(path, discovered, regular, tenant, now)
		return pins, nil, err
	}
	var intermediates []byte
	for _, cert := range path {
		intermediates = append(intermediates, cert.Raw...)
	}
	if len(intermediates) > maxDeviceTrustBundle {
		return nil, nil, fmt.Errorf("Lite intermediates exceed the firmware's 32 KiB limit")
	}
	// The regular EST bundle already has the normal chain. Keep the Lite
	// path exclusively in the untrusted intermediate field.
	return roots, intermediates, nil
}

func meshTrustBundle(path, twins, regular []*x509.Certificate, tenant string, now time.Time) ([]byte, error) {
	if len(path) < 2 {
		return nil, fmt.Errorf("Lite CA has no tenant intermediate")
	}
	var bundle []*x509.Certificate
	for _, ca := range twins {
		if estcacerts.IsLiteDeviceCA(ca, tenant) && bytes.Equal(ca.RawSubject, path[0].RawSubject) &&
			bytes.Equal(ca.RawSubjectPublicKeyInfo, path[0].RawSubjectPublicKeyInfo) &&
			bytes.Equal(ca.RawIssuer, ca.RawSubject) && ca.CheckSignatureFrom(ca) == nil {
			bundle = append(bundle, ca)
			break
		}
	}
	if len(bundle) == 0 {
		return nil, fmt.Errorf("EST did not return the matching self-signed Lite Device CA")
	}
	parent := path[1]
	label, ok := strings.CutSuffix(parent.Subject.CommonName, " Intermediate CA")
	if !ok || label == "" {
		return nil, fmt.Errorf("unexpected tenant Intermediate CA name")
	}
	roots := x509.NewCertPool()
	issuers := x509.NewCertPool()
	for _, ca := range regular {
		if ca.IsCA && bytes.Equal(ca.RawSubject, ca.RawIssuer) && ca.CheckSignatureFrom(ca) == nil {
			roots.AddCert(ca)
		} else if ca.IsCA {
			issuers.AddCert(ca)
		}
	}
	var devices, operators bool
	for _, ca := range regular {
		if !ca.IsCA || !ca.MaxPathLenZero || now.Before(ca.NotBefore) || !now.Before(ca.NotAfter) ||
			!bytes.Equal(ca.RawIssuer, parent.RawSubject) {
			continue
		}
		paths, err := ca.Verify(x509.VerifyOptions{Roots: roots, Intermediates: issuers, CurrentTime: now,
			KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}})
		if err != nil || len(paths) == 0 || len(paths[0]) < 3 {
			continue
		}
		switch ca.Subject.CommonName {
		// PKI's initial certificate and its rotated successors use the
		// public common name and persisted CA label respectively.
		case label + " Identity CA", label + " Device Identity CA":
			devices = true
		case label + " Operator Authority CA":
			operators = true
		default:
			continue
		}
		bundle = append(bundle, ca)
	}
	if !devices || !operators || len(bundle) > 8 {
		return nil, fmt.Errorf("EST did not provide a bounded set of Device and Operator Authority CAs")
	}
	return []byte(estcacerts.PEM(bundle)), nil
}
