package commands

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/discoverycache"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
)

// probeKey mirrors the identity m.probe is keyed by in the discover model:
// discoverycache.Key(ID, DisplayName) — the same identity upsertLANDevice
// merges rows by — not the display name alone, which can collide across
// devices or change out from under a single device.
func probeKey(dev models.LANDevice) string {
	return discoverycache.Key(dev.ID, dev.DisplayName)
}

// lanFoundEvent builds a lanEventMsg for a live-confirmed, agent-probed
// sighting (Probed: true) — the common case in these tests, where the
// device's full metadata is already known.
func lanFoundEvent(dev models.LANDevice) lanEventMsg {
	return lanEventMsg{ev: discovery.LANEvent{Kind: discovery.LANFound, Device: dev, Probed: true}}
}

// discoverAgentCell returns the Agent-column cell of the single table row.
func discoverAgentCell(t *testing.T, m discoverModel) string {
	t.Helper()
	agentIdx := -1
	for i, c := range m.table.Columns() {
		if c.Title == "Agent" {
			agentIdx = i
		}
	}
	if agentIdx < 0 {
		t.Fatalf("no Agent column; columns=%v", m.table.Columns())
	}
	rows := m.table.Rows()
	if len(rows) != 1 {
		t.Fatalf("rows = %d; want 1", len(rows))
	}
	return rows[0][agentIdx]
}

// findLANEventMsg runs cmd and, if it produced a tea.BatchMsg (as
// discoverModel.Init does — the LAN stream cmd is batched alongside the
// spinner tick), searches the batch for the sub-command that yields a
// lanEventMsg. Other sub-commands (e.g. the spinner tick) are executed too
// but their messages are discarded.
func findLANEventMsg(t *testing.T, cmd tea.Cmd) lanEventMsg {
	t.Helper()
	msg, ok := searchLANEventMsg(cmd)
	if !ok {
		t.Fatalf("no lanEventMsg produced by cmd (got %#v)", msg)
	}
	return msg
}

func searchLANEventMsg(cmd tea.Cmd) (lanEventMsg, bool) {
	if cmd == nil {
		return lanEventMsg{}, false
	}
	switch v := cmd().(type) {
	case lanEventMsg:
		return v, true
	case tea.BatchMsg:
		for _, sub := range v {
			if m, ok := searchLANEventMsg(sub); ok {
				return m, true
			}
		}
	}
	return lanEventMsg{}, false
}

// Cached addresses still accelerate discovery, but they need verification.
func TestCLILANStreamOptions_UsesCacheAndProber(t *testing.T) {
	opts := cliLANStreamOptions(context.Background())
	if !opts.UseCache {
		t.Error("UseCache = false; want cached addresses probed in the background")
	}
	if opts.Prober == nil {
		t.Error("Prober = nil; want cached addresses verified")
	}
}

// Exercise Init/Update through the stream so ignoring cache events must still
// re-arm the reader and allow a later confirmation to reach the table.
func TestDiscoverStream_CachedDevicesStayHiddenUntilConfirmed(t *testing.T) {
	orig := lanStreamFn
	t.Cleanup(func() { lanStreamFn = orig })
	ch := make(chan discovery.LANEvent, 8)
	lanStreamFn = func(ctx context.Context, opts discovery.StreamOptions) <-chan discovery.LANEvent {
		if !opts.UseCache || opts.Prober == nil {
			t.Error("LAN stream must probe cached addresses")
		}
		return ch
	}

	m := newDiscoverModel(context.Background(), discovery.DiscoveryOptions{Types: []models.InterfaceType{models.InterfaceLAN}}, true)
	cmd := m.Init()
	dev := models.LANDevice{ID: "dev-1", DisplayName: "alpha", Hostname: "alpha.local", Port: defaultAgentPort}

	for _, kind := range []discovery.LANEventKind{discovery.LANCached, discovery.LANOffline} {
		ch <- discovery.LANEvent{Kind: kind, Device: dev}
		updated, nextCmd := m.Update(findLANEventMsg(t, cmd))
		m, cmd = updated.(discoverModel), nextCmd
		if len(m.collection.LANDevices) != 0 || len(m.table.Rows()) != 0 || len(m.probe) != 0 {
			t.Fatalf("unconfirmed device was listed after event %v: devices=%+v, rows=%v, probe=%v", kind, m.collection.LANDevices, m.table.Rows(), m.probe)
		}
		if m.hasResults {
			t.Fatal("cache-only events must not count as discovery results")
		}
	}

	// A later successful retry adds the device, and subsequent updates merge.
	dev.AgentVersion = "0.10.4"
	dev.OSVersion = "WendyOS-0.10.4"
	for _, kind := range []discovery.LANEventKind{discovery.LANFound, discovery.LANUpdated} {
		ch <- discovery.LANEvent{Kind: kind, Device: dev, Probed: true}
		updated, nextCmd := m.Update(findLANEventMsg(t, cmd))
		m, cmd = updated.(discoverModel), nextCmd
		if got := len(m.collection.LANDevices); got != 1 {
			t.Fatalf("LANDevices = %d after confirmation; want 1", got)
		}
		if m.probe[probeKey(dev)] != tui.ProbeOK {
			t.Fatalf("probe state = %v; want ProbeOK", m.probe[probeKey(dev)])
		}
		if cell := discoverAgentCell(t, m); cell != "0.10.4" {
			t.Fatalf("Agent cell = %q; want 0.10.4", cell)
		}
	}
}

