package commands

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/mldsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/cli/cloudenroll"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

// Pre-expiry certificate renewal against pki-core's renew frontend (WDY-2829).
//
// WHY A PRE-FLIGHT AND NOT A FAILURE HANDLER. pki-core renews a certificate
// only while the presented one is still VALID: possession is proven by the mTLS
// handshake that carries it, and an expired leaf takes the grant-required
// branch instead, which needs a cloud-signed grant rather than a plain renew.
// So the cheap path exists strictly before expiry — reacting to an mTLS
// rejection is already too late. That is why this runs when the auth entry is
// resolved rather than hanging off offerCertRefreshAndRetry.
//
// Renewal is also budgeted, per lineage, and the budget is inherited rather
// than requested: a plain operator leaf renews a fixed number of times and then
// has to be re-minted, and an entitlement-bearing or over-duration operator
// leaf is not renewable at all unless its tenant opted in. Both refusals arrive
// as an explicit denial, so they are reported as themselves rather than
// retried.
const (
	// renewLeadTime is how much remaining validity triggers a renewal attempt.
	// It has to exceed the round trip plus any clock skew between CLI and
	// pki-core, or the renew arrives after the leaf it is renewing has expired
	// and takes the grant branch.
	renewLeadTime = 15 * time.Minute

	// renewEndpointEnv names pki-core's renew frontend, e.g.
	// "https://renew.pki.example:8451/v1/renew".
	//
	// Custom deployments require an explicit endpoint. Known Wendy deployments
	// use the documented renewal URL, never a host/port inferred by substitution.
	renewEndpointEnv = "WENDY_PKI_RENEW_ENDPOINT"

	renewRequestTimeout = 15 * time.Second
)

// renewUnavailableError says the cheap renewal could not be used and why. The
// reason is user-facing: every branch that produces one has a different remedy,
// and collapsing them into "refresh failed" is what made the old behaviour
// misleading.
type renewUnavailableError struct {
	reason string
	detail string

	// needsFreshCert marks the refusals a renewal can never recover from — the
	// lineage is gone, spent, or was never pki-core's — for which the remedy is
	// a newly minted certificate rather than another renewal attempt.
	needsFreshCert bool
}

func (e renewUnavailableError) Error() string {
	if e.detail != "" {
		return e.reason + ": " + e.detail
	}
	return e.reason
}

var (
	// errNoRenewEndpoint is not a failure: nothing is configured to renew
	// against, so the pre-flight has nothing to offer and stays silent.
	errNoRenewEndpoint = errors.New("no pki-core renew endpoint configured; set " + renewEndpointEnv)
	errCertExpired     = renewUnavailableError{reason: "your certificate has expired", needsFreshCert: true}

	// errCertNotPKIIssued is the pre-cloud-teardown session: a leaf minted
	// before certificates carried a tenant principal. pki-core cannot route it
	// to a tenant and holds no lineage record for it, so it is not renewable
	// there by any route and only a fresh login replaces it.
	errCertNotPKIIssued = renewUnavailableError{
		reason:         "this certificate carries no pki-core tenant identity and cannot be renewed",
		needsFreshCert: true,
	}
)

// storedLeaf parses the leaf out of a stored certificate PEM. It normalises
// through certs.LeafCertificatePEM first because pki-core leaves can carry
// trailing ASN.1 bytes that defeat a raw x509 parse, and reads it back with
// ParseCertsFromPEM — the ML-DSA-aware parser the mTLS paths use — so callers
// see exactly the certificate the handshake can present.
func storedLeaf(certPEM string) (*x509.Certificate, error) {
	leafPEM, err := certs.LeafCertificatePEM(certPEM)
	if err != nil {
		return nil, fmt.Errorf("extracting leaf certificate: %w", err)
	}
	leaves, err := certs.ParseCertsFromPEM([]byte(leafPEM))
	if err != nil {
		return nil, err
	}
	if len(leaves) == 0 {
		return nil, errors.New("no certificate in stored leaf PEM")
	}
	return leaves[0], nil
}

