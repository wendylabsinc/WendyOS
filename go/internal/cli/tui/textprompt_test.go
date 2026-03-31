package tui

import (
	"fmt"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func updateTextPrompt(m TextPromptModel, msg tea.Msg) TextPromptModel {
	result, _ := m.Update(msg)
	return result.(TextPromptModel)
}

func tkey(s string) tea.KeyMsg {
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

func TestTextPromptModel_SubmitTrimmed(t *testing.T) {
	m := NewTextPrompt("Name", "", "", nil)
	for _, r := range " hello " {
		m = updateTextPrompt(m, tkey(string(r)))
	}
	m = updateTextPrompt(m, tkey("enter"))
	if m.Value() != "hello" {
		t.Fatalf("expected trimmed value %q, got %q", "hello", m.Value())
	}
}

func TestTextPromptModel_ValidationError(t *testing.T) {
	m := NewTextPrompt("Port", "", "", func(v string) error {
		if v == "" {
			return fmt.Errorf("port is required")
		}
		return nil
	})

	m = updateTextPrompt(m, tkey("enter"))
	if m.err == "" {
		t.Fatal("expected validation error on empty submit")
	}
	if m.done {
		t.Fatal("model should not be done after validation error")
	}
}

func TestTextPromptModel_ValidationErrorClears(t *testing.T) {
	m := NewTextPrompt("Port", "", "", func(v string) error {
		if v == "" {
			return fmt.Errorf("required")
		}
		return nil
	})

	m = updateTextPrompt(m, tkey("enter"))
	if m.err == "" {
		t.Fatal("expected error")
	}

	// Type a character — error should clear.
	m = updateTextPrompt(m, tkey("x"))
	if m.err != "" {
		t.Fatalf("expected error to clear after typing, got %q", m.err)
	}
}

func TestTextPromptModel_DefaultValue(t *testing.T) {
	m := NewTextPrompt("Path", "", "/data", nil)
	if m.input.Value() != "/data" {
		t.Fatalf("expected default value %q, got %q", "/data", m.input.Value())
	}

	m = updateTextPrompt(m, tkey("enter"))
	if m.Value() != "/data" {
		t.Fatalf("expected %q, got %q", "/data", m.Value())
	}
}

func TestTextPromptModel_Cancellation(t *testing.T) {
	m := NewTextPrompt("Name", "", "", nil)
	m = updateTextPrompt(m, tkey("ctrl+c"))
	if !m.Cancelled() {
		t.Fatal("expected Cancelled() after ctrl+c")
	}
}
