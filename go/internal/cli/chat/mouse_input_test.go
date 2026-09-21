package chat

import (
	"fmt"
	"io"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestMouseFragmentsNeverEnterComposerOrCancel(t *testing.T) {
	for _, code := range []int{64, 65, 80, 81} {
		report := fmt.Sprintf("[<%d;42;49M", code)
		for split := 0; split <= len(report); split++ {
			t.Run(fmt.Sprintf("%d/split%d", code, split), func(t *testing.T) {
				m := uiModel(t, nil, &uiExecutor{}, false)
				m.active = true
				m.composer.SetValue("draft")
				m.Update(tea.KeyMsg{Type: tea.KeyEscape})
				for _, fragment := range []string{report[:split], report[split:]} {
					if fragment != "" {
						m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(fragment)})
					}
				}
				if m.composer.Value() != "draft" || m.canceling || m.mouseInput.pending != "" {
					t.Fatalf("mouse corrupted input or canceled work: %q, canceling=%v", m.composer.Value(), m.canceling)
				}
			})
		}
	}
}

func TestMouseOverscrollAndBurst(t *testing.T) {
	m := uiModel(t, nil, &uiExecutor{}, false)
	m.transcript = nil
	m.appendEntry("assistant", "Wendy", strings.Repeat("line\n", 100))
	m.composer.SetValue("draft")
	for _, code := range []int{64, 65} {
		// A split Alt+[ prefix followed by many coalesced wheel reports.
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Alt: true, Runes: []rune{'['}})
		burst := fmt.Sprintf("<%d;42;49M", code) + strings.Repeat(fmt.Sprintf("[<%d;42;49M", code), 200)
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(burst)})
		if code == 64 && m.viewport.YOffset != 0 {
			t.Fatal("wheel did not reach the top")
		}
		if code == 65 && !m.viewport.AtBottom() {
			t.Fatal("wheel did not reach the bottom")
		}
		if m.composer.Value() != "draft" {
			t.Fatalf("overscroll changed draft: %q", m.composer.Value())
		}
	}
}

func TestMouseRecoveryPreservesPasteTypingAndEscape(t *testing.T) {
	m := uiModel(t, nil, &uiExecutor{}, false)
	literal := "[<65;42;49M"
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Paste: true, Runes: []rune(literal)})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(" hello")})
	if m.composer.Value() != literal+" hello" {
		t.Fatalf("paste or typing changed: %q", m.composer.Value())
	}
	m.active = true
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEscape})
	if cmd == nil || m.canceling {
		t.Fatal("Escape must briefly wait for a possible mouse sequence")
	}
	m.Update(chatMouseTimeout(m.mouseInput.version))
	if !m.canceling {
		t.Fatal("standalone Escape no longer cancels")
	}
}

func TestRecoveredMouseScrollsApprovalWithoutChangingDraft(t *testing.T) {
	m := uiModel(t, nil, &uiExecutor{}, false)
	m.composer.SetValue("draft")
	m.approval = &approvalRequest{call: ToolCall{Name: "shell"}, reply: make(chan bool, 1)}
	m.preview.SetContent(strings.Repeat("argument\n", 100))
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Alt: true, Runes: []rune("[<65;42;49M")})
	if m.preview.YOffset == 0 || m.composer.Value() != "draft" || m.approval == nil {
		t.Fatal("recovered wheel did not stay within approval scrolling")
	}
}

// Exercise Bubble Tea's actual byte parser, including the 256-byte read boundary
// hit by trackpad bursts. Unit-level KeyMsgs alone cannot cover that boundary.
func TestMouseRecoveryThroughTerminalParser(t *testing.T) {
	for _, chunkSize := range []int{1, 2, 5, 17, 255, 256, 257} {
		t.Run(fmt.Sprint(chunkSize), func(t *testing.T) {
			m := uiModel(t, nil, &uiExecutor{}, false)
			m.composer.SetValue("draft")
			input := strings.Repeat("\x1b[<65;42;49M", 80) + "\x03"
			reader := &mouseChunkReader{data: input, size: chunkSize}
			program := tea.NewProgram(m, tea.WithInput(reader), tea.WithOutput(io.Discard), tea.WithoutRenderer(), tea.WithoutSignalHandler())
			if _, err := program.Run(); err != nil {
				t.Fatal(err)
			}
			if m.composer.Value() != "draft" {
				t.Fatalf("terminal mouse bytes entered composer: %q", m.composer.Value())
			}
		})
	}
}

type mouseChunkReader struct {
	data string
	size int
}

func (r *mouseChunkReader) Read(p []byte) (int, error) {
	if r.data == "" {
		return 0, io.EOF
	}
	n := copy(p, r.data[:min(len(r.data), r.size)])
	r.data = r.data[n:]
	return n, nil
}

func TestLiteralMouseTextWithoutEscapeReachesComposer(t *testing.T) {
	m := uiModel(t, nil, &uiExecutor{}, false)
	literal := "[<65;42;49M"
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(literal)})
	if m.composer.Value() != literal {
		t.Fatalf("literal input lost: %q", m.composer.Value())
	}
}
