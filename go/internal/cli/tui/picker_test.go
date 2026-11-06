package tui

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	bubbleTable "github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
)

func TestPickerFailedDefaultSavePreservesMarker(t *testing.T) {
	for _, defaultKey := range []string{"previous", "new"} {
		// "previous" exercises a failed set, "new" a failed clear.
		m := NewPicker()
		m.DefaultKey = defaultKey
		m.OnSetDefault = func(PickerItem) (string, error) { return "", errors.New("cannot save config") }
		m.OnUnsetDefault = func() (string, error) { return "", errors.New("cannot save config") }
		updated, _ := m.Update(PickerAddMsg{Items: []PickerItem{{Name: "new", DedupKey: "new"}}})
		m = updated.(PickerModel)
		updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
		m = updated.(PickerModel)
		if m.DefaultKey != defaultKey || !m.flashIsError || m.flashMessage != "cannot save config" {
			t.Fatalf("failed save changed UI state: key=%q error=%t message=%q", m.DefaultKey, m.flashIsError, m.flashMessage)
		}
	}
}

func TestPickerWrappedFlashResizesTableAfterActions(t *testing.T) {
	message := strings.Repeat("Follow the setup instructions and keep the terminal open. ", 4) + "Use WSL2 for managed robots."
	for _, action := range []string{"copy", "create", "enroll", "start", "stop"} {
		t.Run(action, func(t *testing.T) {
			m := NewPickerWithTitle("Simulators")
			m.OnCopyItem = func(PickerItem) string { return message }
			m.OnCreateItem = func() (string, bool) { return message, false }
			m.OnEnrollItem = func(PickerItem) (string, bool) { return message, false }
			m.OnStartItem = func(PickerItem) (string, bool, bool) { return message, false, false }
			m.OnStopItem = func(PickerItem) (string, bool) { return message, false }
			var items []PickerItem
			for i := range 30 {
				items = append(items, PickerItem{Name: fmt.Sprintf("sim-%02d", i)})
			}
			update := func(msg tea.Msg) tea.Cmd {
				updated, cmd := m.Update(msg)
				m = updated.(PickerModel)
				return cmd
			}
			update(tea.WindowSizeMsg{Width: 60, Height: 20})
			update(PickerSetMsg{Items: items})
			update(PickerDoneMsg{})
			m.FocusItem("sim-20")
			initialHeight := m.table.Height()
			key := tea.KeyMsg{Type: tea.KeyEnter}
			switch action {
			case "create":
				key = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'c'}}
			case "enroll":
				key = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}}
			case "start":
				key = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}}
			case "stop":
				update(key)
				cmd := update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
				if m.flashMessage != "" || m.table.Height() != initialHeight || cmd == nil {
					t.Fatal("starting stop did not clear the flash and restore the table height")
				}
				update(cmd())
			}
			if action != "stop" {
				update(key)
			}
			if m.table.Height() >= initialHeight {
				t.Fatal("action did not reserve space for its wrapped flash")
			}
			update(tea.WindowSizeMsg{Width: 80, Height: 24})
			update(tea.WindowSizeMsg{Width: 60, Height: 20})
			view := ansi.Strip(m.View())
			if m.table.Height() >= initialHeight || lipgloss.Height(view) > 20 {
				t.Fatalf("wrapped flash did not fit the terminal:\n%s", view)
			}
			if !strings.Contains(strings.Join(strings.Fields(view), " "), message) || !strings.Contains(view, "sim-20") {
				t.Fatalf("recovery message or selected item hidden:\n%s", view)
			}
			update(tea.KeyMsg{Type: tea.KeyDown})
			if m.flashMessage != "" || m.table.Height() != initialHeight || m.currentCursorKey() != "sim-21" {
				t.Fatal("navigation did not clear the flash and restore the table height")
			}
		})
	}
}

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

func TestPickerStopRunsOutsideUpdateAndRemainsResponsive(t *testing.T) {
	m := NewPickerWithTitle("Simulators")
	called := false
	m.OnStopItem = func(item PickerItem) (string, bool) {
		called = true
		if item.Name != "alpha" {
			t.Fatalf("stopped wrong row: %s", item.Name)
		}
		return "Stopped alpha.", false
	}
	u, _ := m.Update(PickerAddMsg{Items: []PickerItem{{Name: "alpha"}, {Name: "beta"}}})
	m = u.(PickerModel)
	u, stop := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	m = u.(PickerModel)
	if called || stop == nil {
		t.Fatal("stop ran synchronously or was not scheduled")
	}
	if !strings.Contains(m.View(), "Stopping alpha") {
		t.Fatal("no shutdown progress")
	}
	u, duplicate := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	if duplicate != nil {
		t.Fatal("scheduled duplicate shutdown")
	}
	m = u.(PickerModel)
	u, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = u.(PickerModel)
	if m.table.Cursor() != 1 || !strings.Contains(m.View(), "Stopping alpha") {
		t.Fatal("navigation lost progress")
	}
	u, quit := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if quit == nil || !u.(PickerModel).Cancelled() {
		t.Fatal("cannot quit during shutdown")
	}
	u, _ = m.Update(stop())
	m = u.(PickerModel)
	if !called || m.stoppingName != "" || !strings.Contains(m.View(), "Stopped alpha.") {
		t.Fatal("stop result not applied")
	}
}

func TestPickerStartQuitsOnlyWhenTheCallbackAsks(t *testing.T) {
	m := NewPickerWithTitle("Simulators")
	var started []string
	m.OnStartItem = func(item PickerItem) (string, bool, bool) {
		started = append(started, item.Name)
		if item.Name == "alpha" {
			return "alpha is already running.", false, false
		}
		return "Starting beta...", false, true
	}
	u, _ := m.Update(PickerAddMsg{Items: []PickerItem{{Name: "alpha"}, {Name: "beta"}}})
	m = u.(PickerModel)

	u, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("s")})
	m = u.(PickerModel)
	if cmd != nil || !strings.Contains(m.View(), "alpha is already running.") {
		t.Fatalf("a refused start quit or hid its reason: %q", m.View())
	}
	u, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = u.(PickerModel)
	u, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("s")})
	m = u.(PickerModel)
	if cmd == nil {
		t.Fatal("an accepted start did not quit the picker")
	}
	if strings.Join(started, ",") != "alpha,beta" || m.Selected() != nil || m.Cancelled() {
		t.Fatalf("started %v, selected=%v cancelled=%v", started, m.Selected(), m.Cancelled())
	}
}

