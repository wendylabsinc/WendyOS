package wifitable

import (
	"errors"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	agentpb "github.com/wendylabsinc/wendy/proto/gen/agentpb"
)

// fakeHandler records dispatched ops and never blocks — callers drive the
// resulting tea.Cmds manually to assert the Model's follow-up behavior.
type fakeHandler struct {
	connectCalls  int
	forgetCalls   int
	reorderCalls  int
	refreshCalls  int
	lastSSID      string
	lastPassword  string
	lastSecurity  agentpb.WiFiSecurityType
	lastHidden    bool
	lastOrder     []string
	connectResult error
	forgetResult  error
	reorderResult error
}

func (h *fakeHandler) Connect(ssid, password string, sec agentpb.WiFiSecurityType, hidden bool) tea.Cmd {
	h.connectCalls++
	h.lastSSID = ssid
	h.lastPassword = password
	h.lastSecurity = sec
	h.lastHidden = hidden
	err := h.connectResult
	action := ActionConnect
	if hidden {
		action = ActionConnectUnlisted
	}
	return func() tea.Msg { return OpResultMsg{Action: action, SSID: ssid, Err: err} }
}

func (h *fakeHandler) Forget(ssid string) tea.Cmd {
	h.forgetCalls++
	h.lastSSID = ssid
	err := h.forgetResult
	return func() tea.Msg { return OpResultMsg{Action: ActionForget, SSID: ssid, Err: err} }
}

func (h *fakeHandler) Reorder(order []string) tea.Cmd {
	h.reorderCalls++
	h.lastOrder = order
	err := h.reorderResult
	return func() tea.Msg { return OpResultMsg{Action: ActionReorder, Count: len(order), Err: err} }
}

func (h *fakeHandler) Refresh() tea.Cmd {
	h.refreshCalls++
	return func() tea.Msg { return RefreshMsg{} }
}

func sendKey(m Model, k string) Model {
	// Map our canonical strings back to tea.KeyMsg. bubbletea parses these
	// internally, so we construct them directly via tea.KeyMsg.
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
	case "r":
		msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}}
	case "f":
		msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'f'}}
	case "n":
		msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}}
	case "q":
		msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}}
	default:
		msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
	}
	next, _ := m.Update(msg)
	return next.(Model)
}

func TestRankModeReordersAndCommits(t *testing.T) {
	networks := []Network{
		{SSID: "Alpha", Known: true, Priority: 10},
		{SSID: "Bravo", Known: true, Priority: 5},
		{SSID: "Charlie", Known: true, Priority: 1},
		{SSID: "Delta", Signal: 60},
	}
	m := NewModel(networks)

	// Enter rank mode.
	m = sendKey(m, "r")
	if m.mode != modeRanking {
		t.Fatalf("mode = %v; want modeRanking", m.mode)
	}

	// Cursor starts at 0 (Alpha). Move Bravo up by moving cursor down first, then up.
	m.table.SetCursor(1)
	m = sendKey(m, "up")
	if m.networks[0].SSID != "Bravo" || m.networks[1].SSID != "Alpha" {
		t.Fatalf("after MoveUp, want [Bravo Alpha ...], got %v", ssids(m.networks))
	}

	// Commit with enter.
	m = sendKey(m, "enter")
	if !m.done {
		t.Fatalf("expected done=true after enter in rank mode")
	}
	res := m.Result()
	if res.Action != ActionReorder {
		t.Errorf("action = %v; want ActionReorder", res.Action)
	}
	want := []string{"Bravo", "Alpha", "Charlie"}
	if got := res.Order; !equalStrings(got, want) {
		t.Errorf("order = %v; want %v", got, want)
	}
}

