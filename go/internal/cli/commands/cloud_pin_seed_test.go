package commands

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/flock"
	"github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
)

func TestUUIDCloudDiscoveryReplacesLegacyHostnamePin(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete roster", true: "failed roster"}[fail], func(t *testing.T) {
			auth := discoveryV2Auth(t, 1, fail)
			cfg := &config.Config{Auth: []config.AuthConfig{*auth}}
			cfg.SetDevicePinFrom("online-device", 77, "old-cloud.test:443", "625", "", config.PinSourceCloud)
			before, _ := cfg.DevicePinFor("online-device")
			if err := config.Save(cfg); err != nil {
				t.Fatal(err)
			}
			_, fetchErr := fetchCloudDiscoveryDevices(context.Background(), auth, true)
			cfg, err := config.Load()
			if err != nil {
				t.Fatal(err)
			}
			pin, _ := cfg.DevicePinFor("online-device.local")
			if fail {
				if fetchErr == nil || pin != before {
					t.Fatalf("failed roster changed trust: err=%v pin=%+v", fetchErr, pin)
				}
				return
			}
			if fetchErr != nil {
				t.Fatal(fetchErr)
			}
			id := "00000000-0000-4000-8000-000000000001"
			principal := certs.DeviceSPIFFEURI(testOperatorTenant, id)
			if pin.Principal != principal || pin.AssetID != id || pin.OrgID != 0 || pin.CloudGRPC != auth.CloudGRPC || pin.Source != config.PinSourceCloud {
				t.Fatalf("legacy hostname pin not refreshed: %+v", pin)
			}
			obs := observedDeviceIdentity{mTLS: true, assetID: id, principal: principal}
			if _, err := applyDeviceIdentity(cfg, "online-device.local", "", obs); err != nil {
				t.Fatalf("refreshed hostname rejected: %v", err)
			}
			obs.principal = certs.DeviceSPIFFEURI(pinUUIDOtherTenant, id)
			if _, err := applyDeviceIdentity(cfg, "online-device.local", "", obs); err == nil {
				t.Fatal("accepted a different tenant after refreshing")
			}
		})
	}
}

func TestUUIDCloudPinsIgnoreUnboundAssets(t *testing.T) {
	t.Setenv("WENDY_CONFIG_DIR", t.TempDir())
	auth := &config.AuthConfig{CloudGRPC: "cloud.test:443", Certificates: []config.CertificateInfo{{PrincipalURI: "spiffe://wendy.sh/tenant/" + pinUUIDTenant + "/operator/test"}}}
	assets := []*cloudpbv2.Asset{
		nil,
		{Id: pinUUIDDevice, Name: "missing-tenant"},
		{Id: pinUUIDDevice, Name: "other-tenant", OrganizationId: pinUUIDOtherTenant},
		{Id: pinUUIDDevice, OrganizationId: pinUUIDTenant},
		{Name: "missing-id", OrganizationId: pinUUIDTenant},
	}
	if err := seedPinsFromCloudAssetsV2(auth, assets); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.DevicePins) != 0 {
		t.Fatalf("unbound assets created pins: %+v", cfg.DevicePins)
	}
}

func TestUUIDCloudPinsUsePKIBinding(t *testing.T) {
	t.Setenv("WENDY_CONFIG_DIR", t.TempDir())
	auth := &config.AuthConfig{CloudGRPC: "cloud.test:443", Certificates: []config.CertificateInfo{{PrincipalURI: "spiffe://wendy.sh/tenant/" + pinUUIDTenant + "/operator/test"}}}
	deviceName := pinUUIDDevice
	asset := &cloudpbv2.Asset{Id: "different-cloud-row-id", Name: "bound-device", OrganizationId: pinUUIDTenant, PkiDeviceName: &deviceName}
	if err := seedPinsFromCloudAssetsV2(auth, []*cloudpbv2.Asset{asset}); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	pin, ok := cfg.DevicePinFor(asset.Name)
	if !ok || pin.AssetID != deviceName || pin.Principal != certs.DeviceSPIFFEURI(pinUUIDTenant, deviceName) {
		t.Fatalf("pin used the Cloud row ID instead of its PKI binding: %+v", pin)
	}
}

