// Package pkienroll obtains and renews a pki-core device identity certificate
// through pki-core's CSR frontend, using an enrollment token staged on the
// device.
//
// WHY A SECOND IDENTITY AND NOT A REPLACEMENT. The agent already holds one
// certificate, issued by Google Certificate Authority Service (CAS) through the
// Wendy Cloud broker, whose identity is the URI Subject Alternative Name (SAN)
// "urn:wendy:org:<org>:asset:<asset>". Wendy Cloud's interceptors understand
// only that form: a leaf carrying a SPIFFE SAN instead resolves to no identity
// there, and the agent's own inbound mTLS server silently disables its
// org-equality gate when it cannot read an org out of its own certificate. The
// Wendy Data Platform's ingest surface wants the opposite — exactly one
// "spiffe://wendy.sh/tenant/<tenant>/device/<name>" SAN, and it rejects any
// other principal kind. So the device holds two identities on two PEM triples
// and presents each to the peer that understands it. Nothing in this package
// reads or writes the CAS triple.
//
// The pki-core facts below cost hours to discover, so they are recorded where
// they bite:
//
//   - the tenant is a canonical lowercase UUID and rides in the path,
//     /v1/<tenant>/enroll, never as a slug and never as a body field;
//   - the CSR is PEM here. The Enrollment over Secure Transport (EST) frontend
//     wants base64-of-DER instead, and answers with a Cryptographic Message
//     Syntax (CMS) blob that no current OpenSSL build can parse, which is why
//     this client speaks to the CSR frontend;
//   - device_id must equal the enrollment token's device_id byte for byte. The
//     SPIFFE device name is decided when the token is minted, not by the
//     device, so a mismatch is a hard refusal and not a rename;
//   - the "tier" body field is informational. pki-core derives certificate
//     authority, profile and tenant from the token row alone;
//   - pki-core stamps the SPIFFE SAN itself and discards every URI SAN the CSR
//     carried, so this client asserts no identity of its own. It still verifies
//     the one it got back, because a leaf whose SAN is not the expected device
//     principal is useless to the ingest surface and must not be stored;
//   - a device tier pins no key algorithm ("a device leaf follows the CSR"), so
//     the agent's existing ECDSA P-256 key is accepted unchanged;
//   - renewal is authenticated by presenting the current leaf as the TLS client
//     certificate, and only while it is still valid. Past expiry the request
//     takes a grant-required branch this client cannot satisfy, which is why
//     renewal has to run ahead of expiry rather than in response to a failure.
package pkienroll

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
)

const (
	// DefaultTier is the value sent in the request body's "tier" field. It is
	// informational: pki-core reads the real profile off the token row. Sent
	// anyway because the frontend's request shape includes it.
	DefaultTier = "standard"

	// requestTimeout bounds a single enrol or renew round trip. Generous
	// because a Jetson's boot-time network can be slow, bounded because a hung
	// request would stall the renewal loop past the leaf's expiry.
	requestTimeout = 30 * time.Second

	// maxResponseBytes caps how much of a response body is read. The frontend
	// answers with a certificate chain of a few kilobytes; anything far larger
	// is a wrong endpoint, and reading it into memory on a device is not free.
	maxResponseBytes = 1 << 20
)

// EnrollRequest is everything needed for one bearer-token enrolment.
type EnrollRequest struct {
	// CSRFrontendURL is the frontend's base URL, e.g.
	// "https://csr.dev.pki.wendy.sh". A URL that already ends in the
	// /v1/<tenant>/enroll path is accepted as-is, so an operator who pasted a
	// full endpoint is not silently sent somewhere else.
	CSRFrontendURL string

	// TenantUUID is the canonical lowercase tenant UUID. It selects the path
	// segment AND is the tenant the returned leaf's SAN is checked against, so
	// a wrong value fails loudly rather than storing a foreign identity.
	TenantUUID string

	// EnrollmentToken is the single-use bearer credential an operator minted
	// through pki-core's fabric relay. The agent only consumes it; nothing here
	// can mint one.
	EnrollmentToken string

	// Key is the PEM-encoded ECDSA P-256 private key whose public half goes
	// into the CSR. Callers own the slice and may zero it after the call; this
	// package does not retain it.
	Key []byte

	// CommonName is the CSR subject Common Name. The agent passes the same
	// "sh/wendy/<org>/<asset>" it builds for its CAS certificate. pki-core
	// derives the leaf's Distinguished Name from the minted SPIFFE identity
	// instead, so this only matters to frontends that read the CN — which
	// pki-core's own dev shim does, to derive device_id.
	CommonName string

	// DeviceID must equal the token's device_id byte for byte. Empty means
	// "use CommonName", which is the pairing pki-core's dev shim assumes and
	// the convention its own token minting follows.
	DeviceID string

	// HTTPClient is optional; nil uses a plain client with requestTimeout.
	HTTPClient *http.Client
}

