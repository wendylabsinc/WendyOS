package commands

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

// A failed or incomplete status read must never become an unenrolled device.
type DesktopEnrollment struct {
	Enrolled                         bool
	OrganizationID, OrganizationName string
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

func DesktopEnrollDevice(ctx context.Context, conn *grpcclient.AgentConnection, account, organization, name string) error {
	if account == "" || organization == "" {
		return errors.New("choose an organization before enrolling")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	auth, ok := cfg.ContextByName(account)
	if !ok || len(auth.Certificates) == 0 || auth.OrganizationKey() != organization {
		return errors.New("this organization account is no longer available; sign in and choose it again")
	}
	if err := ensureFreshCertificateFn(ctx, auth); err != nil {
		return fmt.Errorf("sign in to this organization again: %w", err)
	}
	if err := validateEnrollmentCertificate(auth); err != nil {
		return err
	}
	current, err := DesktopDeviceEnrollment(ctx, conn)
	if err != nil {
		return fmt.Errorf("check device enrollment before trying again: %w", err)
	}
	if current.Enrolled {
		return errors.New("this device is already enrolled")
	}
	if err := runEnrollDevice(ctx, conn, auth, name, 0); err != nil {
		return err
	}
	enrolled, err := DesktopDeviceEnrollment(ctx, conn)
	if err != nil {
		return fmt.Errorf("enrollment was sent, but could not be verified; reconnect and check its status: %w", err)
	}
	if !enrolled.Enrolled || enrolled.OrganizationID != organization {
		return errors.New("enrollment was sent, but the selected organization has not been verified; reconnect and check its status")
	}
	return nil
}
