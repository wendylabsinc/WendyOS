//go:build darwin || linux || windows

package commands

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/internal/cli/tui"
	"github.com/wendylabsinc/wendy/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/internal/shared/models"
)

// ─── phases ──────────────────────────────────────────────────────────────────

type tourPhase int

const (
	phaseWelcome        tourPhase = iota
	phaseLoadDevices              // spinner while fetching manifest
	phaseDeviceList               // arrow-key device picker
	phaseOSInstalled              // "Is WendyOS installed?" (supported devices)
	phaseExistingDeviceScan       // spinner while scanning for already-running devices
	phaseExistingDevicePicker     // pick a discovered device from the list
	phaseEnterHostname            // ask hostname for "Other Linux" apt path
	phaseAptInstall               // apt install instructions
	phaseStorageGuide             // NVMe vs SD guide
	phaseDriveWait                // refreshing drive table
	phaseDeviceName               // text input for device name
	phaseWifiDetect               // async: detect current SSID
	phaseWifiQuestion             // "Use [SSID]?" or options menu
	phaseWifiScanLoading          // spinner while scanning nearby networks
	phaseWifiNetworkPicker        // pick from scanned networks
	phaseWifiPassword             // enter password for detected/chosen SSID
	phaseWifiManualSSID           // enter SSID manually
	phaseReadyToInstall           // summary before install
	phaseInstalling               // tea.ExecProcess running wendy os install
	phaseBootInstructions         // how to boot the device
	phaseDiscovering              // poll mDNS for target device name
	phaseDeviceFound              // device came online
	phaseCreateProject            // write Python project files
	phaseRunProject               // tea.ExecProcess running wendy run
	phaseAICheck                  // check claude/codex installation
	phaseCloud                    // cloud ready message
	phaseDone                     // quit
	phaseError                    // error with restart hint
)

// ─── messages ────────────────────────────────────────────────────────────────

type (
	tourDevicesLoadedMsg  struct{ devices []deviceInfo; err error }
	tourLANScanDoneMsg    struct{ devices []models.LANDevice; err error }
	tourWifiDetectedMsg   struct{ ssid, password string }
	tourWifiScanDoneMsg   struct{ networks []localWifiNetwork }
	tourDriveRescanMsg    struct{}
	tourDiscoveryTickMsg  struct{}
	tourDiscoveryFoundMsg struct{ addr, name string }
	tourOSInstallDoneMsg  struct{ err error }
	tourRunDoneMsg        struct{ err error }
	tourAICheckDoneMsg    struct{ claudePath, codexPath string }
)

// ─── Python project templates ─────────────────────────────────────────────────

const tourWendyJSONTemplate = `{
    "appId": %q,
    "version": "1.0.0",
    "language": "python",
    "entitlements": [
        {
            "type": "network",
            "mode": "host"
        }
    ],
    "python": {
        "container": {
            "sourceRoot": "/app"
        }
    }
}
`

const tourAppPy = `#!/usr/bin/env python3
"""
Wendy Hello World — a simple HTTP server running on your edge device.
Edit this file and run 'wendy run' to redeploy instantly.
"""
from http.server import HTTPServer, BaseHTTPRequestHandler
import os, socket

class HelloHandler(BaseHTTPRequestHandler):
    def do_GET(self):
        hostname = socket.gethostname()
        self.send_response(200)
        self.send_header("Content-type", "text/html; charset=utf-8")
        self.end_headers()
        self.wfile.write(f"""<!DOCTYPE html>
<html lang="en"><head><meta charset="utf-8">
<title>Hello from {hostname}</title>
<style>
  body {{ font-family: sans-serif; max-width: 600px; margin: 4rem auto; text-align: center }}
  h1 {{ color: #10b981 }}
  code {{ background: #f0fdf4; padding: .2rem .4rem; border-radius: .25rem }}
</style></head>
<body>
  <h1>Hello from {hostname}!</h1>
  <p>Your Wendy edge device is running.</p>
  <p>Edit <code>app.py</code> and run <code>wendy run</code> to redeploy.</p>
</body></html>""".encode())

    def log_message(self, fmt, *args):
        print(f"[{{self.date_time_string()}}] {{fmt % args}}", flush=True)

if __name__ == "__main__":
    port = int(os.environ.get("PORT", 8000))
    addr = ("0.0.0.0", port)
    print(f"Serving on http://0.0.0.0:{{port}}", flush=True)
    HTTPServer(addr, HelloHandler).serve_forever()
`

const tourDockerfile = `FROM python:3.11-slim
WORKDIR /app
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt
COPY app.py .
EXPOSE 8000
ENV PORT=8000
CMD ["python", "app.py"]
`

const tourRequirements = "debugpy\n"

// ─── model ───────────────────────────────────────────────────────────────────

type tourWizardModel struct {
	phase  tourPhase
	width  int
	height int
	err    error

	// device picker
	devices      []deviceInfo
	deviceCursor int
	selected     *deviceInfo // nil = Other Linux
	useNVMe      bool

	// drive table
	drives      []drive
	driveCursor int
	selDrive    *drive

	// embedded text input (reused across input phases)
	input    textinput.Model
	inputVal string

	// existing-device scan (pre-installed path)
	lanDevices []models.LANDevice
	lanCursor  int

	// WiFi
	detectedSSID string
	detectedPass string
	wifiSSID     string
	wifiPass     string
	wifiCursor   int              // options menu cursor
	scanNetworks []localWifiNetwork // results from scanLocalWifiNetworks
	scanCursor   int

	// device name
	deviceName string

	// hostname for "already installed" / apt path
	hostname string

	// discovery
	targetName string // DisplayName to match
	foundAddr  string
	foundName  string

	// project
	projectPath string
	projectID   string

	// AI tools
	claudePath string
	codexPath  string
}

func newTourWizardModel() tourWizardModel {
	ti := textinput.New()
	ti.CharLimit = 128
	ti.Width = 48
	ti.PromptStyle = lipgloss.NewStyle().Foreground(tui.ColorPrimary)
	ti.Cursor.Style = lipgloss.NewStyle().Foreground(tui.ColorPrimary)
	return tourWizardModel{input: ti}
}

// ─── styles ───────────────────────────────────────────────────────────────────

