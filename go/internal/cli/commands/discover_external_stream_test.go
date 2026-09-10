package commands

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/wendylabsinc/wendy/go/internal/cli/providers"
	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
)

// withExternalProviders points the discover command's provider seam at provs
// for the duration of the test.
func withExternalProviders(t *testing.T, provs ...providers.DeviceProvider) {
	t.Helper()
	orig := externalProvidersFn
	t.Cleanup(func() { externalProvidersFn = orig })
	externalProvidersFn = func() []providers.DeviceProvider { return provs }
}

// externalOpts restricts discovery to external devices, so no USB/Ethernet/LAN
// scan runs alongside and the only messages in play are the external ones.
func externalOpts() discovery.DiscoveryOptions {
	return discovery.DiscoveryOptions{Types: []models.InterfaceType{models.InterfaceExternal}}
}

// searchExtMsg runs cmd and returns the first external-discovery message it
// produces, descending into a tea.BatchMsg as discoverModel.Init returns one.
// Other sub-commands (the spinner tick) are executed too, their messages
// discarded. Buffer the fake stream channel before calling this, or the
// snapshot wait blocks.
func searchExtMsg(cmd tea.Cmd) (tea.Msg, bool) {
	if cmd == nil {
		return nil, false
	}
	switch v := cmd().(type) {
	case extScanMsg:
		return v, true
	case extStreamStartMsg:
		return v, true
	case extStreamSnapshotMsg:
		return v, true
	case extStreamEndMsg:
		return v, true
	case tea.BatchMsg:
		for _, sub := range v {
			if m, ok := searchExtMsg(sub); ok {
				return m, true
			}
		}
	}
	return nil, false
}

func findExtMsg(t *testing.T, cmd tea.Cmd) tea.Msg {
	t.Helper()
	msg, ok := searchExtMsg(cmd)
	if !ok {
		t.Fatalf("no external discovery message produced by cmd (got %#v)", msg)
	}
	return msg
}

// externalNames returns the DisplayNames currently in the collection, in order.
func externalNames(m discoverModel) []string {
	names := make([]string, 0, len(m.collection.ExternalDevices))
	for _, d := range m.collection.ExternalDevices {
		names = append(names, d.DisplayName)
	}
	return names
}

func extDevice(name string) models.ExternalDevice {
	return models.ExternalDevice{ProviderKey: "stream", DisplayName: name, ID: name}
}

// streamingFake builds a ContinuousDiscoverer whose channel the test drives.
func streamingFake(key string, buffer int) *fakeContinuousProvider {
	return &fakeContinuousProvider{
		fakeProvider: fakeProvider{key: key},
		ch:           make(chan []models.ExternalDevice, buffer),
	}
}

// TestDiscoverExternal_StreamingProviderUpdatesContinuously is the point of
// this whole path: a provider implementing ContinuousDiscoverer is consumed as
// a stream, each snapshot replaces the last (the contract says every emission
// is the full set), and DiscoverDevices is never called.
func TestDiscoverExternal_StreamingProviderUpdatesContinuously(t *testing.T) {
	prov := streamingFake("stream", 4)
	withExternalProviders(t, prov)

	m := newDiscoverModel(context.Background(), externalOpts(), true)
	start, ok := findExtMsg(t, m.Init()).(extStreamStartMsg)
	if !ok {
		t.Fatalf("Init did not start a stream for a ContinuousDiscoverer")
	}
	updated, cmd := m.Update(start)
	m = updated.(discoverModel)
	if !m.extStreaming["stream"] {
		t.Error("extStreaming[stream] = false after start; want true")
	}
	if !m.hasResults {
		t.Error("hasResults = false after stream start; want true — a stream that finds nothing never sends anything else")
	}

	prov.ch <- []models.ExternalDevice{extDevice("alpha")}
	updated, cmd = m.Update(findExtMsg(t, cmd))
	m = updated.(discoverModel)
	if got := externalNames(m); len(got) != 1 || got[0] != "alpha" {
		t.Fatalf("external devices = %v; want [alpha]", got)
	}

	// The second snapshot carries the whole set, so it replaces rather than
	// accumulates: two devices, not three.
	prov.ch <- []models.ExternalDevice{extDevice("alpha"), extDevice("beta")}
	updated, _ = m.Update(findExtMsg(t, cmd))
	m = updated.(discoverModel)
	if got := externalNames(m); len(got) != 2 {
		t.Fatalf("external devices = %v; want 2 (snapshots replace, never accumulate)", got)
	}

	if calls := prov.discoverCalls.Load(); calls != 0 {
		t.Errorf("DiscoverDevices called %d times; want 0 — a streaming provider is never polled", calls)
	}
}

