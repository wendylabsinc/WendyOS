package commands

import (
	"fmt"
	"os"
	"strconv"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
)

// seedPinsFromCloudAssets records the org's asset roster as cloud-sourced pins.
// The cloud spoke over an authenticated session, so this is authority, not a
// sighting: it overwrites whatever a LAN observation recorded, and closes the
// trust-on-first-use window for every device the cloud knows about before the
// CLI ever meets it on the network.
//
// An asset with no name has no key to pin under and is skipped rather than
// filed under "". An asset with no usable id is skipped for the mirror-image
// reason: it would be pinned as AssetID "0", a non-empty asset constraint no
// certificate can ever present, and because the pin is cloud-sourced
// EvaluateDevicePin would then take its pin.AssetID != assetID branch and
// hard-fail every future dial to that name, with no adoption path out. A
// missing id is missing information, and the honest encoding of that is no pin
// at all — which leaves trust-on-first-use, not a permanent refusal.
//
// Skipping is per-asset: one unusable entry never costs the rest of the roster
// its pins. config.Save is only called when at least one pin actually changed,
// so a roster of entirely unusable assets (or an empty roster) never touches
// disk.
func seedPinsFromCloudAssets(assets []*cloudpb.Asset, orgID int, cloudGRPC string) error {
	cfg, err := loadConfigForPinFn()
	if err != nil {
		return err
	}
	changed := false
	for _, a := range assets {
		name := a.GetName()
		if name == "" || a.GetId() <= 0 {
			continue
		}
		want := config.DevicePin{
			OrgID:     orgID,
			CloudGRPC: cloudGRPC,
			AssetID:   strconv.Itoa(int(a.GetId())),
			Source:    config.PinSourceCloud,
		}
		// Only a pin whose stored value would actually differ counts as a
		// change. Setting the flag for every valid asset made "changed" mean "the
		// roster was non-empty", so the steady state — a roster already fully
		// seeded — rewrote config.json on every pass, which for the TUI's 10s
		// refresh is a disk write every 10 seconds that alters nothing.
		if existing, ok := cfg.DevicePinFor(name); ok && existing == want {
			continue
		}
		// The roster names an asset, not a principal; the principal is
		// backfilled by enforceDeviceIdentity on the first real connect.
		cfg.SetDevicePinFrom(name, orgID, cloudGRPC, want.AssetID, "", config.PinSourceCloud)
		changed = true
	}
	if !changed {
		return nil
	}
	return config.Save(cfg)
}

// seedPinsFromAssetsBestEffort seeds pins from a verified cloud asset roster,
// deriving the org and cloud host from the auth session that fetched it.
//
// Best-effort by design: this runs after a caller's own fetchCloudAssetsFiltered
// call has already succeeded, so a seeding failure (e.g. an unwritable config
// file) must never fail the command the user actually ran — 'wendy cloud
// discover' must still list devices even if pins cannot be written. Errors are
// therefore swallowed and only surfaced via WENDY_TLS_DEBUG.
func seedPinsFromAssetsBestEffort(auth *config.AuthConfig, assets []*cloudpb.Asset) {
	if auth == nil || len(auth.Certificates) == 0 {
		return
	}
	orgID := auth.Certificates[0].OrganizationID
	if err := seedPinsFromCloudAssets(assets, orgID, auth.CloudGRPC); err != nil {
		debugPinSeed("seeding cloud pins for org %d failed: %v", orgID, err)
	}
}

// debugPinSeed logs only when WENDY_TLS_DEBUG is set, matching the CLI's other
// mTLS/pin diagnostics (see debugClock in device_clock.go).
func debugPinSeed(format string, args ...any) {
	if os.Getenv("WENDY_TLS_DEBUG") != "" {
		fmt.Fprintf(os.Stderr, "[pin-seed] "+format+"\n", args...)
	}
}
