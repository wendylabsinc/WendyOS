package commands

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/devicepin"
	"github.com/wendylabsinc/wendy/go/internal/shared/discoverycache"
)

// setPinCache installs a deviceCacheLoadFn serving exactly entries, so a pin
// lookup test controls the alias names (mesh/display) its candidate list can
// see instead of reading whatever the developer's real ~/.wendy/devices.json
// happens to hold.
func setPinCache(t *testing.T, entries ...discoverycache.Entry) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "devices.json")
	seedDeviceCache(t, path, entries...)
	orig := deviceCacheLoadFn
	deviceCacheLoadFn = func() (*discoverycache.Cache, error) { return discoverycache.LoadFrom(path) }
	t.Cleanup(func() { deviceCacheLoadFn = orig })
}

// setFailingPinCache installs a deviceCacheLoadFn that cannot open the cache at
// all — the degradation path every pin lookup must survive.
func setFailingPinCache(t *testing.T) {
	t.Helper()
	orig := deviceCacheLoadFn
	deviceCacheLoadFn = func() (*discoverycache.Cache, error) {
		return nil, errors.New("device cache unavailable")
	}
	t.Cleanup(func() { deviceCacheLoadFn = orig })
}

// setPinConfig installs a config through the loadConfigForPinFn seam so a pin
// lookup test never touches the real config file.
func setPinConfig(t *testing.T, pins map[string]config.DevicePin) {
	t.Helper()
	cfg := &config.Config{DevicePins: pins}
	orig := loadConfigForPinFn
	loadConfigForPinFn = func() (*config.Config, error) { return cfg, nil }
	t.Cleanup(func() { loadConfigForPinFn = orig })
}

// TestPinCandidateKeysSingleKeyWithoutCache: with nothing cached under the
// dialled hostname there are no aliases to add, so the candidate list is
// exactly today's single key. This is the shape every legacy caller sees.
func TestPinCandidateKeysSingleKeyWithoutCache(t *testing.T) {
	setPinCache(t) // empty cache, no entries at all

	got := pinCandidateKeys("wendyos-calm-zinnia.local")
	want := []string{"wendyos-calm-zinnia.local"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("pinCandidateKeys = %q, want %q — an empty cache must degrade to the single dialled key", got, want)
	}

	if keys := pinCandidateKeys(""); len(keys) != 0 {
		t.Fatalf("pinCandidateKeys(\"\") = %q, want none — an empty key disables pin enforcement", keys)
	}
}

// TestPinCandidateKeysAddsMeshThenDisplayName: the cache entry matched by
// hostname contributes its mesh name and then its display name, in that order,
// after the dialled key. Deduplication is by the same normalisation the pin
// store uses, so a display name that is only a cosmetic variant of the dialled
// host does not produce a second, redundant candidate.
func TestPinCandidateKeysAddsMeshThenDisplayName(t *testing.T) {
	setPinCache(t, discoverycache.Entry{
		ID:          "dev-1",
		DisplayName: "calm-zinnia",
		Hostname:    "wendyos-calm-zinnia.local",
		MeshName:    "calm-zinnia.acme.mesh.wendy.internal",
	})

	got := pinCandidateKeys("wendyos-calm-zinnia.local")
	want := []string{"wendyos-calm-zinnia.local", "calm-zinnia.acme.mesh.wendy.internal", "calm-zinnia"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("pinCandidateKeys = %q, want %q (dialled key, then mesh name, then display name)", got, want)
	}

	// Dedup: a display name that normalises onto the dialled key must not
	// repeat it, and an empty mesh name must not become an empty candidate.
	setPinCache(t, discoverycache.Entry{
		ID:          "dev-2",
		DisplayName: "WendyOS-Calm-Zinnia",
		Hostname:    "wendyos-calm-zinnia.local",
	})
	got = pinCandidateKeys("wendyos-calm-zinnia.local")
	want = []string{"wendyos-calm-zinnia.local"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("pinCandidateKeys = %q, want %q — empties dropped and the dialled key never repeated", got, want)
	}
}