// RenewRequest renews a leaf by presenting it. There is no token: possession of
// the current certificate IS the credential, proven by the TLS handshake.
type RenewRequest struct {
	CSRFrontendURL string
	TenantUUID     string

	// CurrentLeafPEM and CurrentChainPEM are the certificate presented as the
	// TLS client certificate. The renewed leaf carries the same device name,
	// read off this certificate's SAN and never off the renewal CSR.
	CurrentLeafPEM  string
	CurrentChainPEM string

	// Key is the device's long-lived key. It is reused rather than rotated:
	// the enrolment key is generated once on the device and carried across
	// renewals, so a renewal cannot orphan a key the store already committed.
	Key []byte

	HTTPClient *http.Client
}

// Result is an accepted, verified issuance.
type Result struct {
	// LeafPEM is the issued leaf on its own.
	LeafPEM string
	// ChainPEM is the intermediates below the leaf, root excluded (pki-core
	// does not return the root). Empty when the response carried a leaf alone.
	ChainPEM string
	// DeviceName is the SPIFFE device name the SAN actually carries. Recorded
	// rather than assumed, because the name is the token's to decide and
	// logging what was issued is the only way to notice a surprise.
	DeviceName string
	// SPIFFEURI is the full verified principal.
	SPIFFEURI string
	// NotBefore and NotAfter bound the leaf's validity; the renewal schedule is
	// computed from them and not from an assumed tier length.
	NotBefore time.Time
	NotAfter  time.Time
}

// StatusError reports a frontend refusal, carrying the HTTP status and whatever
// detail the body held. It is a distinct type because the remedies differ and
// collapsing them into one message is what makes an enrolment failure
// unactionable: 401 means the token is wrong, consumed or revoked and a fresh
// one must be minted; 400 means the request itself is malformed, most often a
// device_id that does not match the token's.
type StatusError struct {
	StatusCode int
	Detail     string
}

func (e *StatusError) Error() string {
	if e.Detail != "" {
		return fmt.Sprintf("pki-core csr frontend answered %d: %s", e.StatusCode, e.Detail)
	}
	return fmt.Sprintf("pki-core csr frontend answered %d", e.StatusCode)
}

// Unauthorized reports whether the refusal was a credential problem.
func (e *StatusError) Unauthorized() bool { return e.StatusCode == http.StatusUnauthorized }

// IdentityError reports that a certificate came back, but not the identity that
// was asked for. It carries the SANs actually seen so the log says what was
// issued rather than only that something was wrong.
type IdentityError struct {
	Want string
	Got  []string
}

func (e *IdentityError) Error() string {
	if len(e.Got) == 0 {
		return fmt.Sprintf("issued leaf carries no tenant SPIFFE URI SAN; want exactly one %q", e.Want)
	}
	return fmt.Sprintf("issued leaf carries tenant SPIFFE URI SAN(s) %s; want exactly one %q",
		strings.Join(e.Got, ", "), e.Want)
}

type enrollRequestBody struct {
	CSR      string `json:"csr"`
	DeviceID string `json:"device_id"`
	Tier     string `json:"tier"`
}

type renewRequestBody struct {
	CSR string `json:"csr"`
}

type certificateResponseBody struct {
	Certificate string `json:"certificate"`
}

type detailResponseBody struct {
	Detail string `json:"detail"`
	Error  string `json:"error"`
}

