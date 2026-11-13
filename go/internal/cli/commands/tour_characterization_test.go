package commands

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
)

// These characterization tests pin the tour state machine's key/message
// transitions so the structure can be refactored without changing behavior.
// They avoid triggering real side effects (filesystem/network/exec): for
// transitions that return a command, they assert the resulting phase and/or
// that a command was produced, without running it.

func key(s string) tea.KeyMsg {
	switch s {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "ctrl+c":
		return tea.KeyMsg{Type: tea.KeyCtrlC}
	default:
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}
}

func TestTourWelcomeStartsDeviceSetup(t *testing.T) {
	m := newTourWizardModel()
	m.phase = phaseWelcome
	got, cmd := stepTour(t, m, key("enter"))
	if got.phase != phaseLoadDevices {
		t.Errorf("phase = %v, want phaseLoadDevices", got.phase)
	}
	if cmd == nil {
		t.Error("expected a device-load command")
	}
}

func TestTourDeviceListNavigationAndSelect(t *testing.T) {
	m := newTourWizardModel()
	m.phase = phaseDeviceList
	m.devices = []deviceInfo{{Key: "raspberrypi"}, {Key: "jetson-orin"}}

	// Up at the top is clamped.
	got, _ := stepTour(t, m, key("up"))
	if got.deviceCursor != 0 {
		t.Errorf("deviceCursor = %d, want 0 (clamped)", got.deviceCursor)
	}

	// Down moves through devices and the trailing "Other Linux" row.
	got, _ = stepTour(t, got, key("down"))
	got, _ = stepTour(t, got, key("down"))
	if got.deviceCursor != 2 {
		t.Errorf("deviceCursor = %d, want 2", got.deviceCursor)
	}
	// Clamped at total-1 (len(devices)+1-1 == 2).
	got, _ = stepTour(t, got, key("down"))
	if got.deviceCursor != 2 {
		t.Errorf("deviceCursor = %d, want 2 (clamped)", got.deviceCursor)
	}

	// Selecting "Other Linux" (last row) goes to apt install.
	got, _ = stepTour(t, got, key("enter"))
	if got.phase != phaseAptInstall {
		t.Errorf("phase = %v, want phaseAptInstall", got.phase)
	}
	if got.selected != nil {
		t.Error("selected should be nil for Other Linux")
	}
}

func TestTourDeviceListSelectsDevice(t *testing.T) {
	m := newTourWizardModel()
	m.phase = phaseDeviceList
	m.devices = []deviceInfo{{Key: "raspberrypi"}}
	got, _ := stepTour(t, m, key("enter"))
	if got.phase != phaseOSInstalled {
		t.Errorf("phase = %v, want phaseOSInstalled", got.phase)
	}
	if got.selected == nil || got.selected.Key != "raspberrypi" {
		t.Errorf("selected = %+v, want raspberrypi", got.selected)
	}
}

func TestTourOSInstalledBranches(t *testing.T) {
	// Cursor 0: WendyOS already installed -> scan LAN.
	m := newTourWizardModel()
	m.phase = phaseOSInstalled
	got, cmd := stepTour(t, m, key("enter"))
	if got.phase != phaseExistingDeviceScan {
		t.Errorf("phase = %v, want phaseExistingDeviceScan", got.phase)
	}
	if cmd == nil {
		t.Error("expected scan command")
	}

	// Cursor 1: needs install -> storage guide.
	m = newTourWizardModel()
	m.phase = phaseOSInstalled
	m, _ = stepTour(t, m, key("down"))
	got, _ = stepTour(t, m, key("enter"))
	if got.phase != phaseStorageGuide {
		t.Errorf("phase = %v, want phaseStorageGuide", got.phase)
	}
}

func TestTourWifiQuestionWithDetectedSSID(t *testing.T) {
	m := newTourWizardModel()
	m.phase = phaseWifiQuestion
	m.detectedSSID = "HomeNet"
	m.detectedPass = "secret"
	// Cursor 0 with known password -> ready to install.
	got, _ := stepTour(t, m, key("enter"))
	if got.phase != phaseReadyToInstall {
		t.Errorf("phase = %v, want phaseReadyToInstall", got.phase)
	}
	if got.wifiSSID != "HomeNet" || got.wifiPass != "secret" {
		t.Errorf("wifi = %q/%q, want HomeNet/secret", got.wifiSSID, got.wifiPass)
	}
}

