//go:build darwin || linux || windows

package commands

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
)

func installPickerTestItems() []tui.PickerItem {
	return []tui.PickerItem{
		{Name: "Raspberry Pi 5", Section: "WendyOS", SortKey: "0_wendyos", Value: "raspberry-pi-5"},
		{Name: "ESP32-C6", Section: "Wendy Lite", SortKey: "1_lite", Value: "wlite_target_esp32c6"},
		{Name: "Linux Desktop", Section: "Linux Desktop", SortKey: "2_linux", Value: linuxDesktopValue},
		{Name: "Headless Mac", Section: "Headless Mac", SortKey: "3_mac", Value: headlessMacValue},
	}
}

func updateInstallPicker(m installPickerModel, msg tea.Msg) installPickerModel {
	updated, _ := m.Update(msg)
	return updated.(installPickerModel)
}

func TestInstallPickerTabs(t *testing.T) {
	m := newInstallPickerModel(installPickerTestItems())
	for _, tt := range []struct {
		label string
		rows  []string
	}{
		{label: "Linux", rows: []string{"Raspberry Pi 5", "Linux Desktop"}},
		{label: "Mac", rows: []string{"Headless Mac"}},
		{label: "Microcontrollers", rows: []string{"ESP32-C6"}},
	} {
		if got := m.tabs[m.active].label; got != tt.label {
			t.Fatalf("active tab = %q, want %q", got, tt.label)
		}
		view := m.View()
		for _, label := range []string{"Linux", "Mac", "Microcontrollers", "tab/shift+tab switch"} {
			if !strings.Contains(view, label) {
				t.Fatalf("missing tab label or hint %q: %s", label, view)
			}
		}
		for _, item := range installPickerTestItems() {
			want := false
			for _, row := range tt.rows {
				want = want || row == item.Name
			}
			if got := strings.Contains(view, item.Name); got != want {
				t.Errorf("%s tab shows %q = %v, want %v", tt.label, item.Name, got, want)
			}
		}
		m = updateInstallPicker(m, tea.KeyMsg{Type: tea.KeyTab})
	}
	if m.active != 0 {
		t.Fatal("Tab did not wrap to Linux")
	}
	for _, want := range []string{"Microcontrollers", "Mac", "Linux"} {
		m = updateInstallPicker(m, tea.KeyMsg{Type: tea.KeyShiftTab})
		if got := m.tabs[m.active].label; got != want {
			t.Fatalf("Shift+Tab selected %q, want %q", got, want)
		}
	}
}

func TestInstallPickerSelectsFromActiveTab(t *testing.T) {
	for _, tt := range []struct {
		name string
		keys []tea.KeyType
		want string
	}{
		{name: "WendyOS", want: "raspberry-pi-5"},
		{name: "Linux Desktop", keys: []tea.KeyType{tea.KeyDown}, want: linuxDesktopValue},
		{name: "Mac", keys: []tea.KeyType{tea.KeyTab}, want: headlessMacValue},
		{name: "Microcontrollers", keys: []tea.KeyType{tea.KeyShiftTab}, want: "wlite_target_esp32c6"},
		{name: "preserved Linux cursor", keys: []tea.KeyType{tea.KeyDown, tea.KeyTab, tea.KeyTab, tea.KeyTab}, want: linuxDesktopValue},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m := newInstallPickerModel(installPickerTestItems())
			for _, key := range tt.keys {
				m = updateInstallPicker(m, tea.KeyMsg{Type: key})
				if m.selected != nil || m.cancelled {
					t.Fatal("navigation closed the picker")
				}
			}
			updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
			m = updated.(installPickerModel)
			if m.selected == nil || m.selected.Value != tt.want {
				t.Fatalf("selection = %+v, want %q", m.selected, tt.want)
			}
			if cmd == nil {
				t.Fatal("selection did not quit")
			}
			if _, ok := cmd().(tea.QuitMsg); !ok {
				t.Fatal("selection did not return tea.Quit")
			}
			if m.View() != "" {
				t.Fatal("selected picker did not clear its view")
			}
		})
	}
}

