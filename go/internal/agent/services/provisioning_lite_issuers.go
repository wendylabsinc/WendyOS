package services

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/estcacerts"
	"go.uber.org/zap"
)

func liteIssuerEndpoint(directory string) (tenant string, endpoint *url.URL) {
	u, err := url.Parse(directory)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" {
		return "", nil
	}
	host, ok := strings.CutPrefix(u.Hostname(), "acme.")
	tenant, okPath := strings.CutSuffix(strings.TrimPrefix(u.Path, "/"), "/acme/directory")
	parsedTenant, err := uuid.Parse(tenant)
	if !ok || !okPath || host == "" || err != nil || parsedTenant.String() != tenant {
		return "", nil
	}
	return tenant, &url.URL{Scheme: "https", Host: "est." + host,
		Path:     "/.well-known/est/" + tenant + "/cacerts",
		RawQuery: "chain_algorithm=ECDSA_P256&full_chain=true"}
}

// Fetch only the public candidates. The enrolled identity establishes the
// platform trust used to validate them before committing provisioning state.
var fetchLiteIssuers = func(ctx context.Context, endpoint *url.URL) (string, error) {
	bundle, algorithm, err := estcacerts.Fetch(ctx, http.DefaultClient, endpoint)
	if err != nil {
		return "", err
	}
	if algorithm != estcacerts.Classical {
		return "", fmt.Errorf("EST did not confirm the parent-signed Lite CA")
	}
	return estcacerts.PEM(bundle), nil
}

func validatedLiteIssuers(bundle, chain, tenant string) (string, error) {
	issuers, err := certs.ParseCertsFromPEM([]byte(bundle))
	if err != nil {
		return "", err
	}
	path, _, err := estcacerts.LitePathForEnrolledPeer(issuers, chain, tenant, time.Now())
	if err != nil {
		return "", err
	}
	return estcacerts.PEM(path), nil
}

func (s *ProvisioningService) LitePeerIssuers() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.litePeerIssuers
}

// RunLiteIssuerRefresh also upgrades devices already enrolled before issuer
// provisioning existed. The old snapshot survives temporary PKI failures.
func (s *ProvisioningService) RunLiteIssuerRefresh(ctx context.Context, onUpdated func()) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		updated, err := s.refreshLiteIssuers(ctx)
		if err != nil && ctx.Err() == nil {
			s.logger.Warn("Refreshing Wendy Net Lite issuers failed", zap.Error(err))
		}
		if updated {
			onUpdated()
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *ProvisioningService) refreshLiteIssuers(ctx context.Context) (bool, error) {
	s.mu.Lock()
	enrolled, directory, ownCert, chain := s.enrolled && s.resetRecoveryErr == nil,
		s.acmeDirectoryURL, s.certPEM, s.chainPEM
	s.mu.Unlock()
	tenant, endpoint := liteIssuerEndpoint(directory)
	if !enrolled || endpoint == nil || chain == "" {
		return false, nil
	}
	provisioned, ok := certs.ScopeFromCertPEM(ownCert)
	if !ok || provisioned.TenantUUID != tenant {
		return false, fmt.Errorf("Lite issuer endpoint does not match the enrolled tenant")
	}
	bundle, err := fetchLiteIssuers(ctx, endpoint)
	if err != nil {
		return false, err
	}
	bundle, err = validatedLiteIssuers(bundle, chain, tenant)
	if err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.enrolled || s.certPEM != ownCert || s.chainPEM != chain || s.litePeerIssuers == bundle {
		return false, nil
	}
	data, err := os.ReadFile(s.statePath())
	if err != nil {
		return false, err
	}
	var state provisioningState
	if err := json.Unmarshal(data, &state); err != nil {
		return false, err
	}
	if state.Unenrollment != nil || state.LocalReset != nil {
		return false, nil
	}
	state.LitePeerIssuers = bundle
	if err := s.saveState(&state); err != nil {
		return false, err
	}
	s.litePeerIssuers = bundle
	return true, nil
}
