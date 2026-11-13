package commands

import (
	tea "github.com/charmbracelet/bubbletea"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui/bttable"
)

type pairTab int

const (
	pairBluetoothTab pairTab = iota
	pairSensorLinkTab
	pairCameraTab
	pairArmTab
	pairUnitreeTab
	pairTabCount
)

// Background results belong to the tab that started the operation, even if
// the user switches tabs before it finishes.
type pairTabMsg struct {
	tab pairTab
	msg tea.Msg
}

type devicePairModel struct {
	bluetooth bttable.Model
	sensor    sensorPairModel
	camera    cameraPairModel
	unitree   unitreePairModel
	arm       armPairModel
	active    pairTab
	started   [pairTabCount]bool
	width     int
	done      bool
}

func newDevicePairModel(bt bttable.Handler, sensor *sensorPairHandler, camera *cameraPairHandler, unitree ...*unitreePairHandler) devicePairModel {
	var unitreeHandler *unitreePairHandler
	if len(unitree) > 0 {
		unitreeHandler = unitree[0]
	}
	return devicePairModel{
		bluetooth: bttable.NewModel(nil).WithHandler(bt),
		sensor:    newSensorPairModel(sensor),
		camera:    newCameraPairModel(camera),
		unitree:   newUnitreePairModel(unitreeHandler),
		arm:       newArmPairModel(nil),
	}
}

func tagPairCmd(cmd tea.Cmd, tab pairTab) tea.Cmd {
	if cmd == nil {
		return nil
	}
	return func() tea.Msg {
		msg := cmd()
		if batch, ok := msg.(tea.BatchMsg); ok {
			tagged := make(tea.BatchMsg, 0, len(batch))
			for _, child := range batch {
				tagged = append(tagged, tagPairCmd(child, tab))
			}
			return tagged
		}
		if _, ok := msg.(tea.QuitMsg); ok {
			return msg
		}
		return pairTabMsg{tab: tab, msg: msg}
	}
}

func (m devicePairModel) Init() tea.Cmd {
	if m.active == pairArmTab {
		return tagPairCmd(m.arm.Init(), pairArmTab)
	}
	if m.active == pairUnitreeTab {
		return tagPairCmd(m.unitree.Init(), pairUnitreeTab)
	}
	if m.active == pairCameraTab {
		return tagPairCmd(m.camera.Init(), pairCameraTab)
	}
	if m.active == pairSensorLinkTab {
		return tagPairCmd(m.sensor.Init(), pairSensorLinkTab)
	}
	return tagPairCmd(m.bluetooth.Init(), pairBluetoothTab)
}

func (m devicePairModel) updateTab(tab pairTab, msg tea.Msg) (devicePairModel, tea.Cmd) {
	if tab == pairArmTab {
		updated, cmd := m.arm.Update(msg)
		m.arm = updated.(armPairModel)
		return m, tagPairCmd(cmd, tab)
	}
	if tab == pairUnitreeTab {
		updated, cmd := m.unitree.Update(msg)
		m.unitree = updated.(unitreePairModel)
		return m, tagPairCmd(cmd, tab)
	}
	if tab == pairCameraTab {
		updated, cmd := m.camera.Update(msg)
		m.camera = updated.(cameraPairModel)
		return m, tagPairCmd(cmd, tab)
	}
	if tab == pairSensorLinkTab {
		updated, cmd := m.sensor.Update(msg)
		m.sensor = updated.(sensorPairModel)
		return m, tagPairCmd(cmd, tab)
	}
	updated, cmd := m.bluetooth.Update(msg)
	m.bluetooth = updated.(bttable.Model)
	return m, tagPairCmd(cmd, tab)
}

func (m devicePairModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// Init starts the initial tab before the first message arrives.
	m.started[m.active] = true
	switch msg := msg.(type) {
	case pairTabMsg:
		return m.updateTab(msg.tab, msg.msg)
	case tea.WindowSizeMsg:
		m.width = msg.Width
		msg.Height = max(1, msg.Height-2)
		var btCmd, sensorCmd, cameraCmd, unitreeCmd, armCmd tea.Cmd
		m, btCmd = m.updateTab(pairBluetoothTab, msg)
		m, sensorCmd = m.updateTab(pairSensorLinkTab, msg)
		m, cameraCmd = m.updateTab(pairCameraTab, msg)
		m, unitreeCmd = m.updateTab(pairUnitreeTab, msg)
		m, armCmd = m.updateTab(pairArmTab, msg)
		return m, tea.Batch(btCmd, sensorCmd, cameraCmd, unitreeCmd, armCmd)
	case tea.KeyMsg:
		if m.active == pairArmTab && m.arm.confirming && msg.String() != "ctrl+c" {
			return m.updateTab(pairArmTab, msg)
		}
		// Credentials may contain shortcut keys. Escape cancels the form,
		// and Tab changes its field; Ctrl+C still exits the entire picker.
		if m.active == pairCameraTab && m.camera.editing && msg.String() != "ctrl+c" {
			return m.updateTab(pairCameraTab, msg)
		}
		if m.active == pairUnitreeTab && m.unitree.form != unitreeNoForm && msg.String() != "ctrl+c" {
			return m.updateTab(pairUnitreeTab, msg)
		}
		switch msg.String() {
		case "q", "esc", "ctrl+c":
			m.unitree.clearForm()
			m.unitree.account = nil
			m.done = true
			return m, tea.Quit
		case "tab", "shift+tab":
			direction := pairTab(1)
			if msg.String() == "shift+tab" {
				direction = pairTabCount - 1
			}
			m.active = (m.active + direction) % pairTabCount
			if !m.started[m.active] {
				m.started[m.active] = true
				return m, m.Init()
			}
			// Refresh on each visit. Children ignore a rescan while an
			// earlier scan or operation is still running.
			return m.updateTab(m.active, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
		}
		return m.updateTab(m.active, msg)
	}
	return m, nil
}

func (m devicePairModel) View() string {
	if m.done {
		return ""
	}
	labels := []string{"Bluetooth", "SensorLink", "IP Cameras", "USB Arms", "Unitree"}
	header := ""
	for i, label := range labels {
		if i > 0 {
			header += devicePickerTabInactive.Render(" | ")
		}
		style := devicePickerTabInactive
		if pairTab(i) == m.active {
			style = devicePickerTabActive
		}
		header += style.Render(label)
	}
	if !(m.camera.editing && m.active == pairCameraTab) && !(m.unitree.form != unitreeNoForm && m.active == pairUnitreeTab) && !(m.arm.confirming && m.active == pairArmTab) {
		header += devicePickerTabInactive.Render("  (tab switch)")
	}
	if m.width > 0 {
		header = tui.CropANSIView(header, 0, m.width)
	}
	body := m.bluetooth.View()
	if m.active == pairSensorLinkTab {
		body = m.sensor.View()
	} else if m.active == pairCameraTab {
		body = m.camera.View()
	} else if m.active == pairUnitreeTab {
		body = m.unitree.View()
	} else if m.active == pairArmTab {
		body = m.arm.View()
	}
	return header + "\n\n" + body
}