// A pinned name that Cloud now gives to another device must stay refused.
// Every Cloud lookup seeds, so a seed that replaced the pinned principal would
// let the retry of a refused connection reach the name's new holder.
func TestUUIDCloudLookupKeepsPinnedNameRefused(t *testing.T) {
	auth := discoveryV2Auth(t, 1, false)
	removed := "00000000-0000-4000-8000-000000000099"
	cfg := &config.Config{Auth: []config.AuthConfig{*auth}}
	cfg.SetDevicePin("online-device", 0, auth.CloudGRPC, removed, certs.DeviceSPIFFEURI(testOperatorTenant, removed))
	before, _ := cfg.DevicePinFor("online-device")
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt <= 2; attempt++ {
		cfg, err := config.Load()
		if err != nil {
			t.Fatal(err)
		}
		pin, _ := cfg.DevicePinFor("online-device")
		if pin != before {
			t.Fatalf("attempt %d: a Cloud lookup replaced the pinned identity: %+v", attempt, pin)
		}
		selector := cloudDefaultSelector("online-device", expectedIdentityForPin(pin))
		if _, err := pickCloudDiscoveryDevice(context.Background(), auth, selector, ""); err == nil {
			t.Fatalf("attempt %d reached the name's new holder", attempt)
		}
	}
}