var (
	wizTitleStyle    = lipgloss.NewStyle().Bold(true).Foreground(tui.ColorPrimary)
	wizSubStyle      = lipgloss.NewStyle().Foreground(tui.ColorDim)
	wizBodyStyle     = lipgloss.NewStyle()
	wizCodeStyle     = lipgloss.NewStyle().Foreground(tui.Emerald300).Bold(true)
	wizHintStyle     = lipgloss.NewStyle().Foreground(tui.ColorDim)
	wizNoticeStyle   = lipgloss.NewStyle().Foreground(tui.ColorNotice)
	wizSuccessStyle  = lipgloss.NewStyle().Foreground(tui.ColorPrimary).Bold(true)
	wizSelectedStyle = lipgloss.NewStyle().Foreground(tui.ColorSelectedFg).Background(tui.ColorSelectedBg).Padding(0, 1)
	wizNormalStyle   = lipgloss.NewStyle().Foreground(tui.ColorDim).Padding(0, 1)
	wizErrorStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("#ef4444")).Bold(true)
	wizBorderStyle   = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(tui.ColorBorder).
				Padding(1, 3)
)

// ─── Init / Update / View ─────────────────────────────────────────────────────

func (m tourWizardModel) Init() tea.Cmd {
	return nil
}

func (m tourWizardModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) { //nolint:cyclop
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case tourDevicesLoadedMsg:
		if msg.err != nil {
			m.err = msg.err
			m.phase = phaseError
			return m, nil
		}
		m.devices = msg.devices
		m.phase = phaseDeviceList
		return m, nil

	case tourLANScanDoneMsg:
		m.lanDevices = msg.devices
		m.lanCursor = 0
		m.phase = phaseExistingDevicePicker
		return m, nil

	case tourWifiDetectedMsg:
		m.detectedSSID = msg.ssid
		m.detectedPass = msg.password
		m.phase = phaseWifiQuestion
		m.wifiCursor = 0
		return m, nil

	case tourWifiScanDoneMsg:
		m.scanNetworks = msg.networks
		m.scanCursor = 0
		m.phase = phaseWifiNetworkPicker
		return m, nil

	case tourDriveRescanMsg:
		if m.phase == phaseDriveWait {
			drives, _ := listExternalDrives()
			m.drives = drives
			return m, rescanDrivesAfter(2 * time.Second)
		}
		return m, nil

	case tourDiscoveryTickMsg:
		if m.phase == phaseDiscovering {
			return m, m.cmdDiscoveryCheck()
		}
		return m, nil

	case tourDiscoveryFoundMsg:
		m.foundAddr = msg.addr
		m.foundName = msg.name
		m.phase = phaseDeviceFound
		return m, nil

	case tourOSInstallDoneMsg:
		if msg.err != nil {
			m.err = msg.err
			m.phase = phaseError
			return m, nil
		}
		m.phase = phaseBootInstructions
		return m, nil

	case tourRunDoneMsg:
		m.phase = phaseAICheck
		return m, m.cmdCheckAITools()

	case tourAICheckDoneMsg:
		m.claudePath = msg.claudePath
		m.codexPath = msg.codexPath
		m.phase = phaseAICheck
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m tourWizardModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Text input phases route keys to the embedded textinput.
	switch m.phase {
	case phaseDeviceName, phaseWifiPassword, phaseWifiManualSSID, phaseEnterHostname:
		return m.handleTextInput(msg)
	}

	key := msg.String()

	switch m.phase {
	case phaseWelcome:
		switch key {
		case "enter", " ":
			m.phase = phaseLoadDevices
			return m, loadDevicesCmd()
		case "q", "ctrl+c":
			return m, tea.Quit
		}

	case phaseLoadDevices:
		if key == "ctrl+c" {
			return m, tea.Quit
		}

	case phaseDeviceList:
		total := len(m.devices) + 1 // +1 for "Other Linux"
		switch key {
		case "up", "k":
			if m.deviceCursor > 0 {
				m.deviceCursor--
			}
		case "down", "j":
			if m.deviceCursor < total-1 {
				m.deviceCursor++
			}
		case "enter", " ":
			if m.deviceCursor < len(m.devices) {
				dev := m.devices[m.deviceCursor]
				m.selected = &dev
				m.useNVMe = deviceUsesNVMe(dev.Key)
				m.phase = phaseOSInstalled
				m.wifiCursor = 0
			} else {
				m.selected = nil
				m.phase = phaseAptInstall
			}
		case "q", "ctrl+c":
			return m, tea.Quit
		}

	case phaseOSInstalled:
		switch key {
		case "up", "k":
			if m.wifiCursor > 0 {
				m.wifiCursor--
			}
		case "down", "j":
			if m.wifiCursor < 1 {
				m.wifiCursor++
			}
		case "enter", " ":
			if m.wifiCursor == 0 {
				// WendyOS already installed — scan the network
				m.phase = phaseExistingDeviceScan
				return m, scanLANDevicesCmd()
			} else {
				// Need to install
				m.phase = phaseStorageGuide
			}
		case "q", "ctrl+c":
			return m, tea.Quit
		}

	case phaseExistingDeviceScan:
		if key == "ctrl+c" {
			return m, tea.Quit
		}

	case phaseExistingDevicePicker:
		switch key {
		case "up", "k":
			if m.lanCursor > 0 {
				m.lanCursor--
			}
		case "down", "j":
			if m.lanCursor < len(m.lanDevices) {
				m.lanCursor++
			}
		case "r":
			// re-scan
			m.phase = phaseExistingDeviceScan
			return m, scanLANDevicesCmd()
		case "enter", " ":
			if m.lanCursor < len(m.lanDevices) {
				dev := m.lanDevices[m.lanCursor]
				m.foundAddr = preferredLANAddress(dev)
				m.foundName = dev.DisplayName
				m.targetName = strings.TrimSuffix(dev.Hostname, ".local")
				m.phase = phaseDeviceFound
			} else {
				// "Enter manually" option at bottom of list
				m.phase = phaseEnterHostname
				m.input.Placeholder = "e.g. my-pi or 192.168.1.100"
				m.input.SetValue("")
				m.input.Focus()
			}
		case "q", "ctrl+c":
			return m, tea.Quit
		}

	case phaseAptInstall:
		switch key {
		case "enter", " ":
			m.phase = phaseEnterHostname
			m.input.Placeholder = "e.g. 192.168.1.50 or my-device"
			m.input.SetValue("")
			m.input.Focus()
		case "q", "ctrl+c":
			return m, tea.Quit
		}

	case phaseStorageGuide:
		switch key {
		case "enter", " ":
			m.phase = phaseDriveWait
			drives, _ := listExternalDrives()
			m.drives = drives
			m.driveCursor = 0
			return m, rescanDrivesAfter(2 * time.Second)
		case "q", "ctrl+c":
			return m, tea.Quit
		}

	case phaseDriveWait:
		switch key {
		case "up", "k":
			if m.driveCursor > 0 {
				m.driveCursor--
			}
		case "down", "j":
			if m.driveCursor < len(m.drives)-1 {
				m.driveCursor++
			}
		case "enter", " ":
			if len(m.drives) > 0 && m.driveCursor < len(m.drives) {
				d := m.drives[m.driveCursor]
				m.selDrive = &d
				m.phase = phaseDeviceName
				m.input.Placeholder = "e.g. my-pi (lowercase, hyphens ok)"
				m.input.SetValue("")
				m.input.Focus()
			}
		case "q", "ctrl+c":
			return m, tea.Quit
		}

	case phaseWifiQuestion:
		opts := wifiQuestionOptions(m.detectedSSID)
		switch key {
		case "up", "k":
			if m.wifiCursor > 0 {
				m.wifiCursor--
			}
		case "down", "j":
			if m.wifiCursor < len(opts)-1 {
				m.wifiCursor++
			}
		case "enter", " ":
			switch m.wifiCursor {
			case 0:
				if m.detectedSSID != "" {
					// "Yes, use [detectedSSID]"
					m.wifiSSID = m.detectedSSID
					if m.detectedPass != "" {
						m.wifiPass = m.detectedPass
						m.phase = phaseReadyToInstall
					} else {
						m.phase = phaseWifiPassword
						m.input.Placeholder = "WiFi password (leave empty for open network)"
						m.input.EchoMode = textinput.EchoPassword
						m.input.SetValue("")
						m.input.Focus()
					}
				} else {
					// "Scan for nearby networks"
					m.phase = phaseWifiScanLoading
					return m, scanWifiCmd()
				}
			case 1:
				if m.detectedSSID != "" {
					// "Scan for a different network"
					m.phase = phaseWifiScanLoading
					return m, scanWifiCmd()
				} else {
					// "Enter WiFi credentials manually"
					m.phase = phaseWifiManualSSID
					m.input.Placeholder = "WiFi network name (SSID)"
					m.input.EchoMode = textinput.EchoNormal
					m.input.SetValue("")
					m.input.Focus()
				}
			case 2:
				if m.detectedSSID != "" {
					// "Enter WiFi credentials manually"
					m.phase = phaseWifiManualSSID
					m.input.Placeholder = "WiFi network name (SSID)"
					m.input.EchoMode = textinput.EchoNormal
					m.input.SetValue("")
					m.input.Focus()
				} else {
					// "Skip WiFi setup"
					m.wifiSSID = ""
					m.wifiPass = ""
					m.phase = phaseReadyToInstall
				}
			case 3:
				// "Skip WiFi setup" (only when detectedSSID != "")
				m.wifiSSID = ""
				m.wifiPass = ""
				m.phase = phaseReadyToInstall
			}
		case "q", "ctrl+c":
			return m, tea.Quit
		}

	case phaseWifiScanLoading:
		if key == "ctrl+c" {
			return m, tea.Quit
		}

	case phaseWifiNetworkPicker:
		switch key {
		case "up", "k":
			if m.scanCursor > 0 {
				m.scanCursor--
			}
		case "down", "j":
			if m.scanCursor < len(m.scanNetworks)-1 {
				m.scanCursor++
			}
		case "enter", " ":
			if len(m.scanNetworks) > 0 && m.scanCursor < len(m.scanNetworks) {
				net := m.scanNetworks[m.scanCursor]
				m.wifiSSID = net.SSID
				if supportsKeychainLookup {
					if pwd, err := lookupKeychainPassword(net.SSID); err == nil && pwd != "" {
						m.wifiPass = pwd
						m.phase = phaseReadyToInstall
						return m, nil
					}
				}
				m.phase = phaseWifiPassword
				m.input.Placeholder = "WiFi password (leave empty for open network)"
				m.input.EchoMode = textinput.EchoPassword
				m.input.SetValue("")
				m.input.Focus()
			}
		case "q", "ctrl+c":
			return m, tea.Quit
		}

	case phaseReadyToInstall:
		switch key {
		case "enter", " ":
			return m, m.cmdRunOSInstall()
		case "q", "ctrl+c":
			return m, tea.Quit
		}

	case phaseBootInstructions:
		switch key {
		case "enter", " ":
			m.phase = phaseDiscovering
			return m, m.cmdDiscoveryCheck()
		case "q", "ctrl+c":
			return m, tea.Quit
		}

	case phaseDeviceFound:
		switch key {
		case "enter", " ":
			m.phase = phaseCreateProject
			err := m.createPythonProject()
			if err != nil {
				m.err = err
				m.phase = phaseError
				return m, nil
			}
			return m, nil
		case "q", "ctrl+c":
			return m, tea.Quit
		}

	case phaseCreateProject:
		switch key {
		case "enter", " ":
			return m, m.cmdRunProject()
		case "q", "ctrl+c":
			return m, tea.Quit
		}

	case phaseAICheck:
		switch key {
		case "enter", " ":
			m.phase = phaseCloud
		case "q", "ctrl+c":
			return m, tea.Quit
		}

	case phaseCloud, phaseDone:
		switch key {
		case "enter", " ", "q", "ctrl+c":
			return m, tea.Quit
		}

	case phaseError:
		switch key {
		case "enter", " ", "q", "ctrl+c":
			return m, tea.Quit
		}

	default:
		if key == "ctrl+c" {
			return m, tea.Quit
		}
	}

	return m, nil
}