// leafNotAfter returns the stored leaf's expiry.
func leafNotAfter(certPEM string) (time.Time, error) {
	leaf, err := storedLeaf(certPEM)
	if err != nil {
		return time.Time{}, err
	}
	return leaf.NotAfter, nil
}

// tenantPrincipalFor reports the tenant SPIFFE principal a stored certificate
// carries, and whether it carries one at all. An unparseable certificate is
// reported as carrying none: the connection attempt diagnoses that with better
// context than a renewal can.
func tenantPrincipalFor(certPEM string) (string, bool) {
	leaf, err := storedLeaf(certPEM)
	if err != nil {
		return "", false
	}
	return certs.TenantPrincipalFromCert(leaf)
}

// certNeedsRenewal reports whether the leaf is close enough to expiry to renew.
func certNeedsRenewal(notAfter, now time.Time) bool {
	return notAfter.Sub(now) < renewLeadTime
}

// renewEndpoint resolves the explicit override or a documented deployment URL.
func renewEndpoint(auth *config.AuthConfig) string {
	if endpoint := strings.TrimSpace(os.Getenv(renewEndpointEnv)); endpoint != "" {
		return endpoint
	}
	if auth == nil {
		return ""
	}
	if host := cloudenroll.PKISiblingHost(auth.PKIEndpoint, "renew"); host != "" {
		return "https://" + host + "/v1/renew"
	}
	return ""
}

// splitLeafAndChain splits a PEM bundle into its first certificate and the
// remainder. pki-core answers a renewal with leaf-first chain in one blob,
// while the CLI stores the two separately.
func splitLeafAndChain(bundlePEM string) (leaf, chain string, err error) {
	rest := []byte(bundlePEM)
	var blocks []*pem.Block
	for {
		block, remainder := pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type == "CERTIFICATE" {
			blocks = append(blocks, block)
		}
		rest = remainder
	}
	if len(blocks) == 0 {
		return "", "", errors.New("renewal response carried no certificate")
	}
	var leafBuf, chainBuf bytes.Buffer
	if err := pem.Encode(&leafBuf, blocks[0]); err != nil {
		return "", "", fmt.Errorf("re-encoding renewed leaf: %w", err)
	}
	for _, b := range blocks[1:] {
		if err := pem.Encode(&chainBuf, b); err != nil {
			return "", "", fmt.Errorf("re-encoding renewed chain: %w", err)
		}
	}
	return leafBuf.String(), chainBuf.String(), nil
}

// renewHTTPClientFor builds a client that presents the CURRENT certificate.
// The handshake proves possession to the renew frontend; the engine behind it
// checks the body's possession_proof instead (renewalPossessionProof).
func renewHTTPClientFor(cert config.CertificateInfo) (*http.Client, error) {
	keyPEM, err := cert.PrivateKeyPEM()
	if err != nil {
		return nil, fmt.Errorf("loading current client key: %w", err)
	}
	tlsCfg, err := certs.LoadTLSConfig(cert.PemCertificate, cert.PemCertificateChain, keyPEM, "")
	if err != nil {
		return nil, fmt.Errorf("building renewal mTLS config: %w", err)
	}
	return &http.Client{
		Timeout:   renewRequestTimeout,
		Transport: &http.Transport{TLSClientConfig: tlsCfg},
	}, nil
}

type renewRequestBody struct {
	CSR             string `json:"csr"`
	PossessionProof string `json:"possession_proof"`
}

