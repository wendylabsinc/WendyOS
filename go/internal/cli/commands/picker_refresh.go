package commands

import (
	"context"
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/wendylabsinc/wendy/internal/cli/tui"
)

type refreshingPickerLoadMsg struct {
	items []tui.PickerItem
	err   error
}

type refreshingPickerModel struct {
	picker   tui.PickerModel
	ctx      context.Context
	interval time.Duration
	load     func(context.Context) ([]tui.PickerItem, error)
	err      error
}

func newRefreshingPickerModel(
	ctx context.Context,
	title string,
	interval time.Duration,
	load func(context.Context) ([]tui.PickerItem, error),
) refreshingPickerModel {
	return refreshingPickerModel{
		picker:   tui.NewPickerWithTitle(title),
		ctx:      ctx,
		interval: interval,
		load:     load,
	}
}

func (m refreshingPickerModel) Init() tea.Cmd {
	return m.loadCmd()
}

func (m refreshingPickerModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case refreshingPickerLoadMsg:
		if msg.err != nil {
			m.err = msg.err
			return m, tea.Quit
		}
		updated, _ := m.picker.Update(tui.PickerSetMsg{Items: msg.items})
		m.picker = updated.(tui.PickerModel)
		return m, delayThen(m.interval, m.loadCmd())
	default:
		updated, cmd := m.picker.Update(msg)
		m.picker = updated.(tui.PickerModel)
		return m, cmd
	}
}

func (m refreshingPickerModel) View() string {
	return m.picker.View()
}

func (m refreshingPickerModel) loadCmd() tea.Cmd {
	return func() tea.Msg {
		items, err := m.load(m.ctx)
		return refreshingPickerLoadMsg{items: items, err: err}
	}
}

func pickRefreshingItem(
	ctx context.Context,
	title string,
	interval time.Duration,
	load func(context.Context) ([]tui.PickerItem, error),
) (tui.PickerItem, error) {
	pollCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	model := newRefreshingPickerModel(pollCtx, title, interval, load)
	p := tea.NewProgram(model)
	finalModel, err := p.Run()
	if err != nil {
		return tui.PickerItem{}, fmt.Errorf("picker: %w", err)
	}

	rm := finalModel.(refreshingPickerModel)
	if rm.err != nil {
		return tui.PickerItem{}, rm.err
	}
	if rm.picker.Cancelled() {
		return tui.PickerItem{}, ErrUserCancelled
	}
	sel := rm.picker.Selected()
	if sel == nil {
		return tui.PickerItem{}, fmt.Errorf("no item selected")
	}
	return *sel, nil
}

func pickRefreshingFromItems(
	ctx context.Context,
	title string,
	interval time.Duration,
	load func(context.Context) ([]tui.PickerItem, error),
) (string, error) {
	item, err := pickRefreshingItem(ctx, title, interval, load)
	if err != nil {
		return "", err
	}
	value, ok := item.Value.(string)
	if !ok {
		return "", fmt.Errorf("invalid picker selection")
	}
	return value, nil
}
