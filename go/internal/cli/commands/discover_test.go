package commands

import (
	"context"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/wendylabsinc/wendy/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/internal/shared/env"
	"github.com/wendylabsinc/wendy/internal/shared/models"
)

func TestEnvDiscoverIntervals(t *testing.T) {
	tests := []struct {
		name   string
		envKey string
		envVal string
		fn     func() time.Duration
		want   time.Duration
	}{
		{"usb default", "WENDY_DISCOVER_USB_INTERVAL", "", env.DiscoverUSBInterval, 3 * time.Second},
		{"usb custom", "WENDY_DISCOVER_USB_INTERVAL", "5s", env.DiscoverUSBInterval, 5 * time.Second},
		{"usb invalid", "WENDY_DISCOVER_USB_INTERVAL", "notaduration", env.DiscoverUSBInterval, 3 * time.Second},
		{"ethernet default", "WENDY_DISCOVER_ETHERNET_INTERVAL", "", env.DiscoverEthernetInterval, 3 * time.Second},
		{"ethernet custom", "WENDY_DISCOVER_ETHERNET_INTERVAL", "500ms", env.DiscoverEthernetInterval, 500 * time.Millisecond},
		{"external default", "WENDY_DISCOVER_EXTERNAL_INTERVAL", "", env.DiscoverExternalInterval, 5 * time.Second},
		{"external custom", "WENDY_DISCOVER_EXTERNAL_INTERVAL", "10s", env.DiscoverExternalInterval, 10 * time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(tt.envKey, tt.envVal)
			got := tt.fn()
			if got != tt.want {
				t.Errorf("got %v; want %v", got, tt.want)
			}
		})
	}
}

func TestDelayThen(t *testing.T) {
	called := false
	inner := func() tea.Msg {
		called = true
		return "done"
	}

	// With zero delay the inner cmd should execute immediately.
	cmd := delayThen(0, inner)
	msg := cmd()
	if !called {
		t.Fatal("inner cmd was not called")
	}
	if msg != "done" {
		t.Errorf("msg = %v; want \"done\"", msg)
	}
}

func TestDelayThen_ActuallyDelays(t *testing.T) {
	inner := func() tea.Msg { return "done" }

	start := time.Now()
	cmd := delayThen(50*time.Millisecond, inner)
	cmd()
	elapsed := time.Since(start)

	if elapsed < 40*time.Millisecond {
		t.Errorf("delayThen returned too fast (%v); expected >= 50ms delay", elapsed)
	}
}

func TestDiscoverModel_UpdateReturnsDelayedCmd(t *testing.T) {
	// Use zero intervals so the test doesn't actually sleep.
	t.Setenv("WENDY_DISCOVER_USB_INTERVAL", "0s")
	t.Setenv("WENDY_DISCOVER_ETHERNET_INTERVAL", "0s")
	t.Setenv("WENDY_DISCOVER_EXTERNAL_INTERVAL", "0s")

	m := newDiscoverModel(context.Background(), defaultOpts())

	// Each scan message type should return a non-nil command (the delayed rescan).
	cases := []struct {
		name string
		msg  tea.Msg
	}{
		{"usb", usbScanMsg{devices: []models.USBDevice{{DisplayName: "test"}}}},
		{"ethernet", ethScanMsg{devices: []models.EthernetInterface{{DisplayName: "eth0"}}}},
		{"external", extScanMsg{devices: []models.ExternalDevice{{DisplayName: "ext0"}}}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			updated, cmd := m.Update(tc.msg)
			um := updated.(discoverModel)
			if !um.hasResults {
				t.Error("expected hasResults = true after scan message")
			}
			if cmd == nil {
				t.Error("expected non-nil cmd (delayed rescan)")
			}
		})
	}
}

func TestDiscoverModel_QuitOnKeyMsg(t *testing.T) {
	m := newDiscoverModel(context.Background(), defaultOpts())

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	um := updated.(discoverModel)
	if !um.quitting {
		t.Error("expected quitting = true after 'q' key")
	}
	if cmd == nil {
		t.Error("expected non-nil quit cmd")
	}
}

func TestDiscoverModel_Init(t *testing.T) {
	m := newDiscoverModel(context.Background(), defaultOpts())
	cmd := m.Init()
	if cmd == nil {
		t.Error("expected non-nil Init cmd (batch of scan commands)")
	}
}

func defaultOpts() discovery.DiscoveryOptions {
	return discovery.DiscoveryOptions{Timeout: time.Second}
}