// Enroll exchanges the staged enrollment token for a device certificate.
//
// It returns a *StatusError for any refusal the frontend expressed as a status
// code, and an *IdentityError when the issued leaf is not the expected device
// principal. In the second case nothing is stored: a leaf whose SAN the ingest
// surface will reject is worse than no leaf at all, because it looks enrolled.
func Enroll(ctx context.Context, req EnrollRequest) (Result, error) {
	if err := req.validate(); err != nil {
		return Result{}, err
	}
	deviceID := req.DeviceID
	if deviceID == "" {
		deviceID = req.CommonName
	}

	csrPEM, err := buildCSR(req.Key, req.CommonName)
	if err != nil {
		return Result{}, err
	}
	body, err := json.Marshal(enrollRequestBody{CSR: csrPEM, DeviceID: deviceID, Tier: DefaultTier})
	if err != nil {
		return Result{}, fmt.Errorf("encoding enrollment request: %w", err)
	}

	endpoint, err := frontendEndpoint(req.CSRFrontendURL, req.TenantUUID, "enroll")
	if err != nil {
		return Result{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return Result{}, fmt.Errorf("building enrollment request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+req.EnrollmentToken)

	client := req.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: requestTimeout}
	}
	return exchange(client, httpReq, req.TenantUUID)
}

// Renew exchanges a CSR for a fresh leaf, authenticated by presenting the
// current one. The device name is not sent and cannot be changed: pki-core
// reads it off the presented certificate.
func Renew(ctx context.Context, req RenewRequest) (Result, error) {
	if req.CSRFrontendURL == "" {
		return Result{}, errors.New("pki enrollment: no csr frontend url")
	}
	if req.TenantUUID == "" {
		return Result{}, errors.New("pki enrollment: no tenant uuid")
	}
	if req.CurrentLeafPEM == "" {
		return Result{}, errors.New("pki enrollment: no current certificate to renew")
	}
	if len(req.Key) == 0 {
		return Result{}, errors.New("pki enrollment: no private key")
	}

	commonName, err := leafCommonName(req.CurrentLeafPEM)
	if err != nil {
		return Result{}, err
	}
	csrPEM, err := buildCSR(req.Key, commonName)
	if err != nil {
		return Result{}, err
	}
	body, err := json.Marshal(renewRequestBody{CSR: csrPEM})
	if err != nil {
		return Result{}, fmt.Errorf("encoding renewal request: %w", err)
	}

	endpoint, err := frontendEndpoint(req.CSRFrontendURL, req.TenantUUID, "renew")
	if err != nil {
		return Result{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return Result{}, fmt.Errorf("building renewal request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	client := req.HTTPClient
	if client == nil {
		// The handshake is the credential, so the client certificate is not
		// optional here: without it pki-core sees an anonymous caller and
		// answers 401. The server side is verified against the system roots,
		// as everywhere else the agent dials a public Wendy endpoint.
		tlsCfg, err := certs.LoadTLSConfig(req.CurrentLeafPEM, req.CurrentChainPEM, string(req.Key), "")
		if err != nil {
			return Result{}, fmt.Errorf("building renewal mTLS config: %w", err)
		}
		client = &http.Client{
			Timeout:   requestTimeout,
			Transport: &http.Transport{TLSClientConfig: tlsCfg},
		}
	}
	return exchange(client, httpReq, req.TenantUUID)
}

func (r EnrollRequest) validate() error {
	var missing []string
	for _, f := range []struct{ name, value string }{
		{"csr frontend url", r.CSRFrontendURL},
		{"tenant uuid", r.TenantUUID},
		{"enrollment token", r.EnrollmentToken},
		{"common name", r.CommonName},
	} {
		if strings.TrimSpace(f.value) == "" {
			missing = append(missing, f.name)
		}
	}
	if len(r.Key) == 0 {
		missing = append(missing, "private key")
	}
	if len(missing) > 0 {
		return fmt.Errorf("pki enrollment request is missing %s", strings.Join(missing, ", "))
	}
	return nil
}

// buildCSR builds the PKCS#10 request. The EKUs match what the agent already
// asks CAS for: the device identity acts as a TLS client to the data platform
// and as a TLS server on the agent's own mTLS port, and pki-core's device tiers
// issue both regardless, so asking for both keeps the two CSRs comparable.
//
// The URI SAN is deliberately empty. pki-core replaces it with the principal it
// minted from the token, so stamping a URN here would be a claim the server
// discards — and an identity this client is not entitled to assert.
func buildCSR(keyPEM []byte, commonName string) (string, error) {
	csrPEM, err := certs.GenerateCSR(keyPEM, commonName, "",
		x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth)
	if err != nil {
		return "", fmt.Errorf("generating pki enrollment CSR: %w", err)
	}
	return csrPEM, nil
}

// frontendEndpoint builds /v1/<tenant>/<action> under base. A base that already
// carries a /v1/ path is returned unchanged, so a fully-specified endpoint from
// configuration is honoured rather than having a second path appended to it.
func frontendEndpoint(base, tenantUUID, action string) (string, error) {
	base = strings.TrimSpace(base)
	if !strings.Contains(base, "://") {
		base = "https://" + base
	}
	u, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("parsing csr frontend url %q: %w", base, err)
	}
	if u.Host == "" {
		return "", fmt.Errorf("csr frontend url %q has no host", base)
	}
	if strings.Contains(u.Path, "/v1/") {
		return u.String(), nil
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + "/v1/" + tenantUUID + "/" + action
	return u.String(), nil
}

// exchange performs the request and turns the response into a verified Result.
func exchange(client *http.Client, req *http.Request, tenantUUID string) (Result, error) {
	resp, err := client.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("contacting pki-core csr frontend: %w", err)
	}
	defer resp.Body.Close()

	limited := &limitedReader{r: resp.Body, remaining: maxResponseBytes}
	if resp.StatusCode != http.StatusOK {
		var d detailResponseBody
		_ = json.NewDecoder(limited).Decode(&d)
		detail := d.Detail
		if detail == "" {
			detail = d.Error
		}
		return Result{}, &StatusError{StatusCode: resp.StatusCode, Detail: detail}
	}

	var ok certificateResponseBody
	if err := json.NewDecoder(limited).Decode(&ok); err != nil {
		return Result{}, fmt.Errorf("decoding pki-core certificate response: %w", err)
	}
	return parseAndVerify(ok.Certificate, tenantUUID)
}

// parseAndVerify splits the returned leaf-first chain and refuses anything that
// is not exactly one device principal for tenantUUID.
func parseAndVerify(bundlePEM, tenantUUID string) (Result, error) {
	leafPEM, chainPEM, err := SplitLeafAndChain(bundlePEM)
	if err != nil {
		return Result{}, err
	}
	// ParseCertsFromPEM is the ML-DSA-aware parser the agent's mTLS paths use,
	// and LeafCertificatePEM strips the trailing ASN.1 bytes some pki-core
	// leaves carry. Verifying through the same pair the handshake will use is
	// the point: a leaf this cannot read is a leaf that cannot be presented.
	normalizedLeaf, err := certs.LeafCertificatePEM(leafPEM)
	if err != nil {
		return Result{}, fmt.Errorf("extracting issued leaf: %w", err)
	}
	parsed, err := certs.ParseCertsFromPEM([]byte(normalizedLeaf))
	if err != nil {
		return Result{}, fmt.Errorf("parsing issued leaf: %w", err)
	}
	if len(parsed) == 0 {
		return Result{}, errors.New("pki-core response carried no parseable leaf")
	}
	leaf := parsed[0]

	uris := certs.TenantSPIFFEURIs(leaf)
	if len(uris) != 1 {
		return Result{}, &IdentityError{Want: certs.DeviceSPIFFEURI(tenantUUID, "<name>"), Got: uris}
	}
	gotTenant, deviceName, err := certs.ParseDeviceSPIFFEURI(uris[0])
	if err != nil {
		return Result{}, &IdentityError{Want: certs.DeviceSPIFFEURI(tenantUUID, "<name>"), Got: uris}
	}
	if !strings.EqualFold(gotTenant, tenantUUID) {
		return Result{}, &IdentityError{Want: certs.DeviceSPIFFEURI(tenantUUID, "<name>"), Got: uris}
	}

	return Result{
		LeafPEM:    normalizedLeaf,
		ChainPEM:   chainPEM,
		DeviceName: deviceName,
		SPIFFEURI:  uris[0],
		NotBefore:  leaf.NotBefore,
		NotAfter:   leaf.NotAfter,
	}, nil
}

// leafCommonName reads the CN off a stored leaf, so a renewal CSR restates the
// subject the current certificate has rather than rebuilding it from state that
// may since have changed.
func leafCommonName(leafPEM string) (string, error) {
	normalized, err := certs.LeafCertificatePEM(leafPEM)
	if err != nil {
		return "", fmt.Errorf("extracting current leaf: %w", err)
	}
	parsed, err := certs.ParseCertsFromPEM([]byte(normalized))
	if err != nil {
		return "", fmt.Errorf("parsing current leaf: %w", err)
	}
	if len(parsed) == 0 {
		return "", errors.New("current leaf PEM carried no certificate")
	}
	return parsed[0].Subject.CommonName, nil
}

// limitedReader is io.LimitedReader with a distinguishable exhaustion error, so
// an oversized body is reported as such rather than as a truncated JSON parse.
type limitedReader struct {
	r         interface{ Read([]byte) (int, error) }
	remaining int64
}

func (l *limitedReader) Read(p []byte) (int, error) {
	if l.remaining <= 0 {
		return 0, fmt.Errorf("pki-core response exceeded %d bytes", int64(maxResponseBytes))
	}
	if int64(len(p)) > l.remaining {
		p = p[:l.remaining]
	}
	n, err := l.r.Read(p)
	l.remaining -= int64(n)
	return n, err
}