// Discovery aliases cannot authenticate a different name, even when the
// alias has a valid Cloud inventory binding of its own.
func TestExpectedIdentityIgnoresDiscoveryAliases(t *testing.T) {
	setPinCache(t, discoverycache.Entry{Hostname: "wendyos-calm-zinnia.local", DisplayName: "calm-zinnia", MeshName: "calm-zinnia.acme.mesh.wendy.internal"})
	setPinConfig(t, map[string]config.DevicePin{"calm-zinnia.acme.mesh.wendy.internal": {OrgID: 7, AssetID: "42", Source: config.PinSourceCloud}})
	if got := expectedIdentityFor("wendyos-calm-zinnia.local"); got != nil {
		t.Fatalf("discovery alias supplied an identity for an unbound hostname: %+v", got)
	}
	if target := newDialTarget("wendyos-calm-zinnia.local", "10.0.0.9:50051"); target.Expected != nil || target.pinned() {
		t.Fatalf("discovery alias supplied a selected-name binding: %+v", target)
	}
	if got := expectedIdentityFor("calm-zinnia.acme.mesh.wendy.internal"); got == nil || got.EntityID != "42" {
		t.Fatalf("explicitly selected Cloud name lost its binding: %+v", got)
	}
}

func TestLookupPinUsesOnlySelectedName(t *testing.T) {
	for _, source := range []string{config.PinSourceLAN, config.PinSourceCloud} {
		for _, asset := range []string{"", "1"} {
			t.Run(source+"/"+asset, func(t *testing.T) {
				setPinCache(t, discoverycache.Entry{Hostname: "robot-a.local", DisplayName: "robot-b", MeshName: "robot-c.mesh.wendy.internal"})
				want := config.DevicePin{OrgID: 7, AssetID: asset, Source: source}
				setPinConfig(t, map[string]config.DevicePin{"robot-a": want, "robot-b": {OrgID: 7, AssetID: "42", Source: config.PinSourceCloud}, "robot-c.mesh.wendy.internal": {OrgID: 7, AssetID: "43", Source: config.PinSourceCloud}})
				cfg, err := loadConfigForPinFn()
				if err != nil {
					t.Fatal(err)
				}
				got, key, ok := lookupPin(cfg, "robot-a.local")
				if !ok || got != want || key != "robot-a.local" {
					t.Fatalf("discovery replaced selected-name binding: got=%+v key=%q found=%v", got, key, ok)
				}
			})
		}
	}
}

// TestLookupPinCacheFailureDegradesToSingleKey is the regression that matters
// most: an unreadable cache must fall back to today's single-key behaviour. If
// it instead reported the host unpinned, enforcement would silently switch off
// and the ladder would offer a pinned host the plaintext rung.
func TestLookupPinCacheFailureDegradesToSingleKey(t *testing.T) {
	setFailingPinCache(t)
	setPinConfig(t, map[string]config.DevicePin{
		"wendyos-calm-zinnia": {OrgID: 7, AssetID: "42", Source: config.PinSourceLAN},
		// Reachable only via the cache's aliases, which are unavailable here.
		"calm-zinnia": {OrgID: 7, AssetID: "43", Source: config.PinSourceCloud},
	})
	cfg, err := loadConfigForPinFn()
	if err != nil {
		t.Fatalf("loadConfigForPinFn: %v", err)
	}

	if got := pinCandidateKeys("wendyos-calm-zinnia.local"); !reflect.DeepEqual(got, []string{"wendyos-calm-zinnia.local"}) {
		t.Fatalf("pinCandidateKeys = %q, want just the dialled key when the cache cannot be opened", got)
	}

	pin, key, ok := lookupPin(cfg, "wendyos-calm-zinnia.local")
	if !ok {
		t.Fatal("lookupPin reported UNPINNED because the cache could not be opened; a cache failure must never disable enforcement for a host that is pinned under its own key")
	}
	if key != "wendyos-calm-zinnia.local" || pin.AssetID != "42" {
		t.Fatalf("lookupPin = %+v under %q, want the dialled key's pin (asset 42)", pin, key)
	}
	if got := expectedIdentityFor("wendyos-calm-zinnia.local"); got == nil || got.EntityID != "42" {
		t.Fatalf("expectedIdentityFor = %+v, want asset 42 — the constraint must survive a cache failure", got)
	}
	if !newDialTarget("wendyos-calm-zinnia.local", "10.0.0.9:50051").pinned() {
		t.Fatal("dialTarget.pinned = false after a cache failure; the pinned host would be offered the plaintext rung")
	}
}

