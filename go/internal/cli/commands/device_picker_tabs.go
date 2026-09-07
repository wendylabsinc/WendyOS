package commands

import (
	"context"
	"fmt"
	"os"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
)

// Child messages are tagged so background commands keep updating the correct
// page after the user changes tabs.
type devicePickerLocalMsg struct{ msg tea.Msg }
type devicePickerSimulatorMsg struct{ msg tea.Msg }
type devicePickerCloudMsg struct{ msg tea.Msg }
type devicePickerOrgMsg struct{ name string }

// devicePickerChoice is what the user picked, tagged by the tab that owned the
// selection. Tab is authoritative: a child keeps whatever it selected on an
// earlier visit, so checking payloads in a fixed order would let one tab's
// leftover beat the tab the user actually confirmed on.
type devicePickerChoice struct {
	Tab       devicePickerTab
	Local     *tui.PickerItem
	Simulator *simulatorChoice
	Cloud     *cloudpb.Asset
}

type devicePickerModel struct {
	local        tui.PickerModel
	sim          simulatorPickerModel
	simStarted   bool
	chosen       devicePickerTab
	hasChosen    bool
	cloud        cloudDiscoverModel
	cloudAuth    *config.AuthConfig
	cloudOrg     string
	cloudStarted bool
	defaultOrg   int32
	active       devicePickerTab
	action       devicePickerAction
	cancelled    bool
	windowWidth  int
}

func newDevicePickerModel(ctx context.Context, local tui.PickerModel, auth *config.AuthConfig, defaultOrg int32) devicePickerModel {
	m := devicePickerModel{
		local:      local,
		sim:        newSimulatorPickerModel(ctx),
		cloudAuth:  auth,
		defaultOrg: defaultOrg,
	}
	if auth != nil {
		m.cloud = newCloudDiscoverModel(ctx, auth, os.Getenv("WENDY_BROKER_URL"), false, true, nil)
	}
	return m
}

func tagDevicePickerCmd(cmd tea.Cmd, tab devicePickerTab) tea.Cmd {
	if cmd == nil {
		return nil
	}
	return func() tea.Msg {
		msg := cmd()
		if batch, ok := msg.(tea.BatchMsg); ok {
			tagged := make(tea.BatchMsg, 0, len(batch))
			for _, child := range batch {
				tagged = append(tagged, tagDevicePickerCmd(child, tab))
			}
			return tagged
		}
		switch tab {
		case devicePickerCloudTab:
			return devicePickerCloudMsg{msg: msg}
		case devicePickerSimulatorTab:
			return devicePickerSimulatorMsg{msg: msg}
		default:
			return devicePickerLocalMsg{msg: msg}
		}
	}
}

func (m devicePickerModel) Init() tea.Cmd {
	// The simulator list is not started here: it polls the VM store, and doing
	// that for a tab nobody opened is both wasted I/O and lock contention with
	// any concurrent `vm start`.
	return tagDevicePickerCmd(m.local.Init(), devicePickerLocalTab)
}

func (m devicePickerModel) startSimulatorCmd() tea.Cmd {
	return tagDevicePickerCmd(m.sim.Init(), devicePickerSimulatorTab)
}

func (m devicePickerModel) startCloudCmd() tea.Cmd {
	if m.cloudAuth == nil {
		return nil
	}
	return tea.Batch(
		tagDevicePickerCmd(m.cloud.Init(), devicePickerCloudTab),
		m.loadOrgNameCmd(),
	)
}

func (m devicePickerModel) loadOrgNameCmd() tea.Cmd {
	ctx := m.cloud.ctx
	auth := m.cloudAuth
	orgID := cloudAuthOrgID(auth)
	return func() tea.Msg {
		orgs, err := listOrgsFromCloud(ctx, auth)
		if err != nil {
			return devicePickerOrgMsg{}
		}
		for _, org := range orgs {
			if org.GetId() == orgID {
				return devicePickerOrgMsg{name: org.GetName()}
			}
		}
		return devicePickerOrgMsg{}
	}
}