func TestRankModeEscRestoresOrder(t *testing.T) {
	networks := []Network{
		{SSID: "Alpha", Known: true, Priority: 10},
		{SSID: "Bravo", Known: true, Priority: 5},
	}
	m := NewModel(networks)
	m = sendKey(m, "r")
	m.table.SetCursor(1)
	m = sendKey(m, "up")
	// Bravo should now be at position 0 after the swap.
	if m.networks[0].SSID != "Bravo" {
		t.Fatalf("pre-esc order unexpected: %v", ssids(m.networks))
	}

	m = sendKey(m, "esc")
	if m.mode != modeBrowsing {
		t.Fatalf("mode = %v; want modeBrowsing after esc", m.mode)
	}
	if m.networks[0].SSID != "Alpha" || m.networks[1].SSID != "Bravo" {
		t.Errorf("order after esc = %v; want [Alpha Bravo]", ssids(m.networks))
	}
}

func TestForgetExitsWithSSID(t *testing.T) {
	networks := []Network{
		{SSID: "SavedOne", Known: true, Priority: 1},
	}
	m := NewModel(networks)
	m.table.SetCursor(0)
	m = sendKey(m, "f")
	if !m.done {
		t.Fatalf("expected done=true after forget")
	}
	if r := m.Result(); r.Action != ActionForget || r.SSID != "SavedOne" {
		t.Errorf("result = %+v", r)
	}
}

// runCmd fires a tea.Cmd synchronously and pipes the resulting msg through
// the Model — used to simulate the async lifecycle in tests.
func runCmd(m Model, cmd tea.Cmd) Model {
	if cmd == nil {
		return m
	}
	msg := cmd()
	next, _ := m.Update(msg)
	return next.(Model)
}

func TestForgetWithHandlerStaysOpenAndRefreshes(t *testing.T) {
	networks := []Network{
		{SSID: "Home", Known: true, Priority: 1},
		{SSID: "Cafe", Signal: 60},
	}
	h := &fakeHandler{}
	m := NewModel(networks).WithHandler(h)
	m.table.SetCursor(0)

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'f'}})
	m = next.(Model)

	if m.done {
		t.Fatalf("menu should stay open after forget; got done=true")
	}
	if h.forgetCalls != 1 || h.lastSSID != "Home" {
		t.Fatalf("forget not dispatched correctly: calls=%d ssid=%s", h.forgetCalls, h.lastSSID)
	}
	if cmd == nil {
		t.Fatalf("expected a tea.Cmd from forget dispatch")
	}
	if !m.busy {
		t.Fatalf("expected busy=true while op is in-flight")
	}

	// Drive the async result through the Model — Refresh should be triggered.
	m = runCmd(m, cmd)
	if m.busy {
		t.Errorf("busy should reset after OpResultMsg")
	}
	if h.refreshCalls != 1 {
		t.Errorf("expected Refresh() to be invoked after successful op, got %d calls", h.refreshCalls)
	}
	if m.flashMessage == "" {
		t.Errorf("expected a flash message after forget")
	}
}

func TestForgetErrorSurfacesAndSkipsRefresh(t *testing.T) {
	networks := []Network{{SSID: "Home", Known: true, Priority: 1}}
	h := &fakeHandler{forgetResult: errors.New("nmcli boom")}
	m := NewModel(networks).WithHandler(h)
	m.table.SetCursor(0)

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'f'}})
	m = runCmd(next.(Model), cmd)

	if m.done {
		t.Fatalf("menu should stay open on error too")
	}
	if !m.flashIsError {
		t.Errorf("expected flashIsError=true on failed forget")
	}
	if h.refreshCalls != 0 {
		t.Errorf("Refresh should not run on failure, got %d calls", h.refreshCalls)
	}
}

func TestRankCommitWithHandlerStaysOpen(t *testing.T) {
	networks := []Network{
		{SSID: "Alpha", Known: true, Priority: 10},
		{SSID: "Bravo", Known: true, Priority: 5},
	}
	h := &fakeHandler{}
	m := NewModel(networks).WithHandler(h)

	m = sendKey(m, "r")
	if m.mode != modeRanking {
		t.Fatalf("mode = %v; want modeRanking", m.mode)
	}
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)

	if m.done {
		t.Fatalf("menu should stay open after rank commit")
	}
	if m.mode != modeBrowsing {
		t.Errorf("expected to return to browsing after rank commit, got mode=%v", m.mode)
	}
	if h.reorderCalls != 1 {
		t.Errorf("expected 1 Reorder call, got %d", h.reorderCalls)
	}
	m = runCmd(m, cmd)
	if h.refreshCalls != 1 {
		t.Errorf("expected Refresh() after successful reorder, got %d", h.refreshCalls)
	}
}

