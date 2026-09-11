package tui

import (
	"strings"
	"testing"

	bubbleTable "github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
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
	u, stop := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("s")})
	m = u.(PickerModel)
	if called || stop == nil {
		t.Fatal("stop ran synchronously or was not scheduled")
	}
	if !strings.Contains(m.View(), "Stopping alpha") {
		t.Fatal("no shutdown progress")
	}
	u, duplicate := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("s")})
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
	m.OnSetDefault = func(item PickerItem) string { return "" }
	m.OnUnsetDefault = func() string { return "" }

	updated, _ := m.Update(PickerAddMsg{Items: []PickerItem{
		{Name: "alpha", Type: "LAN", Value: "alpha"},
		{Name: "beta", Type: "LAN", Value: "beta"},
	}})
	pm := updated.(PickerModel)

	view := pm.View()
	if !strings.Contains(view, "✦") {
		t.Error("expected ✦ indicator for default item")
	}
	if !strings.Contains(view, "d set default") {
		t.Error("expected hint text to contain 'd set default'")
	}
	if !strings.Contains(view, "x clear default") {
		t.Error("expected hint text to contain 'x clear default'")
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
	m.OnSetDefault = func(item PickerItem) string { setItem = item; return "" }
	m.OnUnsetDefault = func() string { return "" }

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
	m.OnSetDefault = func(item PickerItem) string { return "" }
	m.OnUnsetDefault = func() string { unsetCalled = true; return "" }

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