// handleTextInput routes key events to the embedded textinput and advances the
// phase when Enter is pressed with a valid value.
func (m tourWizardModel) handleTextInput(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "enter":
		val := strings.TrimSpace(m.input.Value())
		switch m.phase {
		case phaseDeviceName:
			if err := validateDeviceNameTour(val); err == nil {
				m.deviceName = val
				m.targetName = val
				m.phase = phaseWifiDetect
				return m, detectWifiCmd()
			}
		case phaseEnterHostname:
			if val != "" {
				m.hostname = val
				m.targetName = val
				if m.selected != nil {
					// "already installed" path: skip OS install, go discover
					m.phase = phaseBootInstructions
				} else {
					// apt path: instructions already shown
					m.phase = phaseBootInstructions
				}
			}
		case phaseWifiPassword:
			m.wifiPass = val
			m.phase = phaseReadyToInstall
		case phaseWifiManualSSID:
			if val != "" {
				m.wifiSSID = val
				m.phase = phaseWifiPassword
				m.input.Placeholder = "WiFi password (leave empty for open network)"
				m.input.EchoMode = textinput.EchoPassword
				m.input.SetValue("")
				m.input.Focus()
				var cmd tea.Cmd
				m.input, cmd = m.input.Update(msg)
				return m, cmd
			}
		}
		return m, nil
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

// ─── View ─────────────────────────────────────────────────────────────────────

func (m tourWizardModel) View() string {
	w := m.width
	if w == 0 {
		w = 82
	}
	inner := w - 10
	if inner < 40 {
		inner = 40
	}

	var body string
	switch m.phase {
	case phaseWelcome:
		body = m.viewWelcome(inner)
	case phaseLoadDevices:
		body = m.viewLoading(inner)
	case phaseDeviceList:
		body = m.viewDeviceList(inner)
	case phaseOSInstalled:
		body = m.viewOSInstalled(inner)
	case phaseExistingDeviceScan:
		body = m.viewExistingDeviceScan(inner)
	case phaseExistingDevicePicker:
		body = m.viewExistingDevicePicker(inner)
	case phaseEnterHostname:
		body = m.viewEnterHostname(inner)
	case phaseAptInstall:
		body = m.viewAptInstall(inner)
	case phaseStorageGuide:
		body = m.viewStorageGuide(inner)
	case phaseDriveWait:
		body = m.viewDriveWait(inner)
	case phaseDeviceName:
		body = m.viewDeviceName(inner)
	case phaseWifiDetect:
		body = m.viewWifiDetect(inner)
	case phaseWifiQuestion:
		body = m.viewWifiQuestion(inner)
	case phaseWifiScanLoading:
		body = m.viewWifiScanLoading(inner)
	case phaseWifiNetworkPicker:
		body = m.viewWifiNetworkPicker(inner)
	case phaseWifiPassword, phaseWifiManualSSID:
		body = m.viewTextInput(inner)
	case phaseReadyToInstall:
		body = m.viewReadyToInstall(inner)
	case phaseInstalling:
		body = wizSubStyle.Render("Installing WendyOS...")
	case phaseBootInstructions:
		body = m.viewBootInstructions(inner)
	case phaseDiscovering:
		body = m.viewDiscovering(inner)
	case phaseDeviceFound:
		body = m.viewDeviceFound(inner)
	case phaseCreateProject:
		body = m.viewCreateProject(inner)
	case phaseRunProject:
		body = wizSubStyle.Render("Running your project on the device…")
	case phaseAICheck:
		body = m.viewAICheck(inner)
	case phaseCloud:
		body = m.viewCloud(inner)
	case phaseError:
		body = m.viewError(inner)
	case phaseDone:
		return ""
	default:
		body = ""
	}

	return wizBorderStyle.Width(inner).Render(body) + "\n"
}

// ─── individual phase views ───────────────────────────────────────────────────

func (m tourWizardModel) viewWelcome(w int) string {
	var sb strings.Builder
	sb.WriteString(wizTitleStyle.Render("Welcome to Wendy") + "\n")
	sb.WriteString(wizSubStyle.Render("Let's get you set up from scratch — takes about 5 minutes.") + "\n\n")
	sb.WriteString(wizBodyStyle.Width(w).Render(
		"This wizard will:\n" +
			"  1. Flash WendyOS onto your device\n" +
			"  2. Boot it and connect over the network\n" +
			"  3. Deploy a sample Python app\n\n" +
			"If anything goes wrong you can restart at any time with:\n") + "\n")
	sb.WriteString("  " + wizCodeStyle.Render("wendy tour") + "\n\n")
	sb.WriteString(wizHintStyle.Render("Press Enter to begin"))
	return sb.String()
}

func (m tourWizardModel) viewLoading(w int) string {
	return wizSubStyle.Render("Fetching supported devices…")
}

func (m tourWizardModel) viewDeviceList(w int) string {
	var sb strings.Builder
	sb.WriteString(wizTitleStyle.Render("Step 1 — Select your device") + "\n")
	sb.WriteString(wizSubStyle.Render("Choose the device you want to set up.") + "\n\n")

	total := len(m.devices) + 1
	for i, dev := range m.devices {
		label := dev.Name
		if dev.LatestVersion != "" {
			label += fmt.Sprintf("  (%s)", dev.LatestVersion)
		}
		if i == m.deviceCursor {
			sb.WriteString(wizSelectedStyle.Render("▶ "+label) + "\n")
		} else {
			sb.WriteString(wizNormalStyle.Render("  "+label) + "\n")
		}
	}
	otherLabel := "Other Linux device (apt install)"
	if m.deviceCursor == total-1 {
		sb.WriteString(wizSelectedStyle.Render("▶ "+otherLabel) + "\n")
	} else {
		sb.WriteString(wizNormalStyle.Render("  "+otherLabel) + "\n")
	}

	sb.WriteString("\n" + wizHintStyle.Render("↑/↓ navigate  ·  Enter select  ·  q quit"))
	return sb.String()
}

func (m tourWizardModel) viewOSInstalled(w int) string {
	var sb strings.Builder
	name := ""
	if m.selected != nil {
		name = m.selected.Name
	}
	sb.WriteString(wizTitleStyle.Render("Step 2 — Check existing OS") + "\n")
	sb.WriteString(wizSubStyle.Render("Is WendyOS already installed on your "+name+"?") + "\n\n")

	opts := []string{"Yes, WendyOS is already installed", "No, I need to install it"}
	for i, opt := range opts {
		if i == m.wifiCursor {
			sb.WriteString(wizSelectedStyle.Render("▶ "+opt) + "\n")
		} else {
			sb.WriteString(wizNormalStyle.Render("  "+opt) + "\n")
		}
	}
	sb.WriteString("\n" + wizHintStyle.Render("↑/↓ navigate  ·  Enter select"))
	return sb.String()
}

func (m tourWizardModel) viewExistingDeviceScan(w int) string {
	return wizSubStyle.Render("Scanning the network for WendyOS devices…")
}

func (m tourWizardModel) viewExistingDevicePicker(w int) string {
	var sb strings.Builder
	sb.WriteString(wizTitleStyle.Render("Step 2 — Select your device") + "\n")
	sb.WriteString(wizSubStyle.Render("Choose the device that already has WendyOS installed.") + "\n\n")

	if len(m.lanDevices) == 0 {
		sb.WriteString(wizNoticeStyle.Render("No WendyOS devices found on the network.") + "\n\n")
		sb.WriteString(wizBodyStyle.Width(w).Render(
			"Make sure your device is powered on and connected to the same network.") + "\n\n")
	} else {
		for i, dev := range m.lanDevices {
			label := dev.DisplayName
			if dev.AgentVersion != "" {
				label += fmt.Sprintf("  v%s", dev.AgentVersion)
			}
			addr := preferredLANAddress(dev)
			if addr != "" {
				label += fmt.Sprintf("  (%s)", addr)
			}
			if i == m.lanCursor {
				sb.WriteString(wizSelectedStyle.Render("▶ "+label) + "\n")
			} else {
				sb.WriteString(wizNormalStyle.Render("  "+label) + "\n")
			}
		}
		sb.WriteString("\n")
	}

	// "Enter manually" option always at the bottom
	manualLabel := "Enter hostname / IP manually"
	if m.lanCursor == len(m.lanDevices) {
		sb.WriteString(wizSelectedStyle.Render("▶ "+manualLabel) + "\n")
	} else {
		sb.WriteString(wizNormalStyle.Render("  "+manualLabel) + "\n")
	}

	sb.WriteString("\n" + wizHintStyle.Render("↑/↓ navigate  ·  Enter select  ·  r rescan  ·  q quit"))
	return sb.String()
}

func (m tourWizardModel) viewEnterHostname(w int) string {
	var sb strings.Builder
	sb.WriteString(wizTitleStyle.Render("Step 2 — Find your device") + "\n")
	var hint string
	if m.selected != nil {
		hint = "Enter the hostname or IP of your existing " + m.selected.Name + "."
	} else {
		hint = "Enter the hostname or IP address of your device."
	}
	sb.WriteString(wizSubStyle.Render(hint) + "\n\n")
	sb.WriteString(m.input.View() + "\n\n")
	sb.WriteString(wizHintStyle.Render("Enter to confirm"))
	return sb.String()
}

func (m tourWizardModel) viewAptInstall(w int) string {
	var sb strings.Builder
	sb.WriteString(wizTitleStyle.Render("Step 2 — Install the Wendy Agent") + "\n")
	sb.WriteString(wizSubStyle.Render("Run the following on your Linux device:") + "\n\n")
	sb.WriteString("  " + wizCodeStyle.Render("curl -fsSL https://install.wendy.sh/agent.sh | bash") + "\n\n")
	sb.WriteString(wizBodyStyle.Width(w).Render(
		"Or using APT:\n") + "\n")
	sb.WriteString("  " + wizCodeStyle.Render("curl -fsSL https://install.wendy.sh/apt-key.gpg | sudo apt-key add -") + "\n")
	sb.WriteString("  " + wizCodeStyle.Render(`echo "deb https://apt.wendy.sh stable main" | sudo tee /etc/apt/sources.list.d/wendy.list`) + "\n")
	sb.WriteString("  " + wizCodeStyle.Render("sudo apt update && sudo apt install -y wendy-agent") + "\n\n")
	sb.WriteString(wizBodyStyle.Width(w).Render("Once the agent is installed and the service is running, press Enter.") + "\n\n")
	sb.WriteString(wizHintStyle.Render("Enter to continue"))
	return sb.String()
}

func (m tourWizardModel) viewStorageGuide(w int) string {
	var sb strings.Builder
	name := ""
	if m.selected != nil {
		name = m.selected.Name
	}
	sb.WriteString(wizTitleStyle.Render("Step 2 — Prepare storage") + "\n")
	if m.useNVMe {
		sb.WriteString(wizSubStyle.Render("Your "+name+" uses an NVMe SSD.") + "\n\n")
		sb.WriteString(wizBodyStyle.Width(w).Render(
			"You'll need:\n"+
				"  • An M.2 NVMe SSD (any capacity)\n"+
				"  • A USB-C or NVMe-to-USB adapter\n\n"+
				"Insert the SSD into the adapter and plug it into this computer.\n"+
				"The next screen will detect it automatically.") + "\n")
	} else {
		sb.WriteString(wizSubStyle.Render("Your "+name+" uses a microSD card.") + "\n\n")
		sb.WriteString(wizBodyStyle.Width(w).Render(
			"You'll need:\n"+
				"  • A microSD card (8 GB minimum, Class 10 or better)\n"+
				"  • An SD card reader connected to this computer\n\n"+
				"Insert the SD card and plug in the reader now.\n"+
				"The next screen will detect it automatically.") + "\n")
	}
	sb.WriteString("\n" + wizHintStyle.Render("Enter when ready"))
	return sb.String()
}

func (m tourWizardModel) viewDriveWait(w int) string {
	var sb strings.Builder
	sb.WriteString(wizTitleStyle.Render("Step 3 — Select target drive") + "\n")
	sb.WriteString(wizSubStyle.Render("Select the drive to install WendyOS onto. Refreshes every 2 s.") + "\n\n")

	if len(m.drives) == 0 {
		sb.WriteString(wizNoticeStyle.Render("No external drives detected — insert your drive now.") + "\n")
	} else {
		for i, d := range m.drives {
			label := fmt.Sprintf("%-28s  %s  %s", d.Name, d.DevicePath, d.Size)
			if i == m.driveCursor {
				sb.WriteString(wizSelectedStyle.Render("▶ "+label) + "\n")
			} else {
				sb.WriteString(wizNormalStyle.Render("  "+label) + "\n")
			}
		}
	}

	sb.WriteString("\n" + wizHintStyle.Render("↑/↓ navigate  ·  Enter select  ·  auto-refreshing"))
	return sb.String()
}

func (m tourWizardModel) viewDeviceName(w int) string {
	var sb strings.Builder
	sb.WriteString(wizTitleStyle.Render("Step 4 — Name your device") + "\n")
	sb.WriteString(wizSubStyle.Render("Give your device a short, memorable hostname.") + "\n\n")
	sb.WriteString(m.input.View() + "\n\n")
	sb.WriteString(wizBodyStyle.Width(w).Render("Lowercase letters, digits, and hyphens only. Min 3 characters.") + "\n\n")
	sb.WriteString(wizHintStyle.Render("Enter to confirm"))
	return sb.String()
}

func (m tourWizardModel) viewWifiDetect(w int) string {
	return wizSubStyle.Render("Detecting WiFi network…")
}

func wifiQuestionOptions(detected string) []string {
	if detected != "" {
		return []string{
			fmt.Sprintf("Yes, use \"%s\"", detected),
			"Scan for a different network",
			"Enter WiFi credentials manually",
			"Skip WiFi setup",
		}
	}
	return []string{
		"Scan for nearby networks",
		"Enter WiFi credentials manually",
		"Skip WiFi setup",
	}
}

func (m tourWizardModel) viewWifiQuestion(w int) string {
	var sb strings.Builder
	sb.WriteString(wizTitleStyle.Render("Step 5 — WiFi setup") + "\n")
	if m.detectedSSID != "" {
		sb.WriteString(wizSubStyle.Render(fmt.Sprintf("Your Mac is connected to \"%s\".", m.detectedSSID)) + "\n")
		sb.WriteString(wizBodyStyle.Width(w).Render("Pre-configure the device with the same network?") + "\n\n")
	} else {
		sb.WriteString(wizBodyStyle.Width(w).Render("Pre-configure WiFi so your device connects on first boot.") + "\n\n")
	}

	opts := wifiQuestionOptions(m.detectedSSID)
	for i, opt := range opts {
		if i == m.wifiCursor {
			sb.WriteString(wizSelectedStyle.Render("▶ "+opt) + "\n")
		} else {
			sb.WriteString(wizNormalStyle.Render("  "+opt) + "\n")
		}
	}
	sb.WriteString("\n" + wizHintStyle.Render("↑/↓ navigate  ·  Enter select"))
	return sb.String()
}

func (m tourWizardModel) viewWifiScanLoading(w int) string {
	return wizSubStyle.Render("Scanning for nearby WiFi networks…")
}

func (m tourWizardModel) viewWifiNetworkPicker(w int) string {
	var sb strings.Builder
	sb.WriteString(wizTitleStyle.Render("Step 5 — Select WiFi network") + "\n")
	sb.WriteString(wizSubStyle.Render("Choose the network to provision on your device.") + "\n\n")

	if len(m.scanNetworks) == 0 {
		sb.WriteString(wizNoticeStyle.Render("No networks found nearby.") + "\n\n")
		sb.WriteString(wizHintStyle.Render("Press q to go back"))
	} else {
		for i, net := range m.scanNetworks {
			label := net.SSID
			if net.SignalStrength > 0 {
				label += fmt.Sprintf("  (%d%%)", net.SignalStrength)
			}
			if i == m.scanCursor {
				sb.WriteString(wizSelectedStyle.Render("▶ "+label) + "\n")
			} else {
				sb.WriteString(wizNormalStyle.Render("  "+label) + "\n")
			}
		}
		sb.WriteString("\n" + wizHintStyle.Render("↑/↓ navigate  ·  Enter select  ·  q quit"))
	}
	return sb.String()
}

func (m tourWizardModel) viewTextInput(w int) string {
	var sb strings.Builder
	var title, hint string
	switch m.phase {
	case phaseWifiPassword:
		title = "WiFi Password"
		hint = fmt.Sprintf("Password for \"%s\" (leave blank for open network)", m.wifiSSID)
	case phaseWifiManualSSID:
		title = "WiFi Network"
		hint = "Enter the name (SSID) of the network"
	}
	sb.WriteString(wizTitleStyle.Render(title) + "\n")
	sb.WriteString(wizSubStyle.Render(hint) + "\n\n")
	sb.WriteString(m.input.View() + "\n\n")
	sb.WriteString(wizHintStyle.Render("Enter to confirm"))
	return sb.String()
}

func (m tourWizardModel) viewReadyToInstall(w int) string {
	var sb strings.Builder
	sb.WriteString(wizTitleStyle.Render("Ready to install WendyOS") + "\n\n")

	deviceName := ""
	if m.selected != nil {
		deviceName = m.selected.Name
	}
	drive := ""
	if m.selDrive != nil {
		drive = fmt.Sprintf("%s (%s, %s)", m.selDrive.Name, m.selDrive.DevicePath, m.selDrive.Size)
	}

	sb.WriteString(fmt.Sprintf("  Device type:  %s\n", deviceName))
	sb.WriteString(fmt.Sprintf("  Hostname:     %s\n", m.deviceName))
	sb.WriteString(fmt.Sprintf("  Drive:        %s\n", drive))
	if m.wifiSSID != "" {
		sb.WriteString(fmt.Sprintf("  WiFi SSID:    %s\n", m.wifiSSID))
	} else {
		sb.WriteString("  WiFi:         (skipped)\n")
	}

	sb.WriteString("\n")
	sb.WriteString(wizNoticeStyle.Render("All data on the drive will be erased.") + "\n\n")
	sb.WriteString(wizHintStyle.Render("Enter to begin install  ·  q to quit"))
	return sb.String()
}

func (m tourWizardModel) viewBootInstructions(w int) string {
	var sb strings.Builder
	sb.WriteString(wizTitleStyle.Render("Step 6 — Boot your device") + "\n\n")

	if m.selected != nil {
		name := m.selected.Name
		if m.useNVMe {
			sb.WriteString(wizBodyStyle.Width(w).Render(
				"1. Eject the drive from this computer.\n"+
					"2. Insert the NVMe into your "+name+".\n"+
					"3. Connect the "+name+" to this computer via USB-C.\n"+
					"   (USB connection recommended — WiFi is optional.)\n"+
					"4. Power on the "+name+".") + "\n")
		} else {
			sb.WriteString(wizBodyStyle.Width(w).Render(
				"1. Eject the SD card from this computer.\n"+
					"2. Insert the SD card into your "+name+".\n"+
					"3. Connect the "+name+" to this computer via USB or Ethernet.\n"+
					"   (Wired connection recommended — WiFi is optional.)\n"+
					"4. Power on the "+name+".") + "\n")
		}
	} else {
		sb.WriteString(wizBodyStyle.Width(w).Render(
			"1. Make sure the Wendy agent is running on your device.\n"+
				"2. Connect the device to the same network as this computer.\n"+
				"   USB connection is recommended if available.") + "\n")
	}

	sb.WriteString("\n")
	if m.targetName != "" {
		sb.WriteString(wizBodyStyle.Width(w).Render(
			fmt.Sprintf("Once powered on, Wendy will scan for \"%s\" on the network.", m.targetName)) + "\n")
	}

	sb.WriteString("\n" + wizHintStyle.Render("Enter when powered on and connected"))
	return sb.String()
}

func (m tourWizardModel) viewDiscovering(w int) string {
	var sb strings.Builder
	sb.WriteString(wizTitleStyle.Render("Step 7 — Waiting for device") + "\n\n")

	target := m.targetName
	if target == "" {
		target = m.hostname
	}
	sb.WriteString(wizSubStyle.Render(fmt.Sprintf("Scanning for \"%s\" on the network…", target)) + "\n\n")
	sb.WriteString(wizBodyStyle.Width(w).Render(
		"This can take 30–90 seconds while the device boots.\n"+
			"Make sure it is powered on and connected via USB or the same WiFi network.") + "\n\n")
	sb.WriteString(wizHintStyle.Render("q to quit"))
	return sb.String()
}

func (m tourWizardModel) viewDeviceFound(w int) string {
	var sb strings.Builder
	sb.WriteString(wizSuccessStyle.Render("Device is online!") + "\n\n")
	sb.WriteString(wizBodyStyle.Width(w).Render(
		fmt.Sprintf("Found \"%s\" at %s", m.foundName, m.foundAddr)) + "\n\n")
	sb.WriteString(wizBodyStyle.Width(w).Render("Let's deploy your first Python app.") + "\n\n")
	sb.WriteString(wizHintStyle.Render("Enter to continue"))
	return sb.String()
}

func (m tourWizardModel) viewCreateProject(w int) string {
	var sb strings.Builder
	sb.WriteString(wizTitleStyle.Render("Step 8 — Sample Python project") + "\n\n")

	if m.projectPath != "" {
		sb.WriteString(wizSuccessStyle.Render("Project created at:") + "\n")
		sb.WriteString("  " + wizCodeStyle.Render(m.projectPath) + "\n\n")
		sb.WriteString(wizBodyStyle.Width(w).Render("It's a simple HTTP server. Once deployed, visit:") + "\n")
		deviceHost := m.foundAddr
		if deviceHost == "" {
			deviceHost = m.targetName + ".local"
		}
		sb.WriteString("  " + wizCodeStyle.Render(fmt.Sprintf("http://%s:8000", deviceHost)) + "\n\n")
		sb.WriteString(wizBodyStyle.Width(w).Render("Press Enter to build and deploy it now.") + "\n\n")
		sb.WriteString(wizHintStyle.Render("Enter to deploy  ·  q to skip"))
	} else {
		sb.WriteString(wizSubStyle.Render("Creating project…") + "\n")
	}
	return sb.String()
}

func (m tourWizardModel) viewAICheck(w int) string {
	var sb strings.Builder
	sb.WriteString(wizTitleStyle.Render("Step 9 — Continue development") + "\n\n")

	if m.claudePath != "" {
		sb.WriteString(wizSuccessStyle.Render("Claude Code detected") + "\n")
		sb.WriteString(wizBodyStyle.Width(w).Render(
			"You can continue developing with Claude Code. Open your project in it:") + "\n\n")
		sb.WriteString("  " + wizCodeStyle.Render(fmt.Sprintf("cd %s && claude", m.projectPath)) + "\n\n")
		sb.WriteString(wizBodyStyle.Width(w).Render(
			"Wendy ships with Claude Code skill files to help Claude understand\n"+
				"the platform. Install the Wendy plugin in Claude Code settings.") + "\n")
	} else if m.codexPath != "" {
		sb.WriteString(wizSuccessStyle.Render("Codex detected") + "\n")
		sb.WriteString(wizBodyStyle.Width(w).Render(
			"Continue development with Codex from your project directory:") + "\n\n")
		sb.WriteString("  " + wizCodeStyle.Render(fmt.Sprintf("cd %s && codex", m.projectPath)) + "\n")
	} else {
		sb.WriteString(wizBodyStyle.Width(w).Render(
			"To get AI-assisted development for Wendy apps, install Claude Code:") + "\n\n")
		sb.WriteString("  " + wizCodeStyle.Render("npm install -g @anthropic-ai/claude-code") + "\n\n")
		sb.WriteString(wizBodyStyle.Width(w).Render(
			"Then open your project and run:") + "\n\n")
		sb.WriteString("  " + wizCodeStyle.Render(fmt.Sprintf("cd %s && claude", m.projectPath)) + "\n")
	}
	sb.WriteString("\n" + wizHintStyle.Render("Enter to continue"))
	return sb.String()
}

func (m tourWizardModel) viewCloud(w int) string {
	var sb strings.Builder
	sb.WriteString(wizSuccessStyle.Render("You're all set!") + "\n\n")
	sb.WriteString(wizBodyStyle.Width(w).Render(
		"Your device is running, your first app is deployed.\n\n"+
			"When you're ready to manage multiple devices remotely,\n"+
			"Wendy Cloud can help:") + "\n\n")
	sb.WriteString("  " + wizCodeStyle.Render("wendy auth login") + "\n\n")
	sb.WriteString(wizBodyStyle.Width(w).Render(
		"Wendy Cloud provides:\n"+
			"  • Remote access to all your devices\n"+
			"  • Certificate-based mTLS authentication\n"+
			"  • App deployment pipelines\n\n"+
			"Docs: https://wendy.sh/docs") + "\n\n")
	sb.WriteString(wizHintStyle.Render("Press any key to exit"))
	return sb.String()
}

func (m tourWizardModel) viewError(w int) string {
	var sb strings.Builder
	sb.WriteString(wizErrorStyle.Render("Something went wrong") + "\n\n")
	if m.err != nil {
		sb.WriteString(wizBodyStyle.Width(w).Render(m.err.Error()) + "\n\n")
	}
	sb.WriteString(wizBodyStyle.Width(w).Render("You can restart the tour at any time with:") + "\n\n")
	sb.WriteString("  " + wizCodeStyle.Render("wendy tour") + "\n\n")
	sb.WriteString(wizHintStyle.Render("Press any key to exit"))
	return sb.String()
}

// ─── tea.Cmd helpers ──────────────────────────────────────────────────────────

func loadDevicesCmd() tea.Cmd {
	return func() tea.Msg {
		devices, err := getAvailableDevices()
		return tourDevicesLoadedMsg{devices: devices, err: err}
	}
}

func scanLANDevicesCmd() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		devices, err := discovery.DiscoverLAN(ctx, 8*time.Second)
		return tourLANScanDoneMsg{devices: devices, err: err}
	}
}

