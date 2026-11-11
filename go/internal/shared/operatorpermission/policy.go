package operatorpermission

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Policy is immutable after construction. Audit is the staged rollout default;
// enforce must be selected explicitly on each enrolled agent.
type Policy struct {
	Enforce    bool
	Scope      certs.Scope
	DeviceUUID string
	Logger     *zap.Logger
	Now        func() time.Time
}

func FromTLS(cfg *tls.Config, logger *zap.Logger) (*Policy, error) {
	mode := os.Getenv("WENDY_OPERATOR_PERMISSIONS")
	if mode != "" && mode != "audit" && mode != "enforce" {
		return nil, fmt.Errorf("WENDY_OPERATOR_PERMISSIONS must be audit or enforce")
	}
	if cfg == nil || len(cfg.Certificates) == 0 || len(cfg.Certificates[0].Certificate) == 0 {
		return nil, fmt.Errorf("operator policy requires the device certificate")
	}
	leaf, err := x509.ParseCertificate(cfg.Certificates[0].Certificate[0])
	if err != nil {
		return nil, err
	}
	id, found, err := certs.IdentityFromCert(leaf)
	if err != nil || !found {
		return nil, fmt.Errorf("operator policy requires a device identity")
	}
	p := &Policy{Enforce: mode == "enforce", Scope: id.Scope(), Logger: logger, Now: cfg.Time}
	// Current mesh identities use the Cloud device UUID. Do not interpret legacy
	// PKI device names or numeric asset IDs as a Cloud UUID.
	if device, err := meshid.DeviceFromPrincipal(id.Principal); err == nil {
		p.DeviceUUID = device.UUID().String()
	}
	return p, nil
}

// Check accepts a verified leaf. Metadata and response entitlement lists are
// never consulted. The extension itself is the offline authorization evidence.
func (p *Policy) Check(leaf *x509.Certificate, permission, operation string) error {
	err := p.Decide(leaf, permission)
	if err == nil {
		return nil
	}
	if p.Logger != nil {
		serial := ""
		if leaf != nil && leaf.SerialNumber != nil {
			serial = leaf.SerialNumber.String()
		}
		p.Logger.Warn("operator permission denied", zap.String("serial", serial), zap.String("device", p.DeviceUUID), zap.String("operation", operation), zap.String("permission", permission), zap.Bool("enforced", p.Enforce), zap.Error(err))
	}
	if p.Enforce {
		return err
	}
	return nil
}

func (p *Policy) Decide(leaf *x509.Certificate, permission string) error {
	deny := func(reason string) error { return status.Error(codes.PermissionDenied, reason) }
	if leaf == nil {
		return deny("operator certificate required")
	}
	id, found, err := certs.IdentityFromCert(leaf)
	if err != nil || !found || id.EntityType != certs.EntityUser || !p.Scope.Known() || !id.Scope().Matches(p.Scope) {
		return deny("operator must belong to the device tenant")
	}
	now := time.Now()
	if p.Now != nil {
		now = p.Now()
	}
	if now.Before(leaf.NotBefore) || !now.Before(leaf.NotAfter) {
		return deny("operator certificate is outside its validity window")
	}
	claims, present, err := Parse(leaf)
	if err != nil {
		return deny("malformed operator entitlements")
	}
	if permission == "" {
		return deny("operation has no approved operator permission")
	}
	if !present && permission == "device:read" {
		return nil
	}
	if !present {
		return deny("operation requires a short-lived entitled operator certificate")
	}
	if leaf.NotAfter.Sub(leaf.NotBefore) > 15*time.Minute {
		return deny("entitled certificate lifetime exceeds 15 minutes")
	}
	if !Allows(claims, permission, p.DeviceUUID) {
		return deny("certificate does not grant this permission for this device")
	}
	return nil
}
