package commands

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/devicepin"
	"github.com/wendylabsinc/wendy/go/internal/shared/discoverycache"
)

// seedSPKIPin writes a known_devices.json entry for key directly into the
// wendy config directory (set up by writePinTestConfig beforehand), without
// exercising CheckAndUpdate — that requires a live certificate. It mirrors
// devicepin.Store's on-disk format exactly, so a discrepancy here would be a
// bug in the test fixture, not in the store.
func seedSPKIPin(t *testing.T, key string) {
	t.Helper()
	seedSPKIPins(t, key)
}

// seedSPKIPins is seedSPKIPin for more than one device, which is what the
// blast-radius tests need: proving an unpin left another device's entry alone
// requires another device's entry to exist.
func seedSPKIPins(t *testing.T, keys ...string) {
	t.Helper()
	dir, err := config.ConfigDir()
	if err != nil {
		t.Fatalf("config dir: %v", err)
	}
	devices := map[string]devicepin.PinnedDevice{}
	for _, key := range keys {
		devices[key] = devicepin.PinnedDevice{SPKIFingerprint: "sha256:deadbeef", DisplayName: "thor", LastSeen: "2026-01-01T00:00:00Z"}
	}
	data, err := json.Marshal(devices)
	if err != nil {
		t.Fatalf("marshal seed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "known_devices.json"), data, 0o600); err != nil {
		t.Fatalf("write seed: %v", err)
	}
}

// readSPKIPins loads known_devices.json back from the config dir for
// assertions. A missing file (never written, or the store's own flush
// deleted it — it never does, but nothing guarantees that) reads as no pins.
func readSPKIPins(t *testing.T) map[string]devicepin.PinnedDevice {
	t.Helper()
	dir, err := config.ConfigDir()
	if err != nil {
		t.Fatalf("config dir: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "known_devices.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read known_devices.json: %v", err)
	}
	var devices map[string]devicepin.PinnedDevice
	if err := json.Unmarshal(data, &devices); err != nil {
		t.Fatalf("unmarshal known_devices.json: %v", err)
	}
	return devices
}

// TestDeviceUnpinClearsBothStores is the test the whole command exists for.
// Every identity refusal in enforceDeviceIdentity and
// challengeUnprovisionedDevice tells the user to run `wendy device unpin
// <host>`; if that only cleared one of the two independent pin stores, the
// same refusal would fire again on the very next connection and the command
// would look broken.
func TestDeviceUnpinClearsBothStores(t *testing.T) {
	readPins := writePinTestConfig(t, map[string]config.DevicePin{
		"thor": {OrgID: 7, CloudGRPC: "grpc.a.sh:443", AssetID: "42"},
	})
	seedSPKIPin(t, "urn:wendy:org:7:asset:42")

	cmd := newDeviceUnpinCmd()
	if err := cmd.RunE(cmd, []string{"thor.local"}); err != nil {
		t.Fatalf("unpin: %v", err)
	}

	if pin, ok := readPins()["thor"]; ok {
		t.Errorf("config pin survived unpin: %+v", pin)
	}
	if _, ok := readSPKIPins(t)["urn:wendy:org:7:asset:42"]; ok {
		t.Error("SPKI pin survived unpin: the identity refusal would fire again on the next connect")
	}
}

// TestDeviceUnpinClearsAPinFoundUnderAnAlias is the dead end this closes.
//
// `wendy cloud discover` seeds pins under the name the cloud roster carries
// (the asset name, which is the discovery cache's display name), while dials
// name the device by its mDNS hostname. lookupPin already reconciles the two —
// so a dial to "wendyos-calm-zinnia" is governed, and refused, by the pin filed
// under "calm-zinnia" — but unpin cleared only the key it was handed. The
// refusal named a key nothing was filed under, unpinning it removed nothing,
// and the very next dial refused identically. Forever.
func TestDeviceUnpinClearsAPinFoundUnderAnAlias(t *testing.T) {
	readPins := writePinTestConfig(t, map[string]config.DevicePin{
		"calm-zinnia": {OrgID: 7, CloudGRPC: "grpc.a.sh:443", AssetID: "42", Source: config.PinSourceCloud},
	})
	setPinCache(t, discoverycache.Entry{
		ID:          "dev-1",
		DisplayName: "calm-zinnia",
		Hostname:    "wendyos-calm-zinnia.local",
	})
	seedSPKIPin(t, "urn:wendy:org:7:asset:42")

	cmd := newDeviceUnpinCmd()
	if err := cmd.RunE(cmd, []string{"wendyos-calm-zinnia.local"}); err != nil {
		t.Fatalf("unpin: %v", err)
	}

	if pin, ok := readPins()["calm-zinnia"]; ok {
		t.Errorf("the pin that governs this host survived unpinning it by the name the refusal named (%+v); the next dial refuses identically and there is no way out", pin)
	}
	if _, ok := readSPKIPins(t)["urn:wendy:org:7:asset:42"]; ok {
		t.Error("SPKI pin survived an alias-keyed unpin")
	}
}

// TestDeviceUnpinPrefersTheExactRefusalKey reproduces the Theta failure: a
// stale LAN pin is filed under the hostname named by post-connect enforcement,
// while a newer cloud pin exists under a discovery alias. The refusal tells the
// user to unpin the hostname, so that command must clear the exact stale pin,
// not follow cloud-source precedence to the already-correct alias.
func TestDeviceUnpinPrefersTheExactRefusalKey(t *testing.T) {
	readPins := writePinTestConfig(t, map[string]config.DevicePin{
		"wendyos-wendy-box-theta": {OrgID: 2, CloudGRPC: "cloud.example:443", AssetID: "226", Source: config.PinSourceLAN},
		"wendy-box-theta":         {OrgID: 2, CloudGRPC: "cloud.example:443", AssetID: "379", Source: config.PinSourceCloud},
	})
	setPinCache(t, discoverycache.Entry{
		ID:          "theta",
		Hostname:    "wendyos-wendy-box-theta.local",
		DisplayName: "wendy-box-theta",
		AssetID:     379,
		OrgID:       2,
	})
	seedSPKIPins(t, "urn:wendy:org:2:asset:226", "urn:wendy:org:2:asset:379")

	runUnpin(t, "wendyos-wendy-box-theta.local")

	pins := readPins()
	if pin, ok := pins["wendyos-wendy-box-theta"]; ok {
		t.Errorf("exact stale pin survived hostname unpin: %+v", pin)
	}
	if pin, ok := pins["wendy-box-theta"]; !ok || pin.AssetID != "379" {
		t.Errorf("current cloud alias was removed: pin=%+v ok=%v", pin, ok)
	}
	spki := readSPKIPins(t)
	if _, ok := spki["urn:wendy:org:2:asset:226"]; ok {
		t.Error("stale identity SPKI pin survived")
	}
	if _, ok := spki["urn:wendy:org:2:asset:379"]; !ok {
		t.Error("current identity SPKI pin was removed")
	}
}

// TestDeviceUnpinClearsSPKIPinWithNoConfigPin covers the store that gets
// written where no config pin ever does. getAgentVersionAtAddress runs the dial
// ladder with the SPKI store for every device the mDNS prober enumerates, so
// `wendy device list` alone SPKI-pins the whole LAN with no config pin behind
// any of it. When one of those certificates is reissued, the old unpin declined
// to touch the SPKI store at all (it required an asset-bearing config pin to
// derive the key from), and recovery meant hand-editing known_devices.json.
//
// The discovery cache is what closes the gap: it records the asset and org the
// device advertised, which is exactly the pair the store is keyed by.
func TestDeviceUnpinClearsSPKIPinWithNoConfigPin(t *testing.T) {
	writePinTestConfig(t, nil)
	setPinCache(t, discoverycache.Entry{
		ID:          "dev-1",
		DisplayName: "Thor",
		Hostname:    "wendyos-thor.local",
		AssetID:     42,
		OrgID:       7,
	})
	seedSPKIPin(t, "urn:wendy:org:7:asset:42")

	cmd := newDeviceUnpinCmd()
	if err := cmd.RunE(cmd, []string{"wendyos-thor.local"}); err != nil {
		t.Fatalf("unpin: %v", err)
	}

	if _, ok := readSPKIPins(t)["urn:wendy:org:7:asset:42"]; ok {
		t.Error("SPKI pin survived unpin for a host with no config pin: the refusal it causes has no escape hatch but a text editor")
	}
}

// TestClearDevicePinForRepinClearsSPKIStore holds set-default to the claim
// pki/README.md already makes for it — that naming a device has "the same
// clearing effect" as unpin. Leaving the SPKI half behind made that false for
// the one refusal that has no other way out: set-default would clear the config
// pin, reconnect, and be rejected again by the pin store it never touched.
func TestClearDevicePinForRepinClearsSPKIStore(t *testing.T) {
	readPins := writePinTestConfig(t, map[string]config.DevicePin{
		"wendy-thor": {OrgID: 7, CloudGRPC: "grpc.a.sh:443", AssetID: "42"},
	})
	setPinCache(t) // empty cache: exactly the single-key legacy shape
	seedSPKIPin(t, "urn:wendy:org:7:asset:42")

	clearDevicePinForRepin("wendy-thor.local")

	if pin, ok := readPins()["wendy-thor"]; ok {
		t.Errorf("config pin survived clearDevicePinForRepin: %+v", pin)
	}
	if _, ok := readSPKIPins(t)["urn:wendy:org:7:asset:42"]; ok {
		t.Error("SPKI pin survived clearDevicePinForRepin: set-default cannot re-pin a device whose key rotated, though the docs say it can")
	}
}

// TestDeviceUnpinUnknownHostIsNotAnError is the ambiguity ruling from the
// design: the command's contract is "this host ends up unpinned", which is
// already true for a host that was never pinned, so there is nothing to
// report as a failure.
func TestDeviceUnpinUnknownHostIsNotAnError(t *testing.T) {
	writePinTestConfig(t, nil)

	cmd := newDeviceUnpinCmd()
	if err := cmd.RunE(cmd, []string{"never-pinned.local"}); err != nil {
		t.Fatalf("unpinning an unpinned host: want nil, got %v", err)
	}
}

// TestDeviceUnpinAcceptsHostPortAddress covers the exact bug fixed in
// set-default during Task 6: a pin recorded for a `--device
// wendy-thor.local:50051` connection files under "wendy-thor.local" (the port
// stripped by pinKeyForAddr), so unpinning must strip the port the same way —
// passing the raw argument through would clear a key nothing was ever filed
// under and leave the real pin, and the refusal, in place.
func TestDeviceUnpinAcceptsHostPortAddress(t *testing.T) {
	readPins := writePinTestConfig(t, map[string]config.DevicePin{
		"wendy-thor": {OrgID: 7, CloudGRPC: "grpc.a.sh:443", AssetID: "42"},
	})
	seedSPKIPin(t, "urn:wendy:org:7:asset:42")

	cmd := newDeviceUnpinCmd()
	if err := cmd.RunE(cmd, []string{"wendy-thor.local:50051"}); err != nil {
		t.Fatalf("unpin with an explicit port: %v", err)
	}

	if pin, ok := readPins()["wendy-thor"]; ok {
		t.Errorf("config pin survived unpin with an explicit port: %+v", pin)
	}
	if _, ok := readSPKIPins(t)["urn:wendy:org:7:asset:42"]; ok {
		t.Error("SPKI pin survived unpin with an explicit port")
	}
}

// runUnpin runs the command with its stdout captured, so a test can assert on
// the report as well as on the stores.
func runUnpin(t *testing.T, arg string) string {
	t.Helper()
	cmd := newDeviceUnpinCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := cmd.RunE(cmd, []string{arg}); err != nil {
		t.Fatalf("unpin %q: %v", arg, err)
	}
	return out.String()
}

// TestDeviceUnpinAcceptsAnIdentityURN closes the escape hatch's other dead end.
//
// spkiRefusal fires with the SPKI store's key in hand and nothing else: the
// picker and `wendy device list` dial lanAgentAddresses, whose first entry is
// the IP, so the refusal names an IP that keys no config pin and whose asset no
// cache lookup can derive (cachedDeviceHostEntry matches on Hostname). Agents
// that never advertise `orgid` — the Swift macOS agent, Linux agents before
// 2026-07-18 — leave the same gap even when a hostname is dialed. For both
// populations the hostname-keyed unpin cleared nothing at all, and recovery
// meant hand-editing known_devices.json.
//
// Given the URN the refusal prints, unpin must clear the SPKI entry AND any
// config pin naming the same identity, so the two stores do not drift apart
// into a second refusal under a different message.
func TestDeviceUnpinAcceptsAnIdentityURN(t *testing.T) {
	readPins := writePinTestConfig(t, map[string]config.DevicePin{
		"thor": {OrgID: 7, CloudGRPC: "grpc.a.sh:443", AssetID: "42"},
	})
	setPinCache(t) // no cache entry: exactly the IP-dial / no-orgid shape
	seedSPKIPins(t, "urn:wendy:org:7:asset:42", "urn:wendy:org:7:asset:99")

	out := runUnpin(t, "urn:wendy:org:7:asset:42")

	if _, ok := readSPKIPins(t)["urn:wendy:org:7:asset:42"]; ok {
		t.Error("SPKI pin survived an unpin by its own store key: the refusal that named it has no other way out")
	}
	if pin, ok := readPins()["thor"]; ok {
		t.Errorf("config pin naming the same identity survived (%+v): the stores drift and the next dial refuses under the other one", pin)
	}
	if _, ok := readSPKIPins(t)["urn:wendy:org:7:asset:99"]; !ok {
		t.Error("unpinning one identity removed another device's SPKI pin")
	}
	if !strings.Contains(out, "urn:wendy:org:7:asset:42") || !strings.Contains(out, "thor") {
		t.Errorf("unpin reported %q, want it to name both entries it cleared", out)
	}
}

// TestSPKIRefusalNamesAnArgumentUnpinCanActOn is the round trip the two halves
// of Finding A only make sense together: the exact string spkiRefusal tells the
// user to run must clear the entry that caused the refusal. Asserting the
// message text and the command's behaviour separately would let them drift.
func TestSPKIRefusalNamesAnArgumentUnpinCanActOn(t *testing.T) {
	writePinTestConfig(t, nil)
	setPinCache(t)
	seedSPKIPin(t, "urn:wendy:org:7:asset:42")

	// The IP-dial shape: the ladder knows only the address it dialed.
	err := spkiRefusal("192.168.1.9", &devicepin.PinMismatchError{
		Key: "urn:wendy:org:7:asset:42", DisplayName: "orin",
		Want: "sha256:aaa", Got: "sha256:bbb",
	})

	arg := unpinArgumentFrom(t, err.Error())
	runUnpin(t, arg)

	if _, ok := readSPKIPins(t)["urn:wendy:org:7:asset:42"]; ok {
		t.Errorf("running the refusal's own suggested command (%q) cleared nothing; the refusal is a dead end", arg)
	}
}

// unpinArgumentFrom extracts the argument a refusal told the user to pass to
// `wendy device unpin`.
func unpinArgumentFrom(t *testing.T, msg string) string {
	t.Helper()
	const marker = "wendy device unpin "
	i := strings.Index(msg, marker)
	if i < 0 {
		t.Fatalf("refusal %q names no unpin command at all", msg)
	}
	rest := msg[i+len(marker):]
	return strings.TrimRight(strings.Fields(rest)[0], "'\"")
}

// TestDeviceUnpinLeavesAnotherDevicesPinsAlone is the adversarial case for the
// blast radius, and the reason the "clear every candidate key" rule could not
// stand.
//
// pinCandidateKeys' aliases come from the discovery cache, which Cache.Replace
// fills verbatim from unauthenticated mDNS TXT records. An attacker who can
// answer for the hostname the user is about to unpin chooses that entry's
// displayname, mesh name, assetid and orgid — so pointing them at a victim
// device made `wendy device unpin thor.local` delete the victim's config pin
// and its SPKI entry too. The victim's next connect then started from a
// permissive first use: an escape hatch turned into a downgrade primitive.
//
// Unpinning thor must clear thor's pins and nothing else.
func TestDeviceUnpinLeavesAnotherDevicesPinsAlone(t *testing.T) {
	readPins := writePinTestConfig(t, map[string]config.DevicePin{
		"thor":   {OrgID: 7, CloudGRPC: "grpc.a.sh:443", AssetID: "42"},
		"victim": {OrgID: 7, CloudGRPC: "grpc.a.sh:443", AssetID: "99"},
	})
	// A hostile answer for thor.local claiming the victim's names and asset.
	setPinCache(t, discoverycache.Entry{
		ID:          "dev-1",
		Hostname:    "thor.local",
		DisplayName: "victim",
		MeshName:    "victim",
		AssetID:     99,
		OrgID:       7,
	})
	seedSPKIPins(t, "urn:wendy:org:7:asset:42", "urn:wendy:org:7:asset:99")

	runUnpin(t, "thor.local")

	pins := readPins()
	if pin, ok := pins["thor"]; ok {
		t.Errorf("the pin the user asked to clear survived: %+v", pin)
	}
	if _, ok := pins["victim"]; !ok {
		t.Error("unpinning thor.local dropped another device's config pin, because an unauthenticated TXT record said the two were the same device; that device's next connect is now a permissive first use")
	}
	if _, ok := readSPKIPins(t)["urn:wendy:org:7:asset:99"]; !ok {
		t.Error("unpinning thor.local dropped another device's SPKI pin, from an asset id an attacker chose")
	}
	if _, ok := readSPKIPins(t)["urn:wendy:org:7:asset:42"]; ok {
		t.Error("the SPKI pin for the device actually being unpinned survived")
	}
}

// TestDeviceUnpinClearsEveryAliasOfTheSameDevice guards the property narrowing
// the blast radius must not cost: unpinning by either name a device
// legitimately answers to has to finish the job.
//
// `wendy cloud discover` files a pin under the roster's asset name while dials
// name the device by its mDNS hostname, so one device really is pinned twice.
// Both pins name the same asset, so both are this device's — clearing only the
// governing one would refuse again under the other, and a command that has to
// be run twice reads as a command that does not work.
func TestDeviceUnpinClearsEveryAliasOfTheSameDevice(t *testing.T) {
	readPins := writePinTestConfig(t, map[string]config.DevicePin{
		"wendyos-calm-zinnia": {OrgID: 7, CloudGRPC: "grpc.a.sh:443", AssetID: "42", Source: config.PinSourceLAN},
		"calm-zinnia":         {OrgID: 7, CloudGRPC: "grpc.a.sh:443", AssetID: "42", Source: config.PinSourceCloud},
	})
	setPinCache(t, discoverycache.Entry{
		ID:          "dev-1",
		Hostname:    "wendyos-calm-zinnia.local",
		DisplayName: "calm-zinnia",
	})
	seedSPKIPin(t, "urn:wendy:org:7:asset:42")

	runUnpin(t, "wendyos-calm-zinnia.local")

	for _, key := range []string{"wendyos-calm-zinnia", "calm-zinnia"} {
		if pin, ok := readPins()[key]; ok {
			t.Errorf("pin %q for the same asset survived (%+v); the next dial refuses under it and the user has to unpin twice", key, pin)
		}
	}
	if _, ok := readSPKIPins(t)["urn:wendy:org:7:asset:42"]; ok {
		t.Error("SPKI pin survived")
	}
}

// TestDeviceUnpinIgnoresACacheThatDisagreesWithThePin covers the other half of
// cachedIdentityKey's narrowing. The cache is consulted only where it agrees
// with the governing config pin, or where there is no config pin at all (the
// `wendy device list` case TestDeviceUnpinClearsSPKIPinWithNoConfigPin covers).
// A cache that names a different asset for a pinned host is stale or hostile,
// and either way it is not this device's SPKI entry to remove.
func TestDeviceUnpinIgnoresACacheThatDisagreesWithThePin(t *testing.T) {
	writePinTestConfig(t, map[string]config.DevicePin{
		"thor": {OrgID: 7, CloudGRPC: "grpc.a.sh:443", AssetID: "42"},
	})
	setPinCache(t, discoverycache.Entry{
		ID:       "dev-1",
		Hostname: "thor.local",
		AssetID:  99,
		OrgID:    7,
	})
	seedSPKIPins(t, "urn:wendy:org:7:asset:42", "urn:wendy:org:7:asset:99")

	runUnpin(t, "thor.local")

	if _, ok := readSPKIPins(t)["urn:wendy:org:7:asset:99"]; !ok {
		t.Error("an SPKI entry the discovery cache named — and the governing pin did not — was removed; that asset id is attacker-chosen")
	}
	if _, ok := readSPKIPins(t)["urn:wendy:org:7:asset:42"]; ok {
		t.Error("the SPKI entry the governing pin names survived")
	}
}

// TestDeviceUnpinLegacyPinHasNoSPKIKey covers a pin written before asset ids
// were recorded (config.DevicePin.AssetID == ""): there is no way to derive
// the SPKI store's URN key from it, since that key requires an asset id. Per
// the design's ambiguity ruling, this must clear the config pin and succeed
// rather than fail — there is no SPKI entry to fail on, either, since a
// legacy connection's certificate never had an asset id to pin in the first
// place.
func TestDeviceUnpinLegacyPinHasNoSPKIKey(t *testing.T) {
	readPins := writePinTestConfig(t, map[string]config.DevicePin{
		"wendy-thor": {OrgID: 7, CloudGRPC: "grpc.a.sh:443"},
	})

	cmd := newDeviceUnpinCmd()
	if err := cmd.RunE(cmd, []string{"wendy-thor.local"}); err != nil {
		t.Fatalf("unpin of a legacy (assetless) pin: want nil, got %v", err)
	}
	if pin, ok := readPins()["wendy-thor"]; ok {
		t.Errorf("config pin survived unpin: %+v", pin)
	}
}