// Only the selected name's saved binding makes a target pinned. Discovery
// aliases cannot lend their pins to an otherwise unbound name.
func TestPinnedSelectedName(t *testing.T) {
	setPinCache(t, discoverycache.Entry{ID: "dev-1", DisplayName: "calm-zinnia", Hostname: "wendyos-calm-zinnia.local", MeshName: "calm-zinnia.acme.mesh.wendy.internal"})
	for _, key := range []string{"wendyos-calm-zinnia", "calm-zinnia.acme.mesh.wendy.internal", "calm-zinnia", "some-other-device"} {
		t.Run(key, func(t *testing.T) {
			setPinConfig(t, map[string]config.DevicePin{key: {OrgID: 7, CloudGRPC: "grpc.wendy.dev:443"}})
			want := key == "wendyos-calm-zinnia"
			if got := newDialTarget("wendyos-calm-zinnia.local", "10.0.0.9:50051").pinned(); got != want {
				t.Fatalf("pinned=%v, want %v for binding under %q", got, want, key)
			}
			if got := expectedIdentityFor("wendyos-calm-zinnia.local"); got != nil {
				t.Fatalf("organization-only binding supplied device identity: %+v", got)
			}
		})
	}
}

func TestExpectedIdentityFor(t *testing.T) {
	cases := []struct {
		name string
		pin  *config.DevicePin // nil = unpinned
		want *certs.WendyIdentity
	}{
		{name: "unpinned host is unconstrained", pin: nil, want: nil},
		{
			name: "pinned with asset constrains exactly",
			pin:  &config.DevicePin{OrgID: 7, AssetID: "42"},
			want: &certs.WendyIdentity{OrgID: 7, EntityType: "asset", EntityID: "42"},
		},
		{
			name: "pinned without an asset stays unconstrained",
			pin:  &config.DevicePin{OrgID: 7},
			want: nil,
		},
	}
	// Drive expectedIdentityFor through a config injected via the
	// loadConfigForPinFn seam so the test never touches the real config file.
	// The pin is stored under the normalised key ("orin") and looked up by the
	// name a user would type ("orin.local"), so this also covers the key
	// normalisation the ladder depends on.
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// An empty cache contributes no alias candidates, so this case
			// stays a pure single-key lookup and never reads the developer's
			// real ~/.wendy/devices.json.
			setPinCache(t)
			cfg := &config.Config{}
			if tc.pin != nil {
				cfg.DevicePins = map[string]config.DevicePin{"orin": *tc.pin}
			}
			orig := loadConfigForPinFn
			loadConfigForPinFn = func() (*config.Config, error) { return cfg, nil }
			t.Cleanup(func() { loadConfigForPinFn = orig })

			got := expectedIdentityFor("orin.local")
			switch {
			case tc.want == nil && got != nil:
				t.Fatalf("expectedIdentityFor = %+v, want nil (an unconstrained dial)", *got)
			case tc.want != nil && got == nil:
				t.Fatalf("expectedIdentityFor = nil, want %+v", *tc.want)
			case tc.want != nil && *got != *tc.want:
				t.Fatalf("expectedIdentityFor = %+v, want %+v", *got, *tc.want)
			}

			// newDialTarget must carry exactly that constraint, and the pin's
			// mere existence — asset or not — is what forbids the plaintext
			// rung. A pin with no asset still means "this host has answered
			// mTLS before".
			target := newDialTarget("orin.local", "10.0.0.9:50051")
			if (target.Expected == nil) != (tc.want == nil) {
				t.Fatalf("newDialTarget Expected = %v, want the same constraint as expectedIdentityFor (%v)", target.Expected, tc.want)
			}
			if want := tc.pin != nil; target.pinned() != want {
				t.Fatalf("dialTarget.pinned = %v, want %v", !want, want)
			}
			if target.Addr != "10.0.0.9:50051" || target.PinKey != "orin.local" {
				t.Fatalf("newDialTarget = %+v, want the requested name and the dialled address", target)
			}
		})
	}
}

