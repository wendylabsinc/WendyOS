package services

import (
	"fmt"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
)

// CloudEnrollmentMode describes the enrollment protocol, not the certificate's
// preferred display identity. Legacy Cloud can issue a leaf with both SAN forms.
type CloudEnrollmentMode string

const (
	CloudUnenrolled CloudEnrollmentMode = "unenrolled"
	CloudLegacy     CloudEnrollmentMode = "legacy"
	CloudPKI        CloudEnrollmentMode = "pki"
)

// CloudEnrollment is one consistent snapshot of the saved Cloud binding.
// Its mode never changes in response to a failed network request.
type CloudEnrollment struct {
	Mode                    CloudEnrollmentMode
	Host                    string
	OrganizationID, AssetID int32
	Principal               string
}

func classifyCloudEnrollment(enrolled bool, host string, orgID, assetID int32, principal string) (CloudEnrollment, error) {
	e := CloudEnrollment{Mode: CloudUnenrolled, Host: host, OrganizationID: orgID, AssetID: assetID, Principal: principal}
	if !enrolled {
		return e, nil
	}
	if host == "" {
		return e, fmt.Errorf("enrolled device has no Cloud endpoint")
	}
	if principal != "" {
		id, err := certs.ParsePrincipal(principal)
		if err != nil || id.EntityType != certs.EntityAsset {
			return e, fmt.Errorf("enrolled device has an invalid PKI device principal")
		}
		e.Mode = CloudPKI
		return e, nil
	}
	if orgID <= 0 || assetID <= 0 {
		return e, fmt.Errorf("legacy enrollment requires positive organization and asset IDs")
	}
	e.Mode = CloudLegacy
	return e, nil
}

func (s *ProvisioningService) CloudEnrollment() (CloudEnrollment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return classifyCloudEnrollment(s.enrolled, s.cloudHost, s.orgID, s.assetID, s.principalURI)
}

// PKICloudCredentials keeps v2-only background services dormant for legacy
// enrollments, including legacy leaves which also carry a transitional SAN.
func (s *ProvisioningService) PKICloudCredentials() (host, certificate, chain, key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, err := classifyCloudEnrollment(s.enrolled, s.cloudHost, s.orgID, s.assetID, s.principalURI)
	if err != nil || e.Mode != CloudPKI {
		return "", "", "", ""
	}
	return e.Host, s.certPEM, s.chainPEM, string(s.keyPEM)
}

// LegacyCloudCredentials atomically captures the v1 binding and credentials.
func (s *ProvisioningService) LegacyCloudCredentials() (CloudEnrollment, string, string, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, err := classifyCloudEnrollment(s.enrolled, s.cloudHost, s.orgID, s.assetID, s.principalURI)
	if err != nil || e.Mode != CloudLegacy {
		return e, "", "", "", err
	}
	return e, s.certPEM, s.chainPEM, string(s.keyPEM), nil
}