func scanWifiCmd() tea.Cmd {
	return func() tea.Msg {
		networks, _ := scanLocalWifiNetworks()
		return tourWifiScanDoneMsg{networks: networks}
	}
}

func detectWifiCmd() tea.Cmd {
	return func() tea.Msg {
		ssid := detectCurrentWiFiSSID()
		password := ""
		if ssid != "" && supportsKeychainLookup {
			password, _ = lookupKeychainPassword(ssid)
		}
		return tourWifiDetectedMsg{ssid: ssid, password: password}
	}
}

func rescanDrivesAfter(d time.Duration) tea.Cmd {
	return func() tea.Msg {
		time.Sleep(d)
		return tourDriveRescanMsg{}
	}
}

func (m tourWizardModel) cmdDiscoveryCheck() tea.Cmd {
	target := strings.ToLower(m.targetName)
	if target == "" {
		target = strings.ToLower(m.hostname)
	}
	return func() tea.Msg {
		time.Sleep(3 * time.Second)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		devices, _ := discovery.DiscoverLAN(ctx, 5*time.Second)
		for _, d := range devices {
			name := strings.ToLower(d.DisplayName)
			hostname := strings.ToLower(strings.TrimSuffix(d.Hostname, ".local"))
			if name == target || hostname == target {
				return tourDiscoveryFoundMsg{
					addr: preferredLANAddress(d),
					name: d.DisplayName,
				}
			}
		}
		return tourDiscoveryTickMsg{}
	}
}

