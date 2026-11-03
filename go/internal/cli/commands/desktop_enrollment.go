package commands

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// A failed or incomplete status read must never become an unenrolled device.
type DesktopEnrollment struct {
	Enrolled                         bool
	OrganizationID, OrganizationName string
	DeviceID                         string
}

type DesktopEnrollmentOrganization struct {
	Account, ID, Name, Email string
}

func DesktopDeviceEnrollment(ctx context.Context, conn *grpcclient.AgentConnection) (*DesktopEnrollment, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	response, err := deviceProvisioning(ctx, conn)
	if err != nil {
		return nil, err
	}
	if response.GetNotProvisioned() != nil {
		return &DesktopEnrollment{}, nil
	}
	if response.GetProvisioned() == nil {
		return nil, errors.New("the device did not report its enrollment status")
	}
	// Keep enrolled true even when an older agent omits its organization identity.
	result := &DesktopEnrollment{Enrolled: true}
	provisioned := response.GetProvisioned()
	if principal := provisioned.GetPrincipalUri(); principal != "" {
		if identity, err := certs.ParsePrincipal(principal); err == nil {
			result.OrganizationID = identity.TenantUUID
			if identity.EntityType == certs.EntityAsset {
				result.DeviceID = identity.EntityID
			}
		}
	} else if provisioned.GetOrganizationId() > 0 {
		result.OrganizationID = strconv.FormatInt(int64(provisioned.GetOrganizationId()), 10)
	}
	if result.OrganizationID != "" {
		result.OrganizationName = deviceOrganizationName(ctx, provisioned.GetCloudHost(), result.OrganizationID)
	}
	return result, nil
}

// Each option is a saved organization-scoped identity, not the global default.
func DesktopEnrollmentOrganizations(ctx context.Context) ([]DesktopEnrollmentOrganization, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	result := []DesktopEnrollmentOrganization{}
	for _, auth := range cfg.Auth {
		if len(auth.Certificates) == 0 {
			continue
		}
		id := auth.OrganizationKey()
		if id == "" || id == "0" {
			continue
		}
		name := cloudOrganizationName(ctx, &auth)
		if name == "" {
			name = id
		}
		result = append(result, DesktopEnrollmentOrganization{Account: auth.Name, ID: id, Name: name, Email: desktopAccountInfo(auth).Email})
	}
	return result, nil
}

// DesktopEnrollDevice returns the verified connection for subsequent device operations.
// The caller must replace and close the original connection when it differs.
func DesktopEnrollDevice(ctx context.Context, conn *grpcclient.AgentConnection, account, organization, name string) (*grpcclient.AgentConnection, error) {
	if account == "" || organization == "" {
		return nil, errors.New("choose an organization before enrolling")
	}
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	auth, ok := cfg.ContextByName(account)
	if !ok || len(auth.Certificates) == 0 || auth.OrganizationKey() != organization {
		return nil, errors.New("this organization account is no longer available; sign in and choose it again")
	}
	if err := ensureFreshCertificateFn(ctx, auth); err != nil {
		return nil, fmt.Errorf("sign in to this organization again: %w", err)
	}
	if err := validateEnrollmentCertificate(auth); err != nil {
		return nil, err
	}
	current, err := DesktopDeviceEnrollment(ctx, conn)
	if err != nil {
		return nil, fmt.Errorf("check device enrollment before trying again: %w", err)
	}
	if current.Enrolled {
		return nil, errors.New("this device is already enrolled")
	}
	identity, err := runEnrollDeviceIdentity(ctx, conn, auth, name, 0)
	if err != nil {
		return nil, err
	}
	return verifyAndSaveEnrollment(ctx, conn, auth, identity, name)
}

type enrollmentBindingRecorderKey struct{}

// verifyAndSaveEnrollment shares the exact-identity reconnect and persistence
// between CLI enrollment and desktop enrollment. Reset recovery defers the
// save until its old binding has been cleared, but never defers verification.
func verifyAndSaveEnrollment(ctx context.Context, prior *grpcclient.AgentConnection, auth *config.AuthConfig, identity *certs.WendyIdentity, name string) (*grpcclient.AgentConnection, error) {
	if prior == nil || auth == nil || identity == nil || identity.EntityType != certs.EntityAsset || identity.EntityID == "" || !identity.Scope().Known() {
		return nil, certs.ErrExpectedDeviceIdentity
	}
	organization := identity.TenantUUID
	if organization == "" {
		organization = strconv.FormatInt(int64(identity.OrgID), 10)
	}
	verified, err := desktopEnrollmentConnection(ctx, prior, auth, identity, organization)
	if err != nil {
		return nil, fmt.Errorf("enrollment was sent, but could not be verified; reconnect and check its status: %w", err)
	}
	save := func() error { return saveEnrollmentBinding(prior, auth, identity, name) }
	if record, ok := ctx.Value(enrollmentBindingRecorderKey{}).(func(func() error)); ok {
		record(save)
	} else if err := save(); err != nil {
		if verified != prior {
			verified.Close()
		}
		return nil, fmt.Errorf("enrollment was verified, but its local device binding could not be saved: %w", err)
	}
	return verified, nil
}