// renewalPossessionProof proves to pki-core's engine that the caller holds the
// presented certificate's key (WDY-3437; service-protos
// RenewCertificateRequest.possession_proof). The mTLS handshake proves it only
// to the frontend, which the engine does not take on trust. The JWS binds the
// exact DER the handshake presents and the CSR in the same body; jti is
// single-use and iat must be fresh, so every request signs a new one.
func renewalPossessionProof(cert config.CertificateInfo, csrPEM string, now time.Time) (string, error) {
	keyPEM, err := cert.PrivateKeyPEM()
	if err != nil {
		return "", fmt.Errorf("loading current client key: %w", err)
	}
	// The same parse renewHTTPClientFor's TLS config does, so the hashed DER
	// and the signing key are the ones the handshake presents.
	pair, err := tls.X509KeyPair([]byte(cert.PemCertificate), []byte(keyPEM))
	if err != nil {
		return "", fmt.Errorf("loading current certificate: %w", err)
	}
	csr, _ := pem.Decode([]byte(csrPEM))
	if csr == nil || csr.Type != "CERTIFICATE REQUEST" {
		return "", errors.New("renewal CSR is not a PEM certificate request")
	}
	b64sum := func(b []byte) string {
		sum := sha256.Sum256(b)
		return base64.RawURLEncoding.EncodeToString(sum[:])
	}
	payload, err := json.Marshal(struct {
		Op         string `json:"op"`
		CertSHA256 string `json:"cert_sha256"`
		CSRSHA256  string `json:"csr_sha256"`
		JTI        string `json:"jti"`
		IAT        int64  `json:"iat"`
	}{"renew", b64sum(pair.Certificate[0]), b64sum(csr.Bytes), uuid.NewString(), now.Unix()})
	if err != nil {
		return "", err
	}
	return signRenewalJWS(pair.PrivateKey, payload)
}

// signRenewalJWS signs payload as a JWS Compact whose alg is the key's own,
// from pki-core's set (ML-DSA-44/65/87, ES256, ES384, EdDSA). A key outside
// that set is refused, never signed with a substitute algorithm.
func signRenewalJWS(key crypto.PrivateKey, payload []byte) (string, error) {
	var alg string
	var sign func([]byte) ([]byte, error)
	switch k := key.(type) {
	case *mldsa.PrivateKey:
		switch k.PublicKey().Parameters() {
		case mldsa.MLDSA44():
			alg = "ML-DSA-44"
		case mldsa.MLDSA65():
			alg = "ML-DSA-65"
		case mldsa.MLDSA87():
			alg = "ML-DSA-87"
		}
		// Pure ML-DSA, empty context: what pki-core's reqsig verifies.
		sign = func(in []byte) ([]byte, error) { return k.Sign(rand.Reader, in, &mldsa.Options{}) }
	case *ecdsa.PrivateKey:
		var h crypto.Hash
		switch k.Curve {
		case elliptic.P256():
			alg, h = "ES256", crypto.SHA256
		case elliptic.P384():
			alg, h = "ES384", crypto.SHA384
		}
		// JWS wants fixed-width r||s, not ASN.1.
		sign = func(in []byte) ([]byte, error) {
			d := h.New()
			d.Write(in)
			r, s, err := ecdsa.Sign(rand.Reader, k, d.Sum(nil))
			if err != nil {
				return nil, err
			}
			n := (k.Curve.Params().BitSize + 7) / 8
			out := make([]byte, 2*n)
			r.FillBytes(out[:n])
			s.FillBytes(out[n:])
			return out, nil
		}
	case ed25519.PrivateKey:
		alg = "EdDSA"
		sign = func(in []byte) ([]byte, error) { return ed25519.Sign(k, in), nil }
	}
	if alg == "" {
		// Reported, not swallowed by the pre-flight: no later run can fix it.
		return "", renewUnavailableError{
			reason:         fmt.Sprintf("this certificate's %T key cannot sign the renewal proof pki-core requires", key),
			needsFreshCert: true,
		}
	}
	enc := base64.RawURLEncoding.EncodeToString
	input := enc([]byte(`{"alg":"`+alg+`"}`)) + "." + enc(payload)
	sig, err := sign([]byte(input))
	if err != nil {
		return "", fmt.Errorf("signing renewal proof: %w", err)
	}
	return input + "." + enc(sig), nil
}

type renewSuccessBody struct {
	Certificate string `json:"certificate"`
}

type renewDetailBody struct {
	Detail string `json:"detail"`
}