// TestPinnedHostSkipsPlaintextRung is the security-critical case: a host we
// have seen over mTLS must never be reached unauthenticated, no matter what
// the TXT records or the cache claim.
//
// The mTLS rungs here fail with a plain transport error (nothing is listening),
// which is precisely the shape that today falls through to the plaintext rung —
// isCertRejectionError does not, and must not, match it. The refusal therefore
// has to come from the pin itself, not from error-string matching.
func TestPinnedHostSkipsPlaintextRung(t *testing.T) {
	setTempConfig(t, &config.Config{DevicePins: map[string]config.DevicePin{
		"orin": {OrgID: 7, CloudGRPC: "grpc.wendy.dev:443", AssetID: "42", Source: config.PinSourceLAN},
	}})
	// Pin lookup now consults the device cache for alias keys. Serve it an
	// empty one explicitly rather than leaving this security test's determinism
	// resting on setTempConfig's HOME happening to have no devices.json.
	setPinCache(t)

	addr := deadAgentAddr(t)

	plaintextCalls := 0
	origPlaintext := plaintextConnectFn
	plaintextConnectFn = func(ctx context.Context, address string) (*grpcclient.AgentConnection, error) {
		plaintextCalls++
		return grpcclient.NewFromConn(nil), nil
	}
	t.Cleanup(func() { plaintextConnectFn = origPlaintext })

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	target := newDialTarget("orin.local", addr)
	conn, mtlsErr, err := dialAgentLadderWithCerts(ctx, target, []config.CertificateInfo{selfSignedCLICert(t, 7)})
	if conn != nil {
		conn.Close()
	}

	if plaintextCalls != 0 {
		t.Fatalf("plaintext rung attempted %d times for a pinned host; a host already reached over mTLS must never be dialled unauthenticated", plaintextCalls)
	}
	if conn != nil {
		t.Fatalf("ladder returned a connection (%v) for a pinned host whose mTLS rungs all failed", conn)
	}
	if err == nil {
		t.Fatal("ladder returned no error for a pinned host whose mTLS rungs all failed")
	}
	// The refusal is the unreachable one, not an identity one: nothing answered,
	// so no certificate arrived and nothing was compared against the pin. It
	// must still block the unauthenticated fallback (that is what this test is
	// about) while telling the user the truth about why.
	if !errors.Is(err, errNoAuthenticatedEndpoint) {
		t.Fatalf("err = %v, want errNoAuthenticatedEndpoint", err)
	}
	if !blocksUnauthenticatedFallback(err) {
		t.Fatalf("err = %v must still forbid reaching this pinned device unauthenticated", err)
	}
	if !strings.Contains(err.Error(), "orin.local") {
		t.Fatalf("err = %v, want it to name the host the user dialled", err)
	}
	if strings.Contains(err.Error(), "device unpin") {
		t.Fatalf("err = %v recommends unpinning for a device that never answered", err)
	}
	// The mTLS diagnostic must survive the refusal — it is the only clue to why
	// no authenticated endpoint answered.
	if mtlsErr == nil {
		t.Fatal("mtlsErr = nil, want the last mTLS probe failure preserved for diagnostics")
	}
	// The load-bearing assertion for the guard's independence: this failure is
	// NOT a cert rejection, so the pre-existing isCertRejectionError branch
	// would have fallen straight through to plaintext.
	if isCertRejectionError("192.168.2.253:50052", mtlsErr) {
		t.Fatalf("mtlsErr = %v is a cert rejection; this test must exercise the transport-error shape that otherwise reaches the plaintext rung", mtlsErr)
	}
}