func TestLANPickerCachedDevicesStayHiddenUntilConfirmed(t *testing.T) {
	picker := tui.NewPicker()
	picker.MergeItem = mergePickerItem
	m := newDevicePickerModel(context.Background(), picker, nil, 0, false, devicePickerLocalTab)
	dev := models.LANDevice{ID: "dev-1", DisplayName: "cached-board", Hostname: "cached-board.local", Port: defaultAgentPort, IsMTLS: true}
	steps := []struct {
		ev      discovery.LANEvent
		visible bool
		hint    bool
	}{
		{ev: discovery.LANEvent{Kind: discovery.LANCached, Device: dev}},
		{ev: discovery.LANEvent{Kind: discovery.LANOffline, Device: dev}},
		{ev: discovery.LANEvent{Kind: discovery.LANFound, Device: dev}, visible: true},
		{ev: discovery.LANEvent{Kind: discovery.LANUpdated, Device: dev, ProbeFailed: true}, visible: true, hint: true},
		{ev: discovery.LANEvent{Kind: discovery.LANUpdated, Device: dev, Probed: true}, visible: true},
		{ev: discovery.LANEvent{Kind: discovery.LANRetracted, Device: dev}},
	}
	for _, step := range steps {
		if step.ev.Probed {
			step.ev.Device.AgentVersion = "0.10.4"
		}
		if msg := lanPickerEventMsg(step.ev); msg != nil {
			updated, _ := m.Update(devicePickerLocalMsg{msg: msg})
			m = updated.(devicePickerModel)
		}
		view := m.View()
		if got := strings.Contains(view, dev.DisplayName); got != step.visible {
			t.Fatalf("event %+v: visible = %v, want %v; view=%q", step.ev, got, step.visible, view)
		}
		if got := strings.Contains(view, "does not have access"); got != step.hint {
			t.Fatalf("event %+v: no-access hint = %v, want %v; view=%q", step.ev, got, step.hint, view)
		}
	}
}

// TestDiscoverStream_UpdatedEventAlsoUpdatesInPlace checks the LANUpdated
// event kind (an address change or a re-probe of an already-emitted device)
// merges into the same row rather than appending a second one.
func TestDiscoverStream_UpdatedEventAlsoUpdatesInPlace(t *testing.T) {
	m := newDiscoverModel(context.Background(), defaultOpts(), true)

	dev := models.LANDevice{ID: "dev-2", DisplayName: "gamma", IPAddress: "10.0.0.5", Port: defaultAgentPort}
	updated, _ := m.Update(lanFoundEvent(dev))
	dm := updated.(discoverModel)
	if got := len(dm.collection.LANDevices); got != 1 {
		t.Fatalf("LANDevices = %d after found event; want 1", got)
	}

	moved := dev
	moved.IPAddress = "10.0.0.9"
	updated, _ = dm.Update(lanEventMsg{ev: discovery.LANEvent{Kind: discovery.LANUpdated, Device: moved, Probed: true}})
	dm = updated.(discoverModel)
	if got := len(dm.collection.LANDevices); got != 1 {
		t.Fatalf("LANDevices = %d after updated event; want 1 (merged in place)", got)
	}
	if dm.collection.LANDevices[0].IPAddress != "10.0.0.9" {
		t.Fatalf("IPAddress = %q; want the updated address", dm.collection.LANDevices[0].IPAddress)
	}
	if dm.probe[probeKey(dev)] != tui.ProbeOK {
		t.Fatalf("probe state = %v; want ProbeOK", dm.probe[probeKey(dev)])
	}
}

