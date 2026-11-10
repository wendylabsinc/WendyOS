package builddelivery

import (
	"crypto/x509"
	"errors"

	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

func authorizeEntitlements(leaf *x509.Certificate, target meshid.Device) error {
	rules, err := certificateEntitlements(leaf)
	if err != nil {
		return err
	}
	return authorizeDeployment(rules, target.UUID().String())
}

// Cloud must resolve device scope into the certificate. Unsigned site/tag
// membership cannot expand authority on an offline target; deny always wins.
func authorizeDeployment(rules []entitlement, device string) error {
	allowed := false
	for _, rule := range rules {
		if rule.category != "deployment" || (rule.permission != "create" && rule.permission != "*") {
			continue
		}
		matches := rule.target.matchesDevice(device)
		switch rule.effect {
		case entitlementDeny:
			if matches || rule.target.requiresMembership() {
				return errors.New("operator deployment entitlement denied")
			}
		case entitlementAllow:
			allowed = allowed || matches
		}
	}
	if !allowed {
		return errors.New("operator lacks deployment:create for the target")
	}
	return nil
}