// TestPlaintextGuardUsesTheTargetsResolvedPin locks in that one connect makes
// ONE decision about what the pin says.
//
// The guard used to re-read pin state at the bottom of the ladder, independently
// of the read that produced target.Expected and target.refusalKey. Those two
// reads can disagree: every `wendy` invocation shares one config file, so a
// cloud seeding or an `unpin` landing in another process while the mTLS rungs
// are being tried changes the answer mid-connect. Disagreeing in the unpinned
// direction is the dangerous one — it hands a host that was pinned when the
// connect started an unauthenticated connection.
//
// The seam swap below stands in for that concurrent write. It is the honest
// shape of the bug: nothing about the target changed, only the state a second
// read would observe.
func TestPlaintextGuardUsesTheTargetsResolvedPin(t *testing.T) {
	setTempConfig(t, &config.Config{DevicePins: map[string]config.DevicePin{
		"orin": {OrgID: 7, CloudGRPC: "grpc.wendy.dev:443", AssetID: "42", Source: config.PinSourceLAN},
	}})
	setPinCache(t)

	target := newDialTarget("orin.local", deadAgentAddr(t))
	if !target.pinned() {
		t.Fatal("test precondition: orin.local must be pinned at the moment the target is built")
	}

	// The pin vanishes after the target is resolved and before the ladder
	// reaches its last rung.
	origLoad := loadConfigForPinFn
	loadConfigForPinFn = func() (*config.Config, error) { return &config.Config{}, nil }
	t.Cleanup(func() { loadConfigForPinFn = origLoad })

	plaintextCalls := 0
	origPlaintext := plaintextConnectFn
	plaintextConnectFn = func(context.Context, string) (*grpcclient.AgentConnection, error) {
		plaintextCalls++
		return grpcclient.NewFromConn(nil), nil
	}
	t.Cleanup(func() { plaintextConnectFn = origPlaintext })

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn, _, err := dialAgentLadderWithCerts(ctx, target, []config.CertificateInfo{selfSignedCLICert(t, 7)})
	if conn != nil {
		conn.Close()
	}

	if plaintextCalls != 0 {
		t.Fatalf("plaintext rung attempted %d times for a target that was pinned when the connect started; the guard re-read pin state instead of using the dial's own resolution", plaintextCalls)
	}
	if err == nil || !errors.Is(err, errNoAuthenticatedEndpoint) {
		t.Fatalf("err = %v, want the pinned-host refusal", err)
	}
	// The refusal still names the key the governing pin was filed under, from
	// the same resolution — not a key looked up again against the new state.
	if !strings.Contains(err.Error(), "orin.local") {
		t.Fatalf("err = %v, want the refusal to name the resolved pin key", err)
	}
}

// TestWrongDeviceAbortsLadder covers the other half of the enforcement: a peer
// that proves it is the WRONG device ends the ladder there and then.
//
// The pin key here is deliberately UNPINNED, so the pinned-host guard cannot be
// what suppresses the plaintext rung — only the abort can. That is the configuration
// Task 8 introduces for real: a cloud-seeded Expected with no config pin behind
// it, where this abort is the single thing between a wrong device and an
// unauthenticated connection.
func TestWrongDeviceAbortsLadder(t *testing.T) {
	setTempConfig(t, &config.Config{}) // no pins at all
	// As above: an explicit empty cache, so no alias key can turn this
	// deliberately-unpinned host into a pinned one.
	setPinCache(t)
	if newDialTarget("orin.local", "10.0.0.9:50051").pinned() {
		t.Fatal("test precondition: orin.local must be unpinned, so only the abort can refuse plaintext")
	}

	origMismatch := identityMismatchFn
	mismatchReads := 0
	identityMismatchFn = func(*grpcclient.AgentConnection) (*certs.IdentityMismatchError, bool) {
		mismatchReads++
		return &certs.IdentityMismatchError{WantOrg: 7, WantAsset: "42", GotOrg: 7, GotAsset: "43"}, true
	}
	plaintextCalls := 0
	origPlaintext := plaintextConnectFn
	plaintextConnectFn = func(ctx context.Context, address string) (*grpcclient.AgentConnection, error) {
		plaintextCalls++
		return grpcclient.NewFromConn(nil), nil
	}
	t.Cleanup(func() {
		identityMismatchFn = origMismatch
		plaintextConnectFn = origPlaintext
	})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// Two certs × two ports = four rungs if the ladder kept going.
	allCerts := []config.CertificateInfo{selfSignedCLICert(t, 7), selfSignedCLICert(t, 9)}
	target := dialTarget{
		PinKey:   "orin.local",
		Addr:     deadAgentAddr(t),
		Expected: &certs.WendyIdentity{OrgID: 7, EntityType: "asset", EntityID: "42"},
	}
	conn, mtlsErr, err := dialAgentLadderWithCerts(ctx, target, allCerts)
	if conn != nil {
		conn.Close()
	}

	if mismatchReads != 1 {
		t.Fatalf("ladder examined %d rungs after a wrong-device rejection, want exactly 1; every remaining cert and port would fail identically", mismatchReads)
	}
	if plaintextCalls != 0 {
		t.Fatalf("plaintext rung attempted %d times after a wrong-device rejection", plaintextCalls)
	}
	if conn != nil {
		t.Fatalf("ladder returned a connection (%v) to a device that proved it was the wrong one", conn)
	}
	if err == nil {
		t.Fatal("ladder returned no error for a wrong-device rejection")
	}
	for _, want := range []string{`device "orin.local" identity changed`, "asset 42 in organization 7", "asset 43 in organization 7", "reinstalled or re-enrolled", changedIdentityStep} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want the identity refusal to include %q", err, want)
		}
	}
	if mtlsErr == nil {
		t.Fatal("mtlsErr = nil, want the mTLS probe failure preserved alongside the refusal")
	}
}

