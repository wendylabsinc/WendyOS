package services

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap/zaptest"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
)

func TestCloudDeviceNamePersistsExactRegistrationAndClearsOnRemoval(t *testing.T) {
	dir := t.TempDir()
	self := mustDevice(t, rosterTenant, rosterDevice)
	r := provisionedRoster(t)
	r.UseLocalNameCache(dir)
	if r.localName != (registeredDeviceName{}) {
		t.Fatalf("fresh device name = %+v", r.localName)
	}
	apply := func(name string) {
		r.applyResponse(r.generation, &cloudpbv2.GetMeshRosterResponse{
			Entries: []*cloudpbv2.MeshRosterEntry{
				{Name: "unrelated", AssetId: rosterDeviceB},
				{Name: name, AssetId: rosterDevice},
			},
		})
	}
	apply("camera--a")
	apply("camera--b")
	if got := r.LocalCloudName(self); got != "camera--b" {
		t.Fatalf("local Cloud name = %q", got)
	}
	restarted := provisionedRoster(t)
	restarted.UseLocalNameCache(dir)
	if got := restarted.LocalCloudName(self); got != "camera--b" {
		t.Fatalf("offline Cloud name = %q", got)
	}

	r.applyResponse(r.generation, &cloudpbv2.GetMeshRosterResponse{})
	restarted.UseLocalNameCache(dir)
	if restarted.localName != (registeredDeviceName{Principal: self.Principal()}) {
		t.Fatalf("withdrawn device name = %+v", restarted.localName)
	}
}

func TestCloudDeviceNameDoesNotCrossEnrollments(t *testing.T) {
	dir := t.TempDir()
	r := provisionedRoster(t)
	r.UseLocalNameCache(dir)
	generation := r.generation
	response := &cloudpbv2.GetMeshRosterResponse{
		Entries: []*cloudpbv2.MeshRosterEntry{{Name: "former-name", AssetId: rosterDevice}},
	}
	r.applyResponse(generation, response)

	cert, key := deviceIdentityCertPEM(t, rosterTenant, rosterDeviceB)
	r.UpdateIdentity("cloud.example:443", cert, key, "")
	r.applyResponse(generation, response)
	if r.localName != (registeredDeviceName{}) {
		t.Fatalf("re-enrolled device inherited name %+v", r.localName)
	}
	r.UpdateIdentity("", "", "", "")
	if _, err := os.Stat(filepath.Join(dir, "cloud-device-name.json")); !os.IsNotExist(err) {
		t.Fatalf("unenrollment did not remove cached name: %v", err)
	}
}

func TestCloudDeviceNameUpdatesOnlyTheEnrolledAdvertisement(t *testing.T) {
	principal := certs.DeviceSPIFFEURI(rosterTenant, rosterDevice)
	path := filepath.Join(t.TempDir(), "wendyos-mdns.service")
	original := "<service>\n<type>_wendyos._udp</type>\n" +
		"<txt-record>principal=" + principal + "</txt-record>\n" +
		"<txt-record>name=local-name</txt-record>\n<txt-record>displayname=Local Name</txt-record>\n  </service>\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	restarts := 0
	restart := func() bool { restarts++; return restarts > 1 }
	logger := zaptest.NewLogger(t)
	other := certs.DeviceSPIFFEURI(rosterTenant, rosterDeviceB)
	if !publishCloudName(path, other, "wrong-name", false, restart, logger) || restarts != 0 {
		t.Fatal("another principal touched the advertisement")
	}
	if publishCloudName(path, principal, "cloud--name", false, restart, logger) {
		t.Fatal("failed Avahi restart was reported as applied")
	}
	if !publishCloudName(path, principal, "cloud--name", true, restart, logger) || restarts != 2 {
		t.Fatal("the unchanged file did not retry the failed restart")
	}
	if !publishCloudName(path, principal, "cloud--name", false, restart, logger) || restarts != 2 {
		t.Fatal("an already-applied name restarted Avahi")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "<txt-record>cloud-name=cloud--name</txt-record>") ||
		!strings.Contains(string(got), "<txt-record>name=local-name</txt-record>") {
		t.Fatalf("advertisement did not preserve both names: %s", got)
	}
	if !publishCloudName(path, principal, "3d-printer", false, restart, logger) {
		t.Fatal("could not withdraw an invalid Cloud name")
	}
	got, err = os.ReadFile(path)
	if err != nil || strings.Contains(string(got), "<txt-record>cloud-name=") ||
		!strings.Contains(string(got), "<txt-record>name=local-name</txt-record>") {
		t.Fatalf("withdrawn advertisement = %s (%v)", got, err)
	}
}