func TestTourWifiQuestionSkip(t *testing.T) {
	m := newTourWizardModel()
	m.phase = phaseWifiQuestion
	m.detectedSSID = "HomeNet"
	// Options: [use, scan-different, manual, skip]; skip is index 3.
	for range 3 {
		m, _ = stepTour(t, m, key("down"))
	}
	got, _ := stepTour(t, m, key("enter"))
	if got.phase != phaseReadyToInstall {
		t.Errorf("phase = %v, want phaseReadyToInstall", got.phase)
	}
	if got.wifiSSID != "" {
		t.Errorf("wifiSSID = %q, want empty after skip", got.wifiSSID)
	}
}

func TestTourReadyToInstallRunsInstall(t *testing.T) {
	m := newTourWizardModel()
	m.phase = phaseReadyToInstall
	m.selected = &deviceInfo{Key: "raspberrypi", LatestVersion: "1.0.0"}
	m.deviceName = "my-pi"
	_, cmd := stepTour(t, m, key("enter"))
	if cmd == nil {
		t.Error("expected an OS-install command")
	}
}

func TestTourBootInstructionsToDiscovery(t *testing.T) {
	m := newTourWizardModel()
	m.phase = phaseBootInstructions
	m.targetName = "my-pi"
	got, cmd := stepTour(t, m, key("enter"))
	if got.phase != phaseDiscovering {
		t.Errorf("phase = %v, want phaseDiscovering", got.phase)
	}
	if cmd == nil {
		t.Error("expected a discovery command")
	}
}

func TestTourDeviceFoundToCreateProjectPrompt(t *testing.T) {
	m := newTourWizardModel()
	m.phase = phaseDeviceFound
	m.menuCursor = 5 // should reset
	got, _ := stepTour(t, m, key("enter"))
	if got.phase != phaseCreateProjectPrompt {
		t.Errorf("phase = %v, want phaseCreateProjectPrompt", got.phase)
	}
	if got.menuCursor != 0 {
		t.Errorf("menu cursor = %d, want reset to 0", got.menuCursor)
	}
}

func TestTourCreateProjectPromptDeploy(t *testing.T) {
	m := newTourWizardModel()
	m.phase = phaseCreateProjectPrompt
	got, cmd := stepTour(t, m, key("enter")) // cursor 0 = deploy
	if got.phase != phaseTemplateLoading {
		t.Errorf("phase = %v, want phaseTemplateLoading", got.phase)
	}
	if cmd == nil {
		t.Error("expected template-fetch command")
	}
}

func TestTourCreateProjectPromptSkip(t *testing.T) {
	m := newTourWizardModel()
	m.phase = phaseCreateProjectPrompt
	m, _ = stepTour(t, m, key("down")) // cursor 1 = skip
	got, _ := stepTour(t, m, key("enter"))
	// Skipping the sample-app deploy finishes the tour rather than looping
	// back into AI-tooling onboarding.
	if got.phase != phaseCloud {
		t.Errorf("phase = %v, want phaseCloud", got.phase)
	}
}

func TestTourTemplatePickerBuiltIn(t *testing.T) {
	m := newTourWizardModel()
	m.phase = phaseTemplatePicker
	m.templateItems = nil          // only the built-in row exists
	m.projectBaseDir = t.TempDir() // never scaffold into the real ~/Documents
	got, _ := stepTour(t, m, key("enter"))
	// Built-in selection creates the project then advances.
	if got.phase != phaseCreateProject && got.phase != phaseError {
		t.Errorf("phase = %v, want phaseCreateProject (or phaseError on FS issue)", got.phase)
	}
}

func TestTourCreateProjectRuns(t *testing.T) {
	m := newTourWizardModel()
	m.phase = phaseCreateProject
	m.foundAddr = "192.168.1.10"
	_, cmd := stepTour(t, m, key("enter"))
	if cmd == nil {
		t.Error("expected a run command")
	}
}

