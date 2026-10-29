package services

import (
	"bytes"
	"context"
	"crypto/mldsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
)

// Pre-expiry device certificate renewal against pki-core's renew frontend
// (registry plan T10).
//
// Every renewal uses a fresh ML-DSA-65 key, whatever key the device holds
// today: that is how existing tier A/B devices leave P-256 (sem 2026-10-20
// 21:29). The agent never chooses an algorithm by tier. pki-core's per-tier
// policy is the only validator, so a refusal is reported as an error and never
// retried with P-256 (HARD rule 1). The Go agent is never tier C: Wendy Lite is
// separate firmware.
//
// Possession is proven by the mTLS handshake with the current leaf; pki-core
// needs no body proof for a device lineage. An expired leaf cannot renew here
// (it takes pki-core's operator-approved recovery path), so the check runs well
// before expiry.
const (
	// renewEndpointEnv overrides the renew frontend URL, e.g.
	// "https://renew.pki.example:8451/v1/renew", as it does for the CLI.
	renewEndpointEnv = "WENDY_PKI_RENEW_ENDPOINT"

	renewCheckInterval  = 6 * time.Hour
	renewRequestTimeout = 30 * time.Second

	// renewClockSkew is how far a renewed leaf's NotBefore may run ahead of the
	// time read before the request. pki-core stamps NotBefore with its own
	// clock at issuance and does not backdate, and a device clock may lag
	// (boards without an RTC); the mTLS verifier allows the same for a lagging
	// clock (maxClockSkewTolerance).
	renewClockSkew = 24 * time.Hour

	// pendingDeviceKeyFile holds the renewal key until the renewed certificate
	// and device-key.pem are both durable; see finishInterruptedRenewal.
	pendingDeviceKeyFile = "device-key.pem.next"
)

// renewRootCAs verifies the renew frontend; nil means the system roots, as for
// the CLI. Tests replace it.
var renewRootCAs *x509.CertPool

// renewEndpointFor derives the renew frontend from the ACME directory the
// device enrolled against: pki-core serves both from the same host family
// (acme.<rest> and renew.<rest>, as the CLI derives from identity.<rest>). A
// directory with an explicit port derives nothing: frontends use different
// ports, so the URL is set explicitly with renewEndpointEnv instead.
func renewEndpointFor(acmeDirectoryURL string) string {
	if endpoint := strings.TrimSpace(os.Getenv(renewEndpointEnv)); endpoint != "" {
		return endpoint
	}
	u, err := url.Parse(acmeDirectoryURL)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" {
		return ""
	}
	rest, ok := strings.CutPrefix(u.Hostname(), "acme.")
	if !ok || rest == "" {
		return ""
	}
	return "https://renew." + rest + "/v1/renew"
}

// renewalDue reports whether less than a third of the leaf's lifetime is left.
func renewalDue(leaf *x509.Certificate, now time.Time) bool {
	lifetime := leaf.NotAfter.Sub(leaf.NotBefore)
	return leaf.NotAfter.Sub(now) < lifetime/3
}

