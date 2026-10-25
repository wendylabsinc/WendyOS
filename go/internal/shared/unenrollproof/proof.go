// Package unenrollproof adapts host OCSP retrieval and trust to WendyGo proofs.
package unenrollproof

import (
	"bytes"
	"context"
	"crypto"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"time"

	wendy "github.com/wendylabsinc/wendy-go"
	"github.com/wendylabsinc/wendy-go/enrollment"
	"golang.org/x/crypto/ocsp"
)

const MaxProofBytes = enrollment.MaxProofBytes

func Fingerprint(leaf *x509.Certificate) string {
	d := sha256.Sum256(leaf.Raw)
	return hex.EncodeToString(d[:])
}
func Issuer(leaf *x509.Certificate, chain []*x509.Certificate) (*x509.Certificate, error) {
	var encoded []byte
	for _, cert := range chain {
		encoded = append(encoded, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})...)
	}
	der, err := (receiptVerifier{}).CertificateIssuer(context.Background(), leaf.Raw, string(encoded))
	if err != nil {
		return nil, err
	}
	return x509.ParseCertificate(der)
}

// receiptVerifier preserves the CLI's established operator-algorithm diagnostic
// while WendyGo owns certificate parsing and signature verification.
type receiptVerifier struct{ wendy.GoReceiptVerifier }

func (v receiptVerifier) VerifyMLDSA(ctx context.Context, key, message, signature []byte, algorithm string, certificate bool) error {
	err := v.GoReceiptVerifier.VerifyMLDSA(ctx, key, message, signature, algorithm, certificate)
	if certificate && err != nil && err.Error() == "ML-DSA proof key does not match algorithm" {
		return fmt.Errorf("operator ML-DSA key must match the signature algorithm")
	}
	return err
}

// VerifyRevocation retains the public certificate boundary and delegates policy.
func VerifyRevocation(raw []byte, leaf, issuer *x509.Certificate, now time.Time) error {
	if len(raw) == 0 || len(raw) > MaxProofBytes {
		return fmt.Errorf("missing or oversized revocation evidence")
	}
	if leaf == nil || issuer == nil {
		return fmt.Errorf("invalid installed certificate issuer")
	}
	return enrollment.VerifyRevocation(context.Background(), receiptVerifier{}, raw, leaf.Raw, issuer.Raw, now)
}

// Dial only resolved public addresses, using the checked IP for the actual
// connection so DNS rebinding cannot redirect an AIA request into a local
// network. Standard HTTP OCSP is retained; issuer signatures supply authority.
func dialOCSP(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("OCSP host has no public address")
	}
	for _, ip := range ips {
		ip = ip.Unmap()
		if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || netip.MustParsePrefix("100.64.0.0/10").Contains(ip) {
			return nil, fmt.Errorf("OCSP responder is not a public address")
		}
	}
	dialer := net.Dialer{Timeout: 10 * time.Second}
	for _, ip := range ips {
		conn, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if dialErr == nil {
			return conn, nil
		}
		err = dialErr
	}
	return nil, err
}

func rejectOCSPRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

func FetchRevocation(ctx context.Context, leaf, issuer *x509.Certificate) ([]byte, error) {
	transport := &http.Transport{DialContext: dialOCSP, TLSHandshakeTimeout: 10 * time.Second}
	defer transport.CloseIdleConnections()
	client := &http.Client{Timeout: 15 * time.Second, Transport: transport,
		CheckRedirect: rejectOCSPRedirect,
	}
	return fetchRevocation(ctx, leaf, issuer, client)
}

func fetchRevocation(ctx context.Context, leaf, issuer *x509.Certificate, client *http.Client) ([]byte, error) {
	// Wendy PKI pre-signs SHA-256 CertIDs. The OCSP library's default SHA-1
	// request has no matching response and is correctly rejected as unauthorized.
	request, err := ocsp.CreateRequest(leaf, issuer, &ocsp.RequestOptions{Hash: crypto.SHA256})
	if err != nil {
		return nil, err
	}
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

// Completion is public device-signed reset evidence, never a credential.
type Completion = enrollment.Completion

func SignCompletion(record Completion, key crypto.Signer) ([]byte, error) {
	signer, err := wendy.NewCompletionSigner(key)
	if err != nil {
		return nil, err
	}
	return enrollment.SignCompletion(context.Background(), record, signer)
}

// ReadCompletion checks receipt evidence without trusting embedded roots/keys.
// Remote callers retain the existing independent trust checks before cleanup.
func ReadCompletion(raw []byte) (*Completion, *x509.Certificate, error) {
	record, err := enrollment.ReadCompletion(context.Background(), receiptVerifier{}, raw)
	if err != nil {
		return nil, nil, err
	}
	leaf, err := x509.ParseCertificate(record.Certificate)
	if err != nil {
		return nil, nil, err
	}
	return record, leaf, nil
}
