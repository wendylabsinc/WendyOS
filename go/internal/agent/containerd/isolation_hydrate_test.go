package containerd

import (
	"testing"

	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
)

func TestHydrateIsolationDoesNotRestoreRemovedIsolation(t *testing.T) {
	c := &Client{logger: zap.NewNop(), appIsolation: map[string]string{"myapp": ""}}
	c.hydrateIsolation("myapp", map[string]string{labelKeyIsolation: "shared-network"})
	if got := c.appIsolation["myapp"]; got != "" {
		t.Fatalf("old sibling restored removed isolation: %q", got)
	}
}

func TestHydrateIsolationRecordsEmptyAsLoaded(t *testing.T) {
	c := &Client{logger: zap.NewNop()}
	c.hydrateIsolation("myapp", map[string]string{})
	c.hydrateIsolation("myapp", map[string]string{labelKeyIsolation: "isolated"})
	if value, loaded := c.appIsolation["myapp"]; !loaded || value != "" {
		t.Fatalf("empty isolation was not authoritative: %q, loaded=%v", value, loaded)
	}
}

// TestHydrateIsolation_SetsFromLabelWhenEmpty is the RED test for the reboot bug:
// after a fresh process start (or right after ListBootContainers reads a
// container's persisted labels), the in-memory appIsolation cache is empty for
// that appID. hydrateIsolation must populate it from the label so downstream
// reads (getIsolation, used by StartContainer/GroupRestartAppID/RestartGroup)
// see the isolation mode the container was actually created with.
func TestHydrateIsolation_SetsFromLabelWhenEmpty(t *testing.T) {
	c := &Client{logger: zap.NewNop(), appIsolation: make(map[string]string)}

	labels := map[string]string{labelKeyAppID: "myapp", labelKeyIsolation: "isolated"}
	c.hydrateIsolation("myapp", labels)

	c.mu.Lock()
	got := c.getIsolation("myapp")
	c.mu.Unlock()
	if got != "isolated" {
		t.Fatalf("getIsolation(%q) = %q; want %q", "myapp", got, "isolated")
	}
}

// TestHydrateIsolation_NilCacheMap verifies hydrateIsolation initializes
// c.appIsolation lazily, matching the nil-map pattern used elsewhere in Client
// (e.g. CreateContainerWithProgress).
func TestHydrateIsolation_NilCacheMap(t *testing.T) {
	c := &Client{logger: zap.NewNop()} // appIsolation left nil, as it would be on a freshly constructed Client

	labels := map[string]string{labelKeyAppID: "myapp", labelKeyIsolation: "shared-network"}
	c.hydrateIsolation("myapp", labels)

	c.mu.Lock()
	got := c.getIsolation("myapp")
	c.mu.Unlock()
	if got != "shared-network" {
		t.Fatalf("getIsolation(%q) = %q; want %q", "myapp", got, "shared-network")
	}
}

// TestHydrateIsolation_DoesNotOverrideLiveValue is the idempotency guarantee:
// a value already present in the cache — whether written by a live
// CreateContainerWithProgress in this process, or by an earlier hydrate call —
// must never be clobbered by a later hydrate call, even if the label disagrees
// (which should not happen in practice, but the guarantee must hold regardless).
func TestHydrateIsolation_DoesNotOverrideLiveValue(t *testing.T) {
	c := &Client{logger: zap.NewNop(), appIsolation: map[string]string{"myapp": "isolated"}}

	labels := map[string]string{labelKeyAppID: "myapp", labelKeyIsolation: "shared-ipc"}
	c.hydrateIsolation("myapp", labels)

	c.mu.Lock()
	got := c.getIsolation("myapp")
	c.mu.Unlock()
	if got != "isolated" {
		t.Fatalf("hydrateIsolation overrode a live value: getIsolation(%q) = %q; want unchanged %q", "myapp", got, "isolated")
	}
}

// TestHydrateIsolation_CalledTwiceIsIdempotent covers the ListBootContainers
// call pattern directly: hydrateIsolation runs once per container seen during
// boot reconcile, and a device may enumerate the same appID's containers more
// than once (multi-service apps). A second call with the same label must be a
// no-op, not an error and not a redundant map write that could race with a
// concurrent StartContainer.
func TestHydrateIsolation_CalledTwiceIsIdempotent(t *testing.T) {
	c := &Client{logger: zap.NewNop(), appIsolation: make(map[string]string)}
	labels := map[string]string{labelKeyAppID: "myapp", labelKeyIsolation: "isolated"}

	c.hydrateIsolation("myapp", labels)
	c.hydrateIsolation("myapp", labels)

	c.mu.Lock()
	got := c.getIsolation("myapp")
	c.mu.Unlock()
	if got != "isolated" {
		t.Fatalf("getIsolation(%q) = %q; want %q", "myapp", got, "isolated")
	}
}