// TestDiscoverExternal_StreamNeverPollsOnStartError and
// TestDiscoverExternal_StreamCloseStopsWithoutPolling pin a deliberate
// divergence from discoverProviderForPicker, which does fall back to polling in
// both cases. Do not "fix" these back: DiscoverDevices cannot see BLE, so for a
// BLE-capable provider polling discards the coverage the stream provided. See
// waitExternalSnapshot.
func TestDiscoverExternal_StreamNeverPollsOnStartError(t *testing.T) {
	prov := &fakeContinuousProvider{
		fakeProvider: fakeProvider{key: "stream", devices: []models.ExternalDevice{extDevice("alpha")}},
		err:          errors.New("no radio"),
	}
	withExternalProviders(t, prov)

	m := newDiscoverModel(context.Background(), externalOpts(), true)
	end, ok := findExtMsg(t, m.Init()).(extStreamEndMsg)
	if !ok {
		t.Fatalf("a stream that cannot start should report extStreamEndMsg")
	}
	updated, cmd := m.Update(end)
	m = updated.(discoverModel)

	if cmd != nil {
		t.Error("stream start error returned a follow-up cmd; want nil (no polling fallback)")
	}
	if calls := prov.discoverCalls.Load(); calls != 0 {
		t.Errorf("DiscoverDevices called %d times; want 0", calls)
	}
	if m.extStreaming["stream"] {
		t.Error("extStreaming[stream] = true after end; want false")
	}
	if !m.hasResults {
		t.Error("hasResults = false; want true so the view can say no devices were found")
	}
}

func TestDiscoverExternal_StreamCloseStopsWithoutPolling(t *testing.T) {
	prov := streamingFake("stream", 2)
	withExternalProviders(t, prov)

	m := newDiscoverModel(context.Background(), externalOpts(), true)
	updated, cmd := m.Update(findExtMsg(t, m.Init()))
	m = updated.(discoverModel)

	prov.ch <- []models.ExternalDevice{extDevice("alpha")}
	updated, cmd = m.Update(findExtMsg(t, cmd))
	m = updated.(discoverModel)

	close(prov.ch)
	end, ok := findExtMsg(t, cmd).(extStreamEndMsg)
	if !ok {
		t.Fatalf("a closed stream should report extStreamEndMsg")
	}
	updated, cmd = m.Update(end)
	m = updated.(discoverModel)

	if cmd != nil {
		t.Error("stream close returned a follow-up cmd; want nil (no polling fallback)")
	}
	if calls := prov.discoverCalls.Load(); calls != 0 {
		t.Errorf("DiscoverDevices called %d times; want 0", calls)
	}
	// The rows the stream found stay listed: nothing replaces a dead stream, so
	// clearing them would delete devices that are probably still there.
	if got := externalNames(m); len(got) != 1 || got[0] != "alpha" {
		t.Errorf("external devices = %v; want [alpha] retained through the stream ending", got)
	}
}

