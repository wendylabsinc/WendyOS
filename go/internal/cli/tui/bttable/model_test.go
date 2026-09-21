package bttable

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fakeHandler records dispatched ops and returns canned tea.Cmds so callers can
// drive the Model's async lifecycle manually.
type fakeHandler struct {
	startScanCalls   int
	nextCalls        int
	connectCalls     int
	disconnectCalls  int
	forgetCalls      int
	lastAddress      string
	connectResult    error
	disconnectResult error
	forgetResult     error
	// connectPaired/connectPairedKnown mirror the agent's reported post-connect
	// pairing state (PairedKnown=false simulates an older agent).
	connectPaired      bool
	connectPairedKnown bool
}

func (h *fakeHandler) StartScan() tea.Cmd {
	h.startScanCalls++
	return func() tea.Msg { return ScanResultMsg{} }
}

func (h *fakeHandler) NextScanEvent() tea.Cmd {
	h.nextCalls++
	return func() tea.Msg { return ScanDoneMsg{} }
}

func (h *fakeHandler) Connect(address string) tea.Cmd {
	h.connectCalls++
	h.lastAddress = address
	err := h.connectResult
	paired, pairedKnown := h.connectPaired, h.connectPairedKnown
	return func() tea.Msg {
		return OpResultMsg{Action: ActionConnect, Address: address, Err: err, Paired: paired, PairedKnown: pairedKnown}
	}
}

func (h *fakeHandler) Disconnect(address string) tea.Cmd {
	h.disconnectCalls++
	h.lastAddress = address
	err := h.disconnectResult
	return func() tea.Msg { return OpResultMsg{Action: ActionDisconnect, Address: address, Err: err} }
}

func (h *fakeHandler) Forget(address string) tea.Cmd {
	h.forgetCalls++
	h.lastAddress = address
	err := h.forgetResult
	return func() tea.Msg { return OpResultMsg{Action: ActionForget, Address: address, Err: err} }
}

func sendKey(m Model, k string) Model {
	var msg tea.KeyMsg
	switch k {
	case "up":
		msg = tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		msg = tea.KeyMsg{Type: tea.KeyDown}
	case "enter":
		msg = tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		msg = tea.KeyMsg{Type: tea.KeyEsc}
	default:
		msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
	}
	next, _ := m.Update(msg)
	return next.(Model)
}

// runCmd fires a tea.Cmd synchronously and pipes the resulting msg through the
// Model — used to simulate the async lifecycle in tests.
func runCmd(m Model, cmd tea.Cmd) Model {
	if cmd == nil {
		return m
	}
	msg := cmd()
	next, _ := m.Update(msg)
	return next.(Model)
}

// finishScan replays the model's seeded peripherals as a completed scan so
// tests can leave the initial scanning state without the reconcile step
// pruning their seed data.
func finishScan(m Model) Model {
	m, _ = updateModel(m, ScanResultMsg{Peripherals: m.peripherals})
	m, _ = updateModel(m, ScanDoneMsg{})
	return m
}

func cursorTo(m *Model, address string) {
	for i, p := range m.peripherals {
		if p.Address == address {
			m.table.SetCursor(i)
			return
		}
	}
}

func find(m Model, address string) *Peripheral {
	for i := range m.peripherals {
		if m.peripherals[i].Address == address {
			return &m.peripherals[i]
		}
	}
	return nil
}

func TestScanResultUpsertsDedupsAndSorts(t *testing.T) {
	m := NewModel(nil)
	m, _ = updateModel(m, ScanResultMsg{Peripherals: []Peripheral{
		{Name: "Buds", Address: "BB", Paired: true, RSSI: -60},
		{Name: "Watch", Address: "CC", Connected: true, Paired: true, RSSI: -70},
	}})
	// Same address rediscovered with updated state — must dedup, not append.
	m, _ = updateModel(m, ScanResultMsg{Peripherals: []Peripheral{
		{Name: "Buds", Address: "BB", Paired: true, Connected: true, RSSI: -55},
	}})

	if len(m.peripherals) != 2 {
		t.Fatalf("expected 2 deduped peripherals, got %d", len(m.peripherals))
	}
	// Connected sorts first; Watch and Buds are both connected now, tie broken by
	// RSSI (Buds -55 > Watch -70).
	if m.peripherals[0].Address != "BB" {
		t.Errorf("sort order wrong: want BB first, got %s", m.peripherals[0].Address)
	}
	if buds := find(m, "BB"); buds == nil || !buds.Connected {
		t.Errorf("Buds should have been updated to connected: %+v", buds)
	}
}

