package commands

import (
	"context"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
)

const wifiPickerRefreshInterval = 5 * time.Second

type localWifiScanMsg struct {
	networks []localWifiNetwork
	err      error
	cached   bool
}

type localWifiRefreshMsg struct{}

type localWifiPickerModel struct {
	picker    tui.PickerModel
	ctx       context.Context
	cached    func(context.Context) []localWifiNetwork
	scan      func(context.Context) ([]localWifiNetwork, error)
	selection wifiScanSelection
	cancelled bool
}

func newLocalWifiPickerModel(ctx context.Context) localWifiPickerModel {
	picker := tui.NewPickerWithTitleAndColumns("Select WiFi network (or esc to type manually)", wifiPickerColumns())
	picker.Filterable = true
	return localWifiPickerModel{
		picker: picker,
		ctx:    ctx,
		cached: cachedLocalWifiNetworks,
		scan:   scanLocalWifiNetworks,
	}
}

func (m localWifiPickerModel) Init() tea.Cmd {
	return func() tea.Msg {
		return localWifiScanMsg{networks: m.cached(m.ctx), cached: true}
	}
}

func (m localWifiPickerModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if m.picker.Cancelled() || m.picker.Selected() != nil || m.ctx.Err() != nil {
		return m, nil
	}
	switch msg := msg.(type) {
	case localWifiScanMsg:
		if msg.err == nil {
			updated, _ := m.picker.Update(tui.PickerSetMsg{Items: localWifiPickerItems(msg.networks)})
			m.picker = updated.(tui.PickerModel)
			m.selection.HadNetworks = len(msg.networks) > 0
		}
		if msg.cached {
			return m, m.scanCmd()
		}
		// Keep the previous results on transient failures, and retry after the
		// same delay. Only schedule another scan once this one has finished.
		m.selection.ScanErr = msg.err
		updated, _ := m.picker.Update(tui.PickerDoneMsg{})
		m.picker = updated.(tui.PickerModel)
		return m, tea.Tick(wifiPickerRefreshInterval, func(time.Time) tea.Msg {
			return localWifiRefreshMsg{}
		})
	case localWifiRefreshMsg:
		return m, m.scanCmd()
	case tea.KeyMsg:
		// Escape retains the advertised manual-entry behavior; Ctrl+C aborts
		// setup even when the user has typed a filter.
		if msg.Type == tea.KeyCtrlC {
			m.cancelled = true
		}
	}
	updated, cmd := m.picker.Update(msg)
	m.picker = updated.(tui.PickerModel)
	return m, cmd
}

func (m localWifiPickerModel) View() string {
	view := m.picker.View()
	if view != "" {
		view += "\n  Refreshes automatically. Ctrl+C to cancel.\n"
	}
	return view
}

func (m localWifiPickerModel) scanCmd() tea.Cmd {
	return func() tea.Msg {
		if err := m.ctx.Err(); err != nil {
			return localWifiScanMsg{err: err}
		}
		networks, err := m.scan(m.ctx)
		return localWifiScanMsg{networks: networks, err: err}
	}
}

func (m localWifiPickerModel) result() (wifiScanSelection, error) {
	if m.cancelled {
		return wifiScanSelection{}, ErrUserCancelled
	}
	sel := m.selection
	if picked := m.picker.Selected(); picked != nil {
		sel.SSID, _ = picked.Value.(string)
	}
	return sel, nil
}
