package commands

import (
	"errors"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

func TestDevicePinRefusalRecoveryPresentation(t *testing.T) {
	profile := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(profile) })
	readPins := writePinTestConfig(t, map[string]config.DevicePin{
		"wendyos-ccr1": {OrgID: 2, CloudGRPC: "cloud.example:443", AssetID: "481"},
	})
	var err error
	out := captureStderr(t, func() {
		err = enforceDeviceIdentity("wendyos-ccr1.local", observedDeviceIdentity{})
	})
	if out != "" {
		t.Fatalf("refusal printed before the CLI rendered it: %q", out)
	}
	if !errors.Is(err, errDeviceIdentityRefused) || !blocksUnauthenticatedFallback(err) {
		t.Fatalf("refusal must still block unauthenticated fallback: %v", err)
	}
	if pin := readPins()["wendyos-ccr1"]; pin.OrgID != 2 || pin.AssetID != "481" {
		t.Fatalf("refusal changed the saved pin: %+v", pin)
	}
	msg := err.Error()
	command := "\n  wendy device unpin wendyos-ccr1.local\n"
	for _, want := range []string{
		"local pin", "intentionally unenrolled", "changed this device's organization",
		command, "retry your command", "only clears this CLI", "keep the pin", "wendy auth login",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal missing %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "\x1b") {
		t.Fatalf("programmatic error contains terminal styling: %q", msg)
	}
	if strings.Index(msg, command) > strings.Index(msg, "Saved:") {
		t.Errorf("recovery command is buried after certificate details:\n%s", msg)
	}
	var diagnostic interface{ CLIMessage() string }
	if !errors.As(err, &diagnostic) {
		t.Fatalf("refusal has no CLI presentation: %v", err)
	}
	rendered := diagnostic.CLIMessage()
	if got := ansi.Strip(rendered); got != "✗ "+msg {
		t.Errorf("CLI and programmatic recovery guidance differ:\n%s\n%s", got, msg)
	}
	if strings.Count(rendered, "✗") != 1 {
		t.Errorf("want one error heading:\n%s", rendered)
	}
	lines := strings.Split(rendered, "\n")
	if !strings.Contains(lines[0], "\x1b[") {
		t.Error("test precondition: heading must be colored")
	}
	for _, line := range lines[1:] {
		plain := ansi.Strip(line)
		if strings.HasPrefix(plain, "  wendy device unpin ") || strings.HasPrefix(plain, "Saved:") || strings.HasPrefix(plain, "Now:") {
			continue
		}
		if strings.Contains(line, "\x1b[") {
			t.Errorf("recovery prose should be plain: %q", line)
		}
	}
	t.Log("\n" + rendered)
}