func TestScanDoneStopsScanning(t *testing.T) {
	m := NewModel(nil)
	if !m.scanning {
		t.Fatalf("model should start scanning")
	}
	m, _ = updateModel(m, ScanDoneMsg{})
	if m.scanning {
		t.Errorf("scanning should be false after ScanDoneMsg")
	}
}

func TestScanDoneWithErrorFlashes(t *testing.T) {
	m := NewModel(nil)
	m, _ = updateModel(m, ScanDoneMsg{Err: errors.New("boom")})
	if m.scanning {
		t.Errorf("scanning should stop on error")
	}
	if !m.flashIsError || m.flashMessage == "" {
		t.Errorf("expected an error flash, got %q isErr=%v", m.flashMessage, m.flashIsError)
	}
}

func TestEnterConnectsDisconnectedDevice(t *testing.T) {
	m := NewModel([]Peripheral{{Name: "Buds", Address: "BB"}})
	m = finishScan(m)
	m.table.SetCursor(0)
	m = sendKey(m, "enter")
	if !m.done {
		t.Fatalf("no-handler enter should record Result and quit")
	}
	if r := m.Result(); r.Action != ActionConnect || r.Address != "BB" {
		t.Errorf("result = %+v; want ActionConnect BB", r)
	}
}

func TestEnterOnConnectedDeviceFlashesNoAction(t *testing.T) {
	h := &fakeHandler{}
	m := NewModel([]Peripheral{{Name: "Watch", Address: "CC", Connected: true, Paired: true}}).WithHandler(h)
	m = finishScan(m)
	m.table.SetCursor(0)
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	if h.connectCalls != 0 {
		t.Errorf("connect should not dispatch for an already-connected device")
	}
	if m.flashMessage == "" {
		t.Errorf("expected an 'already connected' flash")
	}
}

func TestEnterWhileScanningFlashesNoConnect(t *testing.T) {
	h := &fakeHandler{}
	// NewModel starts in the scanning state; connecting during an active
	// discovery can fail at the HCI level, so enter must be gated.
	m := NewModel([]Peripheral{{Name: "Buds", Address: "BB"}}).WithHandler(h)
	m.table.SetCursor(0)
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	if h.connectCalls != 0 {
		t.Errorf("connect must not dispatch while a scan is running")
	}
	if m.busy {
		t.Errorf("model must not go busy for a gated connect")
	}
	if m.flashMessage == "" {
		t.Errorf("expected a 'scan in progress' flash")
	}
}

func TestEnterInPickerModeWorksWhileScanning(t *testing.T) {
	// The no-handler picker mode never receives a ScanDoneMsg (the caller reads
	// the scan stream itself), so the scanning gate must not apply to it.
	m := NewModel([]Peripheral{{Name: "Buds", Address: "BB"}})
	m.table.SetCursor(0)
	m = sendKey(m, "enter")
	if !m.done {
		t.Fatalf("no-handler enter must record Result and quit even while scanning")
	}
	if r := m.Result(); r.Action != ActionConnect || r.Address != "BB" {
		t.Errorf("result = %+v; want ActionConnect BB", r)
	}
}