func (m tourWizardModel) cmdRunOSInstall() tea.Cmd {
	exePath, err := os.Executable()
	if err != nil {
		return func() tea.Msg { return tourOSInstallDoneMsg{err: err} }
	}

	args := []string{
		"os", "install",
		"--device-type", m.selected.Key,
		"--version", m.selected.LatestVersion,
		"--device-name", m.deviceName,
		"--force",
	}
	if m.selDrive != nil {
		args = append(args, "--drive", m.selDrive.DevicePath)
	}
	if m.wifiSSID != "" {
		entry := fmt.Sprintf("ssid=%s", m.wifiSSID)
		if m.wifiPass != "" {
			entry += fmt.Sprintf(",password=%s", m.wifiPass)
		}
		args = append(args, "--wifi", entry)
	} else {
		args = append(args, "--no-wifi")
	}

	cmd := exec.Command(exePath, args...)
	return tea.ExecProcess(cmd, func(err error) tea.Msg {
		return tourOSInstallDoneMsg{err: err}
	})
}

func (m tourWizardModel) cmdRunProject() tea.Cmd {
	exePath, err := os.Executable()
	if err != nil {
		return func() tea.Msg { return tourRunDoneMsg{err: err} }
	}

	deviceAddr := m.foundAddr
	if deviceAddr == "" {
		deviceAddr = m.targetName + ".local"
	}
	if deviceAddr == "" {
		deviceAddr = m.hostname
	}

	cmd := exec.Command(exePath, "run", "--device", deviceAddr)
	cmd.Dir = m.projectPath
	m.phase = phaseRunProject
	return tea.ExecProcess(cmd, func(err error) tea.Msg {
		return tourRunDoneMsg{err: err}
	})
}