func TestPickerRemoveAsksFirstAndDefaultsToNo(t *testing.T) {
	newModel := func() (PickerModel, *[]string) {
		m := NewPickerWithTitle("Simulators")
		removed := new([]string)
		m.OnRemoveItem = func(item PickerItem) (string, bool, *PickerItem) {
			*removed = append(*removed, item.Name)
			return "Removed " + item.Name + ".", false, nil
		}
		m.RemoveConfirmation = func(item PickerItem) string {
			if item.Name == "nothing-to-lose" {
				return ""
			}
			return "Remove " + item.Name + " and its disk? [y/N]"
		}
		u, _ := m.Update(PickerAddMsg{Items: []PickerItem{{Name: "alpha"}, {Name: "nothing-to-lose"}}})
		return u.(PickerModel), removed
	}
	press := func(m PickerModel, key tea.KeyMsg) PickerModel {
		u, _ := m.Update(key)
		return u.(PickerModel)
	}
	r := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")}

	for _, answer := range []tea.KeyMsg{
		{Type: tea.KeyRunes, Runes: []rune("n")},
		{Type: tea.KeyEnter},
		{Type: tea.KeyEsc},
		{Type: tea.KeyDown},
	} {
		m, removed := newModel()
		m = press(m, r)
		if !strings.Contains(m.View(), "Remove alpha and its disk? [y/N]") {
			t.Fatalf("no confirmation shown: %q", m.View())
		}
		m = press(m, answer)
		if len(*removed) != 0 || len(m.items) != 2 || m.Cancelled() {
			t.Fatalf("%q removed %v or quit; only y may remove", answer.String(), *removed)
		}
		if strings.Contains(m.View(), "[y/N]") {
			t.Fatalf("%q left the question up: %q", answer.String(), m.View())
		}
	}

	m, removed := newModel()
	m = press(press(m, r), tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	if strings.Join(*removed, ",") != "alpha" || len(m.items) != 1 || !strings.Contains(m.View(), "Removed alpha.") {
		t.Fatalf("y did not remove the row: removed=%v view=%q", *removed, m.View())
	}

	// A row with nothing to lose is removed (or refused) without asking.
	m, removed = newModel()
	m = press(press(m, tea.KeyMsg{Type: tea.KeyDown}), r)
	if strings.Join(*removed, ",") != "nothing-to-lose" {
		t.Fatalf("an empty question still asked: removed=%v", *removed)
	}
}

func TestPickerRemoveQuestionFitsTheWindow(t *testing.T) {
	// The question takes the flash's line under the table. Without room kept
	// for it, a full alt-screen view grew a line and lost its title.
	question := "Remove robot-dog-simulator-2 and its disk? Its snapshots and console logs go with it. [y/N]"
	m := NewPickerWithTitle("Simulators")
	m.OnRemoveItem = func(PickerItem) (string, bool, *PickerItem) { return "", false, nil }
	m.RemoveConfirmation = func(PickerItem) string { return question }
	var items []PickerItem
	for i := range 30 {
		items = append(items, PickerItem{Name: fmt.Sprintf("sim-%02d", i)})
	}
	update := func(msg tea.Msg) {
		updated, _ := m.Update(msg)
		m = updated.(PickerModel)
	}
	update(tea.WindowSizeMsg{Width: 60, Height: 20})
	update(PickerSetMsg{Items: items})
	update(PickerDoneMsg{})
	initialHeight := m.table.Height()

	update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	if m.table.Height() >= initialHeight {
		t.Fatal("the question did not take its lines from the table")
	}
	view := ansi.Strip(m.View())
	if lipgloss.Height(view) > 20 || !strings.HasPrefix(view, "Simulators") {
		t.Fatalf("question pushed the view past the window:\n%s", view)
	}
	if !strings.Contains(strings.Join(strings.Fields(view), " "), question) {
		t.Fatalf("question cut off:\n%s", view)
	}
	for _, line := range strings.Split(view, "\n") {
		if ansi.StringWidth(line) > 60 {
			t.Errorf("line is %d wide: %q", ansi.StringWidth(line), line)
		}
	}

	update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	if m.table.Height() != initialHeight {
		t.Fatalf("answering no left the table at %d rows, want %d back", m.table.Height(), initialHeight)
	}
}

func TestPickerStartCanReportAnError(t *testing.T) {
	m := NewPickerWithTitle("Simulators")
	m.OnStartItem = func(PickerItem) (string, bool, bool) { return "store unreadable", true, false }
	u, _ := m.Update(PickerAddMsg{Items: []PickerItem{{Name: "alpha"}}})
	m = u.(PickerModel)
	u, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("s")})
	m = u.(PickerModel)
	if m.flashMessage != "store unreadable" || !m.flashIsError {
		t.Fatalf("flash=%q isError=%t, want the failure shown as an error", m.flashMessage, m.flashIsError)
	}
}

func TestPickerKeyHintsWrapUnderTheTitleWithoutLosingAKey(t *testing.T) {
	// The hints shared the title's line and were cropped to the window, so a
	// picker with every action showed "... c create, s" at 100 columns.
	newFull := func(width int) PickerModel {
		m := NewPickerWithTitle("Select a simulator")
		m.RemoveHint = "remove"
		m.OnCopyItem = func(PickerItem) string { return "" }
		m.OnSetDefault = func(PickerItem) (string, error) { return "", nil }
		m.OnUnsetDefault = func() (string, error) { return "", nil }
		m.OnCreateItem = func() (string, bool) { return "", false }
		m.OnStartItem = func(PickerItem) (string, bool, bool) { return "", false, false }
		m.OnStopItem = func(PickerItem) (string, bool) { return "", false }
		m.OnRemoveItem = func(PickerItem) (string, bool, *PickerItem) { return "", false, nil }
		u, _ := m.Update(tea.WindowSizeMsg{Width: width, Height: 30})
		u, _ = u.(PickerModel).Update(PickerAddMsg{Items: []PickerItem{{Name: "sim", Type: "stopped"}}})
		return u.(PickerModel)
	}
	keys := []string{"↑/↓ navigate", "enter copy", "d toggle default", "c create", "s start", "x stop", "r remove", "q quit"}

	for _, width := range []int{100, 80, 40} {
		view := ansi.Strip(newFull(width).View())
		lines := strings.Split(view, "\n")
		if lines[0] != "Select a simulator" {
			t.Errorf("width %d: title line = %q, want the title alone", width, lines[0])
		}
		for _, line := range lines {
			if w := ansi.StringWidth(line); w > width {
				t.Errorf("width %d: line is %d wide: %q", width, w, line)
			}
		}
		for _, key := range keys {
			if !strings.Contains(view, key) {
				t.Errorf("width %d: key %q is missing from %q", width, key, view)
			}
		}
	}

	// At 100 columns the whole set fits on one line under the title.
	if lines := strings.Split(ansi.Strip(newFull(100).View()), "\n"); !strings.HasPrefix(lines[1], "  ↑/↓ navigate") || !strings.HasSuffix(lines[1], "q quit") {
		t.Errorf("hint line at 100 columns = %q, want every key on it", lines[1])
	}
}

func TestPickerKeyHintLinesBreakOnlyBetweenEntries(t *testing.T) {
	keys := []string{"↑/↓ navigate", "enter copy", "d toggle default", "q quit"}
	if got := pickerKeyHintLines(keys, 0); len(got) != 1 || got[0] != "  ↑/↓ navigate, enter copy, d toggle default, q quit" {
		t.Errorf("unbounded = %q", got)
	}
	got := pickerKeyHintLines(keys, 30)
	want := []string{"  ↑/↓ navigate, enter copy,", "  d toggle default, q quit"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("wrapped = %q, want %q", got, want)
	}
}