// TestDiscoverStream_UnprobedFoundShowsPending checks a live mDNS sighting
// that has not yet been confirmed by an agent probe (Probed: false) renders
// as ProbePending, matching the "connecting" spinner the picker shows for
// the same case.
func TestDiscoverStream_UnprobedFoundShowsPending(t *testing.T) {
	m := newDiscoverModel(context.Background(), defaultOpts(), true)

	dev := models.LANDevice{ID: "dev-3", DisplayName: "delta", IPAddress: "10.0.0.6", Port: defaultAgentPort}
	updated, _ := m.Update(lanEventMsg{ev: discovery.LANEvent{Kind: discovery.LANFound, Device: dev, Probed: false}})
	dm := updated.(discoverModel)

	if dm.probe[probeKey(dev)] != tui.ProbePending {
		t.Fatalf("probe state = %v; want ProbePending", dm.probe[probeKey(dev)])
	}
	if cell := discoverAgentCell(t, dm); cell == "" {
		t.Fatal("unprobed device Agent cell should show a spinner frame, got blank")
	}
}

// TestDiscoverStream_SharedDisplayNameKeepsIndependentProbeStates covers the
// bug this rekey fixes: two distinct devices (different TXT IDs) that
// happen to advertise the same DisplayName must render as two rows with
// independent probe states — one going ProbeOK must never paint onto the
// other, and neither can orphan the other's map entry.
func TestDiscoverStream_SharedDisplayNameKeepsIndependentProbeStates(t *testing.T) {
	m := newDiscoverModel(context.Background(), defaultOpts(), true)

	devA := models.LANDevice{ID: "dev-a", DisplayName: "wendy", IPAddress: "10.0.0.10", Port: defaultAgentPort}
	devB := models.LANDevice{ID: "dev-b", DisplayName: "wendy", IPAddress: "10.0.0.11", Port: defaultAgentPort}

	updated, _ := m.Update(lanEventMsg{ev: discovery.LANEvent{Kind: discovery.LANFound, Device: devA, Probed: false}})
	dm := updated.(discoverModel)
	updated, _ = dm.Update(lanEventMsg{ev: discovery.LANEvent{Kind: discovery.LANFound, Device: devB, Probed: false}})
	dm = updated.(discoverModel)

	if got := len(dm.collection.LANDevices); got != 2 {
		t.Fatalf("LANDevices = %d; want 2 distinct rows for distinct IDs sharing a DisplayName", got)
	}

	// devA's probe resolves; devB must stay pending, independently.
	probedA := devA
	probedA.AgentVersion = "1.2.3"
	updated, _ = dm.Update(lanEventMsg{ev: discovery.LANEvent{Kind: discovery.LANUpdated, Device: probedA, Probed: true}})
	dm = updated.(discoverModel)

	if dm.probe[probeKey(probedA)] != tui.ProbeOK {
		t.Fatalf("devA probe state = %v; want ProbeOK", dm.probe[probeKey(probedA)])
	}
	if dm.probe[probeKey(devB)] != tui.ProbePending {
		t.Fatalf("devB probe state = %v; want ProbePending (unaffected by devA's probe)", dm.probe[probeKey(devB)])
	}
	if got := len(dm.collection.LANDevices); got != 2 {
		t.Fatalf("LANDevices = %d after devA's probe; want 2 (still two independent rows)", got)
	}

	// Confirm the table actually renders two distinct rows, not one row that
	// silently absorbed the other's state.
	agentIdx := -1
	for i, c := range dm.table.Columns() {
		if c.Title == "Agent" {
			agentIdx = i
		}
	}
	if agentIdx < 0 {
		t.Fatalf("no Agent column; columns=%v", dm.table.Columns())
	}
	rows := dm.table.Rows()
	if len(rows) != 2 {
		t.Fatalf("rows = %d; want 2", len(rows))
	}
	var sawResolved, sawPending bool
	for _, row := range rows {
		switch row[agentIdx] {
		case "1.2.3":
			sawResolved = true
		case "":
			t.Fatalf("pending row Agent cell should show a spinner frame, got blank: %v", row)
		default:
			sawPending = true
		}
	}
	if !sawResolved || !sawPending {
		t.Fatalf("expected one resolved row and one still-pending row, got rows=%v", rows)
	}
}

