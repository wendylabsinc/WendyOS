package chat

import (
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// Bubble Tea can decode an SGR mouse report split across short TTY reads as
// Escape/Alt+[ followed by text. Reassemble those reports before the composer
// or Escape cancellation sees them. A timeout preserves standalone Escape and
// ordinary Alt+[ input, and bracketed paste is always left untouched.
type chatMouseInput struct {
	pending string
	keys    []tea.KeyMsg
	version uint64
}

type chatMouseTimeout uint64

func (f *chatMouseInput) flush() []tea.Msg {
	var messages []tea.Msg
	for _, key := range f.keys {
		messages = append(messages, key)
	}
	f.pending, f.keys = "", nil
	f.version++
	return messages
}

func (f *chatMouseInput) feed(key tea.KeyMsg) ([]tea.Msg, tea.Cmd) {
	var raw string
	switch {
	case key.Paste:
	case key.Type == tea.KeyEscape && !key.Alt:
		raw = "\x1b"
	case key.Type == tea.KeyRunes:
		raw = string(key.Runes)
		if key.Alt {
			raw = "\x1b" + raw
		}
	}
	if raw == "" || (f.pending == "" && !strings.HasPrefix(raw, "\x1b")) {
		return append(f.flush(), key), nil
	}
	f.pending += raw
	f.keys = append(f.keys, key)
	var messages []tea.Msg
	for f.pending != "" {
		length, button, incomplete := sgrMouseReport(f.pending)
		if incomplete {
			f.version++
			version := f.version
			return messages, tea.Tick(50*time.Millisecond, func(time.Time) tea.Msg { return chatMouseTimeout(version) })
		}
		if length == 0 {
			return append(messages, f.flush()...), nil
		}
		// Only wheel events need recovery; clicks and motion do not edit input.
		if button&64 != 0 && button&128 == 0 {
			event := tea.MouseEvent{Action: tea.MouseActionPress}
			switch button & 3 {
			case 0:
				event.Button, event.Type = tea.MouseButtonWheelUp, tea.MouseWheelUp
			case 1:
				event.Button, event.Type = tea.MouseButtonWheelDown, tea.MouseWheelDown
			case 2:
				event.Button, event.Type = tea.MouseButtonWheelLeft, tea.MouseWheelLeft
			case 3:
				event.Button, event.Type = tea.MouseButtonWheelRight, tea.MouseWheelRight
			}
			messages = append(messages, tea.MouseMsg(event))
		}
		f.pending = f.pending[length:]
		f.keys = nil
		if f.pending != "" {
			f.keys = []tea.KeyMsg{{Type: tea.KeyRunes, Runes: []rune(f.pending)}}
		}
	}
	f.version++
	return messages, nil
}

// Return a complete report's byte length and button code, or whether this is
// still a valid prefix. Bounds prevent malformed input from buffering forever.
func sgrMouseReport(text string) (length, button int, incomplete bool) {
	body := strings.TrimPrefix(text, "\x1b")
	prefix := "[<"
	if len(body) < len(prefix) {
		return 0, 0, strings.HasPrefix(prefix, body)
	}
	if !strings.HasPrefix(body, prefix) {
		return 0, 0, false
	}
	field, digits := 0, 0
	for i := 2; i < len(body); i++ {
		c := body[i]
		switch {
		case c >= '0' && c <= '9':
			digits++
			if digits > 9 {
				return 0, 0, false
			}
			if field == 0 {
				button = button*10 + int(c-'0')
			}
		case c == ';' && digits > 0 && field < 2:
			field++
			digits = 0
		case (c == 'M' || c == 'm') && field == 2 && digits > 0:
			return len(text) - len(body) + i + 1, button, false
		default:
			return 0, 0, false
		}
	}
	return 0, 0, true
}