func TestUnknownUnspecifiedSecurityPromptsForPassword(t *testing.T) {
	// Regression: after forgetting a network with an ambiguous (UNSPECIFIED)
	// security type, pressing enter used to connect without a password prompt.
	// Unknown + non-OPEN security must always prompt.
	networks := []Network{
		{SSID: "wendy", Signal: 90, Known: false, Security: agentpb.WiFiSecurityType_WIFI_SECURITY_TYPE_UNSPECIFIED},
	}
	h := &fakeHandler{}
	m := NewModel(networks).WithHandler(h)
	m.table.SetCursor(0)

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)

	if m.mode != modePassword {
		t.Fatalf("expected modePassword after enter on unknown UNSPECIFIED network, got %v", m.mode)
	}
	if h.connectCalls != 0 {
		t.Errorf("Connect should NOT be dispatched before password is supplied; got %d calls", h.connectCalls)
	}
}

func TestUnknownOpenNetworkConnectsWithoutPrompt(t *testing.T) {
	networks := []Network{
		{SSID: "CoffeeShop", Signal: 70, Known: false, Security: agentpb.WiFiSecurityType_WIFI_SECURITY_TYPE_OPEN},
	}
	h := &fakeHandler{}
	m := NewModel(networks).WithHandler(h)
	m.table.SetCursor(0)

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)

	if m.mode == modePassword {
		t.Fatalf("explicitly-OPEN network should not prompt for a password")
	}
	if h.connectCalls != 1 {
		t.Errorf("expected direct Connect on OPEN network, got %d calls", h.connectCalls)
	}
}

func TestRefreshDoesNotClobberRecentConnect(t *testing.T) {
	// Regression: nmcli's `device wifi list` can return the just-connected
	// SSID without IsConnected/IsKnown set while the rescan catches up.
	// reconcileRefresh must preserve the optimistic flags until a refresh
	// confirms the connect.
	networks := []Network{
		{SSID: "wendy", Signal: 90, Known: false, Security: agentpb.WiFiSecurityType_WIFI_SECURITY_TYPE_WPA2_PSK},
	}
	h := &fakeHandler{}
	m := NewModel(networks).WithHandler(h)
	m.table.SetCursor(0)

	// Enter password mode and submit a password.
	m = sendKey(m, "enter") // unknown + WPA2 → modePassword
	if m.mode != modePassword {
		t.Fatalf("expected modePassword, got %v", m.mode)
	}
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = runCmd(next.(Model), cmd) // OpResultMsg → optimistic update + refresh cmd

	// Optimistic update should have marked wendy.
	var wendy *Network
	for i := range m.networks {
		if m.networks[i].SSID == "wendy" {
			wendy = &m.networks[i]
		}
	}
	if wendy == nil || !wendy.Connected || !wendy.Known {
		t.Fatalf("optimistic update did not mark wendy connected/known: %+v", wendy)
	}
	if m.stickyConnectedSSID != "wendy" {
		t.Errorf("expected stickyConnectedSSID=wendy, got %q", m.stickyConnectedSSID)
	}

	// Simulate a stale refresh (nmcli hasn't caught up yet).
	staleRefresh := RefreshMsg{Networks: []Network{
		{SSID: "wendy", Signal: 85, Known: false, Connected: false},
		{SSID: "Neighbor", Signal: 50, Known: false, Connected: false},
	}}
	next2, _ := m.Update(staleRefresh)
	m = next2.(Model)

	wendy = nil
	for i := range m.networks {
		if m.networks[i].SSID == "wendy" {
			wendy = &m.networks[i]
		}
	}
	if wendy == nil {
		t.Fatalf("wendy disappeared after refresh")
	}
	if !wendy.Connected || !wendy.Known {
		t.Errorf("stale refresh clobbered optimistic state: %+v", *wendy)
	}
	if m.stickyConnectedSSID != "wendy" {
		t.Errorf("sticky SSID should still be set while unconfirmed, got %q", m.stickyConnectedSSID)
	}

	// Now a confirming refresh arrives. Sticky should clear.
	confirmingRefresh := RefreshMsg{Networks: []Network{
		{SSID: "wendy", Signal: 85, Known: true, Connected: true},
	}}
	next3, _ := m.Update(confirmingRefresh)
	m = next3.(Model)
	if m.stickyConnectedSSID != "" {
		t.Errorf("sticky SSID should clear once confirmed, got %q", m.stickyConnectedSSID)
	}
}

