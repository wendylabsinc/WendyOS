package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func updateChecklist(m ChecklistModel, msg tea.Msg) ChecklistModel {
	result, _ := m.Update(msg)
	return result.(ChecklistModel)
}

func key(s string) tea.KeyMsg {
	if len(s) == 1 {
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}
	switch s {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "ctrl+c":
		return tea.KeyMsg{Type: tea.KeyCtrlC}
	default:
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}
}

func TestChecklistModel_ToggleItem(t *testing.T) {
	m := NewChecklist("Test", []ChecklistItem{
		{Label: "A", Value: "a"},
		{Label: "B", Value: "b"},
	})

	m = updateChecklist(m, key("j")) // move to first item
	m = updateChecklist(m, key(" ")) // toggle on

	if !m.items[0].Selected {
		t.Fatal("expected first item to be selected after toggle")
	}
	if m.items[1].Selected {
		t.Fatal("expected second item to remain unselected")
	}
}

func TestChecklistModel_SelectAll(t *testing.T) {
	m := NewChecklist("Test", []ChecklistItem{
		{Label: "A", Value: "a"},
		{Label: "B", Value: "b"},
	})

	// Cursor at 0 = select-all row. Toggle on.
	m = updateChecklist(m, key(" "))

	for i, item := range m.items {
		if !item.Selected {
			t.Fatalf("item[%d] should be selected after select-all", i)
		}
	}

	// Toggle off.
	m = updateChecklist(m, key(" "))
	for i, item := range m.items {
		if item.Selected {
			t.Fatalf("item[%d] should be deselected after select-all toggle off", i)
		}
	}
}

func TestChecklistModel_YNKeys(t *testing.T) {
	m := NewChecklist("Test", []ChecklistItem{
		{Label: "A", Value: "a"},
	})

	m = updateChecklist(m, key("j")) // move to item
	m = updateChecklist(m, key("y"))
	if !m.items[0].Selected {
		t.Fatal("expected item selected after 'y'")
	}
	m = updateChecklist(m, key("n"))
	if m.items[0].Selected {
		t.Fatal("expected item deselected after 'n'")
	}
}

func TestChecklistModel_Cancellation(t *testing.T) {
	m := NewChecklist("Test", []ChecklistItem{
		{Label: "A", Value: "a"},
	})

	m = updateChecklist(m, key("ctrl+c"))
	if !m.Cancelled() {
		t.Fatal("expected Cancelled() after ctrl+c")
	}
}

func TestChecklistModel_EmptyItems(t *testing.T) {
	m := NewChecklist("Test", nil)

	// Should not panic on toggle with empty items.
	m = updateChecklist(m, key(" "))
	m = updateChecklist(m, key("y"))
	m = updateChecklist(m, key("n"))

	if len(m.SelectedItems()) != 0 {
		t.Fatal("expected no selected items")
	}
}

func TestChecklistModel_EnterConfirms(t *testing.T) {
	m := NewChecklist("Test", []ChecklistItem{
		{Label: "A", Value: "a", Selected: true},
		{Label: "B", Value: "b"},
	})

	result, cmd := m.Update(key("enter"))
	model := result.(ChecklistModel)
	if cmd == nil {
		t.Fatal("expected quit command on enter")
	}
	selected := model.SelectedItems()
	if len(selected) != 1 || selected[0].Value != "a" {
		t.Fatalf("expected [a], got %+v", selected)
	}
}