func TestDiscoverExternal_NonContinuousProviderStillPolls(t *testing.T) {
	t.Setenv("WENDY_DISCOVER_EXTERNAL_INTERVAL", "1ms")

	prov := &fakeProvider{key: "poll", devices: []models.ExternalDevice{extDevice("alpha")}}
	withExternalProviders(t, prov)

	m := newDiscoverModel(context.Background(), externalOpts(), true)
	scan, ok := findExtMsg(t, m.Init()).(extScanMsg)
	if !ok {
		t.Fatalf("a provider without a stream should be polled")
	}
	updated, cmd := m.Update(scan)
	m = updated.(discoverModel)
	if got := externalNames(m); len(got) != 1 || got[0] != "alpha" {
		t.Fatalf("external devices = %v; want [alpha]", got)
	}

	// The re-arm polls the same provider again.
	if _, ok := findExtMsg(t, cmd).(extScanMsg); !ok {
		t.Fatal("polling did not re-arm with another extScanMsg")
	}
	if calls := prov.discoverCalls.Load(); calls != 2 {
		t.Errorf("DiscoverDevices called %d times; want 2", calls)
	}
}

// TestDiscoverExternal_PerProviderSnapshotsDoNotClobber is the regression the
// old single-slice ExternalDevices made impossible to avoid: one provider
// reporting used to replace every provider's rows.
func TestDiscoverExternal_PerProviderSnapshotsDoNotClobber(t *testing.T) {
	t.Setenv("WENDY_DISCOVER_EXTERNAL_INTERVAL", "1h") // no re-poll during the test

	poll := &fakeProvider{key: "poll", devices: []models.ExternalDevice{extDevice("polled")}}
	stream := streamingFake("stream", 2)
	withExternalProviders(t, poll, stream)

	m := newDiscoverModel(context.Background(), externalOpts(), true)
	m.Init()

	updated, _ := m.Update(extScanMsg{provider: poll, devices: poll.devices})
	m = updated.(discoverModel)
	updated, _ = m.Update(extStreamSnapshotMsg{provider: stream, devices: []models.ExternalDevice{extDevice("streamed")}})
	m = updated.(discoverModel)

	got := externalNames(m)
	if len(got) != 2 || got[0] != "polled" || got[1] != "streamed" {
		t.Fatalf("external devices = %v; want [polled streamed] — both providers, in registration order", got)
	}

	// A fresh snapshot from one provider leaves the other's rows alone.
	updated, _ = m.Update(extStreamSnapshotMsg{provider: stream, devices: []models.ExternalDevice{extDevice("streamed2")}})
	m = updated.(discoverModel)
	got = externalNames(m)
	if len(got) != 2 || got[0] != "polled" || got[1] != "streamed2" {
		t.Fatalf("external devices = %v; want [polled streamed2]", got)
	}
}

func TestDiscoverExternal_PollErrorClearsOnlyThatProvider(t *testing.T) {
	t.Setenv("WENDY_DISCOVER_EXTERNAL_INTERVAL", "1h")

	good := &fakeProvider{key: "good", devices: []models.ExternalDevice{extDevice("kept")}}
	bad := &fakeProvider{key: "bad", devices: []models.ExternalDevice{extDevice("gone")}}
	withExternalProviders(t, good, bad)

	m := newDiscoverModel(context.Background(), externalOpts(), true)
	updated, _ := m.Update(extScanMsg{provider: good, devices: good.devices})
	m = updated.(discoverModel)
	updated, _ = m.Update(extScanMsg{provider: bad, devices: bad.devices})
	m = updated.(discoverModel)
	if got := externalNames(m); len(got) != 2 {
		t.Fatalf("external devices = %v; want 2 before the failure", got)
	}

	// A failing scan reports no devices (scanExternalProvider drops the error),
	// which clears that provider's rows and no one else's.
	bad.discoverErr = errors.New("docker not running")
	updated, _ = m.Update(findExtMsg(t, m.scanExternalProvider(bad)))
	m = updated.(discoverModel)
	if got := externalNames(m); len(got) != 1 || got[0] != "kept" {
		t.Fatalf("external devices = %v; want [kept] — only the failing provider's rows clear", got)
	}
}