// Persist the identity minted by enrollment only after its exact-identity
// reconnect succeeds. This gives local PKI and BLE users a trusted expectation
// for later connections without learning a binding from an arbitrary peer.
func saveEnrollmentBinding(prior *grpcclient.AgentConnection, auth *config.AuthConfig, identity *certs.WendyIdentity, name string) error {
	if prior == nil || auth == nil || identity == nil || identity.EntityType != certs.EntityAsset || identity.EntityID == "" || !identity.Scope().Known() {
		return certs.ErrExpectedDeviceIdentity
	}
	selectedKey := prior.DevicePinKey
	if selectedKey == "" {
		selectedKey = prior.Host
		if selectedKey == "" && prior.Addr != "" {
			selectedKey = pinKeyForAddr(prior.Addr)
		}
	}
	keys := []string{name, selectedKey}
	return config.Update(func(cfg *config.Config) (bool, error) {
		for _, key := range keys {
			if key == "" {
				continue
			}
			if pin, ok := cfg.DevicePinFor(key); ok {
				expected := expectedIdentityForPin(pin)
				if expected != nil && !expected.SameEntity(*identity) {
					return false, fmt.Errorf("saved binding for %q belongs to another device; clear that binding explicitly before replacing it", key)
				}
			}
		}
		changed := false
		for _, key := range keys {
			if key == "" {
				continue
			}
			if pin, ok := cfg.DevicePinFor(key); ok {
				if expected := expectedIdentityForPin(pin); expected != nil && expected.SameEntity(*identity) {
					continue
				}
			}
			cfg.SetDevicePin(key, int(identity.OrgID), auth.CloudGRPC, identity.EntityID, identity.Principal)
			changed = true
		}
		return changed, nil
	})
}

// TCP enrollment closes the plaintext listener. Use the selected account and
// the identity returned by enrollment, never the global account or a new picker.
// The caller owns the returned connection; the original remains caller-owned.
func desktopEnrollmentConnection(ctx context.Context, prior *grpcclient.AgentConnection, auth *config.AuthConfig, identity *certs.WendyIdentity, organization string) (*grpcclient.AgentConnection, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	next := prior
	if prior.Addr != "" {
		host, portText, err := net.SplitHostPort(prior.Addr)
		if err != nil {
			return nil, err
		}
		port, err := strconv.Atoi(portText)
		if err != nil || port < 1 || port >= 65535 {
			return nil, errors.New("invalid enrollment endpoint port")
		}
		if !prior.IsMTLS {
			port++
		}
		next, err = grpcclient.ConnectWithTLSExpecting(ctx, net.JoinHostPort(host, strconv.Itoa(port)), &auth.Certificates[0], openPinStore(), identity)
		if err != nil {
			return nil, err
		}
		next.SimulatorName = prior.SimulatorName
		next.DevicePinKey = prior.DevicePinKey
	}
	// Unix sockets and existing authenticated transports keep their connection.
	closeFailed := func() {
		if next != prior {
			next.Close()
		}
	}
	var err error
	for {
		var enrolled *DesktopEnrollment
		enrolled, err = DesktopDeviceEnrollment(ctx, next)
		if err == nil {
			if !enrolled.Enrolled || enrolled.OrganizationID != organization {
				err = errors.New("the device did not confirm enrollment in the selected organization")
				break
			}
			return next, nil
		}
		if mismatch, ok := next.IdentityMismatch(); ok {
			err = mismatch
			break
		}
		if mismatch, ok := next.PinMismatch(); ok {
			err = mismatch
			break
		}
		if ctx.Err() != nil || (status.Code(err) != codes.Unavailable && status.Code(err) != codes.DeadlineExceeded) {
			break
		}
		select {
		case <-ctx.Done():
			closeFailed()
			return nil, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	closeFailed()
	return nil, err
}