// renewViaPKICore exchanges a fresh CSR for a renewed certificate. It returns
// the new leaf, chain and private key, leaving persistence to the caller so a
// failed write cannot lose the still-valid current certificate.
// renewCSRFor builds the renewal CSR. A renewal is a re-issue: new key, same
// identity. The URN comes from stored config rather than the old CN, which may
// be a legacy form carrying no parseable org.
func renewCSRFor(current config.CertificateInfo) (csrPEM, keyPEM string, err error) {
	cn, err := certCommonName(current.PemCertificate)
	if err != nil {
		return "", "", fmt.Errorf("reading current cert CN: %w", err)
	}
	newKeyPEM, err := renewalKeyLike(current)
	if err != nil {
		return "", "", err
	}
	csr, err := certs.GenerateCSR([]byte(newKeyPEM), cn, []string{storedCertIdentityURN(current)})
	if err != nil {
		return "", "", fmt.Errorf("generating CSR: %w", err)
	}
	return csr, newKeyPEM, nil
}

// renewalKeyLike mints the new key in the current key's algorithm and
// parameters. A renewal re-keys, and must never step down: a fixed P-256 here
// silently turned an ML-DSA-65 operator into an EC one.
func renewalKeyLike(current config.CertificateInfo) (string, error) {
	keyPEM, err := current.PrivateKeyPEM()
	if err != nil {
		return "", fmt.Errorf("loading current client key: %w", err)
	}
	cur, err := certs.ParseSigningPrivateKeyPEM([]byte(keyPEM))
	if err != nil {
		return "", fmt.Errorf("parsing current client key: %w", err)
	}
	var next crypto.Signer
	switch k := cur.(type) {
	case *mldsa.PrivateKey:
		next, err = mldsa.GenerateKey(k.PublicKey().Parameters())
	case *ecdsa.PrivateKey:
		next, err = ecdsa.GenerateKey(k.Curve, rand.Reader)
	case ed25519.PrivateKey:
		_, next, err = ed25519.GenerateKey(rand.Reader)
	default:
		return "", fmt.Errorf("cannot renew a certificate whose key is %T", cur)
	}
	if err != nil {
		return "", fmt.Errorf("generating key pair: %w", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(next)
	if err != nil {
		return "", fmt.Errorf("marshaling key pair: %w", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), nil
}

func renewViaPKICoreImpl(ctx context.Context, endpoint string, auth *config.AuthConfig) (certPEM, chainPEM, keyPEM string, err error) {
	if len(auth.Certificates) == 0 {
		return "", "", "", errors.New("auth entry has no certificate to renew")
	}
	current := auth.Certificates[0]

	csrPEM, newKeyPEM, err := renewCSRForFn(current)
	if err != nil {
		return "", "", "", err
	}

	proof, err := renewalPossessionProof(current, csrPEM, timeNowFn())
	if err != nil {
		return "", "", "", err
	}
	body, err := json.Marshal(renewRequestBody{CSR: csrPEM, PossessionProof: proof})
	if err != nil {
		return "", "", "", fmt.Errorf("encoding renewal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", "", "", fmt.Errorf("building renewal request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client, err := renewHTTPClientForFn(current)
	if err != nil {
		return "", "", "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", "", fmt.Errorf("contacting renew frontend: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		var ok renewSuccessBody
		if err := json.NewDecoder(resp.Body).Decode(&ok); err != nil {
			return "", "", "", fmt.Errorf("decoding renewal response: %w", err)
		}
		leaf, chain, err := splitLeafAndChain(ok.Certificate)
		if err != nil {
			return "", "", "", err
		}
		return leaf, chain, newKeyPEM, nil

	case http.StatusAccepted:
		// The presented certificate is no longer renewable on its own and needs a
		// signed grant. Reported rather than retried: the CLI cannot mint one.
		var d renewDetailBody
		_ = json.NewDecoder(resp.Body).Decode(&d)
		return "", "", "", renewUnavailableError{
			reason: "this certificate can no longer be renewed on its own and needs an approved grant",
			detail: d.Detail,
		}

	case http.StatusForbidden:
		// Budget exhausted, or the lineage was never renewable — an
		// entitlement-bearing or over-duration operator certificate is not
		// renewable unless the tenant opted in. Either way a new certificate has
		// to be issued from scratch.
		var d renewDetailBody
		_ = json.NewDecoder(resp.Body).Decode(&d)
		return "", "", "", renewUnavailableError{
			reason:         "this certificate is not renewable, or its renewal budget is used up",
			detail:         d.Detail,
			needsFreshCert: true,
		}

	case http.StatusUnauthorized:
		return "", "", "", renewUnavailableError{
			reason:         "the renew frontend did not accept the current certificate",
			needsFreshCert: true,
		}

	default:
		return "", "", "", renewUnavailableError{
			reason: fmt.Sprintf("the renew frontend answered %s", resp.Status),
		}
	}
}

// ensureFreshCertificate is the pre-flight: it renews the stored certificate
// before it expires, and reports honestly when it cannot.
//
// Returning nil means "carry on with the stored certificate" — including when
// nothing is configured to renew against, which is a supported state and not a
// failure. A non-nil error is always something the user has to act on.
func ensureFreshCertificate(ctx context.Context, auth *config.AuthConfig) error {
	if auth == nil || len(auth.Certificates) == 0 {
		return nil
	}
	notAfter, err := leafNotAfterFn(auth.Certificates[0].PemCertificate)
	if err != nil {
		// An unparseable stored certificate is not this function's problem to
		// diagnose; the connection attempt reports it with better context.
		return nil
	}
	if !certNeedsRenewal(notAfter, timeNowFn()) {
		return nil
	}

	endpoint := renewEndpoint(auth)
	if endpoint == "" {
		if timeNowFn().After(notAfter) {
			// Nothing to renew against and the certificate is already dead: say so
			// plainly instead of letting the caller discover it as a TLS error.
			return errCertExpired
		}
		return nil
	}
	if timeNowFn().After(notAfter) {
		// Past expiry the renew frontend requires a grant, so skip the round trip
		// and report the remedy directly.
		return errCertExpired
	}

	certPEM, chainPEM, keyPEM, err := renewViaPKICore(ctx, endpoint, auth)
	if err != nil {
		var unavailable renewUnavailableError
		if errors.As(err, &unavailable) {
			return unavailable
		}
		// A transport failure while the certificate is still valid is not fatal:
		// the stored certificate has life left, so carry on and let the next run
		// try again rather than blocking the command.
		return nil
	}

	updated := auth.Certificates[0]
	updated.PemCertificate = certPEM
	updated.PemCertificateChain = chainPEM
	updated.PemPrivateKey = keyPEM
	if err := persistRenewedCertificate(auth, updated); err != nil {
		return fmt.Errorf("saving renewed certificate: %w", err)
	}
	auth.Certificates[0] = updated
	return nil
}

// persistRenewedCertificate writes the renewed material back to the matching
// stored session. Indirected for tests.
var persistRenewedCertificate = func(auth *config.AuthConfig, updated config.CertificateInfo) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	for i := range cfg.Auth {
		if cfg.Auth[i].CloudGRPC == auth.CloudGRPC && cfg.Auth[i].CloudDashboard == auth.CloudDashboard &&
			cfg.Auth[i].OAuthIssuer == auth.OAuthIssuer &&
			cfg.Auth[i].OrganizationKey() == auth.OrganizationKey() {
			if len(cfg.Auth[i].Certificates) == 0 {
				cfg.Auth[i].Certificates = []config.CertificateInfo{updated}
			} else {
				cfg.Auth[i].Certificates[0] = updated
			}
			return config.Save(cfg)
		}
	}
	return errors.New("stored session for the renewed certificate no longer exists")
}

// These seams keep the pieces separately testable: the HTTP status mapping is
// pki-core's contract and is worth pinning without standing up real mTLS, which
// is pki-core's own possession proof and is covered there.
var (
	// timeNowFn is indirected so expiry arithmetic is testable.
	timeNowFn = time.Now

	leafNotAfterFn       = leafNotAfter
	renewCSRForFn        = renewCSRFor
	renewHTTPClientForFn = renewHTTPClientFor
	renewViaPKICore      = renewViaPKICoreImpl
)

// ensureFreshCertificateFn is the pre-flight entry point, indirected so tests
// of the callers do not perform network I/O.
var ensureFreshCertificateFn = ensureFreshCertificate
