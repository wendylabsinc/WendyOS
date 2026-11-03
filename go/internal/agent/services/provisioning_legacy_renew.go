package services

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// dialLegacyCloud preserves the Cloud Run certificate proof as well as direct
// endpoint mTLS. Each proof is freshly bound to its actual v1 RPC method.
func dialLegacyCloud(client *TunnelBrokerClient) (*grpc.ClientConn, brokerRequestMetadata, error) {
	opts, proof, err := client.buildDialOpts()
	if err != nil {
		return nil, nil, err
	}
	conn, err := grpc.NewClient(certificateServiceAddr(client.url), opts...)
	return conn, proof, err
}

func (s *ProvisioningService) renewLegacyIfDue(ctx context.Context, now time.Time) (bool, error) {
	s.mu.Lock()
	e, err := classifyCloudEnrollment(s.enrolled, s.cloudHost, s.orgID, s.assetID, s.principalURI)
	previous, chain, key := s.certPEM, s.chainPEM, string(s.keyPEM)
	blocked := s.resetRecoveryErr != nil || s.resetCleanupErr != nil || s.resetRestartPending
	s.mu.Unlock()
	if err != nil || e.Mode != CloudLegacy || blocked {
		return false, err
	}
	leaf, err := storedDeviceLeaf(previous)
	if err != nil {
		return false, err
	}
	if !renewalDue(leaf, now) {
		return false, nil
	}
	if !now.Before(leaf.NotAfter) {
		return false, errors.New("legacy device certificate expired; operator re-enrollment is required")
	}
	identity, found, err := certs.IdentityFromCert(leaf)
	if err != nil || !found || identity.LegacyURN() != certs.AssetURN(e.OrganizationID, e.AssetID) {
		return false, errors.New("legacy certificate does not match the saved Cloud binding")
	}
	newKey, err := certs.GenerateKeyPair()
	if err != nil {
		return false, err
	}
	// Preserve the legacy SAN and any Cloud-issued transitional SAN. Renewal
	// must not change the enrollment protocol or invent a new tenant mapping.
	uris := make([]string, 0, len(leaf.URIs))
	for _, uri := range leaf.URIs {
		uris = append(uris, uri.String())
	}
	csr, err := certs.GenerateCSR([]byte(newKey), leaf.Subject.CommonName, uris, x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth)
	if err != nil {
		return false, err
	}
	conn, proof, err := dialLegacyCloud(NewTunnelBrokerClient(s.logger, e.Host, previous, key, chain, 0))
	if err != nil {
		return false, err
	}
	defer conn.Close()
	md, err := proof(cloudpb.CertificateService_RefreshCertificate_FullMethodName)
	if err != nil {
		return false, err
	}
	rpcCtx, cancel := context.WithTimeout(metadata.NewOutgoingContext(ctx, md), renewRequestTimeout)
	defer cancel()
	response, err := cloudpb.NewCertificateServiceClient(conn).RefreshCertificate(rpcCtx, &cloudpb.RefreshCertificateRequest{PemCsr: csr})
	if err != nil {
		return false, fmt.Errorf("refreshing legacy device certificate: %w", err)
	}
	if response.GetError() != nil {
		return false, fmt.Errorf("legacy certificate refresh refused: %s", response.GetError().GetMessage())
	}
	issued := response.GetCertificate()
	newLeaf, newChain, err := checkLegacyRenewedIdentity(issued.GetPemCertificate(), issued.GetPemCertificateChain(), newKey, identity, chain, now)
	if err != nil {
		return false, err
	}
	if err := writeProvisioningFile(s.configPath, pendingDeviceKeyFile, []byte(newKey)); err != nil {
		return false, err
	}
	if err := s.commitRenewal(previous, newLeaf, newChain, []byte(newKey)); err != nil {
		_ = os.Remove(filepath.Join(s.configPath, pendingDeviceKeyFile))
		return false, err
	}
	return true, nil
}

func checkLegacyRenewedIdentity(leafPEM, chainPEM, keyPEM string, previous certs.WendyIdentity, trustedChain string, now time.Time) (string, string, error) {
	if _, err := certs.TLSKeyPair(leafPEM, "", keyPEM); err != nil {
		return "", "", fmt.Errorf("legacy renewal key mismatch: %w", err)
	}
	leaf, err := storedDeviceLeaf(leafPEM)
	if err != nil {
		return "", "", err
	}
	key, ok := leaf.PublicKey.(*ecdsa.PublicKey)
	if !ok || key.Curve != elliptic.P256() {
		return "", "", errors.New("legacy renewal requires a P-256 device certificate")
	}
	identity, found, err := certs.IdentityFromCert(leaf)
	if err != nil || !found || identity.LegacyURN() != previous.LegacyURN() || identity.Principal != previous.Principal {
		return "", "", errors.New("legacy renewal changed the enrolled device identity")
	}
	if chainPEM == "" {
		chainPEM = trustedChain
	}
	trusted, err := certs.ParseCertsFromPEM([]byte(trustedChain))
	if err != nil || len(trusted) == 0 {
		return "", "", errors.New("legacy enrollment has no renewal trust anchor")
	}
	issued, err := certs.ParseCertsFromPEM([]byte(chainPEM))
	if err != nil {
		return "", "", err
	}
	anchor := trusted[len(trusted)-1:]
	for _, candidate := range append([]*x509.Certificate{leaf}, issued...) {
		if candidate.Equal(anchor[0]) {
			continue
		}
		if err := certs.VerifyPeerCertificateChain(candidate, issued, anchor, x509.ExtKeyUsageClientAuth, now, now.Add(renewClockSkew)); err != nil {
			return "", "", fmt.Errorf("legacy renewed identity is not trusted: %w", err)
		}
	}
	return leafPEM, chainPEM, nil
}