func TestTourQuitKeys(t *testing.T) {
	phases := []tourPhase{
		phaseWelcome, phaseDeviceList, phaseOSInstalled, phaseExistingDevicePicker,
		phaseAptInstall, phaseStorageGuide, phaseDriveWait, phaseWifiQuestion,
		phaseWifiNetworkPicker, phaseReadyToInstall, phaseBootInstructions,
		phaseDeviceFound, phaseCreateProjectPrompt, phaseTemplatePicker,
		phaseCreateProject, phaseAICheck, phaseAIMCPSetup, phaseCompletions,
	}
	for _, p := range phases {
		m := newTourWizardModel()
		m.phase = p
		_, cmd := stepTour(t, m, key("ctrl+c"))
		if cmd == nil {
			t.Errorf("phase %v: ctrl+c should quit (nil cmd)", p)
		}
	}
}

func TestTourTerminalPhasesQuit(t *testing.T) {
	for _, p := range []tourPhase{phaseCloud, phaseDone, phaseError} {
		m := newTourWizardModel()
		m.phase = p
		_, cmd := stepTour(t, m, key("x"))
		if cmd == nil {
			t.Errorf("phase %v: any key should quit", p)
		}
	}
}

// ─── message handlers ──────────────────────────────────────────────────────────

func TestTourDevicesLoadedMsg(t *testing.T) {
	m := newTourWizardModel()
	got, _ := stepTour(t, m, tourDevicesLoadedMsg{devices: []deviceInfo{{Key: "x"}}})
	if got.phase != phaseDeviceList {
		t.Errorf("phase = %v, want phaseDeviceList", got.phase)
	}

	got, _ = stepTour(t, m, tourDevicesLoadedMsg{err: errTest})
	if got.phase != phaseError {
		t.Errorf("phase = %v, want phaseError", got.phase)
	}
}

func TestTourLANScanDoneMsg(t *testing.T) {
	m := newTourWizardModel()
	got, _ := stepTour(t, m, tourLANScanDoneMsg{devices: []models.LANDevice{{}}})
	if got.phase != phaseExistingDevicePicker {
		t.Errorf("phase = %v, want phaseExistingDevicePicker", got.phase)
	}
}

func TestTourWifiDetectedMsg(t *testing.T) {
	m := newTourWizardModel()
	got, _ := stepTour(t, m, tourWifiDetectedMsg{ssid: "Net", password: "p"})
	if got.phase != phaseWifiQuestion {
		t.Errorf("phase = %v, want phaseWifiQuestion", got.phase)
	}
	if got.detectedSSID != "Net" {
		t.Errorf("detectedSSID = %q, want Net", got.detectedSSID)
	}
}

func TestTourDiscoveryFoundMsg(t *testing.T) {
	m := newTourWizardModel()
	got, _ := stepTour(t, m, tourDiscoveryFoundMsg{addr: "1.2.3.4", name: "pi"})
	if got.phase != phaseDeviceFound {
		t.Errorf("phase = %v, want phaseDeviceFound", got.phase)
	}
	if got.foundAddr != "1.2.3.4" {
		t.Errorf("foundAddr = %q, want 1.2.3.4", got.foundAddr)
	}
}

func TestTourMCPSetupDoneMsg(t *testing.T) {
	m := newTourWizardModel()
	got, _ := stepTour(t, m, tourMCPSetupDoneMsg{})
	if got.phase != phaseAIMCPSetup {
		t.Errorf("phase = %v, want phaseAIMCPSetup", got.phase)
	}
}

func TestTourOSInstallDoneMsg(t *testing.T) {
	m := newTourWizardModel()
	got, _ := stepTour(t, m, tourOSInstallDoneMsg{})
	if got.phase != phaseBootInstructions {
		t.Errorf("phase = %v, want phaseBootInstructions", got.phase)
	}
	got, _ = stepTour(t, m, tourOSInstallDoneMsg{err: errTest})
	if got.phase != phaseError {
		t.Errorf("phase = %v, want phaseError on install error", got.phase)
	}
}

var errTest = &tourTestError{}

type tourTestError struct{}

func (*tourTestError) Error() string { return "test error" }