func TestDiscoverExternal_RampsPerProvider(t *testing.T) {
	t.Setenv("WENDY_DISCOVER_EXTERNAL_INTERVAL", "3s")

	a := &fakeProvider{key: "a"}
	b := &fakeProvider{key: "b"}
	withExternalProviders(t, a, b)

	m := newDiscoverModel(context.Background(), externalOpts(), true)
	for range 2 {
		updated, _ := m.Update(extScanMsg{provider: a})
		m = updated.(discoverModel)
	}

	if got := m.extIntervals["a"].next; got != 2*time.Second {
		t.Errorf("next interval for a = %v; want 2s", got)
	}
	// b never reported, so its ramp is untouched — the whole reason each
	// provider carries its own.
	if got := m.extIntervals["b"].next; got != 0 {
		t.Errorf("next interval for b = %v; want 0 (never polled)", got)
	}
}

// TestDiscoverExternal_HasResultsWithoutDevices covers the case a stream makes
// possible and polling never did: nothing is ever reported, because a provider
// with no devices emits no snapshot at all.
func TestDiscoverExternal_HasResultsWithoutDevices(t *testing.T) {
	for _, tc := range []struct {
		name string
		msg  func(p providers.DeviceProvider) tea.Msg
	}{
		{"stream started", func(p providers.DeviceProvider) tea.Msg {
			return extStreamStartMsg{provider: p, ch: make(chan []models.ExternalDevice)}
		}},
		{"stream never started", func(p providers.DeviceProvider) tea.Msg {
			return extStreamEndMsg{provider: p}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prov := streamingFake("stream", 0)
			withExternalProviders(t, prov)

			m := newDiscoverModel(context.Background(), externalOpts(), true)
			updated, _ := m.Update(tc.msg(prov))
			m = updated.(discoverModel)

			if !m.hasResults {
				t.Fatal("hasResults = false; want true")
			}
			if !strings.Contains(m.View(), "No devices found yet") {
				t.Errorf("view does not say no devices were found:\n%s", m.View())
			}
		})
	}
}

func TestDiscoverExternal_BLEWarningHiddenWhileStreaming(t *testing.T) {
	prov := streamingFake("stream", 1)
	withExternalProviders(t, prov)

	m := newDiscoverModel(context.Background(), externalOpts(), true)
	// The legacy one-shot Bluetooth scanner is a disabled stub, so this is what
	// the TUI always gets from it today.
	updated, _ := m.Update(btScanMsg{err: errors.New("Bluetooth discovery is currently disabled")})
	m = updated.(discoverModel)
	if m.bleWarningLine() == "" {
		t.Fatal("bleWarningLine is empty with no stream running; want the warning")
	}

	updated, _ = m.Update(extStreamStartMsg{provider: prov, ch: prov.ch})
	m = updated.(discoverModel)
	if got := m.bleWarningLine(); got != "" {
		t.Errorf("bleWarningLine = %q while a stream covers BLE; want empty", got)
	}

	// Once the stream is gone nothing is scanning BLE, so the warning is true
	// again. This is why the stream reports its end instead of going quiet.
	updated, _ = m.Update(extStreamEndMsg{provider: prov})
	m = updated.(discoverModel)
	if m.bleWarningLine() == "" {
		t.Error("bleWarningLine is empty after the stream ended; want the warning back")
	}
}

func TestDiscoverExternalDevices_UsesSameProviderSet(t *testing.T) {
	local := &fakeProvider{key: providers.ProviderKeyDocker, devices: []models.ExternalDevice{extDevice("docker")}}
	remote := &fakeProvider{key: "wendy-lite", devices: []models.ExternalDevice{extDevice("board")}}
	withExternalProviders(t, local, remote)

	if got := discoverExternalDevices(context.Background(), false); len(got) != 1 || got[0].DisplayName != "board" {
		t.Errorf("discoverExternalDevices(includeLocal=false) = %v; want only the non-local provider's device", got)
	}
	if got := discoverExternalDevices(context.Background(), true); len(got) != 2 {
		t.Errorf("discoverExternalDevices(includeLocal=true) = %v; want both providers' devices", got)
	}
}
