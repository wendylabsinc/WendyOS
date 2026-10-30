package commands

import (
	"context"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/sessionbroker"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

func TestDesktopMeshPreservesBrokerIdentity(t *testing.T) {
	now := time.Now()
	selector := "mesh://11111111-1111-4111-8111-111111111111/22222222-2222-4222-8222-222222222222"
	principal := "spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/device/22222222-2222-4222-8222-222222222222"
	snapshot := sessionbroker.OperatorDirectory{State: "ready", Captured: now.UnixMilli(), Devices: []sessionbroker.OperatorTargetInfo{{Selector: selector, Principal: principal, Device: "22222222-2222-4222-8222-222222222222", Name: "robot", Platform: "WendyOS", Address: "[fd00::1234]:50052", Expires: now.Add(time.Minute).UnixMilli()}}}
	devices := desktopMeshStatus(snapshot).Devices
	if len(devices) != 1 {
		t.Fatalf("devices: %+v", devices)
	}
	row := meshDeviceRow(devices[0])
	if row.Hostname != selector || row.PrincipalURI != principal || !row.Mesh || row.Port != 50052 {
		t.Fatalf("lost broker target identity: %+v", row)
	}
	snapshot.Captured = now.Add(-time.Minute).UnixMilli()
	if len(desktopMeshStatus(snapshot).Devices) != 0 {
		t.Fatal("stale broker snapshot became a desktop target")
	}
}

func TestDesktopMeshIncludesNativeLiteAndRejectsStaleOrUntrustedRows(t *testing.T) {
	now := time.Now()
	tenant := "11111111-1111-4111-8111-111111111111"
	device := "22222222-2222-4222-8222-222222222222"
	selector := "mesh://" + tenant + "/" + device
	row := sessionbroker.OperatorTargetInfo{Selector: selector, Principal: "spiffe://wendy.sh/tenant/" + tenant + "/device/" + device, Tenant: tenant, Device: device, Name: "C6", Platform: "Lite", Transport: "mtls-v1", Address: "[fd00::1234]:43024", Expires: now.Add(time.Minute).UnixMilli()}
	snapshot := sessionbroker.OperatorDirectory{State: "ready", Captured: now.UnixMilli(), Devices: []sessionbroker.OperatorTargetInfo{row}}
	got := DesktopMeshDevices(snapshot)
	if len(got) != 1 || got[0].Selector != selector || !got[0].Lite || got[0].Transport != "Mesh" {
		t.Fatalf("missing native Lite: %+v", got)
	}
	snapshot.Captured = now.Add(-time.Minute).UnixMilli()
	if len(DesktopMeshDevices(snapshot)) != 0 {
		t.Fatal("published stale target")
	}
	snapshot.Captured = now.UnixMilli()
	snapshot.Devices[0].Tenant = "33333333-3333-4333-8333-333333333333"
	if len(DesktopMeshDevices(snapshot)) != 0 {
		t.Fatal("accepted different-tenant Lite metadata")
	}
}

func TestMobileAccountSelectionIsExactAndPreservesOtherProfiles(t *testing.T) {
	t.Setenv("WENDY_CONFIG_DIR", t.TempDir())
	tenant := "11111111-1111-4111-8111-111111111111"
	cfg := &config.Config{CurrentContext: "first", Auth: []config.AuthConfig{{Name: "first"}, {Name: "second", Certificates: []config.CertificateInfo{{PrincipalURI: "spiffe://wendy.sh/tenant/" + tenant + "/operator/two"}}}}}
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	if err := MobileUseAccount("second", tenant); err != nil {
		t.Fatal(err)
	}
	current, err := config.Load()
	if err != nil || current.CurrentContext != "second" || len(current.Auth) != 2 {
		t.Fatalf("selection: %+v %v", current, err)
	}
	if err := MobileUseAccount("first", tenant); err == nil {
		t.Fatal("silently selected an account outside this tenant")
	}
	current, _ = config.Load()
	if current.CurrentContext != "second" {
		t.Fatal("failed selection changed context")
	}
}

func TestMobileMeshRenewalRunsAlongsideDiscoveryAndCancels(t *testing.T) {
	setTempConfig(t, &config.Config{CurrentContext: "mobile", Auth: []config.AuthConfig{{
		Name: "mobile", Certificates: []config.CertificateInfo{meshTestCert("11111111-1111-4111-8111-111111111111", "operator")},
	}}})
	old := ensureFreshCertificateFn
	t.Cleanup(func() { ensureFreshCertificateFn = old })
	started := make(chan struct{})
	ensureFreshCertificateFn = func(ctx context.Context, _ *config.AuthConfig) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := mobileMeshDiscoveryStream(ctx)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("renewal did not start asynchronously")
	}
	cancel()
	select {
	case _, ok := <-events:
		if ok {
			t.Fatal("canceled renewal published a device")
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not drain mobile discovery")
	}
}
