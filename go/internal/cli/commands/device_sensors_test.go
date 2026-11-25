package commands

import (
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	"testing"
)

func TestSelectSensorPrincipalRefusesAmbiguousOrUnenrolledSources(t *testing.T) {
	const tenant = "5c9e14bb-49c0-4178-9cc5-493a151f9885"
	principal := certs.DeviceSPIFFEURI(tenant, "08c747f1-1993-45aa-8f7d-886fdf0fb73e")
	source := models.DiscoveredDevice{TenantUUID: tenant, PrincipalURI: principal, IsMTLS: true, Sensorlink: true, WendyLite: true}
	// Use the shared principal parser rather than assume an accepted URI scheme.
	if sensorSourcePrincipal(source) == "" {
		t.Fatal("invalid principal fixture")
	}
	if _, err := selectSensorPrincipal([]models.DiscoveredDevice{source}, principal); err != nil {
		t.Fatal(err)
	}
	if _, err := selectSensorPrincipal([]models.DiscoveredDevice{source, source}, principal); err == nil {
		t.Fatal("accepted ambiguous source")
	}
	source.IsMTLS = false
	if _, err := selectSensorPrincipal([]models.DiscoveredDevice{source}, principal); err == nil {
		t.Fatal("accepted unauthenticated source")
	}
	if _, err := selectSensorPrincipal(nil, ""); err == nil {
		t.Fatal("accepted empty principal")
	}
}