// TestHydrateIsolation_EmptyLabelValueLeavesCacheEmpty ensures a container with
// no isolation label (i.e. it was never isolated) does not get spuriously
// populated — getIsolation must keep returning "" so IsSharedNamespaceIsolation
// and the "isolated" checks in StartContainer correctly treat it as unisolated.
func TestHydrateIsolation_EmptyLabelValueLeavesCacheEmpty(t *testing.T) {
	c := &Client{logger: zap.NewNop(), appIsolation: make(map[string]string)}
	labels := map[string]string{labelKeyAppID: "myapp"} // no labelKeyIsolation

	c.hydrateIsolation("myapp", labels)

	c.mu.Lock()
	got := c.getIsolation("myapp")
	c.mu.Unlock()
	if got != "" {
		t.Fatalf("getIsolation(%q) = %q; want empty (no isolation label present)", "myapp", got)
	}
}

// TestHydrateIsolation_EmptyAppIDIsNoop guards against accidentally keying the
// cache under "" when a caller (e.g. ListBootContainers on a malformed
// container) passes an empty appID.
func TestHydrateIsolation_EmptyAppIDIsNoop(t *testing.T) {
	c := &Client{logger: zap.NewNop(), appIsolation: make(map[string]string)}
	labels := map[string]string{labelKeyIsolation: "isolated"}

	c.hydrateIsolation("", labels)

	c.mu.Lock()
	_, ok := c.appIsolation[""]
	c.mu.Unlock()
	if ok {
		t.Fatal("hydrateIsolation must not create an entry for an empty appID")
	}
}

// TestReconcileRoundTrip_LabelPersistsAcrossSimulatedReboot is the end-to-end
// regression test for the bug: it drives the exact label shape
// CreateContainerWithProgress persists (via wendyLabels) through a *fresh*
// Client — standing in for the agent process that comes up after a reboot,
// where c.appIsolation starts out empty — and verifies that once the boot
// reconcile path hydrates from that label (as ListBootContainers now does for
// every container it enumerates), getIsolation reports the original isolation
// mode. That is precisely the value GroupRestartAppID/RestartGroup and
// StartContainer's CNI-ADD gate read, so a correct result here means the
// isolated networking path is taken again after reboot instead of silently
// skipped.
func TestReconcileRoundTrip_LabelPersistsAcrossSimulatedReboot(t *testing.T) {
	appID := "camera-app"

	// Step 1: simulate CreateContainerWithProgress persisting labels at create
	// time (client.go:826) for an isolated multi-service app.
	createTimeLabels := wendyLabels(appID, "streamer", "1.0.0", nil, nil, "isolated", nil)
	if createTimeLabels[labelKeyIsolation] != "isolated" {
		t.Fatalf("precondition: wendyLabels did not persist isolation label, got %+v", createTimeLabels)
	}

	// Step 2: simulate the agent restarting. A brand new Client has an empty
	// appIsolation cache, exactly like the real agent after a device reboot.
	c := &Client{logger: zap.NewNop(), appIsolation: make(map[string]string)}
	c.mu.Lock()
	got := c.getIsolation(appID)
	c.mu.Unlock()
	if got != "" {
		t.Fatalf("precondition: fresh Client must start with no cached isolation, got %q", got)
	}

	// Step 3: simulate what ListBootContainers now does for each enumerated
	// container: read its persisted labels (here, the same labels containerd
	// would return from ctr.Info(ctx).Labels) and hydrate.
	c.hydrateIsolation(appID, createTimeLabels)

	// Step 4: this is exactly what StartContainer's CNI-ADD gate (~client.go:1211),
	// GroupRestartAppID (~client.go:1827), and RestartGroup (~client.go:1854) read.
	c.mu.Lock()
	got = c.getIsolation(appID)
	c.mu.Unlock()
	if got != "isolated" {
		t.Fatalf("after simulated reboot + reconcile hydrate, getIsolation(%q) = %q; want %q (isolated networking path would be skipped)", appID, got, "isolated")
	}
	if !appconfig.IsSharedNamespaceIsolation("shared-network") {
		t.Fatal("sanity check: IsSharedNamespaceIsolation helper behaves unexpectedly")
	}
}