// Refusals must identify the saved selected-name binding.
func TestRefusalNamesTheKeyTheGoverningPinIsFiledUnder(t *testing.T) {
	setUp := func(t *testing.T) {
		t.Helper()
		setTempConfig(t, &config.Config{DevicePins: map[string]config.DevicePin{
			// A verified saved binding for the selected hostname.
			"wendyos-calm-zinnia": {OrgID: 7, CloudGRPC: "grpc.wendy.dev:443", AssetID: "42", Source: config.PinSourceCloud},
		}})
		setPinCache(t, discoverycache.Entry{
			ID:          "dev-1",
			DisplayName: "wendyos-calm-zinnia",
			Hostname:    "wendyos-calm-zinnia.local",
		})
	}

	t.Run("the target carries the matched key", func(t *testing.T) {
		setUp(t)
		target := newDialTarget("wendyos-calm-zinnia.local", "10.0.0.9:50051")
		if target.PinnedKey != "wendyos-calm-zinnia.local" {
			t.Fatalf("PinnedKey = %q, want \"wendyos-calm-zinnia.local\" — the key the pin is actually filed under", target.PinnedKey)
		}
		if target.refusalKey() != "wendyos-calm-zinnia.local" {
			t.Fatalf("refusalKey = %q, want \"wendyos-calm-zinnia.local\"", target.refusalKey())
		}
		if target.Expected == nil || target.Expected.EntityID != "42" {
			t.Fatalf("Expected = %v, want the selected name's asset 42 still enforced", target.Expected)
		}
	})

	t.Run("a wrong-device refusal names it", func(t *testing.T) {
		setUp(t)
		origMismatch := identityMismatchFn
		identityMismatchFn = func(*grpcclient.AgentConnection) (*certs.IdentityMismatchError, bool) {
			return &certs.IdentityMismatchError{WantOrg: 7, WantAsset: "42", GotOrg: 7, GotAsset: "43"}, true
		}
		origPlaintext := plaintextConnectFn
		plaintextConnectFn = func(context.Context, string) (*grpcclient.AgentConnection, error) {
			t.Error("plaintext rung attempted after a wrong-device rejection")
			return grpcclient.NewFromConn(nil), nil
		}
		t.Cleanup(func() { identityMismatchFn, plaintextConnectFn = origMismatch, origPlaintext })

		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		target := newDialTarget("wendyos-calm-zinnia.local", deadAgentAddr(t))
		conn, _, err := dialAgentLadderWithCerts(ctx, target, []config.CertificateInfo{selfSignedCLICert(t, 7)})
		if conn != nil {
			conn.Close()
		}
		if err == nil {
			t.Fatal("no error for a wrong-device rejection")
		}
		if !strings.Contains(err.Error(), `device "wendyos-calm-zinnia.local" identity changed`) {
			t.Fatalf("err = %v, want it to name wendyos-calm-zinnia.local — the key that actually holds the pin", err)
		}
	})

	t.Run("an unauthenticated-fallback refusal names it", func(t *testing.T) {
		setUp(t)
		origPlaintext := plaintextConnectFn
		plaintextConnectFn = func(context.Context, string) (*grpcclient.AgentConnection, error) {
			t.Error("plaintext rung attempted for a pinned host")
			return grpcclient.NewFromConn(nil), nil
		}
		t.Cleanup(func() { plaintextConnectFn = origPlaintext })

		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		target := newDialTarget("wendyos-calm-zinnia.local", deadAgentAddr(t))
		conn, _, err := dialAgentLadderWithCerts(ctx, target, []config.CertificateInfo{selfSignedCLICert(t, 7)})
		if conn != nil {
			conn.Close()
		}
		if err == nil {
			t.Fatal("no error for a pinned host whose mTLS rungs all failed")
		}
		// A reachability failure names the selected device without suggesting unpin.
		if !strings.Contains(err.Error(), "wendyos-calm-zinnia") {
			t.Fatalf("err = %v, want it to name 'calm-zinnia' — the key that actually holds the pin", err)
		}
		if strings.Contains(err.Error(), "device unpin") {
			t.Fatalf("err = %v recommends unpinning for a device that never answered", err)
		}
	})
}

