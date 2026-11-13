package commands

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

// withTourCompletionsInstalled pins HOME to a temp dir and, when installed is
// true, seeds a config marking shell completions as installed. This makes the
// AI-onboarding → device-setup transition deterministic in tests, since it now
// interposes the completions phase when completions are missing.
func withTourCompletionsInstalled(t *testing.T, installed bool) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	if installed {
		if err := config.Save(&config.Config{CompletionInstalled: true}); err != nil {
			t.Fatalf("seed config: %v", err)
		}
	}
}

// stepTour runs one Update and returns the resulting model + command.
func stepTour(t *testing.T, m tourWizardModel, msg tea.Msg) (tourWizardModel, tea.Cmd) {
	t.Helper()
	next, cmd := m.Update(msg)
	tm, ok := next.(tourWizardModel)
	if !ok {
		t.Fatalf("Update returned %T, want tourWizardModel", next)
	}
	return tm, cmd
}

func TestTourAICheckRouting(t *testing.T) {
	t.Run("claude detected goes to AI step", func(t *testing.T) {
		m := newTourWizardModel()
		m.phase = phaseWelcome
		m, _ = stepTour(t, m, tourAICheckDoneMsg{claudePath: "/usr/bin/claude"})
		if m.phase != phaseAICheck {
			t.Fatalf("phase = %v, want phaseAICheck", m.phase)
		}
	})

	t.Run("codex detected goes to AI step", func(t *testing.T) {
		m := newTourWizardModel()
		m.phase = phaseWelcome
		m, _ = stepTour(t, m, tourAICheckDoneMsg{codexPath: "/usr/bin/codex"})
		if m.phase != phaseAICheck {
			t.Fatalf("phase = %v, want phaseAICheck", m.phase)
		}
	})

	t.Run("neither detected skips to device load", func(t *testing.T) {
		withTourCompletionsInstalled(t, true)
		m := newTourWizardModel()
		m.phase = phaseWelcome
		m, cmd := stepTour(t, m, tourAICheckDoneMsg{})
		if m.phase != phaseLoadDevices {
			t.Fatalf("phase = %v, want phaseLoadDevices", m.phase)
		}
		if cmd == nil {
			t.Fatal("expected loadDevicesCmd, got nil")
		}
	})
}

func TestTourAIStepTransitions(t *testing.T) {
	t.Run("skip leads to device load", func(t *testing.T) {
		withTourCompletionsInstalled(t, true)
		m := newTourWizardModel()
		m.phase = phaseAICheck
		m.codexPath = "/usr/bin/codex"
		m.menuCursor = 1 // "No, skip"
		m, cmd := stepTour(t, m, tea.KeyMsg{Type: tea.KeyEnter})
		if m.phase != phaseLoadDevices {
			t.Fatalf("phase = %v, want phaseLoadDevices", m.phase)
		}
		if cmd == nil {
			t.Fatal("expected loadDevicesCmd, got nil")
		}
	})

	t.Run("yes with codex only triggers MCP setup", func(t *testing.T) {
		m := newTourWizardModel()
		m.phase = phaseAICheck
		m.codexPath = "/usr/bin/codex" // no claude
		m.menuCursor = 0               // "Yes, set up MCP"
		m, cmd := stepTour(t, m, tea.KeyMsg{Type: tea.KeyEnter})
		if cmd == nil {
			t.Fatal("expected runMCPSetupCmd, got nil") // not executed — would write config
		}
		if m.phase != phaseAICheck {
			t.Fatalf("phase = %v, want phaseAICheck (waiting for setup result)", m.phase)
		}
	})

	t.Run("mcp results screen leads to device load", func(t *testing.T) {
		withTourCompletionsInstalled(t, true)
		m := newTourWizardModel()
		m.phase = phaseAIMCPSetup
		m, cmd := stepTour(t, m, tea.KeyMsg{Type: tea.KeyEnter})
		if m.phase != phaseLoadDevices {
			t.Fatalf("phase = %v, want phaseLoadDevices", m.phase)
		}
		if cmd == nil {
			t.Fatal("expected loadDevicesCmd, got nil")
		}
	})
}

func TestTourDeployWaitsForReadiness(t *testing.T) {
	m := newTourWizardModel()
	m.phase = phaseRunProject
	m, _ = stepTour(t, m, tourRunDoneMsg{})
	if m.phase != phaseSampleReadiness {
		t.Fatalf("phase = %v, want phaseSampleReadiness", m.phase)
	}
	m, _ = stepTour(t, m, tourSampleReadyMsg{})
	if m.phase != phaseCloud {
		t.Fatalf("phase = %v, want phaseCloud", m.phase)
	}
}

func TestParseNetshSSID(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "typical connected interface",
			in: `There is 1 interface on the system:

    Name                   : Wi-Fi
    Description            : Intel(R) Wi-Fi 6 AX201 160MHz
    GUID                   : abcdef01-2345-6789-abcd-ef0123456789
    Physical address       : 00:11:22:33:44:55
    State                  : connected
    SSID                   : MyHomeNetwork
    BSSID                  : aa:bb:cc:dd:ee:ff
    Network type           : Infrastructure
`,
			want: "MyHomeNetwork",
		},
		{
			name: "BSSID line alone does not match",
			in: `    Name                   : Wi-Fi
    BSSID                  : aa:bb:cc:dd:ee:ff
`,
			want: "",
		},
		{
			name: "SSID with spaces and punctuation",
			in: `    SSID                   : My Coffee Shop - 5G
    BSSID                  : aa:bb:cc:dd:ee:ff
`,
			want: "My Coffee Shop - 5G",
		},
		{
			name: "disconnected interface (empty SSID)",
			in: `    State                  : disconnected
    SSID                   :
`,
			want: "",
		},
		{
			name: "no SSID line",
			in: `    State                  : disconnected
`,
			want: "",
		},
		{
			name: "empty output",
			in:   ``,
			want: "",
		},
		{
			name: "SSID label not followed by colon is skipped",
			in: `    SSIDLike  not a real label
    SSID                   : RealNetwork
`,
			want: "RealNetwork",
		},
		{
			name: "first SSID wins when multiple present",
			in: `    SSID                   : First
    SSID                   : Second
`,
			want: "First",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseNetshSSID(c.in)
			if got != c.want {
				t.Fatalf("parseNetshSSID(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}
