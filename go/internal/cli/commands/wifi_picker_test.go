package commands

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/synctest"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

func updateLocalWifiPicker(m *localWifiPickerModel, msg tea.Msg) tea.Cmd {
	updated, cmd := m.Update(msg)
	*m = updated.(localWifiPickerModel)
	return cmd
}

func TestLocalWifiPickerRefreshes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := newLocalWifiPickerModel(context.Background())
		m.cached = func(context.Context) []localWifiNetwork {
			return []localWifiNetwork{{SSID: "Old network"}, {SSID: "Home", SignalStrength: 40, Security: "WPA2"}}
		}
		scans := 0
		m.scan = func(context.Context) ([]localWifiNetwork, error) {
			scans++
			if scans == 1 {
				return []localWifiNetwork{{SSID: "Home", SignalStrength: 80, Security: "WPA3"}}, nil
			}
			return []localWifiNetwork{{SSID: "New hotspot", SignalStrength: 95, Security: "Open"}}, nil
		}

		scan := updateLocalWifiPicker(&m, m.Init()())
		if view := m.View(); !strings.Contains(view, "Old network") || !strings.Contains(view, "40%") {
			t.Fatalf("cached networks missing while scan is pending: %s", view)
		}
		if !m.selection.HadNetworks || scans != 0 {
			t.Fatalf("cached results must be usable before the fresh scan finishes: %+v, scans=%d", m.selection, scans)
		}

		tick := updateLocalWifiPicker(&m, scan())
		if view := m.View(); strings.Contains(view, "Old network") || !strings.Contains(view, "80%") || !strings.Contains(view, "WPA3") {
			t.Fatalf("fresh results must replace cached rows and update signal/security: %s", view)
		}
		if tick == nil {
			t.Fatal("scan did not schedule a refresh")
		}
		msg := tick()
		if _, ok := msg.(localWifiRefreshMsg); !ok {
			t.Fatalf("scheduled command returned %T, want refresh", msg)
		}
		scan = updateLocalWifiPicker(&m, msg)
		if scans != 1 {
			t.Fatal("refresh must run the scan as a command without blocking Update")
		}
		updateLocalWifiPicker(&m, scan())
		if view := m.View(); strings.Contains(view, "Home") || !strings.Contains(view, "New hotspot") {
			t.Fatalf("periodic scan did not replace the previous results: %s", view)
		}
		if scans != 2 {
			t.Fatalf("scans = %d, want 2", scans)
		}
	})
}

func TestLocalWifiPickerPreservesFilterAndHighlight(t *testing.T) {
	m := newLocalWifiPickerModel(context.Background())
	updateLocalWifiPicker(&m, localWifiScanMsg{networks: []localWifiNetwork{
		{SSID: "Home B"}, {SSID: "Home C"}, {SSID: "Guest"},
	}})
	updateLocalWifiPicker(&m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("Home")})
	updateLocalWifiPicker(&m, tea.KeyMsg{Type: tea.KeyDown})
	updateLocalWifiPicker(&m, localWifiScanMsg{networks: []localWifiNetwork{
		{SSID: "Home A"}, {SSID: "Home B"}, {SSID: "Home C", SignalStrength: 75}, {SSID: "Guest"},
	}})
	view := ansi.Strip(m.View())
	if !strings.Contains(view, "Filter: Home") || strings.Contains(view, "Guest") {
		t.Fatalf("refresh did not preserve the active filter: %s", view)
	}
	updateLocalWifiPicker(&m, tea.KeyMsg{Type: tea.KeyEnter})
	sel, err := m.result()
	if err != nil || sel.SSID != "Home C" {
		t.Fatalf("highlight moved during refresh: selection=%+v, err=%v", sel, err)
	}
	if got := m.picker.Selected().Size; got != "75%" {
		t.Fatalf("selected row has stale signal: %s", got)
	}
}

func TestLocalWifiPickerRetriesFailedAndEmptyScans(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := newLocalWifiPickerModel(context.Background())
		updateLocalWifiPicker(&m, localWifiScanMsg{networks: []localWifiNetwork{{SSID: "Home"}}})
		wantErr := errors.New("scan temporarily unavailable")
		tick := updateLocalWifiPicker(&m, localWifiScanMsg{err: wantErr})
		if !strings.Contains(m.View(), "Home") || !errors.Is(m.selection.ScanErr, wantErr) {
			t.Fatalf("failed scan must preserve the previous results: %+v", m.selection)
		}
		if tick == nil {
			t.Fatal("failed scan did not schedule a retry")
		}
		if _, ok := tick().(localWifiRefreshMsg); !ok {
			t.Fatal("failed scan quit instead of scheduling a retry")
		}
		tick = updateLocalWifiPicker(&m, localWifiScanMsg{})
		if m.selection.ScanErr != nil || m.selection.HadNetworks || strings.Contains(m.View(), "Home") {
			t.Fatalf("successful empty scan must clear stale results and errors: %+v", m.selection)
		}
		if tick == nil {
			t.Fatal("empty scan did not schedule another refresh")
		}
		if _, ok := tick().(localWifiRefreshMsg); !ok {
			t.Fatal("empty scan quit instead of scheduling a retry")
		}
	})
}

func TestLocalWifiPickerCancelAndManualEntry(t *testing.T) {
	for _, tc := range []struct {
		name   string
		key    tea.KeyType
		filter string
		cancel bool
	}{
		{name: "ctrl+c cancels", key: tea.KeyCtrlC, cancel: true},
		{name: "ctrl+c cancels with filter", key: tea.KeyCtrlC, filter: "Home", cancel: true},
		{name: "escape enters manually", key: tea.KeyEsc},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newLocalWifiPickerModel(context.Background())
			updateLocalWifiPicker(&m, localWifiScanMsg{networks: []localWifiNetwork{{SSID: "Home"}}})
			if tc.filter != "" {
				updateLocalWifiPicker(&m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(tc.filter)})
			}
			quit := updateLocalWifiPicker(&m, tea.KeyMsg{Type: tc.key})
			if quit == nil {
				t.Fatal("expected picker to close")
			}
			if _, ok := quit().(tea.QuitMsg); !ok {
				t.Fatal("expected quit command")
			}
			sel, err := m.result()
			if tc.cancel {
				if !errors.Is(err, ErrUserCancelled) {
					t.Fatalf("expected clean cancellation, got %v", err)
				}
			} else if err != nil || sel.SSID != "" || !sel.HadNetworks {
				t.Fatalf("expected manual entry with known networks, got %+v, %v", sel, err)
			}
			if cmd := updateLocalWifiPicker(&m, localWifiRefreshMsg{}); cmd != nil {
				t.Fatal("closed picker started another scan")
			}
		})
	}
}

func TestLocalWifiPickerScanCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := newLocalWifiPickerModel(ctx)
	started := make(chan struct{})
	m.scan = func(scanCtx context.Context) ([]localWifiNetwork, error) {
		close(started)
		<-scanCtx.Done()
		return nil, scanCtx.Err()
	}
	finished := make(chan tea.Msg, 1)
	scan := m.scanCmd()
	go func() { finished <- scan() }()
	<-started
	updateLocalWifiPicker(&m, tea.KeyMsg{Type: tea.KeyCtrlC})
	cancel()
	msg := <-finished
	if !errors.Is(msg.(localWifiScanMsg).err, context.Canceled) {
		t.Fatalf("scan did not receive cancellation: %+v", msg)
	}
	if cmd := updateLocalWifiPicker(&m, msg); cmd != nil {
		t.Fatal("cancelled scan scheduled another refresh")
	}
}