func (m tourWizardModel) cmdCheckAITools() tea.Cmd {
	return func() tea.Msg {
		claudePath, _ := exec.LookPath("claude")
		codexPath, _ := exec.LookPath("codex")
		return tourAICheckDoneMsg{claudePath: claudePath, codexPath: codexPath}
	}
}

// ─── project creation ─────────────────────────────────────────────────────────

func (m *tourWizardModel) createPythonProject() error {
	docs, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("finding home directory: %w", err)
	}
	docs = filepath.Join(docs, "Documents")

	appID := "sh.wendy.hello"
	if m.deviceName != "" {
		appID = fmt.Sprintf("sh.wendy.hello.%s", m.deviceName)
	}
	m.projectID = appID

	// Find a non-conflicting directory name.
	base := filepath.Join(docs, "wendy-hello")
	dir := base
	for i := 1; ; i++ {
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			break
		}
		dir = fmt.Sprintf("%s-%d", base, i)
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating project directory: %w", err)
	}

	files := map[string]string{
		"wendy.json":   fmt.Sprintf(tourWendyJSONTemplate, appID),
		"app.py":       tourAppPy,
		"Dockerfile":   tourDockerfile,
		"requirements.txt": tourRequirements,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			return fmt.Errorf("writing %s: %w", name, err)
		}
	}

	m.projectPath = dir
	return nil
}