func TestDisconnectGuardedToConnected(t *testing.T) {
	// Not connected → flash, no dispatch.
	h := &fakeHandler{}
	m := NewModel([]Peripheral{{Name: "Buds", Address: "BB", Paired: true}}).WithHandler(h)
	m.table.SetCursor(0)
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	m = next.(Model)
	if h.disconnectCalls != 0 {
		t.Errorf("disconnect should not dispatch for a non-connected device")
	}
	if !m.flashIsError {
		t.Errorf("expected an error flash for invalid disconnect")
	}

	// Connected → dispatch.
	h2 := &fakeHandler{}
	m2 := NewModel([]Peripheral{{Name: "Watch", Address: "CC", Connected: true, Paired: true}}).WithHandler(h2)
	m2.table.SetCursor(0)
	next2, cmd := m2.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	m2 = next2.(Model)
	if h2.disconnectCalls != 1 || h2.lastAddress != "CC" {
		t.Errorf("disconnect not dispatched: calls=%d addr=%s", h2.disconnectCalls, h2.lastAddress)
	}
	if !m2.busy {
		t.Errorf("expected busy=true during disconnect")
	}
	_ = cmd
}

func TestForgetGuardedToPaired(t *testing.T) {
	// Not paired → flash, no dispatch.
	h := &fakeHandler{}
	m := NewModel([]Peripheral{{Name: "Stranger", Address: "EE"}}).WithHandler(h)
	m.table.SetCursor(0)
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'f'}})
	m = next.(Model)
	if h.forgetCalls != 0 {
		t.Errorf("forget should not dispatch for an unpaired device")
	}
	if !m.flashIsError {
		t.Errorf("expected an error flash for invalid forget")
	}

	// Paired → dispatch.
	h2 := &fakeHandler{}
	m2 := NewModel([]Peripheral{{Name: "Buds", Address: "BB", Paired: true}}).WithHandler(h2)
	m2.table.SetCursor(0)
	next2, _ := m2.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'f'}})
	m2 = next2.(Model)
	if h2.forgetCalls != 1 || h2.lastAddress != "BB" {
		t.Errorf("forget not dispatched: calls=%d addr=%s", h2.forgetCalls, h2.lastAddress)
	}
}

func TestConnectOptimisticUpdateAndNoAutoRescan(t *testing.T) {
	h := &fakeHandler{}
	m := NewModel([]Peripheral{{Name: "Buds", Address: "BB", Paired: false}}).WithHandler(h)
	m = finishScan(m)
	m.table.SetCursor(0)

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	if !m.busy {
		t.Fatalf("expected busy=true while connect is in-flight")
	}
	if h.connectCalls != 1 {
		t.Fatalf("expected Connect dispatch, got %d", h.connectCalls)
	}

	// Drive the OpResultMsg back through the model.
	m = runCmd(m, cmd)
	if m.busy {
		t.Errorf("busy should reset after OpResultMsg")
	}
	buds := find(m, "BB")
	if buds == nil || !buds.Connected || !buds.Paired || !buds.Trusted {
		t.Errorf("optimistic update should mark connected/paired/trusted: %+v", buds)
	}
	// Divergence from wifi: no auto-rescan after a successful op.
	if h.startScanCalls != 0 {
		t.Errorf("expected NO auto-rescan after op, got %d StartScan calls", h.startScanCalls)
	}
}

func TestConnectFallbackUnpairedIsNotShownAsPaired(t *testing.T) {
	// The agent's pair-failure fallback can connect without pairing; the row
	// must reflect the reported state instead of assuming Paired.
	h := &fakeHandler{connectPairedKnown: true, connectPaired: false}
	m := NewModel([]Peripheral{{Name: "Buds", Address: "BB"}}).WithHandler(h)
	m = finishScan(m)
	m.table.SetCursor(0)

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = runCmd(next.(Model), cmd)

	buds := find(m, "BB")
	if buds == nil || !buds.Connected {
		t.Fatalf("row should be connected: %+v", buds)
	}
	if buds.Paired || buds.Trusted {
		t.Errorf("unpaired fallback connect must not mark paired/trusted: %+v", buds)
	}
	if !strings.Contains(m.flashMessage, "not paired") {
		t.Errorf("flash should mention the unpaired outcome, got %q", m.flashMessage)
	}

	// Forget stays guarded because the row is (correctly) not paired.
	m.table.SetCursor(0)
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'f'}})
	m = next.(Model)
	if h.forgetCalls != 0 {
		t.Errorf("forget should stay guarded for the unpaired row")
	}
}