func TestInstallPickerCancel(t *testing.T) {
	for tab := range 3 {
		for _, key := range []tea.KeyMsg{
			{Type: tea.KeyRunes, Runes: []rune{'q'}},
			{Type: tea.KeyEsc},
			{Type: tea.KeyCtrlC},
		} {
			t.Run(fmt.Sprintf("tab%d/%s", tab, key.String()), func(t *testing.T) {
				m := newInstallPickerModel(installPickerTestItems())
				for range tab {
					m = updateInstallPicker(m, tea.KeyMsg{Type: tea.KeyTab})
				}
				updated, cmd := m.Update(key)
				m = updated.(installPickerModel)
				if !m.cancelled || m.selected != nil || m.View() != "" || cmd == nil {
					t.Fatal("cancel did not close the picker without a selection")
				}
				if _, ok := cmd().(tea.QuitMsg); !ok {
					t.Fatal("cancel did not return tea.Quit")
				}
			})
		}
	}
}

func TestInstallPickerOmitsEmptyTabs(t *testing.T) {
	items := installPickerTestItems()
	for _, tt := range []struct {
		name  string
		items []tui.PickerItem
		want  []string
	}{
		{name: "PR build", items: items[:1], want: []string{"Linux"}},
		{name: "firmware unavailable", items: items[2:], want: []string{"Linux", "Mac"}},
		{name: "Linux unavailable", items: []tui.PickerItem{items[1], items[3]}, want: []string{"Mac", "Microcontrollers"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m := newInstallPickerModel(tt.items)
			if len(m.tabs) != len(tt.want) {
				t.Fatalf("tab count = %d, want %d", len(m.tabs), len(tt.want))
			}
			for _, want := range tt.want {
				if got := m.tabs[m.active].label; got != want {
					t.Fatalf("active tab = %q, want %q", got, want)
				}
				m = updateInstallPicker(m, tea.KeyMsg{Type: tea.KeyTab})
			}
			if m.active != 0 {
				t.Fatal("Tab did not wrap to the first available category")
			}
			m = updateInstallPicker(m, tea.KeyMsg{Type: tea.KeyShiftTab})
			if m.active != len(tt.want)-1 {
				t.Fatal("Shift+Tab did not wrap to the last available category")
			}
			if got := strings.Contains(m.View(), "tab/shift+tab switch"); got != (len(tt.want) > 1) {
				t.Fatalf("switch hint shown = %v for %d tabs", got, len(tt.want))
			}
		})
	}
}

func TestInstallPickerResize(t *testing.T) {
	items := installPickerTestItems()
	for i := range 30 {
		items = append(items, tui.PickerItem{Name: fmt.Sprintf("Linux device %02d", i), Value: fmt.Sprintf("linux-%d", i)})
		items = append(items, tui.PickerItem{Name: fmt.Sprintf("ESP32 board %02d", i), Value: fmt.Sprintf("wlite_%d", i)})
	}
	m := newInstallPickerModel(items)
	for _, size := range []tea.WindowSizeMsg{{Width: 80, Height: 18}, {Width: 40, Height: 12}} {
		m = updateInstallPicker(m, size)
		for range len(m.tabs) {
			view := m.View()
			if got := strings.Count(strings.TrimRight(view, "\n"), "\n") + 1; got > size.Height {
				t.Errorf("view uses %d rows, terminal has %d", got, size.Height)
			}
			for _, line := range strings.Split(view, "\n") {
				if got := ansi.StringWidth(line); got > size.Width {
					t.Errorf("line uses %d columns, terminal has %d: %q", got, size.Width, line)
				}
			}
			m = updateInstallPicker(m, tea.KeyMsg{Type: tea.KeyTab})
		}
	}
}
