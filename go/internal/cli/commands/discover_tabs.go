package commands

import (
	"context"
	"fmt"
	"os"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

type discoverTabsLocalMsg struct{ msg tea.Msg }
type discoverTabsSimulatorMsg struct{ msg tea.Msg }
type discoverTabsCloudMsg struct{ msg tea.Msg }
type discoverTabsOrgMsg struct{ name string }

type discoverTabsModel struct {
	local        discoverModel
	sim          simulatorPickerModel
	simStarted   bool
	cloud        cloudDiscoverModel
	cloudAuth    *config.AuthConfig
	cloudOrg     string
	cloudStarted bool
	defaultOrg   int32
	active       devicePickerTab
	action       devicePickerAction
	createVMName string
	cancelled    bool
	windowWidth  int
}

// active selects the tab to open on. Creating a VM leaves and re-enters this
// view, and coming back on Local would drop the user somewhere they did not ask
// to be, with no sign the create happened.
func newDiscoverTabsModel(ctx context.Context, local discoverModel, auth *config.AuthConfig, defaultOrg int32, active devicePickerTab) discoverTabsModel {
	m := discoverTabsModel{
		local:      local,
		sim:        newSimulatorListModel(ctx),
		cloudAuth:  auth,
		defaultOrg: defaultOrg,
		active:     active,
	}
	// The simulator list polls only once its tab is first shown; opening
	// straight onto it has to start that here instead.
	if active == devicePickerSimulatorTab {
		m.simStarted = true
	}
	if auth != nil {
		m.cloud = newCloudDiscoverModel(ctx, auth, os.Getenv("WENDY_BROKER_URL"), false, false, nil)
	}
	return m
}

// tagDiscoverTabsCmd preserves which dashboard owns an asynchronous result.
// discoverModel.Init returns tea.Batch, so BatchMsg children must be tagged
// individually rather than hiding the batch inside one wrapper message.
func tagDiscoverTabsCmd(cmd tea.Cmd, tab devicePickerTab) tea.Cmd {
	if cmd == nil {
		return nil
	}
	return func() tea.Msg {
		msg := cmd()
		if batch, ok := msg.(tea.BatchMsg); ok {
			tagged := make(tea.BatchMsg, 0, len(batch))
			for _, child := range batch {
				tagged = append(tagged, tagDiscoverTabsCmd(child, tab))
			}
			return tagged
		}
		switch tab {
		case devicePickerCloudTab:
			return discoverTabsCloudMsg{msg: msg}
		case devicePickerSimulatorTab:
			return discoverTabsSimulatorMsg{msg: msg}
		default:
			return discoverTabsLocalMsg{msg: msg}
		}
	}
}

func (m discoverTabsModel) Init() tea.Cmd {
	if m.simStarted {
		return tea.Batch(tagDiscoverTabsCmd(m.local.Init(), devicePickerLocalTab), m.startSimulatorCmd())
	}
	// Otherwise simulator polling starts on first entry.
	return tagDiscoverTabsCmd(m.local.Init(), devicePickerLocalTab)
}

func (m discoverTabsModel) startSimulatorCmd() tea.Cmd {
	return tagDiscoverTabsCmd(m.sim.Init(), devicePickerSimulatorTab)
}

func (m discoverTabsModel) startCloudCmd() tea.Cmd {
	if m.cloudAuth == nil {
		return nil
	}
	return tea.Batch(
		tagDiscoverTabsCmd(m.cloud.Init(), devicePickerCloudTab),
		m.loadOrgNameCmd(),
	)
}

func (m discoverTabsModel) loadOrgNameCmd() tea.Cmd {
	ctx := m.cloud.ctx
	auth := m.cloudAuth
	orgID := cloudAuthOrgID(auth)
	return func() tea.Msg {
		orgs, err := listOrgsFromCloud(ctx, auth)
		if err != nil {
			return discoverTabsOrgMsg{}
		}
		for _, org := range orgs {
			if org.GetId() == orgID {
				return discoverTabsOrgMsg{name: org.GetName()}
			}
		}
		return discoverTabsOrgMsg{}
	}
}

func (m discoverTabsModel) updateLocal(msg tea.Msg) (discoverTabsModel, tea.Cmd) {
	updated, cmd := m.local.Update(msg)
	m.local = updated.(discoverModel)
	if m.local.quitting {
		m.cancelled = true
		return m, tea.Quit
	}
	return m, tagDiscoverTabsCmd(cmd, devicePickerLocalTab)
}

// updateSimulator forwards to the shared simulator list. A selection is ignored
// -- discover shows devices, it does not connect to one, and enter copies
// instead -- but 'c' still has to leave, because the download needs the
// terminal this view is holding.
func (m discoverTabsModel) updateSimulator(msg tea.Msg) (discoverTabsModel, tea.Cmd) {
	updated, cmd := m.sim.Update(msg)
	m.sim = updated
	if m.sim.createRequested() {
		if choice := m.sim.selected(); choice != nil {
			m.createVMName = choice.Name
		}
		m.action = devicePickerCreateVM
		return m, tea.Quit
	}
	if m.sim.cancelled() {
		m.cancelled = true
		return m, tea.Quit
	}
	return m, tagDiscoverTabsCmd(cmd, devicePickerSimulatorTab)
}

func (m discoverTabsModel) updateCloud(msg tea.Msg) (discoverTabsModel, tea.Cmd) {
	updated, cmd := m.cloud.Update(msg)
	m.cloud = updated.(cloudDiscoverModel)
	if m.cloud.quitting {
		m.cancelled = true
		return m, tea.Quit
	}
	return m, tagDiscoverTabsCmd(cmd, devicePickerCloudTab)
}

func (m discoverTabsModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case discoverTabsLocalMsg:
		return m.updateLocal(msg.msg)
	case discoverTabsSimulatorMsg:
		return m.updateSimulator(msg.msg)
	case discoverTabsCloudMsg:
		if m.cloudAuth == nil {
			return m, nil
		}
		return m.updateCloud(msg.msg)
	case discoverTabsOrgMsg:
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

func (m discoverTabsModel) View() string {
	if m.cancelled || m.action != devicePickerNoAction {
		return ""
	}
	header := deviceTabsHeader(m.active, deviceTabOrder(), m.windowWidth)
	switch m.active {
	case devicePickerLocalTab:
		return header + "\n\n" + m.local.View()
	case devicePickerSimulatorTab:
		return header + "\n\n" + m.sim.View() + "\n" + devicePickerOrgStyle.Render(
			"  enter copy address, tab switch, q quit — start one with 'wendy vm start <name>'")
	}

	var body strings.Builder
	body.WriteString(header)
	body.WriteString("\n\n")
	if m.cloudAuth == nil {
		body.WriteString("Discover cloud devices\n")
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

// copySimulatorAddress puts a running VM's agent address on the clipboard, so
// the address can be pasted straight into a --device flag. A stopped VM has no
// address to copy, so it says how to get one instead.
func copySimulatorAddress(choice *simulatorChoice) string {
	switch {
	case choice == nil:
		return ""
	case choice.Create:
		return "No simulator yet — press c to create one."
	case choice.Address == "":
		return fmt.Sprintf("%s is not running — start it with 'wendy vm start %s'.", choice.Name, choice.Name)
	}
	if err := clipboardWriter(choice.Address); err != nil {
		return fmt.Sprintf("Copy failed: %v", err)
	}
	return "Copied " + choice.Address + " to clipboard."
}