func TestConnectReportedPairedMarksPaired(t *testing.T) {
	h := &fakeHandler{connectPairedKnown: true, connectPaired: true}
	m := NewModel([]Peripheral{{Name: "Buds", Address: "BB"}}).WithHandler(h)
	m = finishScan(m)
	m.table.SetCursor(0)
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = runCmd(next.(Model), cmd)
	if buds := find(m, "BB"); buds == nil || !buds.Paired || !buds.Trusted || !buds.Connected {
		t.Errorf("reported-paired connect should mark connected/paired/trusted: %+v", buds)
	}
}

func TestDisconnectOptimisticUpdate(t *testing.T) {
	h := &fakeHandler{}
	m := NewModel([]Peripheral{{Name: "Watch", Address: "CC", Connected: true, Paired: true, Trusted: true}}).WithHandler(h)
	m.table.SetCursor(0)
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	m = runCmd(next.(Model), cmd)
	watch := find(m, "CC")
	if watch == nil || watch.Connected {
		t.Errorf("disconnect should clear Connected: %+v", watch)
	}
}

func TestForgetOptimisticUpdateKeepsRow(t *testing.T) {
	h := &fakeHandler{}
	m := NewModel([]Peripheral{{Name: "Buds", Address: "BB", Paired: true, Connected: true, Trusted: true}}).WithHandler(h)
	m.table.SetCursor(0)
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'f'}})
	m = runCmd(next.(Model), cmd)
	buds := find(m, "BB")
	if buds == nil {
		t.Fatalf("forget should keep the row (device may still advertise)")
	}
	if buds.Paired || buds.Connected || buds.Trusted {
		t.Errorf("forget should clear paired/connected/trusted: %+v", buds)
	}
}

func TestOpErrorSurfacesAndSkipsOptimisticUpdate(t *testing.T) {
	h := &fakeHandler{connectResult: errors.New("pair failed")}
	m := NewModel([]Peripheral{{Name: "Buds", Address: "BB"}}).WithHandler(h)
	m = finishScan(m)
	m.table.SetCursor(0)
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = runCmd(next.(Model), cmd)
	if !m.flashIsError {
		t.Errorf("expected error flash on failed connect")
	}
	if buds := find(m, "BB"); buds != nil && buds.Connected {
		t.Errorf("failed connect must not optimistically mark connected")
	}
}

func TestRescanKeyStartsScan(t *testing.T) {
	h := &fakeHandler{}
	m := NewModel(nil).WithHandler(h)
	// Finish the initial scan first.
	m, _ = updateModel(m, ScanDoneMsg{})
	if m.scanning {
		t.Fatalf("precondition: scanning should be false")
	}
	m = sendKey(m, "r")
	if !m.scanning {
		t.Errorf("'r' should set scanning=true")
	}
	if h.startScanCalls != 1 {
		t.Errorf("'r' should call StartScan once, got %d", h.startScanCalls)
	}
}

func TestQuitKeyExits(t *testing.T) {
	m := NewModel(nil)
	m = sendKey(m, "q")
	if !m.done {
		t.Errorf("'q' should quit")
	}
	if m.Result().Action != ActionQuit {
		t.Errorf("result action = %v; want ActionQuit", m.Result().Action)
	}
}

func TestViewRendersTitleFooterAndScanning(t *testing.T) {
	m := NewModel([]Peripheral{{Name: "Buds", Address: "BB"}})
	view := m.View()
	for _, want := range []string{"Bluetooth", "connect", "disconnect", "forget", "rescan", "quit"} {
		if !strings.Contains(view, want) {
			t.Errorf("View missing %q\n%s", want, view)
		}
	}
	if !strings.Contains(strings.ToLower(view), "scanning") {
		t.Errorf("View should show a scanning indicator while scanning\n%s", view)
	}
	m, _ = updateModel(m, ScanDoneMsg{})
	if strings.Contains(strings.ToLower(m.View()), "scanning") {
		t.Errorf("View should not show scanning after ScanDoneMsg")
	}
}