// ─── platform helpers ─────────────────────────────────────────────────────────

// detectCurrentWiFiSSID returns the SSID the host machine is currently
// connected to, or "" if unavailable or not connected.
func detectCurrentWiFiSSID() string {
	switch runtime.GOOS {
	case "darwin":
		for _, iface := range []string{"en0", "en1", "en2", "en3"} {
			out, err := exec.Command("/usr/sbin/networksetup", "-getairportnetwork", iface).Output()
			if err != nil {
				continue
			}
			line := strings.TrimSpace(string(out))
			if after, found := strings.CutPrefix(line, "Current Wi-Fi Network: "); found && after != "" {
				return after
			}
		}
	case "linux":
		if out, err := exec.Command("iwgetid", "-r").Output(); err == nil {
			if ssid := strings.TrimSpace(string(out)); ssid != "" {
				return ssid
			}
		}
		out, err := exec.Command("sh", "-c",
			"nmcli -t -f active,ssid dev wifi 2>/dev/null | grep '^yes:' | head -1 | cut -d: -f2").Output()
		if err == nil {
			if ssid := strings.TrimSpace(string(out)); ssid != "" {
				return ssid
			}
		}
	}
	return ""
}

// deviceUsesNVMe returns true for devices that boot from NVMe rather than SD.
func deviceUsesNVMe(key string) bool {
	nvmeDevices := []string{"jetson", "orin", "agx"}
	lower := strings.ToLower(key)
	for _, kw := range nvmeDevices {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

// validateDeviceNameTour checks that a name is a valid device hostname.
func validateDeviceNameTour(name string) error {
	if len(name) < 3 || len(name) > 64 {
		return fmt.Errorf("must be 3–64 characters")
	}
	for i, c := range name {
		if c >= 'a' && c <= 'z' {
			continue
		}
		if (c >= '0' && c <= '9' || c == '-') && i > 0 {
			continue
		}
		return fmt.Errorf("lowercase letters, digits, and hyphens only; must start with a letter")
	}
	return nil
}

// ─── command entry point ──────────────────────────────────────────────────────

func newTourCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "tour",
		Short: "Interactive guided setup tour for new users",
		Long:  "Walk through device setup, OS install, sample project deployment, and first steps.",
		RunE: func(cmd *cobra.Command, args []string) error {
			m := newTourWizardModel()
			_, err := tea.NewProgram(m, tea.WithAltScreen()).Run()
			return err
		},
	}
}