// RunRenewal checks the device certificate at start and then every
// renewCheckInterval until ctx ends. After a successful renewal it calls
// onRenewed, which must load the new identity (the agent restarts).
func (s *ProvisioningService) RunRenewal(ctx context.Context, onRenewed func()) {
	ticker := time.NewTicker(renewCheckInterval)
	defer ticker.Stop()
	for {
		renewed, err := s.RenewIfDue(ctx, time.Now())
		switch {
		case err != nil:
			s.logger.Error("Device certificate renewal failed; retrying at the next check",
				zap.String("event", "device_cert_renewal_failed"),
				zap.String("requested_key_algorithm", "ML-DSA-65"),
				zap.Error(err))
		case renewed:
			s.logger.Info("Device certificate renewed",
				zap.String("event", "device_cert_renewed"),
				zap.String("key_algorithm", "ML-DSA-65"))
			onRenewed()
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// RenewIfDue renews a pki-core device certificate that is due at now with a
// fresh ML-DSA-65 key. It returns false without error when there is nothing to
// renew: not enrolled, a legacy (non pki-core) enrollment, or not yet due.
func (s *ProvisioningService) RenewIfDue(ctx context.Context, now time.Time) (bool, error) {
	s.mu.Lock()
	enrolled, principal, directory := s.enrolled && s.resetRecoveryErr == nil, s.principalURI, s.acmeDirectoryURL
	certPEM, chainPEM, keyPEM := s.certPEM, s.chainPEM, string(s.keyPEM)
	s.mu.Unlock()
	if !enrolled || principal == "" || directory == "" {
		return false, nil
	}
	leaf, err := storedDeviceLeaf(certPEM)
	if err != nil {
		return false, err
	}
	if !renewalDue(leaf, now) {
		return false, nil
	}
	if !now.Before(leaf.NotAfter) {
		return false, errors.New("device certificate has expired; it needs pki-core's operator-approved recovery, not a renewal")
	}
	endpoint := renewEndpointFor(directory)
	if endpoint == "" {
		return false, fmt.Errorf("no pki-core renew endpoint derivable from %q; set %s", directory, renewEndpointEnv)
	}

	newKey, err := certs.GenerateMLDSAKeyPair()
	if err != nil {
		return false, err
	}
	csrPEM, err := certs.GenerateCSR([]byte(newKey), leaf.Subject.CommonName, nil,
		x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth)
	if err != nil {
		return false, fmt.Errorf("building renewal CSR: %w", err)
	}
	// The key must outlive a crash between the response and the state write.
	if err := writeProvisioningFile(s.configPath, pendingDeviceKeyFile, []byte(newKey)); err != nil {
		return false, err
	}
	bundle, err := postRenewal(ctx, endpoint, certPEM, chainPEM, keyPEM, csrPEM)
	if err != nil {
		return false, err
	}
	newLeaf, newChain, err := checkRenewedIdentity(bundle, newKey, principal, chainPEM, now)
	if err != nil {
		return false, err
	}
	if err := s.commitRenewal(certPEM, newLeaf, newChain, []byte(newKey)); err != nil {
		return false, err
	}
	return true, nil
}

func storedDeviceLeaf(certPEM string) (*x509.Certificate, error) {
	leafPEM, err := certs.LeafCertificatePEM(certPEM)
	if err != nil {
		return nil, fmt.Errorf("reading device certificate: %w", err)
	}
	leaves, err := certs.ParseCertsFromPEM([]byte(leafPEM))
	if err != nil || len(leaves) == 0 {
		return nil, fmt.Errorf("parsing device certificate: %v", err)
	}
	return leaves[0], nil
}

// postRenewal presents the current identity to the renew frontend and returns
// the PEM bundle of a RENEWED answer. Every other outcome is an error that
// names it; none is retried with another key.
func postRenewal(ctx context.Context, endpoint, certPEM, chainPEM, keyPEM, csrPEM string) (string, error) {
	pair, err := certs.TLSKeyPair(certPEM, chainPEM, keyPEM)
	if err != nil {
		return "", fmt.Errorf("loading current device identity: %w", err)
	}
	client := &http.Client{
		Timeout: renewRequestTimeout,
		// Never re-send the CSR or present the leaf to another host.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: &http.Transport{
			// The proxy from the agent's environment, as ACME enrollment uses.
			Proxy: http.ProxyFromEnvironment,
			// One request per attempt; keep no connection that authenticates
			// with the current leaf until the next attempt hours later.
			DisableKeepAlives: true,
			TLSClientConfig: &tls.Config{
				MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{pair}, RootCAs: renewRootCAs,
			},
		},
	}
	body, _ := json.Marshal(map[string]string{"csr": csrPEM})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("pki-core renew request: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	var answer struct {
		Certificate string `json:"certificate"`
		Detail      string `json:"detail"`
	}
	_ = json.Unmarshal(raw, &answer)
	switch resp.StatusCode {
	case http.StatusOK:
		if answer.Certificate == "" {
			return "", errors.New("pki-core renewed but returned no certificate")
		}
		return answer.Certificate, nil
	case http.StatusAccepted:
		return "", fmt.Errorf("pki-core requires an operator-approved grant to renew: %s", answer.Detail[:min(len(answer.Detail), 512)])
	case http.StatusForbidden:
		return "", fmt.Errorf("pki-core denied the renewal: %s", answer.Detail[:min(len(answer.Detail), 512)])
	default:
		return "", fmt.Errorf("pki-core renew answered HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw[:min(len(raw), 512)])))
	}
}

// checkRenewedIdentity splits the bundle and refuses a leaf that is not for
// newKey, is not ML-DSA-65, or names another principal. The returned chain
// becomes the agent's mTLS trust, so the leaf and every certificate in it must
// verify to the top of the chain the device already holds (its trust anchor);
// a renewal can never introduce a new CA. With no chain returned the current
// one is kept.
func checkRenewedIdentity(bundle, newKey, principal, currentChainPEM string, now time.Time) (leafPEM, chainPEM string, err error) {
	var leaf []byte
	var chain strings.Builder
	for rest := []byte(bundle); ; {
		var block *pem.Block
		if block, rest = pem.Decode(rest); block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		if leaf == nil {
			leaf = pem.EncodeToMemory(block)
		} else {
			chain.Write(pem.EncodeToMemory(block))
		}
	}
	if leaf == nil {
		return "", "", errors.New("pki-core renewal carried no certificate")
	}
	if _, err := certs.TLSKeyPair(string(leaf), "", newKey); err != nil {
		return "", "", fmt.Errorf("renewed certificate does not match the renewal key: %w", err)
	}
	parsed, err := storedDeviceLeaf(string(leaf))
	if err != nil {
		return "", "", err
	}
	if pub, ok := parsed.PublicKey.(*mldsa.PublicKey); !ok || pub.Parameters() != mldsa.MLDSA65() {
		return "", "", fmt.Errorf("renewed certificate key is %T, want ML-DSA-65", parsed.PublicKey)
	}
	if got, _ := certs.TenantPrincipalFromCert(parsed); got != principal {
		return "", "", fmt.Errorf("renewed certificate names %q, want %q", got, principal)
	}
	leafNow := now.Add(renewClockSkew)
	if parsed.NotBefore.After(leafNow) {
		return "", "", fmt.Errorf("renewed certificate is not valid until %s; the device clock read %s",
			parsed.NotBefore.UTC().Format(time.RFC3339), now.UTC().Format(time.RFC3339))
	}
	current, err := certs.ParseCertsFromPEM([]byte(currentChainPEM))
	if err != nil || len(current) == 0 {
		return "", "", fmt.Errorf("device holds no CA chain to check the renewal against: %v", err)
	}
	chainPEM = chain.String()
	if chainPEM == "" {
		chainPEM = currentChainPEM
	}
	issued, err := certs.ParseCertsFromPEM([]byte(chainPEM))
	if err != nil {
		return "", "", fmt.Errorf("parsing renewed chain: %w", err)
	}
	anchor := []*x509.Certificate{current[len(current)-1]}
	if err := certs.VerifyPeerCertificateChain(parsed, issued, anchor, x509.ExtKeyUsageClientAuth, now, leafNow); err != nil {
		return "", "", fmt.Errorf("renewed certificate does not chain to the device's trust anchor: %w", err)
	}
	for _, ca := range issued {
		if ca.Equal(anchor[0]) {
			continue
		}
		if err := certs.VerifyPeerCertificateChain(ca, issued, anchor, x509.ExtKeyUsageClientAuth, now, leafNow); err != nil {
			return "", "", fmt.Errorf("renewed chain carries %q, which does not chain to the device's trust anchor: %w", ca.Subject.CommonName, err)
		}
	}
	return string(leaf), chainPEM, nil
}

// commitRenewal persists the renewed identity unless it changed meanwhile
// (unenrollment, re-enrollment) or a Cloud unenrollment is pending: that
// saves a receipt bound to the current leaf before it clears the enrollment,
// so replacing the leaf in between would strand the reset. Order: certificate
// first, then device-key.pem, then the pending key goes; a crash in between
// is finished by finishInterruptedRenewal at the next start.
func (s *ProvisioningService) commitRenewal(previousCertPEM, certPEM, chainPEM string, keyPEM []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.enrolled || s.certPEM != previousCertPEM {
		return errors.New("device identity changed during renewal; renewed certificate discarded")
	}
	data, err := os.ReadFile(s.statePath())
	if err != nil {
		return fmt.Errorf("reading provisioning state: %w", err)
	}
	var state provisioningState
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("parsing provisioning state: %w", err)
	}
	if state.Unenrollment != nil {
		return errors.New("device unenrollment is pending; renewed certificate discarded")
	}
	state.CertPEM, state.ChainPEM = certPEM, chainPEM
	if err := s.saveState(&state); err != nil {
		return fmt.Errorf("saving renewed certificate: %w", err)
	}
	// The certificate is committed: from here the renewal must complete, so a
	// failed key move is left to finishInterruptedRenewal after the restart
	// rather than reported as a failure that would start another renewal.
	if err := WriteDeviceKey(s.configPath, keyPEM); err != nil {
		s.logger.Error("Saving the renewal key failed; it is recovered from the pending key at the next start",
			zap.String("event", "device_cert_renewal_key_pending"), zap.Error(err))
	} else {
		_ = os.Remove(filepath.Join(s.configPath, pendingDeviceKeyFile))
	}
	for i := range s.keyPEM {
		s.keyPEM[i] = 0
	}
	s.certPEM, s.chainPEM, s.keyPEM = certPEM, chainPEM, keyPEM
	return nil
}

// finishInterruptedRenewal runs from loadState, before the service is shared.
// A pending key that matches the stored certificate means a renewal committed
// the certificate but not device-key.pem: adopt it. Any other pending key —
// a renewal that never committed, or one that outlived an unenrollment — is
// dropped.
func (s *ProvisioningService) finishInterruptedRenewal() {
	path := filepath.Join(s.configPath, pendingDeviceKeyFile)
	pending, err := os.ReadFile(path)
	if err != nil {
		return
	}
	if _, err := certs.TLSKeyPair(s.certPEM, "", string(pending)); err == nil {
		if err := WriteDeviceKey(s.configPath, pending); err != nil {
			s.logger.Error("Cannot finish interrupted certificate renewal", zap.Error(err))
			s.keyPEM = pending
			return
		}
		s.keyPEM = pending
		s.logger.Info("Finished interrupted device certificate renewal",
			zap.String("event", "device_cert_renewal_recovered"))
	}
	_ = os.Remove(path)
}