func (m devicePickerModel) updateLocal(msg tea.Msg) (devicePickerModel, tea.Cmd) {
	updated, cmd := m.local.Update(msg)
	m.local = updated.(tui.PickerModel)
	if m.local.Selected() != nil {
		m.chosen, m.hasChosen = devicePickerLocalTab, true
		return m, tea.Quit
	}
	if m.local.Cancelled() {
		m.cancelled = true
		return m, tea.Quit
	}
	return m, tagDevicePickerCmd(cmd, devicePickerLocalTab)
}

func (m devicePickerModel) updateSimulator(msg tea.Msg) (devicePickerModel, tea.Cmd) {
	updated, cmd := m.sim.Update(msg)
	m.sim = updated
	if m.sim.selected() != nil {
		m.chosen, m.hasChosen = devicePickerSimulatorTab, true
		return m, tea.Quit
	}
	if m.sim.cancelled() {
		m.cancelled = true
		return m, tea.Quit
	}
	return m, tagDevicePickerCmd(cmd, devicePickerSimulatorTab)
}

func (m devicePickerModel) updateCloud(msg tea.Msg) (devicePickerModel, tea.Cmd) {
	updated, cmd := m.cloud.Update(msg)
	m.cloud = updated.(cloudDiscoverModel)
	if m.cloud.selected != nil {
		m.chosen, m.hasChosen = devicePickerCloudTab, true
		return m, tea.Quit
	}
	if m.cloud.quitting {
		m.cancelled = true
		return m, tea.Quit
	}
	return m, tagDevicePickerCmd(cmd, devicePickerCloudTab)
}

func (m devicePickerModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case devicePickerLocalMsg:
		return m.updateLocal(msg.msg)
	case devicePickerSimulatorMsg:
		return m.updateSimulator(msg.msg)
	case devicePickerCloudMsg:
		if m.cloudAuth == nil {
			return m, nil
		}
		return m.updateCloud(msg.msg)
	case devicePickerOrgMsg:
		m.cloudOrg = msg.name
		return m, nil
	case tea.WindowSizeMsg:
		m.windowWidth = msg.Width
		var cmds []tea.Cmd
		local, localCmd := m.updateLocal(msg)
		m = local
		cmds = append(cmds, localCmd)
		sim, simCmd := m.updateSimulator(msg)
		m = sim
		cmds = append(cmds, simCmd)
		// Accumulated rather than early-returned: the old shape bailed out
		// before the second child when there was no cloud auth, which would
		// leave the simulator table unsized.
		if m.cloudAuth != nil {
			cloud, cloudCmd := m.updateCloud(msg)
			m = cloud
			cmds = append(cmds, cloudCmd)
		}
		return m, tea.Batch(cmds...)
	case tea.KeyMsg:
		switch msg.String() {
		case "tab", "shift+tab":
			m.active = cycleTab(deviceTabOrder(), m.active, tabCycleDelta(msg.String()))
			// Latched rather than keyed off "arrived from Local": with
			// wrap-around, Cloud is reachable from either neighbour and
			// discovery must still start exactly once.
			if m.active == devicePickerCloudTab && m.cloudAuth != nil && !m.cloudStarted {
				m.cloudStarted = true
				return m, m.startCloudCmd()
			}
			if m.active == devicePickerSimulatorTab && !m.simStarted {
				m.simStarted = true
				return m, m.startSimulatorCmd()
			}
			return m, nil
		case "o":
			if m.active == devicePickerCloudTab && m.cloudAuth != nil {
				m.action = devicePickerSwitchOrg
				return m, tea.Quit
			}
		case "enter":
			if m.active == devicePickerCloudTab && m.cloudAuth == nil {
				m.action = devicePickerLogin
				return m, tea.Quit
			}
		case "q", "esc", "ctrl+c":
			if m.active == devicePickerCloudTab && m.cloudAuth == nil {
				m.cancelled = true
				return m, tea.Quit
			}
		}

		switch m.active {
		case devicePickerCloudTab:
			if m.cloudAuth == nil {
				return m, nil
			}
			return m.updateCloud(msg)
		case devicePickerSimulatorTab:
			return m.updateSimulator(msg)
		default:
			return m.updateLocal(msg)
		}
	}
	return m, nil
}