// TestSPKIMismatchAbortsLadderAndNamesUnpin covers the OTHER pin store: the
// SPKI one, keyed by the certificate's own asset URN, which hard-fails when a
// device's public key changes while its pinned certificate is still valid.
//
// That rejection is raised inside VerifyConnection and reached the user only as
// whatever survived gRPC's "authentication handshake failed" wrapper — a
// message naming neither the host as the user knows it nor any way out. The
// pin key here is deliberately UNPINNED in config, which is the real shape of
// the problem: `wendy device list` SPKI-pins every device the prober enumerates
// without writing a single config pin, so the pinned-host guard cannot be what
// refuses the plaintext rung here — only the abort can.
func TestSPKIMismatchAbortsLadderAndNamesUnpin(t *testing.T) {
	setTempConfig(t, &config.Config{}) // no config pins at all
	setPinCache(t)
	if newDialTarget("orin.local", "10.0.0.9:50051").pinned() {
		t.Fatal("test precondition: orin.local must be unpinned in config")
	}

	pinReads := 0
	origPin := pinMismatchFn
	pinMismatchFn = func(*grpcclient.AgentConnection) (*devicepin.PinMismatchError, bool) {
		pinReads++
		return &devicepin.PinMismatchError{
			Key: "urn:wendy:org:7:asset:42", DisplayName: "orin",
			Want: "sha256:aaa", Got: "sha256:bbb",
		}, true
	}
	plaintextCalls := 0
	origPlaintext := plaintextConnectFn
	plaintextConnectFn = func(context.Context, string) (*grpcclient.AgentConnection, error) {
		plaintextCalls++
		return grpcclient.NewFromConn(nil), nil
	}
	t.Cleanup(func() { pinMismatchFn, plaintextConnectFn = origPin, origPlaintext })

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// Two certs × two ports = four rungs if the ladder kept going.
	allCerts := []config.CertificateInfo{selfSignedCLICert(t, 7), selfSignedCLICert(t, 9)}
	target := newDialTarget("orin.local", deadAgentAddr(t))
	// An explicit inventory expectation exists even without a local config pin.
	target.Expected = &certs.WendyIdentity{OrgID: 7, EntityType: certs.EntityAsset, EntityID: "42"}
	conn, mtlsErr, err := dialAgentLadderWithCerts(ctx, target, allCerts)
	if conn != nil {
		conn.Close()
	}

	if pinReads != 1 {
		t.Fatalf("ladder examined %d rungs after an SPKI rejection, want exactly 1; the key belongs to the device, so every remaining cert and port fails identically", pinReads)
	}
	if plaintextCalls != 0 {
		t.Fatalf("plaintext rung attempted %d times after an SPKI rejection", plaintextCalls)
	}
	if err == nil {
		t.Fatal("ladder returned no error for an SPKI pin rejection")
	}
	// The refusal names the store's key, the only name that reaches the entry,
	// and says how to accept the new key.
	if !strings.Contains(err.Error(), "urn:wendy:org:7:asset:42") || !strings.Contains(err.Error(), changedIdentityStep) {
		t.Fatalf("err = %v, want the SPKI refusal naming the identity and how to accept the new key", err)
	}
	// The host is still what the user recognises, so the refusal must keep
	// naming it — the URN tells them what to run, not what broke.
	if !strings.Contains(err.Error(), "orin.local") {
		t.Fatalf("err = %v no longer names the host the user dialed", err)
	}
	if !errors.Is(err, errDeviceIdentityRefused) {
		t.Fatalf("err = %v does not read as an identity refusal; the picker's BLE fallback would take it for an unreachable device", err)
	}
	if mtlsErr == nil {
		t.Fatal("mtlsErr = nil, want the mTLS probe failure preserved alongside the refusal")
	}
}

