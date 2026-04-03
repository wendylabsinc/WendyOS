package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestPickerModel_SelectsFromTable(t *testing.T) {
	m := NewPickerWithTitle("Select a WiFi network")

	updated, _ := m.Update(PickerAddMsg{Items: []PickerItem{
		{Name: "alpha", Type: "82%", Value: "alpha"},
		{Name: "beta", Type: "65%", Value: "beta"},
	}})
	pm := updated.(PickerModel)

	view := pm.View()
	for _, want := range []string{"Select a WiFi network", "Name", "Type", "alpha", "82%"} {
		if !strings.Contains(view, want) {
			t.Fatalf("expected picker view to contain %q, got %q", want, view)
		}
	}

	updated, _ = pm.Update(tea.KeyMsg{Type: tea.KeyDown})
	pm = updated.(PickerModel)

	if pm.table.Cursor() != 1 {
		t.Fatalf("expected cursor on second row, got %d", pm.table.Cursor())
	}

	updated, cmd := pm.Update(tea.KeyMsg{Type: tea.KeyEnter})
	pm = updated.(PickerModel)

	if cmd == nil {
		t.Fatal("expected enter to return quit command")
	}
	if pm.Selected() == nil {
		t.Fatal("expected selected item after enter")
	}
	if got := pm.Selected().Value.(string); got != "beta" {
		t.Fatalf("selected value = %q, want %q", got, "beta")
	}
}

func TestPickerModel_DedupesItems(t *testing.T) {
	m := NewPicker()

	updated, _ := m.Update(PickerAddMsg{Items: []PickerItem{
		{Name: "wendy-alpha", Type: "LAN", Address: "192.168.1.10", Value: "a"},
	}})
	pm := updated.(PickerModel)

	updated, _ = pm.Update(PickerAddMsg{Items: []PickerItem{
		{Name: "wendy-alpha", Type: "LAN", Address: "192.168.1.10", Value: "b"},
	}})
	pm = updated.(PickerModel)

	if got := len(pm.items); got != 1 {
		t.Fatalf("expected 1 deduped item, got %d", got)
	}
}

func TestPickerModel_ShowsDescriptionColumnWhenPresent(t *testing.T) {
	m := NewPickerWithTitle("Select a target")

	updated, _ := m.Update(PickerAddMsg{Items: []PickerItem{
		{Name: "WendyOS", Description: "Full Linux-based edge device", Value: "wendyos"},
	}})
	pm := updated.(PickerModel)

	view := pm.View()
	for _, want := range []string{"Description", "Full Linux-based edge device"} {
		if !strings.Contains(view, want) {
			t.Fatalf("expected picker view to contain %q, got %q", want, view)
		}
	}
}

func TestPickerModel_DefaultKeyShowsStar(t *testing.T) {
	m := NewPickerWithTitle("Select a device")
	m.DefaultKey = "alpha"
	m.OnSetDefault = func(item PickerItem) {}
	m.OnUnsetDefault = func() {}

	updated, _ := m.Update(PickerAddMsg{Items: []PickerItem{
		{Name: "alpha", Type: "LAN", Value: "alpha"},
		{Name: "beta", Type: "LAN", Value: "beta"},
	}})
	pm := updated.(PickerModel)

	view := pm.View()
	if !strings.Contains(view, "★") {
		t.Error("expected ★ indicator for default item")
	}
	if !strings.Contains(view, "d set default") {
		t.Error("expected hint text to contain 'd set default'")
	}
	if !strings.Contains(view, "x unset default") {
		t.Error("expected hint text to contain 'x unset default'")
	}
}

func TestPickerModel_DKeySetsDefault(t *testing.T) {
	m := NewPickerWithTitle("Select a device")
	var setItem PickerItem
	m.OnSetDefault = func(item PickerItem) { setItem = item }
	m.OnUnsetDefault = func() {}

	// Add items.
	updated, _ := m.Update(PickerAddMsg{Items: []PickerItem{
		{Name: "alpha", Type: "LAN", Value: "alpha"},
		{Name: "beta", Type: "LAN", Value: "beta"},
	}})
	pm := updated.(PickerModel)

	// Press 'd' on the first item (cursor starts at 0).
	updated, _ = pm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	pm = updated.(PickerModel)

	if setItem.Name != "alpha" {
		t.Errorf("OnSetDefault called with %q, want alpha", setItem.Name)
	}
	if pm.DefaultKey != "alpha" {
		t.Errorf("DefaultKey = %q, want alpha", pm.DefaultKey)
	}
}

func TestPickerModel_XKeyClearsDefault(t *testing.T) {
	m := NewPickerWithTitle("Select a device")
	m.DefaultKey = "alpha"
	var unsetCalled bool
	m.OnSetDefault = func(item PickerItem) {}
	m.OnUnsetDefault = func() { unsetCalled = true }

	updated, _ := m.Update(PickerAddMsg{Items: []PickerItem{
		{Name: "alpha", Type: "LAN", Value: "alpha"},
	}})
	pm := updated.(PickerModel)

	updated, _ = pm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	pm = updated.(PickerModel)

	if !unsetCalled {
		t.Error("OnUnsetDefault was not called")
	}
	if pm.DefaultKey != "" {
		t.Errorf("DefaultKey = %q, want empty", pm.DefaultKey)
	}
}

func TestPickerModel_DXIgnoredWithoutCallbacks(t *testing.T) {
	m := NewPickerWithTitle("Select")
	// No OnSetDefault/OnUnsetDefault set.

	updated, _ := m.Update(PickerAddMsg{Items: []PickerItem{
		{Name: "alpha", Value: "alpha"},
	}})
	pm := updated.(PickerModel)

	// Press 'd' — should not panic or set anything.
	updated, _ = pm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	pm = updated.(PickerModel)
	if pm.DefaultKey != "" {
		t.Error("DefaultKey should remain empty without callback")
	}

	// View should NOT contain d/x hint.
	view := pm.View()
	if strings.Contains(view, "d set default") {
		t.Error("d/x hint should not appear without callbacks")
	}
}