func TestRescanRemovesStalePeripherals(t *testing.T) {
	h := &fakeHandler{}
	m := NewModel(nil).WithHandler(h)

	// Initial scan finds two devices.
	m, _ = updateModel(m, ScanResultMsg{Peripherals: []Peripheral{
		{Name: "A", Address: "AA"},
		{Name: "B", Address: "BB"},
	}})
	m, _ = updateModel(m, ScanDoneMsg{})
	if len(m.peripherals) != 2 {
		t.Fatalf("setup: expected 2 peripherals, got %d", len(m.peripherals))
	}

	// Rescan: only A is still present — B must be reconciled away.
	m = sendKey(m, "r")
	m, _ = updateModel(m, ScanResultMsg{Peripherals: []Peripheral{{Name: "A", Address: "AA"}}})
	m, _ = updateModel(m, ScanDoneMsg{})
	if len(m.peripherals) != 1 || m.peripherals[0].Address != "AA" {
		t.Fatalf("stale peripheral B not removed: %+v", m.peripherals)
	}

	// A rescan that returns no ScanResultMsg before ScanDoneMsg clears the table
	// (the Linux agent omits the batch entirely when nothing is found).
	m = sendKey(m, "r")
	m, _ = updateModel(m, ScanDoneMsg{})
	if len(m.peripherals) != 0 {
		t.Fatalf("expected empty table after an empty rescan, got %d", len(m.peripherals))
	}
}

func TestFailedRescanKeepsExistingPeripherals(t *testing.T) {
	h := &fakeHandler{}
	m := NewModel(nil).WithHandler(h)
	m, _ = updateModel(m, ScanResultMsg{Peripherals: []Peripheral{{Name: "A", Address: "AA"}}})
	m, _ = updateModel(m, ScanDoneMsg{})

	// A rescan that fails must not wipe the previously-known devices.
	m = sendKey(m, "r")
	m, _ = updateModel(m, ScanDoneMsg{Err: errors.New("scan boom")})
	if len(m.peripherals) != 1 {
		t.Fatalf("failed rescan should preserve existing peripherals, got %d", len(m.peripherals))
	}
	if !m.flashIsError {
		t.Errorf("expected an error flash on failed scan")
	}
}

func TestStaleFlashClearIgnored(t *testing.T) {
	m := NewModel(nil)
	m.setFlash("A", false)
	tokenA := m.flashToken
	m.setFlash("B", true)

	// A clear stamped with the older token must not wipe the newer flash.
	next, _ := m.Update(flashClearMsg{token: tokenA})
	m = next.(Model)
	if m.flashMessage != "B" {
		t.Errorf("stale clear wiped a newer flash: %q", m.flashMessage)
	}

	// The current token's clear does wipe it.
	next2, _ := m.Update(flashClearMsg{token: m.flashToken})
	m = next2.(Model)
	if m.flashMessage != "" {
		t.Errorf("matching clear should wipe the flash, got %q", m.flashMessage)
	}
}

func TestUserFacingErrorStripsGRPCFraming(t *testing.T) {
	err := status.Error(codes.Internal, "failed to connect bluetooth peripheral: boom")
	got := userFacingError(err)
	if got != "failed to connect bluetooth peripheral: boom" {
		t.Errorf("userFacingError = %q", got)
	}
	if strings.Contains(got, "rpc error") {
		t.Errorf("grpc framing leaked into UI string: %q", got)
	}
	if userFacingError(errors.New("plain")) != "plain" {
		t.Errorf("non-grpc error should fall back to Error()")
	}
}

