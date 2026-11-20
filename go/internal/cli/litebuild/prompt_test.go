package litebuild

import (
	"bytes"
	"strings"
	"testing"
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