func TestRefreshWithoutStickyPassesThrough(t *testing.T) {
	networks := []Network{{SSID: "Home", Known: true, Priority: 1}}
	m := NewModel(networks)

	incoming := RefreshMsg{Networks: []Network{
		{SSID: "Cafe", Signal: 40, Known: false, Connected: false},
	}}
	next, _ := m.Update(incoming)
	m = next.(Model)
	if len(m.networks) != 1 || m.networks[0].SSID != "Cafe" {
		t.Errorf("refresh without sticky should replace networks wholesale; got %v", ssids(m.networks))
	}
}

func TestConnectOptimisticUpdateMarksRowConnected(t *testing.T) {
	// Simulates the reported bug: pressing enter on a known network didn't
	// update the table because the async refresh lagged behind nmcli's
	// rescan. The optimistic update should flip Known/Connected immediately.
	networks := []Network{
		{SSID: "wendy", Signal: 90, Known: true},
		{SSID: "Neighbor", Signal: 60, Known: true, Connected: true},
	}
	h := &fakeHandler{}
	m := NewModel(networks).WithHandler(h)
	m.table.SetCursor(0) // cursor on wendy (after sort Neighbor may come first)

	// Find the index of wendy to put the cursor there.
	for i, n := range m.networks {
		if n.SSID == "wendy" {
			m.table.SetCursor(i)
			break
		}
	}

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = runCmd(next.(Model), cmd)

	var wendy *Network
	for i := range m.networks {
		if m.networks[i].SSID == "wendy" {
			wendy = &m.networks[i]
		}
	}
	if wendy == nil {
		t.Fatalf("wendy row missing after connect")
	}
	if !wendy.Connected {
		t.Errorf("wendy should be marked Connected immediately, got %+v", *wendy)
	}
	if !wendy.Known {
		t.Errorf("wendy should be marked Known immediately, got %+v", *wendy)
	}
	// The Neighbor row should have Connected=false (any previously-connected
	// row gets reset).
	for _, n := range m.networks {
		if n.SSID == "Neighbor" && n.Connected {
			t.Errorf("Neighbor should not be Connected after connecting to wendy")
		}
	}
}

func TestForgetOptimisticUpdateClearsKnown(t *testing.T) {
	networks := []Network{
		{SSID: "Home", Known: true, Priority: 5, Signal: 80},
		{SSID: "OldNet", Known: true, Priority: 2}, // saved but not visible
	}
	h := &fakeHandler{}
	m := NewModel(networks).WithHandler(h)

	// Forget "Home" (visible) — should become unknown, not dropped.
	for i, n := range m.networks {
		if n.SSID == "Home" {
			m.table.SetCursor(i)
			break
		}
	}
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'f'}})
	m = runCmd(next.(Model), cmd)

	var home *Network
	for i := range m.networks {
		if m.networks[i].SSID == "Home" {
			home = &m.networks[i]
		}
	}
	if home == nil {
		t.Fatalf("Home row should still be present (visible in scan)")
	}
	if home.Known {
		t.Errorf("Home should be marked as unknown after forget")
	}

	// Forget "OldNet" (invisible) — row should be dropped entirely.
	for i, n := range m.networks {
		if n.SSID == "OldNet" {
			m.table.SetCursor(i)
			break
		}
	}
	next, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'f'}})
	m = runCmd(next.(Model), cmd)

	for _, n := range m.networks {
		if n.SSID == "OldNet" {
			t.Errorf("OldNet should be removed from the list (not visible, no longer saved)")
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