func TestOpErrorFlashIsSanitized(t *testing.T) {
	h := &fakeHandler{connectResult: status.Error(codes.Internal, "pair failed")}
	m := NewModel([]Peripheral{{Name: "Buds", Address: "BB"}}).WithHandler(h)
	m = finishScan(m)
	m.table.SetCursor(0)
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = runCmd(next.(Model), cmd)
	if strings.Contains(m.flashMessage, "rpc error") {
		t.Errorf("op error flash should be sanitized, got %q", m.flashMessage)
	}
	if !strings.Contains(m.flashMessage, "pair failed") {
		t.Errorf("sanitized flash should keep the useful message, got %q", m.flashMessage)
	}
}

// updateModel is a thin wrapper that applies a message and returns the concrete
// Model plus the command, keeping the test bodies terse.
func updateModel(m Model, msg tea.Msg) (Model, tea.Cmd) {
	next, cmd := m.Update(msg)
	return next.(Model), cmd
}

func TestUnnamedDevicesToggleAndActionSelection(t *testing.T) {
	h := &fakeHandler{}
	m := finishScan(NewModel([]Peripheral{
		{Address: "AA", Paired: true, Connected: true},
		{Name: "Headphones", Address: "BB"},
		{Name: "  ", Address: "CC"},
	}).WithHandler(h))
	if len(m.visible) != 1 || strings.Contains(m.View(), "AA") || !strings.Contains(m.View(), "2 hidden") {
		t.Fatalf("unnamed devices should be hidden by default: %s", m.View())
	}
	m, cmd := updateModel(m, tea.KeyMsg{Type: tea.KeyEnter})
	if h.lastAddress != "BB" {
		t.Fatalf("visible row dispatched action to hidden peripheral: %q", h.lastAddress)
	}
	m = runCmd(m, cmd)
	m = sendKey(m, "h")
	if len(m.visible) != 3 || !strings.Contains(m.View(), "h hide unnamed") {
		t.Fatalf("toggle did not reveal unnamed devices: %s", m.View())
	}
	for i, p := range m.visible {
		if p.Address == "AA" {
			m.table.SetCursor(i)
		}
	}
	m, _ = updateModel(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'f'}})
	if h.lastAddress != "AA" || h.forgetCalls != 1 {
		t.Fatal("revealed device could not be forgotten")
	}
	m = sendKey(m, "h")
	if p, ok := m.selected(); !ok || p.Address != "BB" {
		t.Fatal("hiding the selected unnamed device did not select a visible device")
	}
}

func TestOnlyUnnamedDevicesEmptyStateAndDiscoveryRename(t *testing.T) {
	m := finishScan(NewModel([]Peripheral{{Address: "AA"}}))
	if _, ok := m.selected(); ok || !strings.Contains(m.View(), "Press h") {
		t.Fatal("hidden-only table should show a toggle hint with no selection")
	}
	m, _ = updateModel(m, ScanResultMsg{Peripherals: []Peripheral{{Address: "AA", Name: "Keyboard"}}})
	if p, ok := m.selected(); !ok || p.Name != "Keyboard" {
		t.Fatal("newly discovered name did not make the device visible")
	}
}

func TestRemotePeripheralControlsAreSanitizedWithoutChangingIdentity(t *testing.T) {
	rawAddress := "AA\x1b[2J\r\u202e"
	h := &fakeHandler{}
	m := finishScan(NewModel([]Peripheral{{Name: "\r\n\u202e", Address: rawAddress}}).WithHandler(h))
	if len(m.visible) != 0 {
		t.Fatal("control-only name was not hidden")
	}
	m, _ = updateModel(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("h")})
	m, _ = updateModel(m, tea.KeyMsg{Type: tea.KeyEnter})
	if h.lastAddress != rawAddress {
		t.Fatal("display sanitization changed action identity")
	}
	for _, rendered := range []string{m.View(), m.flashMessage, displayName(Peripheral{Name: "Buds\x1b]52;c;secret\a\n", Address: rawAddress})} {
		for _, control := range []string{"\x1b[2J", "\x1b]", "\r", "\a", "\u202e"} {
			if strings.Contains(rendered, control) {
				t.Fatalf("remote control survived: %q", rendered)
			}
		}
	}
}