// TestDiscoverStream_ProbeFailedShowsFailedState covers the event the engine
// emits for a device mDNS can see but whose agent does not answer. Before it
// existed the row spun on "verifying" forever; the model must render it as
// ProbeFailed instead.
func TestDiscoverStream_ProbeFailedShowsFailedState(t *testing.T) {
	m := newDiscoverModel(context.Background(), defaultOpts(), true)

	dev := models.LANDevice{ID: "dev-4", DisplayName: "epsilon", IPAddress: "10.0.0.8", Port: defaultAgentPort}
	updated, _ := m.Update(lanEventMsg{ev: discovery.LANEvent{Kind: discovery.LANFound, Device: dev}})
	dm := updated.(discoverModel)
	if dm.probe[probeKey(dev)] != tui.ProbePending {
		t.Fatalf("probe state = %v; want ProbePending before the probe concludes", dm.probe[probeKey(dev)])
	}

	updated, _ = dm.Update(lanEventMsg{ev: discovery.LANEvent{Kind: discovery.LANUpdated, Device: dev, ProbeFailed: true}})
	dm = updated.(discoverModel)

	if dm.probe[probeKey(dev)] != tui.ProbeFailed {
		t.Fatalf("probe state = %v; want ProbeFailed", dm.probe[probeKey(dev)])
	}
	if got := len(dm.collection.LANDevices); got != 1 {
		t.Fatalf("LANDevices = %d; want 1 (a failed probe never removes the row)", got)
	}
	if cell := discoverAgentCell(t, dm); cell != tui.ProbeFailedGlyph {
		t.Fatalf("failed row Agent cell = %q; want %q", cell, tui.ProbeFailedGlyph)
	}
}

// TestDiscoverStream_SupersededRowIsDropped covers the connect-minted
// duplicate: `wendy run` caches an identity minted from the hostname, the
// live scan then identifies the same device by its TXT device id, and the
// engine reports which identity it replaced. The model must drop that row so
// one device never occupies two.
func TestDiscoverStream_SupersededRowIsDropped(t *testing.T) {
	m := newDiscoverModel(context.Background(), defaultOpts(), true)

	minted := models.LANDevice{ID: "orin", DisplayName: "orin", Hostname: "orin.local", IPAddress: "10.0.0.5", Port: defaultAgentPort}
	updated, _ := m.Update(lanFoundEvent(minted))
	dm := updated.(discoverModel)
	if got := len(dm.collection.LANDevices); got != 1 {
		t.Fatalf("LANDevices = %d after the confirmed minted row; want 1", got)
	}

	real := models.LANDevice{ID: "uuid-1", DisplayName: "orin", Hostname: "orin.local", IPAddress: "10.0.0.5", Port: defaultAgentPort}
	updated, _ = dm.Update(lanEventMsg{ev: discovery.LANEvent{
		Kind: discovery.LANFound, Device: real, Probed: true, Supersedes: probeKey(minted),
	}})
	dm = updated.(discoverModel)

	if got := len(dm.collection.LANDevices); got != 1 {
		t.Fatalf("LANDevices = %d; want 1 (the minted identity must be replaced, not duplicated)", got)
	}
	if dm.collection.LANDevices[0].ID != "uuid-1" {
		t.Fatalf("surviving row = %+v; want the TXT-id identity", dm.collection.LANDevices[0])
	}
	if _, stale := dm.probe[probeKey(minted)]; stale {
		t.Fatal("superseded identity left its probe state behind")
	}
	if dm.probe[probeKey(real)] != tui.ProbeOK {
		t.Fatalf("probe state = %v; want ProbeOK", dm.probe[probeKey(real)])
	}
}

// TestLANRowState pins the single mapping both LAN surfaces (device picker
// and discover TUI) render events through, including the two states the row
// can only reach through a probe outcome: ProbeFailed (seen on the network,
// agent silent) and the insecure marker, which only a real probe can speak
// for.
func TestLANRowState(t *testing.T) {
	mtls := models.LANDevice{ID: "dev", DisplayName: "orin", IsMTLS: true}
	plain := models.LANDevice{ID: "dev", DisplayName: "orin"}

	cases := []struct {
		name         string
		ev           discovery.LANEvent
		wantProbe    tui.ProbeState
		wantInsecure bool
	}{
		{"unprobed sighting verifies", discovery.LANEvent{Kind: discovery.LANFound, Device: plain}, tui.ProbePending, false},
		{"probed mTLS row is secure", discovery.LANEvent{Kind: discovery.LANFound, Device: mtls, Probed: true}, tui.ProbeOK, false},
		{"probed plaintext row is insecure", discovery.LANEvent{Kind: discovery.LANUpdated, Device: plain, Probed: true}, tui.ProbeOK, true},
		{"failed probe stops the spinner", discovery.LANEvent{Kind: discovery.LANUpdated, Device: plain, ProbeFailed: true}, tui.ProbeFailed, false},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			probe, insecure := lanRowState(tt.ev)
			if probe != tt.wantProbe || insecure != tt.wantInsecure {
				t.Fatalf("lanRowState = (%v, %v); want (%v, %v)", probe, insecure, tt.wantProbe, tt.wantInsecure)
			}
		})
	}
}