func TestPickerKeyHintsGiveUpTableRows(t *testing.T) {
	// Every hint line is a line the table cannot use; without counting them a
	// full table pushed the title off the top of an alt-screen view.
	m := NewPickerWithTitle("Select")
	m.OnSetDefault = func(PickerItem) (string, error) { return "", nil }
	m.OnUnsetDefault = func() (string, error) { return "", nil }
	m.OnStartItem = func(PickerItem) (string, bool, bool) { return "", false, false }
	m.OnStopItem = func(PickerItem) (string, bool) { return "", false }
	items := make([]PickerItem, 40)
	for i := range items {
		items[i] = PickerItem{Name: fmt.Sprintf("vm-%02d", i)}
	}
	for _, width := range []int{120, 30} {
		u, _ := m.Update(tea.WindowSizeMsg{Width: width, Height: 20})
		u, _ = u.(PickerModel).Update(PickerAddMsg{Items: items})
		pm := u.(PickerModel)
		if got := strings.Count(pm.View(), "\n"); got > 20 {
			t.Errorf("width %d: view is %d lines in a 20-line window", width, got)
		}
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

func TestPickerModel_SupersedesReplacesRow(t *testing.T) {
	m := NewPicker()

	updated, _ := m.Update(PickerAddMsg{Items: []PickerItem{
		{Name: "ESP32 (unflashed)", DedupKey: "unflashed:/dev/tty1", Value: "ghost"},
	}})
	pm := updated.(PickerModel)

	updated, _ = pm.Update(PickerAddMsg{Items: []PickerItem{
		{Name: "wendy-alpha", DedupKey: "wendy-alpha", Supersedes: "unflashed:/dev/tty1", Value: "real"},
	}})
	pm = updated.(PickerModel)

	if got := len(pm.items); got != 1 {
		t.Fatalf("expected the superseded row to be replaced, got %d items: %+v", got, pm.items)
	}
	if pm.items[0].Value != "real" {
		t.Errorf("remaining item = %v, want the superseding one", pm.items[0].Value)
	}
	if _, ok := pm.seenIdx["unflashed:/dev/tty1"]; ok {
		t.Error("superseded key still present in seenIdx")
	}
}

func TestPickerModel_SupersedesUnknownKeyKeepsRows(t *testing.T) {
	m := NewPicker()

	updated, _ := m.Update(PickerAddMsg{Items: []PickerItem{
		{Name: "wendy-alpha", DedupKey: "wendy-alpha", Value: "a"},
	}})
	pm := updated.(PickerModel)

	updated, _ = pm.Update(PickerAddMsg{Items: []PickerItem{
		{Name: "wendy-beta", DedupKey: "wendy-beta", Supersedes: "never-added", Value: "b"},
	}})
	pm = updated.(PickerModel)

	if got := len(pm.items); got != 2 {
		t.Fatalf("expected both rows to survive a no-op supersede, got %d", got)
	}
}

func TestPickerModel_SupersedesKeepsCursorOnReplacement(t *testing.T) {
	m := NewPicker()

	// Two rows sorting either side of the replacement, so a cursor that fails
	// to follow lands on a neighbour instead.
	updated, _ := m.Update(PickerAddMsg{Items: []PickerItem{
		{Name: "aaa", DedupKey: "aaa", Value: "aaa"},
		{Name: "ESP32 (unflashed)", DedupKey: "mmm-unflashed", Value: "ghost"},
		{Name: "zzz", DedupKey: "zzz", Value: "zzz"},
	}})
	pm := updated.(PickerModel)

	updated, _ = pm.Update(tea.KeyMsg{Type: tea.KeyDown})
	pm = updated.(PickerModel)
	if got := pm.currentCursorKey(); got != "mmm-unflashed" {
		t.Fatalf("cursor = %q, want it on the row about to be superseded", got)
	}

	updated, _ = pm.Update(PickerAddMsg{Items: []PickerItem{
		{Name: "wendy-alpha", DedupKey: "mmm-real", Supersedes: "mmm-unflashed", Value: "real"},
	}})
	pm = updated.(PickerModel)

	if got := pm.currentCursorKey(); got != "mmm-real" {
		t.Errorf("cursor = %q, want it to follow onto the replacement", got)
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

func TestPickerModel_CustomColumns(t *testing.T) {
	m := NewPickerWithTitleAndColumns("Select a model", []PickerColumn{
		{
			Title:    "model",
			MinWidth: 16,
			Required: true,
			Value: func(item PickerItem) string {
				return item.Name
			},
		},
		{
			Title: "size",
			Value: func(item PickerItem) string {
				return item.Size
			},
		},
		{
			Title: "parameters",
			Value: func(item PickerItem) string {
				return item.Parameters
			},
		},
		{
			Title: "comments",
			Value: func(item PickerItem) string {
				return item.Comments
			},
		},
	})

	updated, _ := m.Update(PickerAddMsg{Items: []PickerItem{
		{
			Name:       "gemma4:e2b",
			Size:       "edge",
			Parameters: "E2B",
			Comments:   "default for small devices",
			Value:      "gemma4:e2b",
		},
	}})
	pm := updated.(PickerModel)

	view := pm.View()
	for _, want := range []string{"model", "size", "parameters", "comments", "gemma4:e2b", "default for small devices"} {
		if !strings.Contains(view, want) {
			t.Fatalf("expected picker view to contain %q, got %q", want, view)
		}
	}
}

func TestPickerModel_CustomColumnsNilFallsBackToDefaultPicker(t *testing.T) {
	m := NewPickerWithTitleAndColumns("Select", nil)

	updated, _ := m.Update(PickerAddMsg{Items: []PickerItem{
		{Name: "alpha", Description: "only show populated optional columns", Value: "alpha"},
	}})
	pm := updated.(PickerModel)

	if hasColumn(pm.table.Columns(), "Type") {
		t.Fatal("nil custom columns should not force the fixed default column layout")
	}
	if !hasColumn(pm.table.Columns(), "Description") {
		t.Fatal("nil custom columns should preserve the default picker dynamic column behavior")
	}
}

func TestPickerTableData_KeepsFullColumnContentForScrolling(t *testing.T) {
	items := []PickerItem{{
		Name:        "wendyos-sunny-daisy",
		Type:        "USB, LAN",
		Address:     "wendyos-sunny-daisy.local:50052",
		Description: "Full Linux-based edge device",
		Value:       "sunny",
	}}

	cols, rows := PickerTableData(items, "", false)

	for _, want := range []string{"Name", "Type", "Address", "Description"} {
		if !hasColumn(cols, want) {
			t.Fatalf("expected %s column to remain scrollable", want)
		}
	}
	// The port is stripped for display, so the column need only fit the host.
	if width := columnWidth(cols, "Address"); width < len("wendyos-sunny-daisy.local") {
		t.Fatalf("address column width = %d, want enough for full hostname", width)
	}
	if len(rows) != 1 || len(rows[0]) != len(cols) {
		t.Fatalf("row/column mismatch: rows=%v cols=%v", rows, cols)
	}
	if addr := rows[0][columnIndex(cols, "Address")]; strings.TrimSpace(addr) != "wendyos-sunny-daisy.local" {
		t.Fatalf("address cell = %q, want port stripped for display", addr)
	}
}

func TestDisplayAddress(t *testing.T) {
	cases := map[string]string{
		"wendyos-thor.local:50051":             "wendyos-thor.local",
		"wendyos-agx-orin.local:50052":         "wendyos-agx-orin.local",
		"192.168.1.10:50051":                   "192.168.1.10",
		"[fe80::1%en0]:50051":                  "fe80::1%en0",
		"wendyos-thor.local":                   "wendyos-thor.local", // no port
		"EE48983F-300C-3A22-395B-290A76741CA1": "EE48983F-300C-3A22-395B-290A76741CA1",
		"docker: my-container":                 "docker: my-container", // non-numeric "port"
		"":                                     "",
	}
	for in, want := range cases {
		if got := displayAddress(in); got != want {
			t.Errorf("displayAddress(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPickerTableData_KeepsDefaultMarkerInNarrowWidth(t *testing.T) {
	items := []PickerItem{{
		Name:     "alpha",
		Type:     "LAN",
		DedupKey: "alpha",
		Value:    "alpha",
	}}

	cols, rows := PickerTableData(items, "alpha", true)

	if len(cols) < 2 || cols[0].Title != "" || cols[1].Title != "Name" {
		t.Fatalf("columns = %v, want marker and Name columns", cols)
	}
	if len(rows) != 1 || len(rows[0]) < 1 || rows[0][0] != "✦" {
		t.Fatalf("default marker row = %v, want leading star", rows)
	}
}

func TestPickerDeviceTableData_UsesStableDeviceSchema(t *testing.T) {
	cols, rows := PickerDeviceTableData([]PickerItem{{
		Name: "alpha",
	}}, "", false)

	for _, want := range []string{"Name", "Type", "Address", "Agent", "OS"} {
		if !hasColumn(cols, want) {
			t.Fatalf("expected stable device column %q, got %v", want, cols)
		}
	}
	for _, gone := range []string{"Description", "P"} {
		if hasColumn(cols, gone) {
			t.Fatalf("expected no %q column, got %v", gone, cols)
		}
	}
	if len(rows) != 1 || len(rows[0]) != len(cols) {
		t.Fatalf("row/column mismatch: rows=%v cols=%v", rows, cols)
	}
}

func TestPickerDeviceTableData_ShowsDescriptionWhenAnyItemSetsIt(t *testing.T) {
	cols, _ := PickerDeviceTableData([]PickerItem{
		{Name: "alpha"},
		{Name: "beta", Description: "Full Linux-based edge device"},
	}, "", false)

	if !hasColumn(cols, "Description") {
		t.Fatalf("expected Description column when an item sets it, got %v", cols)
	}
}

func TestNewPicker_UsesStableDeviceSchemaBeforeMetadataArrives(t *testing.T) {
	m := NewPicker()

	updated, _ := m.Update(PickerAddMsg{Items: []PickerItem{{Name: "alpha", Value: "alpha"}}})
	pm := updated.(PickerModel)

	for _, want := range []string{"Name", "Type", "Address", "Agent", "OS"} {
		if !hasColumn(pm.table.Columns(), want) {
			t.Fatalf("expected stable device column %q, got %v", want, pm.table.Columns())
		}
	}
}

func TestPickerDeviceTableData_ShowsProvisionedStateInMarkerColumn(t *testing.T) {
	cols, rows := PickerDeviceTableData([]PickerItem{
		{Name: "alpha", Provisioned: "Provisioned"},
		{Name: "beta", Provisioned: "Unprovisioned"},
		{Name: "gamma"},
	}, "", false)

	// The marker column appears even without default tracking because rows
	// carry provisioned glyphs.
	if cols[0].Title != "" {
		t.Fatalf("cols[0] = %v, want unlabeled marker column", cols)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %v, want 3", rows)
	}
	if rows[0][0] != "●" {
		t.Fatalf("provisioned cell = %q, want \"●\"", rows[0][0])
	}
	if rows[1][0] != "○" {
		t.Fatalf("provisioned cell = %q, want \"○\"", rows[1][0])
	}
	if rows[2][0] != "" {
		t.Fatalf("provisioned cell = %q, want empty for unknown state", rows[2][0])
	}
}

func TestPickerDeviceTableData_CombinesDefaultMarkerAndProvisionedGlyph(t *testing.T) {
	_, rows := PickerDeviceTableData([]PickerItem{
		{Name: "alpha", Provisioned: "Provisioned"},
	}, "alpha", true)

	if rows[0][0] != "● ✦" {
		t.Fatalf("marker cell = %q, want \"● ✦\"", rows[0][0])
	}
}

func TestPickerDeviceTableData_OmitsMarkerColumnWithoutDefaultsOrGlyphs(t *testing.T) {
	cols, _ := PickerDeviceTableData([]PickerItem{{Name: "alpha"}}, "", false)

	if cols[0].Title != "Name" {
		t.Fatalf("cols = %v, want Name first without marker column", cols)
	}
}

func TestNewPicker_ShowsDeviceTableLegend(t *testing.T) {
	m := NewPicker()

	updated, _ := m.Update(PickerAddMsg{Items: []PickerItem{{Name: "alpha", Value: "alpha"}}})
	pm := updated.(PickerModel)

	if !strings.Contains(pm.View(), DeviceTableLegendBase) {
		t.Fatalf("expected device picker view to contain legend %q, got %q", DeviceTableLegendBase, pm.View())
	}
}

func TestNewPickerWithTitle_HasNoDeviceTableLegend(t *testing.T) {
	m := NewPickerWithTitle("Select a WiFi network")

	updated, _ := m.Update(PickerAddMsg{Items: []PickerItem{{Name: "alpha", Value: "alpha"}}})
	pm := updated.(PickerModel)

	if strings.Contains(pm.View(), "● provisioned") {
		t.Fatalf("expected non-device picker view without legend, got %q", pm.View())
	}
}

func TestPickerDeviceTableData_ShowsAgentAndWendyOSVersions(t *testing.T) {
	_, rows := PickerDeviceTableData([]PickerItem{{
		Name:         "alpha",
		Type:         "LAN",
		Address:      "alpha.local:50052",
		AgentVersion: "2026.05.30-161141",
		OSVersion:    "WendyOS-0.10.4",
	}}, "", false)

	if len(rows) != 1 || len(rows[0]) < 5 {
		t.Fatalf("rows = %v, want version cells", rows)
	}
	if rows[0][3] != "2026.05.30-161141" {
		t.Fatalf("agent version cell = %q", rows[0][3])
	}
	if rows[0][4] != "WendyOS-0.10.4" {
		t.Fatalf("WendyOS version cell = %q", rows[0][4])
	}
}

func TestPickerModel_WindowWidthControlsColumns(t *testing.T) {
	m := NewPickerWithTitle("Select a device")

	updated, _ := m.Update(tea.WindowSizeMsg{Width: 24, Height: 20})
	pm := updated.(PickerModel)
	updated, _ = pm.Update(PickerAddMsg{Items: []PickerItem{{
		Name:        "wendyos-sunny-daisy",
		Type:        "USB, LAN",
		Address:     "wendyos-sunny-daisy.local:50052",
		Description: "Full Linux-based edge device",
		Value:       "sunny",
	}}})
	pm = updated.(PickerModel)

	for _, want := range []string{"Name", "Type", "Address", "Description"} {
		if !hasColumn(pm.table.Columns(), want) {
			t.Fatalf("expected narrow picker to keep %s column for scrolling", want)
		}
	}
	for _, line := range strings.Split(pm.tableView(), "\n") {
		if got := ansi.StringWidth(line); got > 24 {
			t.Fatalf("cropped line width = %d, want <= 24: %q", got, line)
		}
	}
	// The port is stripped for display, but the (port-less) address must still be
	// preserved in full without ellipsis for horizontal scrolling.
	if table := pm.table.FullView(); !strings.Contains(table, "wendyos-sunny-daisy.local") || strings.Contains(table, ":50052") {
		t.Fatalf("underlying table should preserve full port-less address without ellipsis, got %q", table)
	}

	updated, _ = pm.Update(tea.WindowSizeMsg{Width: 120, Height: 20})
	pm = updated.(PickerModel)

	if !hasColumn(pm.table.Columns(), "Address") {
		t.Fatal("expected wide picker to show Address column")
	}
	if !hasColumn(pm.table.Columns(), "Description") {
		t.Fatal("expected wide picker to show Description column")
	}
}

func TestPickerModel_LeftRightScrollsWithoutBreakingVerticalNavigation(t *testing.T) {
	m := NewPickerWithTitle("Select a device")

	updated, _ := m.Update(tea.WindowSizeMsg{Width: 24, Height: 20})
	pm := updated.(PickerModel)
	updated, _ = pm.Update(PickerAddMsg{Items: []PickerItem{
		{
			Name:        "wendyos-alpha",
			Type:        "USB, LAN",
			Address:     "wendyos-alpha.local:50052",
			Description: "Full Linux-based edge device",
			Value:       "alpha",
		},
		{
			Name:        "wendyos-beta",
			Type:        "LAN",
			Address:     "wendyos-beta.local:50052",
			Description: "Another device",
			Value:       "beta",
		},
	}})
	pm = updated.(PickerModel)

	if !pm.canScrollTable() {
		t.Fatalf("expected table width %d to exceed viewport %d", PickerTableWidth(pm.table.Columns()), pm.tableViewportWidth())
	}
	before := pm.tableView()

	updated, _ = pm.Update(tea.KeyMsg{Type: tea.KeyRight})
	pm = updated.(PickerModel)

	if pm.table.ScrollOffset() == 0 {
		t.Fatal("expected right arrow to advance horizontal offset")
	}
	if after := pm.tableView(); after == before {
		t.Fatal("expected scrolled table view to change")
	}

	updated, _ = pm.Update(tea.KeyMsg{Type: tea.KeyDown})
	pm = updated.(PickerModel)

	if pm.table.Cursor() != 1 {
		t.Fatalf("expected down arrow to move cursor, got %d", pm.table.Cursor())
	}
	if pm.table.ScrollOffset() == 0 {
		t.Fatal("expected vertical navigation to preserve horizontal offset")
	}

	updated, _ = pm.Update(tea.KeyMsg{Type: tea.KeyLeft})
	pm = updated.(PickerModel)

	if pm.table.ScrollOffset() != 0 {
		t.Fatalf("expected left arrow to return to zero offset, got %d", pm.table.ScrollOffset())
	}
}

func TestPickerModel_ShowsUSBInTypeColumn(t *testing.T) {
	m := NewPickerWithTitle("Select a device")

	updated, _ := m.Update(PickerAddMsg{Items: []PickerItem{
		{Name: "Sunny Daisy", Type: "LAN", USB: "WendyOS Device sunny-daisy (en40)", Address: "wendyos-sunny-daisy.local:50052", Value: "sunny"},
	}})
	pm := updated.(PickerModel)

	view := pm.View()
	if !strings.Contains(view, "USB, LAN") {
		t.Fatalf("expected picker view to contain %q, got %q", "USB, LAN", view)
	}
}

func TestPickerModel_ShowsSelectedHintAtBottom(t *testing.T) {
	dockerHint := "Hint: Use Docker for local container or Compose runs when you do not need WendyOS hardware."
	localHint := "Hint: Use This Mac for native Swift, Go, or Python apps that should run directly on this computer."
	m := NewPicker()

	updated, _ := m.Update(PickerAddMsg{Items: []PickerItem{
		{Name: "Docker", Type: "Docker", Hint: dockerHint, Value: "docker"},
		{Name: "This Mac", Type: "This Mac", Hint: localHint, Value: "local"},
	}})
	pm := updated.(PickerModel)

	plain := ansi.Strip(pm.View())
	if got := lastNonEmptyLine(plain); got != dockerHint {
		t.Fatalf("footer hint = %q, want %q", got, dockerHint)
	}
	if strings.Contains(plain, localHint) {
		t.Fatalf("unexpected non-selected hint %q in view %q", localHint, plain)
	}

	updated, _ = pm.Update(tea.KeyMsg{Type: tea.KeyDown})
	pm = updated.(PickerModel)

	plain = ansi.Strip(pm.View())
	if got := lastNonEmptyLine(plain); got != localHint {
		t.Fatalf("footer hint after moving cursor = %q, want %q", got, localHint)
	}
	if strings.Contains(plain, dockerHint) {
		t.Fatalf("unexpected previous hint %q in view %q", dockerHint, plain)
	}
}

func TestPickerModel_DefaultKeyShowsStar(t *testing.T) {
	m := NewPickerWithTitle("Select a device")
	m.DefaultKey = "alpha"
	m.OnSetDefault = func(item PickerItem) (string, error) { return "", nil }
	m.OnUnsetDefault = func() (string, error) { return "", nil }

	updated, _ := m.Update(PickerAddMsg{Items: []PickerItem{
		{Name: "alpha", Type: "LAN", Value: "alpha"},
		{Name: "beta", Type: "LAN", Value: "beta"},
	}})
	pm := updated.(PickerModel)

	view := pm.View()
	if !strings.Contains(view, "✦") {
		t.Error("expected ✦ indicator for default item")
	}
	if !strings.Contains(view, "d toggle default") {
		t.Error("expected hint text to contain 'd toggle default'")
	}
	if strings.Contains(view, "x clear default") {
		t.Error("hint still advertises 'x clear default'; d toggles now")
	}

	// Without a way to clear, 'd' only sets, and the hint says so.
	m.OnUnsetDefault = nil
	if view := m.View(); !strings.Contains(view, "d set default") {
		t.Errorf("set-only picker hint = %q, want 'd set default'", view)
	}
}

func TestFormatOSNameVersion(t *testing.T) {
	tests := []struct {
		name    string
		os      string
		version string
		want    string
	}{
		{name: "name and version", os: "ubuntu", version: "24.04", want: "ubuntu 24.04"},
		{name: "version only", os: "", version: "24.04", want: "24.04"},
		{name: "name only", os: "arch", version: "", want: "arch"},
		{name: "both empty", os: "", version: "", want: ""},
		{name: "trims surrounding space", os: " ubuntu ", version: " 24.04 ", want: "ubuntu 24.04"},
		{name: "drops redundant distro prefix", os: "wendyos", version: "WendyOS-0.16.2", want: "WendyOS-0.16.2"},
		{name: "drops redundant prefix case-insensitive", os: "WendyOS", version: "wendyos 1.0", want: "wendyos 1.0"},
		{name: "keeps name when version is not a prefix match", os: "ubuntu", version: "24.04", want: "ubuntu 24.04"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatOSNameVersion(tt.os, tt.version); got != tt.want {
				t.Errorf("formatOSNameVersion(%q, %q) = %q; want %q", tt.os, tt.version, got, tt.want)
			}
		})
	}
}

func TestPickerDeviceTableData_OSColumnShowsDistroName(t *testing.T) {
	_, rows := PickerDeviceTableData([]PickerItem{{
		Name:      "wendy-ser9",
		Type:      "LAN",
		OS:        "ubuntu",
		OSVersion: "24.04",
	}}, "", false)

	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if !strings.Contains(strings.Join(rows[0], " "), "ubuntu 24.04") {
		t.Errorf("row %v does not contain combined OS name+version %q", rows[0], "ubuntu 24.04")
	}
}

func TestPickerTableData_DefaultKeysShowStar(t *testing.T) {
	_, rows := PickerTableData([]PickerItem{{
		Name:        "ubuntu",
		Type:        "LAN",
		DedupKey:    "ubuntu",
		DefaultKeys: []string{"ubuntu.local"},
	}}, "ubuntu.local", true)

	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if rows[0][0] != "✦" {
		t.Fatalf("default marker = %q, want ✦", rows[0][0])
	}
}

func TestPickerModel_DKeySetsDefault(t *testing.T) {
	m := NewPickerWithTitle("Select a device")
	var setItem PickerItem
	m.OnSetDefault = func(item PickerItem) (string, error) { setItem = item; return "", nil }
	m.OnUnsetDefault = func() (string, error) { return "", nil }

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

func TestPickerModel_DKeyOnTheDefaultClearsIt(t *testing.T) {
	m := NewPickerWithTitle("Select a device")
	m.DefaultKey = "alpha"
	var setCalls, unsetCalls int
	m.OnSetDefault = func(item PickerItem) (string, error) { setCalls++; return "", nil }
	m.OnUnsetDefault = func() (string, error) { unsetCalls++; return "", nil }

	updated, _ := m.Update(PickerAddMsg{Items: []PickerItem{
		{Name: "alpha", Type: "LAN", Value: "alpha"},
		{Name: "beta", Type: "LAN", Value: "beta"},
	}})
	pm := updated.(PickerModel)

	// 'x' no longer clears: it is the stop key now.
	updated, _ = pm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	pm = updated.(PickerModel)
	if unsetCalls != 0 || pm.DefaultKey != "alpha" {
		t.Fatalf("x cleared the default: unset=%d key=%q", unsetCalls, pm.DefaultKey)
	}

	updated, _ = pm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	pm = updated.(PickerModel)
	if unsetCalls != 1 || setCalls != 0 || pm.DefaultKey != "" {
		t.Fatalf("d on the default: set=%d unset=%d key=%q, want it cleared", setCalls, unsetCalls, pm.DefaultKey)
	}

	// Pressed again, it sets the same row back.
	updated, _ = pm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	pm = updated.(PickerModel)
	if setCalls != 1 || pm.DefaultKey != "alpha" {
		t.Fatalf("second d: set=%d key=%q, want alpha set again", setCalls, pm.DefaultKey)
	}

	// On another row 'd' moves the default rather than clearing it.
	updated, _ = pm.Update(tea.KeyMsg{Type: tea.KeyDown})
	pm = updated.(PickerModel)
	updated, _ = pm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	pm = updated.(PickerModel)
	if setCalls != 2 || unsetCalls != 1 || pm.DefaultKey != "beta" {
		t.Fatalf("d on another row: set=%d unset=%d key=%q, want beta", setCalls, unsetCalls, pm.DefaultKey)
	}
}

func TestPickerModel_SetMsgReplacesItems(t *testing.T) {
	m := NewPicker()

	updated, _ := m.Update(PickerSetMsg{Items: []PickerItem{
		{Name: "alpha", Value: "alpha"},
		{Name: "beta", Value: "beta"},
	}})
	pm := updated.(PickerModel)

	updated, _ = pm.Update(PickerSetMsg{Items: []PickerItem{
		{Name: "gamma", Value: "gamma"},
	}})
	pm = updated.(PickerModel)

	if got := len(pm.items); got != 1 {
		t.Fatalf("expected replacement to leave 1 item, got %d", got)
	}
	if got := pm.items[0].Name; got != "gamma" {
		t.Fatalf("remaining item = %q, want gamma", got)
	}
	if _, ok := pm.seenIdx["alpha"]; ok {
		t.Fatal("stale alpha key remained in seenIdx")
	}
}

func TestPickerModel_SetMsgPreservesCursorByItem(t *testing.T) {
	m := NewPicker()

	updated, _ := m.Update(PickerSetMsg{Items: []PickerItem{
		{Name: "alpha", Value: "alpha"},
		{Name: "beta", Value: "beta"},
	}})
	pm := updated.(PickerModel)
	pm.table.SetCursor(1)

	updated, _ = pm.Update(PickerSetMsg{Items: []PickerItem{
		{Name: "beta", Value: "beta"},
		{Name: "alpha", Value: "alpha"},
		{Name: "gamma", Value: "gamma"},
	}})
	pm = updated.(PickerModel)

	cursor := pm.table.Cursor()
	if cursor < 0 || cursor >= len(pm.items) {
		t.Fatalf("cursor out of range: %d", cursor)
	}
	if got := pm.items[cursor].Name; got != "beta" {
		t.Fatalf("cursor item = %q, want beta", got)
	}
}

func TestPickerModel_SetMsgClampsCursorWhenItemRemoved(t *testing.T) {
	m := NewPicker()

	updated, _ := m.Update(PickerSetMsg{Items: []PickerItem{
		{Name: "alpha", Value: "alpha"},
		{Name: "beta", Value: "beta"},
	}})
	pm := updated.(PickerModel)
	pm.table.SetCursor(1)

	updated, _ = pm.Update(PickerSetMsg{Items: []PickerItem{
		{Name: "alpha", Value: "alpha"},
	}})
	pm = updated.(PickerModel)

	if got := pm.table.Cursor(); got != 0 {
		t.Fatalf("cursor = %d, want 0", got)
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

func TestPickerModel_EnrollHighlightedItem(t *testing.T) {
	for _, quit := range []bool{false, true} {
		t.Run(map[bool]string{false: "flash", true: "quit"}[quit], func(t *testing.T) {
			m := sectionedPicker(t)
			for i := 0; i < 2; i++ {
				updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
				m = updated.(PickerModel)
			}
			var enrolled PickerItem
			m.OnEnrollItem = func(item PickerItem) (string, bool) {
				enrolled = item
				return "Enrollment requested.", quit
			}
			m.flashIsError = true

			updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
			m = updated.(PickerModel)
			if enrolled.Value != "c5" {
				t.Fatalf("enrolled value = %v, want c5 (highlighted row after section header)", enrolled.Value)
			}
			if m.Selected() != nil || m.Cancelled() {
				t.Fatal("enrollment must not select a device or mark the picker cancelled")
			}
			if !strings.Contains(m.View(), "Enrollment requested.") || m.flashIsError {
				t.Fatal("enrollment confirmation must be rendered without stale error styling")
			}
			if quit {
				if cmd == nil {
					t.Fatal("expected enrollment to close the picker")
				}
				if _, ok := cmd().(tea.QuitMsg); !ok {
					t.Fatal("expected a quit command")
				}
			} else if cmd != nil {
				t.Fatal("enrollment flash must leave the picker open")
			}
		})
	}
}

func TestPickerModel_EnrollRequiresSelectableItem(t *testing.T) {
	for _, name := range []string{"empty", "section header"} {
		t.Run(name, func(t *testing.T) {
			m := NewPicker()
			if name == "section header" {
				m = sectionedPicker(t)
				m.table.SetCursor(0)
			}
			m.OnEnrollItem = func(PickerItem) (string, bool) {
				t.Fatal("enrollment called without a selectable item")
				return "", true
			}
			updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
			m = updated.(PickerModel)
			if cmd != nil || m.Selected() != nil || m.Cancelled() || m.flashMessage != "" {
				t.Fatal("enrollment without a selectable item must leave the picker open and unchanged")
			}
		})
	}
}

func TestPickerModel_EnrollHintAndAvailability(t *testing.T) {
	for _, tc := range []struct {
		name       string
		callback   bool
		filterable bool
		wantHint   bool
	}{
		{name: "unavailable"},
		{name: "available", callback: true, wantHint: true},
		{name: "filterable", callback: true, filterable: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := sectionedPicker(t)
			m.Filterable = tc.filterable
			called := false
			if tc.callback {
				m.OnEnrollItem = func(PickerItem) (string, bool) {
					called = true
					return "", false
				}
			}
			if got := strings.Contains(m.View(), ", e enroll"); got != tc.wantHint {
				t.Fatalf("enroll hint present = %v, want %v", got, tc.wantHint)
			}
			updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
			m = updated.(PickerModel)
			if called != tc.wantHint {
				t.Fatalf("enrollment called = %v, want %v", called, tc.wantHint)
			}
			if cmd != nil || m.Selected() != nil || m.Cancelled() {
				t.Fatal("picker unexpectedly closed or selected an item")
			}
			if tc.filterable && m.filter != "e" {
				t.Fatalf("filter = %q, want e", m.filter)
			}
		})
	}
}

// ── Section headers ──────────────────────────────────────────────────

// sectionedPicker builds a one-shot picker whose items belong to two ordered
// sections: "WendyOS" (SortKey prefix 0_) before "Wendy Lite" (prefix 1_).
// After sorting, the display order is:
//
//	── WendyOS      (header)
//	Jetson Orin     (0_wendyos_jetson)
//	Raspberry Pi 5  (0_wendyos_rpi5)
//	── Wendy Lite   (header)
//	ESP32-C5        (1_lite_c5)
//	ESP32-C6        (1_lite_c6)
func sectionedPicker(t *testing.T) PickerModel {
	t.Helper()
	m := NewPickerWithTitle("Select a device")
	updated, _ := m.Update(PickerAddMsg{Items: []PickerItem{
		{Name: "Raspberry Pi 5", Section: "WendyOS", SortKey: "0_wendyos_rpi5", Value: "rpi5"},
		{Name: "Jetson Orin", Section: "WendyOS", SortKey: "0_wendyos_jetson", Value: "jetson"},
		{Name: "ESP32-C6", Section: "Wendy Lite", SortKey: "1_lite_c6", Value: "c6"},
		{Name: "ESP32-C5", Section: "Wendy Lite", SortKey: "1_lite_c5", Value: "c5"},
	}})
	return updated.(PickerModel)
}

func TestPickerModel_RendersSectionHeaders(t *testing.T) {
	pm := sectionedPicker(t)

	view := ansi.Strip(pm.View())
	for _, want := range []string{"WendyOS", "Wendy Lite", "Jetson Orin", "Raspberry Pi 5", "ESP32-C5", "ESP32-C6"} {
		if !strings.Contains(view, want) {
			t.Fatalf("expected sectioned picker view to contain %q, got %q", want, view)
		}
	}
	// The WendyOS header must appear before the Wendy Lite header.
	if strings.Index(view, "WendyOS") > strings.Index(view, "Wendy Lite") {
		t.Fatalf("expected WendyOS section before Wendy Lite section, got %q", view)
	}
}

// TestPickerModel_RendersSectionHeadersInColor verifies that section headers
// render correctly in truecolor mode. The header cell text stays plain (no
// lipgloss styling) to avoid ANSI truncation issues in the bubble table, but
// this test ensures the label survives intact when the terminal supports
// truecolor.
func TestPickerModel_RendersSectionHeadersInColor(t *testing.T) {
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(termenv.Ascii)

	pm := sectionedPicker(t)

	view := ansi.Strip(pm.View())
	for _, want := range []string{"WendyOS", "Wendy Lite"} {
		if !strings.Contains(view, want) {
			t.Fatalf("expected colorized sectioned picker to render header %q intact, got %q", want, view)
		}
	}
}

// TestPickerModel_SectionHeadersAreVisuallyDistinct guards the two cues that set
// a header apart from a selectable device row: the plain-text rule prefix (so it
// reads as a header even with no color) and the accent color applied after
// layout (so it pops on capable terminals). Without both, a header like
// "WendyOS" is indistinguishable from a device name.
func TestPickerModel_SectionHeadersAreVisuallyDistinct(t *testing.T) {
	// Rule prefix survives ANSI stripping, so it's present regardless of color.
	pm := sectionedPicker(t)
	plain := ansi.Strip(pm.View())
	for _, label := range []string{"WendyOS", "Wendy Lite"} {
		if !strings.Contains(plain, sectionHeaderPrefix+label) {
			t.Fatalf("expected header %q to carry the %q rule prefix, got %q", label, sectionHeaderPrefix, plain)
		}
	}
	// A device row must NOT carry the header prefix.
	if strings.Contains(plain, sectionHeaderPrefix+"Raspberry Pi 5") {
		t.Fatalf("device row unexpectedly rendered as a section header: %q", plain)
	}

	// In truecolor the header line carries escape codes (accent color) that a
	// plain device row does not.
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(termenv.Ascii)
	colored := sectionedPicker(t).View()
	var headerLine string
	for _, line := range strings.Split(colored, "\n") {
		if strings.Contains(ansi.Strip(line), sectionHeaderPrefix+"WendyOS") {
			headerLine = line
			break
		}
	}
	if headerLine == "" {
		t.Fatal("could not locate the WendyOS header line in the colored view")
	}
	if headerLine == ansi.Strip(headerLine) {
		t.Fatalf("expected the header line to be colorized, got plain %q", headerLine)
	}
}

func TestPickerModel_SectionEnterSelectsDeviceNotHeader(t *testing.T) {
	pm := sectionedPicker(t)

	// The initial cursor must land on the first device, never the leading
	// section header. Enter should select that device.
	updated, cmd := pm.Update(tea.KeyMsg{Type: tea.KeyEnter})
	pm = updated.(PickerModel)

	if cmd == nil || pm.Selected() == nil {
		t.Fatal("expected enter to select the first device, not a header row")
	}
	if got := pm.Selected().Value.(string); got != "jetson" {
		t.Fatalf("selected %q, want jetson (first selectable row)", got)
	}
}

func TestPickerModel_SectionDownSkipsHeader(t *testing.T) {
	pm := sectionedPicker(t)

	// From Jetson (first row), Down → Raspberry Pi 5, Down → must skip the
	// Wendy Lite header and land on ESP32-C5.
	for i := 0; i < 2; i++ {
		updated, _ := pm.Update(tea.KeyMsg{Type: tea.KeyDown})
		pm = updated.(PickerModel)
	}
	updated, _ := pm.Update(tea.KeyMsg{Type: tea.KeyEnter})
	pm = updated.(PickerModel)

	if pm.Selected() == nil {
		t.Fatal("expected a device selection after navigating down, got none (cursor on header?)")
	}
	if got := pm.Selected().Value.(string); got != "c5" {
		t.Fatalf("after 2× down selected %q, want c5 (first Wendy Lite device)", got)
	}
}

func TestPickerModel_SectionUpSkipsHeader(t *testing.T) {
	pm := sectionedPicker(t)

	// Move down onto ESP32-C5, then Up must skip the Wendy Lite header back to
	// Raspberry Pi 5 (last WendyOS device).
	for i := 0; i < 2; i++ {
		updated, _ := pm.Update(tea.KeyMsg{Type: tea.KeyDown})
		pm = updated.(PickerModel)
	}
	updated, _ := pm.Update(tea.KeyMsg{Type: tea.KeyUp})
	pm = updated.(PickerModel)
	updated, _ = pm.Update(tea.KeyMsg{Type: tea.KeyEnter})
	pm = updated.(PickerModel)

	if pm.Selected() == nil {
		t.Fatal("expected a device selection after navigating up, got none (cursor on header?)")
	}
	if got := pm.Selected().Value.(string); got != "rpi5" {
		t.Fatalf("after down,down,up selected %q, want rpi5", got)
	}
}

func hasColumn(cols []bubbleTable.Column, title string) bool {
	for _, col := range cols {
		if col.Title == title {
			return true
		}
	}
	return false
}

func columnWidth(cols []bubbleTable.Column, title string) int {
	for _, col := range cols {
		if col.Title == title {
			return col.Width
		}
	}
	return 0
}

func columnIndex(cols []bubbleTable.Column, title string) int {
	for i, col := range cols {
		if col.Title == title {
			return i
		}
	}
	return -1
}

func lastNonEmptyLine(view string) string {
	lines := strings.Split(strings.TrimSpace(view), "\n")
	if len(lines) == 0 {
		return ""
	}
	return strings.TrimSpace(lines[len(lines)-1])
}

// ── Filterable picker ────────────────────────────────────────────────

func typeIntoPicker(t *testing.T, m PickerModel, s string) PickerModel {
	t.Helper()
	for _, r := range s {
		updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = updated.(PickerModel)
	}
	return m
}

func newFilterablePicker(t *testing.T) PickerModel {
	t.Helper()
	m := NewPickerWithTitle("Select a WiFi network")
	m.Filterable = true
	updated, _ := m.Update(PickerAddMsg{Items: []PickerItem{
		{Name: "HomeNet", Type: "82%", Value: "HomeNet"},
		{Name: "my home 5G", Type: "70%", Value: "my home 5G"},
		{Name: "CafeSpot", Type: "55%", Value: "CafeSpot"},
	}})
	return updated.(PickerModel)
}

func TestPickerModel_FilterNarrowsCaseInsensitively(t *testing.T) {
	pm := typeIntoPicker(t, newFilterablePicker(t), "home")

	visible := pm.visibleItems()
	if len(visible) != 2 {
		t.Fatalf("expected 2 matches for 'home', got %d: %+v", len(visible), visible)
	}
	for _, item := range visible {
		if !strings.Contains(strings.ToLower(item.Name), "home") {
			t.Errorf("unexpected item in filtered view: %q", item.Name)
		}
	}
	if view := pm.View(); !strings.Contains(view, "Filter: home") {
		t.Errorf("expected the active filter to be shown, got %q", view)
	}
}

func TestPickerModel_FilterSpaceAndBackspace(t *testing.T) {
	pm := typeIntoPicker(t, newFilterablePicker(t), "my")
	updated, _ := pm.Update(tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}})
	pm = updated.(PickerModel)
	pm = typeIntoPicker(t, pm, "homex")

	if got := len(pm.visibleItems()); got != 0 {
		t.Fatalf("expected 0 matches for 'my homex', got %d", got)
	}

	updated, _ = pm.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	pm = updated.(PickerModel)
	if got := pm.filter; got != "my home" {
		t.Fatalf("filter after backspace = %q, want %q", got, "my home")
	}
	if got := len(pm.visibleItems()); got != 1 {
		t.Fatalf("expected 1 match for 'my home', got %d", got)
	}
}

func TestPickerModel_EnterSelectsFromFilteredView(t *testing.T) {
	pm := typeIntoPicker(t, newFilterablePicker(t), "cafe")

	updated, _ := pm.Update(tea.KeyMsg{Type: tea.KeyEnter})
	pm = updated.(PickerModel)
	if pm.Selected() == nil {
		t.Fatal("expected a selection")
	}
	if got := pm.Selected().Value.(string); got != "CafeSpot" {
		t.Fatalf("selected %q, want CafeSpot", got)
	}
}

func TestPickerModel_EscClearsFilterThenQuits(t *testing.T) {
	pm := typeIntoPicker(t, newFilterablePicker(t), "cafe")

	updated, _ := pm.Update(tea.KeyMsg{Type: tea.KeyEsc})
	pm = updated.(PickerModel)
	if pm.filter != "" {
		t.Fatalf("first esc should clear the filter, got %q", pm.filter)
	}
	if pm.Cancelled() {
		t.Fatal("first esc must not quit while a filter is active")
	}
	if got := len(pm.visibleItems()); got != 3 {
		t.Fatalf("expected full list after clearing filter, got %d", got)
	}

	updated, _ = pm.Update(tea.KeyMsg{Type: tea.KeyEsc})
	pm = updated.(PickerModel)
	if !pm.Cancelled() {
		t.Fatal("second esc should quit the picker")
	}
}

func TestPickerModel_FilterableRoutesQToQuery(t *testing.T) {
	pm := typeIntoPicker(t, newFilterablePicker(t), "q")
	if pm.Cancelled() {
		t.Fatal("'q' must filter, not quit, when Filterable is set")
	}
	if pm.filter != "q" {
		t.Fatalf("filter = %q, want %q", pm.filter, "q")
	}
}

func TestPickerModel_NonFilterableKeepsQQuit(t *testing.T) {
	m := NewPickerWithTitle("plain")
	updated, _ := m.Update(PickerAddMsg{Items: []PickerItem{{Name: "a", Value: "a"}}})
	pm := updated.(PickerModel)
	updated, _ = pm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	pm = updated.(PickerModel)
	if !pm.Cancelled() {
		t.Fatal("'q' should still quit a non-filterable picker")
	}
}

func TestPickerModel_NoMatchesShowsHint(t *testing.T) {
	pm := typeIntoPicker(t, newFilterablePicker(t), "zzz")
	if view := pm.View(); !strings.Contains(view, "No matches") {
		t.Errorf("expected a no-matches hint, got %q", view)
	}
}