var (
	devicePickerTabActive   = lipgloss.NewStyle().Bold(true).Underline(true).Foreground(tui.ColorPrimary)
	devicePickerTabInactive = lipgloss.NewStyle().Foreground(tui.ColorDim)
	devicePickerOrgStyle    = lipgloss.NewStyle().Foreground(tui.ColorDim)
)

func (m devicePickerModel) View() string {
	if m.cancelled || m.action != devicePickerNoAction || m.hasChosen {
		return ""
	}

	header := deviceTabsHeader(m.active, deviceTabOrder(), m.windowWidth)

	switch m.active {
	case devicePickerLocalTab:
		return header + "\n\n" + m.local.View()
	case devicePickerSimulatorTab:
		return header + "\n\n" + m.sim.View()
	}

	var body strings.Builder
	body.WriteString(header)
	body.WriteString("\n\n")
	if m.cloudAuth == nil {
		body.WriteString("Select a cloud device\n")
		body.WriteString(devicePickerOrgStyle.Render("  ☐  Wendy Cloud login   Not logged in"))
		body.WriteString("\n")
		body.WriteString(devicePickerOrgStyle.Render("  enter log in, tab local, q quit"))
		body.WriteString("\n")
		return body.String()
	}

	body.WriteString(devicePickerOrgStyle.Render(deviceCloudOrgLabel(m.cloudAuth, m.cloudOrg, m.defaultOrg)))
	body.WriteString("\n\n")
	body.WriteString(m.cloud.View())
	return body.String()
}

func deviceCloudOrgLabel(auth *config.AuthConfig, name string, defaultOrg int32) string {
	orgID := cloudAuthOrgID(auth)
	label := fmt.Sprintf("Organization: org %d", orgID)
	if name != "" {
		label = fmt.Sprintf("Organization: %s (org %d)", name, orgID)
	}
	if orgID != 0 && orgID == defaultOrg {
		label += "  ✦ default"
	}
	return label + "  (o switch)"
}

// choice reports the confirmed selection. ok is false when the picker exited
// without one: cancelled, or quit to log in or switch org.
func (m devicePickerModel) choice() (devicePickerChoice, bool) {
	if !m.hasChosen || m.cancelled || m.action != devicePickerNoAction {
		return devicePickerChoice{}, false
	}
	c := devicePickerChoice{Tab: m.chosen}
	switch m.chosen {
	case devicePickerSimulatorTab:
		c.Simulator = m.sim.selected()
	case devicePickerCloudTab:
		c.Cloud = m.selectedCloud()
	default:
		c.Local = m.selectedLocal()
	}
	return c, true
}

func (m devicePickerModel) selectedLocal() *tui.PickerItem {
	return m.local.Selected()
}

func (m devicePickerModel) selectedCloud() *cloudpb.Asset {
	if m.cloudAuth == nil {
		return nil
	}
	return m.cloud.selected
}

func cloudAuthOrgID(auth *config.AuthConfig) int32 {
	if auth == nil || len(auth.Certificates) == 0 {
		return 0
	}
	return int32(auth.Certificates[0].OrganizationID)
}

// devicePickerInitialAuth chooses the persisted default session when possible.
// If the user has authenticated sessions but no default yet, the first valid
// session is still useful as the initial cloud page; the 'o' hotkey lets them
// switch and mark another session as default.
func devicePickerInitialAuth(cfg *config.Config) *config.AuthConfig {
	if cfg == nil || len(cfg.Auth) == 0 {
		return nil
	}
	if auth, err := config.ResolveAuth(cfg, "", nil); err == nil {
		return auth
	}
	for i := range cfg.Auth {
		if len(cfg.Auth[i].Certificates) > 0 {
			return &cfg.Auth[i]
		}
	}
	return nil
}
