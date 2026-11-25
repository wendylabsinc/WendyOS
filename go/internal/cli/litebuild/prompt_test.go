package litebuild

import (
	"bytes"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// TestTUIPrompter drives the CLI prompts with scripted key input.
func TestTUIPrompter(t *testing.T) {
	m, err := Load([]byte(`
attributes:
  board: {type: string, enum: [a, b, c]}
  net: {type: boolean}
  psram: {type: size}
display:
  board: {title: Board, values: {b: Board B}}
  net: {title: Wendy Net}
`), "test")
	if err != nil {
		t.Fatal(err)
	}
	board, net, psram := m.Attributes["board"], m.Attributes["net"], m.Attributes["psram"]

	t.Run("select default", func(t *testing.T) {
		var out bytes.Buffer
		p := &TUIPrompter{In: strings.NewReader("\r"), Out: &out}
		i, err := p.Select(Question{Attr: board, Options: board.Enum, Default: "b"})
		if err != nil || i != 1 {
			t.Fatalf("Select = %d, %v, want 1 (the default)", i, err)
		}
		if !strings.Contains(out.String(), "Board: Board B") {
			t.Errorf("output %q does not show the answer", out.String())
		}
	})
	t.Run("confirm", func(t *testing.T) {
		var out bytes.Buffer
		p := &TUIPrompter{In: strings.NewReader("y"), Out: &out}
		got, err := p.Confirm(Question{Attr: net, Options: []any{false, true}})
		if err != nil || !got {
			t.Fatalf("Confirm = %v, %v, want true", got, err)
		}
	})
	t.Run("select warning", func(t *testing.T) {
		q := Question{Attr: board, Options: board.Enum, Default: "c",
			Warnings: [][]Warning{nil, nil, {{ID: "w1", Message: "C is slow."}}}}
		// The highlighted option shows its warning under the list.
		view := ansi.Strip(selectPicker(q).View())
		if !strings.Contains(view, "c ⚠") || !strings.Contains(view, "⚠ C is slow.") {
			t.Errorf("view %q does not show the warning of c", view)
		}
		q.Default = "a"
		if view := ansi.Strip(selectPicker(q).View()); !strings.Contains(view, "c ⚠") || strings.Contains(view, "C is slow.") {
			t.Errorf("view %q: want the ⚠ of c, and its warning only when c is highlighted", view)
		}
		var out bytes.Buffer
		p := &TUIPrompter{In: strings.NewReader("\r"), Out: &out}
		if i, err := p.Select(q); err != nil || i != 0 {
			t.Fatalf("Select = %d, %v, want 0 (the default)", i, err)
		}
	})
	t.Run("confirm warning", func(t *testing.T) {
		var out bytes.Buffer
		p := &TUIPrompter{In: strings.NewReader("n"), Out: &out}
		q := Question{Attr: net, Options: []any{false, true},
			Warnings: [][]Warning{nil, {{ID: "w2", Message: "Wendy Net is slow."}}}}
		if _, err := p.Confirm(q); err != nil {
			t.Fatal(err)
		}
		got := ansi.Strip(out.String())
		warning := strings.Index(got, "⚠ If you answer Yes: Wendy Net is slow.")
		if warning < 0 || warning > strings.Index(got, "✓ Wendy Net: no") {
			t.Errorf("output %q does not show the warning of Yes before the question", got)
		}
	})
	t.Run("warn", func(t *testing.T) {
		var out bytes.Buffer
		p := &TUIPrompter{Out: &out}
		p.Warn(Warning{ID: "w1", Message: "C is slow."})
		if got := ansi.Strip(out.String()); got != "⚠ C is slow.\n" {
			t.Errorf("output %q, want the warning line", got)
		}
	})
	t.Run("text", func(t *testing.T) {
		var out bytes.Buffer
		p := &TUIPrompter{In: strings.NewReader("8MiB\r"), Out: &out}
		got, err := p.Text(Question{Attr: psram}, func(s string) error {
			_, err := psram.Normalize(s)
			return err
		})
		if err != nil || got != "8MiB" {
			t.Fatalf("Text = %q, %v, want 8MiB", got, err)
		}
	})
}
