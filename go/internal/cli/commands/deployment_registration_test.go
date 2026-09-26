package commands

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

const registrationTenant = "123e4567-e89b-12d3-a456-426614174000"

func registrationDeviceV2() *deploymentEnrollment {
	return &deploymentEnrollment{
		cloudHost:      "cloud.example:443",
		organizationID: registrationTenant,
	}
}

func registrationConfigV2() *config.Config {
	return &config.Config{Auth: []config.AuthConfig{
		{
			CloudGRPC: "cloud.example:443",
			Certificates: []config.CertificateInfo{
				{PrincipalURI: certs.UserSPIFFEURI("223e4567-e89b-12d3-a456-426614174000", "other")},
				{PrincipalURI: certs.UserSPIFFEURI(registrationTenant, "operator")},
			},
		},
	}}
}

func TestDeploymentAuthUsesOnlyDeviceOrganizationAndTrustedHost(t *testing.T) {
	auth, err := deploymentAuth(registrationConfigV2(), registrationDeviceV2())
	if err != nil || len(auth.Certificates) != 1 || auth.Certificates[0].TenantUUID() != registrationTenant {
		t.Fatalf("wrong operator selection: %v, %v", auth, err)
	}

	for _, modify := range []func(*deploymentEnrollment){
		func(device *deploymentEnrollment) { device.cloudHost = "attacker.example:443" },
		func(device *deploymentEnrollment) { device.organizationID = "323e4567-e89b-12d3-a456-426614174000" },
		func(device *deploymentEnrollment) { device.organizationID = "" },
	} {
		device := registrationDeviceV2()
		modify(device)
		if _, err := deploymentAuth(registrationConfigV2(), device); err == nil {
			t.Fatal("accepted unmatched or incomplete enrollment")
		}
	}

	cfg := registrationConfigV2()
	cfg.Auth[0].Certificates = []config.CertificateInfo{{PrincipalURI: certs.DeviceSPIFFEURI(registrationTenant, "device-1")}}
	if _, err := deploymentAuth(cfg, registrationDeviceV2()); err == nil {
		t.Fatal("used device credentials for operator registration")
	}
}

func TestRegistrationSkipsUnenrolledDevicesWithoutLoadingCredentials(t *testing.T) {
	err := registerDeviceApps(context.Background(), func(context.Context) (*deploymentEnrollment, error) {
		return nil, nil
	}, []string{"app"}, func() (*config.Config, error) {
		t.Fatal("local deployment loaded Cloud credentials")
		return nil, nil
	}, func(context.Context, *config.AuthConfig, *deploymentEnrollment, []string) error {
		t.Fatal("local deployment contacted Cloud")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRegistrationDeduplicatesAppsAndPropagatesCloudFailure(t *testing.T) {
	device := registrationDeviceV2()
	denied := errors.New("viewer cannot register deployments")
	called := false
	err := registerDeviceApps(context.Background(), func(context.Context) (*deploymentEnrollment, error) {
		return device, nil
	}, []string{"app", "app", "campaign:people"}, func() (*config.Config, error) {
		return registrationConfigV2(), nil
	}, func(_ context.Context, auth *config.AuthConfig, got *deploymentEnrollment, apps []string) error {
		called = true
		if got != device || auth.OrganizationKey() != registrationTenant || !reflect.DeepEqual(apps, []string{"app", "campaign:people"}) {
			t.Fatalf("wrong registration: %v %v", got, apps)
		}
		return denied
	})
	if !called || !errors.Is(err, denied) {
		t.Fatalf("failure hidden: %v", err)
	}
}

func TestSkipCloudRegistrationDoesNotContactDevice(t *testing.T) {
	if err := registerCloudApps(context.Background(), nil, []string{"app"}, true); err != nil {
		t.Fatal(err)
	}
	if newRunCmd().Flags().Lookup("skip-cloud-registration") == nil {
		t.Fatal("offline option missing")
	}
}