// deadAgentAddr returns a 127.0.0.1 address whose port AND port+1 both refuse
// connections, so every rung of the ladder (which tries both) fails with a
// transport error rather than anything cert-shaped.
func deadAgentAddr(t *testing.T) string {
	t.Helper()
	for attempt := 0; attempt < 20; attempt++ {
		first, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("reserving a port: %v", err)
		}
		port := first.Addr().(*net.TCPAddr).Port
		second, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port+1))
		first.Close()
		if err != nil {
			continue // port+1 is in use; try another pair
		}
		second.Close()
		return fmt.Sprintf("127.0.0.1:%d", port)
	}
	t.Fatal("could not find two consecutive free ports")
	return ""
}

// selfSignedCLICert builds a usable client CertificateInfo (loadable keypair
// plus a chain PEM, both of which ConnectWithTLSExpecting requires before it
// will dial) so the mTLS rungs fail at the transport, not while assembling
// their TLS config.
func selfSignedCLICert(t *testing.T, orgID int) config.CertificateInfo {
	t.Helper()
	return selfSignedCLICertUntil(t, orgID, time.Now().Add(time.Hour))
}

func selfSignedCLICertUntil(t *testing.T, orgID int, until time.Time) config.CertificateInfo {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "wendy/test/cli"},
		NotBefore:             until.Add(-24 * time.Hour),
		NotAfter:              until,
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("creating certificate: %v", err)
	}
	certPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshalling key: %v", err)
	}
	keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	return config.CertificateInfo{
		OrganizationID:      orgID,
		PemCertificate:      certPEM,
		PemPrivateKey:       keyPEM,
		PemCertificateChain: certPEM,
	}
}

func TestPinKeyDerivationIsUnchangedForEveryAddress(t *testing.T) {
	// This is shared, security-relevant derivation used by every device. Two
	// VMs colliding on 127.0.0.1 is a real annoyance, but neither cure worked:
	// an empty key reads as "unpinned" and disarms the plaintext-downgrade
	// guard, and a port-qualified key orphans pins users already hold under the
	// bare host, turning a mismatch into a silent first-use. A running VM's own
	// forward is the one exception (it keys as vm:<name>; see
	// TestPinKeyDerivationKeysOnlyARunningVMsForward), so with no VM running
	// this stays exactly what ships today.
	stubLoopbackVMs(t, nil)
	for addr, want := range map[string]string{
		"127.0.0.1:50051":  "127.0.0.1",
		"localhost:50051":  "localhost",
		"[::1]:50051":      "::1",
		"127.0.0.1":        "127.0.0.1",
		"rpi5.local:50051": "rpi5.local",
		"192.168.2.253":    "192.168.2.253",
	} {
		if got := pinKeyForAddr(addr); got != want {
			t.Errorf("pinKeyForAddr(%q) = %q, want %q", addr, got, want)
		}
	}
	for _, addr := range []string{"127.0.0.1:50051", "localhost:50051"} {
		if pinKeyForAddr(addr) == "" {
			t.Errorf("pinKeyForAddr(%q) is empty; that would disarm the downgrade guard", addr)
		}
	}
}
