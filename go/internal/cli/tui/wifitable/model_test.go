package wifitable

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

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