func TestUUIDCloudPinsKeepOtherPrincipals(t *testing.T) {
	t.Setenv("WENDY_CONFIG_DIR", t.TempDir())
	auth := &config.AuthConfig{CloudGRPC: "cloud.test:443", Certificates: []config.CertificateInfo{{PrincipalURI: "spiffe://wendy.sh/tenant/" + pinUUIDTenant + "/operator/test"}}}
	id := func(n int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012d", n) }
	cfg := &config.Config{}
	cfg.SetDevicePin("renamed", 0, auth.CloudGRPC, pinUUIDDevice, certs.DeviceSPIFFEURI(pinUUIDTenant, pinUUIDDevice))
	cfg.SetDevicePin("gateway", 0, "other-cloud.test:443", id(2), certs.DeviceSPIFFEURI(pinUUIDOtherTenant, id(2)))
	cfg.SetDevicePin("same-device", 0, "old-cloud.test:443", id(3), certs.DeviceSPIFFEURI(pinUUIDTenant, id(3)))
	renamed, _ := cfg.DevicePinFor("renamed")
	gateway, _ := cfg.DevicePinFor("gateway")
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	assets := []*cloudpbv2.Asset{
		{Id: id(1), Name: "renamed", OrganizationId: pinUUIDTenant},
		{Id: id(2), Name: "gateway", OrganizationId: pinUUIDTenant},
		{Id: id(3), Name: "same-device", OrganizationId: pinUUIDTenant},
	}
	if err := seedPinsFromCloudAssetsV2(auth, assets); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if pin, _ := cfg.DevicePinFor("renamed"); pin != renamed {
		t.Fatalf("roster moved a pinned name to another device: %+v", pin)
	}
	if pin, _ := cfg.DevicePinFor("gateway"); pin != gateway {
		t.Fatalf("roster replaced another tenant's pin: %+v", pin)
	}
	want := config.DevicePin{CloudGRPC: auth.CloudGRPC, AssetID: id(3), Principal: certs.DeviceSPIFFEURI(pinUUIDTenant, id(3)), Source: config.PinSourceCloud}
	if pin, _ := cfg.DevicePinFor("same-device"); pin != want {
		t.Fatalf("pin for the same principal not refreshed: %+v", pin)
	}
}

// resolveCloudDiscoveryDevice refuses a name two assets share. Pinning either
// asset would refuse the other one on the LAN.
func TestUUIDCloudPinsSkipSharedNames(t *testing.T) {
	t.Setenv("WENDY_CONFIG_DIR", t.TempDir())
	auth := &config.AuthConfig{CloudGRPC: "cloud.test:443", Certificates: []config.CertificateInfo{{PrincipalURI: "spiffe://wendy.sh/tenant/" + pinUUIDTenant + "/operator/test"}}}
	id := func(n int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012d", n) }
	cfg := &config.Config{}
	cfg.SetDevicePinFrom("gateway", 77, "old-cloud.test:443", "625", "", config.PinSourceCloud)
	before, _ := cfg.DevicePinFor("gateway")
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	assets := []*cloudpbv2.Asset{
		{Id: id(1), Name: "Robot", OrganizationId: pinUUIDTenant},
		{Id: id(2), Name: "robot.local", OrganizationId: pinUUIDTenant},
		{Id: id(3), Name: "gateway", OrganizationId: pinUUIDTenant},
		{Id: id(4), Name: "gateway", OrganizationId: pinUUIDTenant},
		{Id: id(5), Name: "solo", OrganizationId: pinUUIDTenant},
	}
	if err := seedPinsFromCloudAssetsV2(auth, assets); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if pin, ok := cfg.DevicePinFor("robot"); ok {
		t.Fatalf("pinned a name two assets share: %+v", pin)
	}
	if pin, _ := cfg.DevicePinFor("gateway"); pin != before {
		t.Fatalf("shared name replaced its existing pin: %+v", pin)
	}
	if pin, ok := cfg.DevicePinFor("solo"); !ok || pin.AssetID != id(5) {
		t.Fatalf("unique name not pinned: %+v", pin)
	}
}

// Discovery refreshes every 10s in the TUI and runs on every Cloud connect. A
// roster whose pins already match must not wait on another process's lock.
func TestUUIDCloudPinsSkipLockWhenUnchanged(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WENDY_CONFIG_DIR", dir)
	auth := &config.AuthConfig{CloudGRPC: "cloud.test:443", Certificates: []config.CertificateInfo{{PrincipalURI: "spiffe://wendy.sh/tenant/" + pinUUIDTenant + "/operator/test"}}}
	assets := []*cloudpbv2.Asset{{Id: pinUUIDDevice, Name: "bound-device", OrganizationId: pinUUIDTenant}}
	if err := seedPinsFromCloudAssetsV2(auth, assets); err != nil {
		t.Fatal(err)
	}
	release, err := flock.Acquire(filepath.Join(dir, "config.lock"), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := seedPinsFromCloudAssetsV2(auth, assets); err != nil {
		t.Fatalf("unchanged roster waited on the config lock: %v", err)
	}
}

// stubLoadConfigForPin points the loadConfigForPinFn seam at cfg for the
// duration of a test.
func stubLoadConfigForPin(t *testing.T, cfg *config.Config) {
	t.Helper()
	orig := loadConfigForPinFn
	loadConfigForPinFn = func() (*config.Config, error) { return cfg, nil }
	t.Cleanup(func() { loadConfigForPinFn = orig })
}

// TestSeedPinsFromCloudAssets covers seedPinsFromCloudAssets in isolation via
// the loadConfigForPinFn seam, so no test here touches the developer's real
// config file.
func TestSeedPinsFromCloudAssets(t *testing.T) {
	t.Run("writes a cloud-sourced pin per asset", func(t *testing.T) {
		cfg := &config.Config{}
		stubLoadConfigForPin(t, cfg)
		// config.Save is not seamed, so it still hits the real filesystem path
		// derived from HOME; point that at a scratch dir for this test.
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("USERPROFILE", home)

		assets := []*cloudpb.Asset{
			{Id: 42, Name: "calm-zinnia"},
		}
		if err := seedPinsFromCloudAssets(assets, 7, "grpc.a.sh:443"); err != nil {
			t.Fatalf("seedPinsFromCloudAssets: unexpected error: %v", err)
		}

		pin, ok := cfg.DevicePinFor("calm-zinnia")
		if !ok {
			t.Fatal("expected a pin for \"calm-zinnia\", found none")
		}
		want := config.DevicePin{OrgID: 7, CloudGRPC: "grpc.a.sh:443", AssetID: "42", Source: config.PinSourceCloud}
		if pin != want {
			t.Fatalf("pin = %+v, want %+v", pin, want)
		}

		// Must actually have persisted (config.Save called), not just mutated
		// the in-memory cfg the seam handed back.
		onDisk, err := config.Load()
		if err != nil {
			t.Fatalf("config.Load: %v", err)
		}
		if p, ok := onDisk.DevicePinFor("calm-zinnia"); !ok || p != want {
			t.Fatalf("on-disk pin = %+v (ok=%v), want %+v", p, ok, want)
		}
	})

	t.Run("overwrites a conflicting lan pin", func(t *testing.T) {
		cfg := &config.Config{
			DevicePins: map[string]config.DevicePin{
				"calm-zinnia": {OrgID: 7, CloudGRPC: "grpc.a.sh:443", AssetID: "99", Source: config.PinSourceLAN},
			},
		}
		stubLoadConfigForPin(t, cfg)
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("USERPROFILE", home)

		assets := []*cloudpb.Asset{
			{Id: 42, Name: "calm-zinnia"},
		}
		if err := seedPinsFromCloudAssets(assets, 7, "grpc.a.sh:443"); err != nil {
			t.Fatalf("seedPinsFromCloudAssets: unexpected error: %v", err)
		}

		pin, ok := cfg.DevicePinFor("calm-zinnia")
		if !ok {
			t.Fatal("expected a pin for \"calm-zinnia\", found none")
		}
		if pin.AssetID != "42" {
			t.Fatalf("AssetID = %q, want %q — the cloud roster must overwrite the conflicting LAN sighting (asset 99)", pin.AssetID, "42")
		}
		if pin.Source != config.PinSourceCloud {
			t.Fatalf("Source = %q, want %q — a cloud-fetched roster is authority, not a sighting, and must win outright", pin.Source, config.PinSourceCloud)
		}
	})

	t.Run("skips assets with no name", func(t *testing.T) {
		cfg := &config.Config{}
		stubLoadConfigForPin(t, cfg)

		// Point HOME at a path that cannot hold a ~/.wendy directory (a regular
		// file, not a directory), so that if seedPinsFromCloudAssets called
		// config.Save it would fail loudly. Every asset below is unnamed, so
		// "changed" must stay false and Save must never be attempted — proven
		// by this call succeeding despite the broken HOME.
		unwritableHome := t.TempDir() + "-not-a-directory"
		if err := os.WriteFile(unwritableHome, []byte("not a directory"), 0o600); err != nil {
			t.Fatalf("writing sentinel file: %v", err)
		}
		t.Setenv("HOME", unwritableHome)
		t.Setenv("USERPROFILE", unwritableHome)

		assets := []*cloudpb.Asset{
			{Id: 1, Name: ""},
			{Id: 2, Name: ""},
		}
		if err := seedPinsFromCloudAssets(assets, 7, "grpc.a.sh:443"); err != nil {
			t.Fatalf("seedPinsFromCloudAssets: unexpected error: %v (config.Save should never have been attempted)", err)
		}

		if len(cfg.DevicePins) != 0 {
			t.Fatalf("DevicePins = %+v, want empty — an unnamed asset must not be pinned under \"\"", cfg.DevicePins)
		}
		if _, ok := cfg.DevicePinFor(""); ok {
			t.Fatal("an unnamed asset was pinned under the empty-string key")
		}
	})

	// An asset with no usable id would be pinned as AssetID "0" — a non-empty
	// asset constraint that no real certificate can ever present. Because the
	// pin is cloud-sourced, EvaluateDevicePin's pin.AssetID != assetID branch
	// would then hard-fail every future dial to that name, with no adoption
	// path out. Skipping mirrors the empty-name guard: no key, no pin.
	t.Run("skips assets with no id", func(t *testing.T) {
		cfg := &config.Config{}
		stubLoadConfigForPin(t, cfg)

		// Same sentinel as the empty-name subtest: a HOME that cannot hold
		// ~/.wendy, so an attempted config.Save would fail loudly. Every asset
		// below is skipped, so "changed" must stay false and Save never run.
		unwritableHome := t.TempDir() + "-not-a-directory"
		if err := os.WriteFile(unwritableHome, []byte("not a directory"), 0o600); err != nil {
			t.Fatalf("writing sentinel file: %v", err)
		}
		t.Setenv("HOME", unwritableHome)
		t.Setenv("USERPROFILE", unwritableHome)

		assets := []*cloudpb.Asset{
			{Id: 0, Name: "calm-zinnia"},
			{Id: -1, Name: "bold-fern"},
		}
		if err := seedPinsFromCloudAssets(assets, 7, "grpc.a.sh:443"); err != nil {
			t.Fatalf("seedPinsFromCloudAssets: unexpected error: %v (config.Save should never have been attempted)", err)
		}

		if len(cfg.DevicePins) != 0 {
			t.Fatalf("DevicePins = %+v, want empty — an asset with no id must not be pinned to the unmatchable asset \"0\"", cfg.DevicePins)
		}
		if pin, ok := cfg.DevicePinFor("calm-zinnia"); ok {
			t.Fatalf("pinned %+v for an id-less asset; a cloud-sourced AssetID %q hard-fails every future dial to this name", pin, pin.AssetID)
		}
	})

	// The steady state. `wendy cloud discover`'s TUI re-fetches the roster every
	// 10 seconds, and a roster that has already been seeded describes pins that
	// are byte-for-byte what is on disk. Marking "changed" for every valid asset
	// made the flag mean "the roster was non-empty", so that steady state wrote
	// config.json every 10 seconds to store exactly what it already held.
	t.Run("does not rewrite config when every pin already matches", func(t *testing.T) {
		cfg := &config.Config{DevicePins: map[string]config.DevicePin{
			"calm-zinnia": {OrgID: 7, CloudGRPC: "grpc.a.sh:443", AssetID: "42", Source: config.PinSourceCloud},
			"bold-fern":   {OrgID: 7, CloudGRPC: "grpc.a.sh:443", AssetID: "43", Source: config.PinSourceCloud},
		}}
		stubLoadConfigForPin(t, cfg)

		// Same sentinel as the subtests above: a HOME that cannot hold ~/.wendy,
		// so an attempted config.Save fails loudly. Every asset below is already
		// pinned exactly as the roster describes, so Save must never run.
		unwritableHome := t.TempDir() + "-not-a-directory"
		if err := os.WriteFile(unwritableHome, []byte("not a directory"), 0o600); err != nil {
			t.Fatalf("writing sentinel file: %v", err)
		}
		t.Setenv("HOME", unwritableHome)
		t.Setenv("USERPROFILE", unwritableHome)

		assets := []*cloudpb.Asset{
			{Id: 42, Name: "calm-zinnia"},
			{Id: 43, Name: "bold-fern"},
		}
		if err := seedPinsFromCloudAssets(assets, 7, "grpc.a.sh:443"); err != nil {
			t.Fatalf("seedPinsFromCloudAssets: unexpected error: %v (config.Save should never have been attempted for an unchanged roster)", err)
		}
	})

	// The other half: suppression must be scoped to pins that genuinely match.
	// One drifted asset in an otherwise-unchanged roster still has to be written.
	t.Run("writes when a single asset in an otherwise unchanged roster drifts", func(t *testing.T) {
		cfg := &config.Config{DevicePins: map[string]config.DevicePin{
			"calm-zinnia": {OrgID: 7, CloudGRPC: "grpc.a.sh:443", AssetID: "42", Source: config.PinSourceCloud},
			"bold-fern":   {OrgID: 7, CloudGRPC: "grpc.a.sh:443", AssetID: "99", Source: config.PinSourceCloud},
		}}
		stubLoadConfigForPin(t, cfg)
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("USERPROFILE", home)

		assets := []*cloudpb.Asset{
			{Id: 42, Name: "calm-zinnia"},
			{Id: 43, Name: "bold-fern"},
		}
		if err := seedPinsFromCloudAssets(assets, 7, "grpc.a.sh:443"); err != nil {
			t.Fatalf("seedPinsFromCloudAssets: unexpected error: %v", err)
		}

		onDisk, err := config.Load()
		if err != nil {
			t.Fatalf("config.Load: %v", err)
		}
		pin, ok := onDisk.DevicePinFor("bold-fern")
		if !ok || pin.AssetID != "43" {
			t.Fatalf("on-disk pin for bold-fern = %+v (ok=%v), want asset 43 persisted — a re-pointed asset must still reach disk", pin, ok)
		}
	})

	// A roster is not all-or-nothing: one unusable asset must not cost the
	// others their pins.
	t.Run("seeds the usable assets in a mixed roster", func(t *testing.T) {
		cfg := &config.Config{}
		stubLoadConfigForPin(t, cfg)
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("USERPROFILE", home)

		assets := []*cloudpb.Asset{
			{Id: 0, Name: "calm-zinnia"},
			{Id: 7, Name: ""},
			{Id: 42, Name: "bold-fern"},
		}
		if err := seedPinsFromCloudAssets(assets, 7, "grpc.a.sh:443"); err != nil {
			t.Fatalf("seedPinsFromCloudAssets: unexpected error: %v", err)
		}

		if len(cfg.DevicePins) != 1 {
			t.Fatalf("DevicePins = %+v, want exactly the one usable asset", cfg.DevicePins)
		}
		pin, ok := cfg.DevicePinFor("bold-fern")
		if !ok {
			t.Fatal("the usable asset was not pinned; one bad roster entry must not suppress the rest")
		}
		if pin.AssetID != "42" || pin.Source != config.PinSourceCloud {
			t.Fatalf("pin = %+v, want AssetID 42 from a cloud source", pin)
		}
	})
}
