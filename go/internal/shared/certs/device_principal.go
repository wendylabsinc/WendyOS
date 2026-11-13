package certs

import (
	"fmt"
	"strings"
)

// ParseDevicePrincipal requires a direct PKI device principal using the name
// grammar enforced by pki-core at issuance. A discovery result is still only
// a hint until the peer certificate is verified against this identity.
func ParseDevicePrincipal(principal string) (WendyIdentity, error) {
	id, err := ParsePrincipal(principal)
	if err != nil || principal != DeviceSPIFFEURI(id.TenantUUID, id.EntityID) {
		return WendyIdentity{}, fmt.Errorf("invalid PKI device principal")
	}
	for _, segment := range strings.Split(id.EntityID, "/") {
		if len(segment) == 0 || len(segment) > 64 || segment == "." || segment == ".." {
			return WendyIdentity{}, fmt.Errorf("invalid PKI device name")
		}
		for _, c := range segment {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
				return WendyIdentity{}, fmt.Errorf("invalid PKI device name")
			}
		}
	}
	return id, nil
}
