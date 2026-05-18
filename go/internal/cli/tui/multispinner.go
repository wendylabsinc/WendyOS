package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// MultiSpinnerServiceStatus describes the build state of a single service row.
type MultiSpinnerServiceStatus int

const (
	MultiSpinnerPending MultiSpinnerServiceStatus = iota
	MultiSpinnerRunning
	MultiSpinnerDone
	MultiSpinnerFailed
)

// MultiSpinnerStartMsg signals that a service's build has begun.
type MultiSpinnerStartMsg struct{ Name string }

// MultiSpinnerDetailMsg updates the detail text shown next to a service row.
type MultiSpinnerDetailMsg struct {
	Name   string
	Detail string
}

// MultiSpinnerDoneMsg signals that a service's build has finished.
type MultiSpinnerDoneMsg struct {
	Name string
	Err  error
	Dur  time.Duration
}

// MultiSpinnerAllDoneMsg signals that all service builds have completed.
type MultiSpinnerAllDoneMsg struct{}

type multiSpinnerRow struct {
	name   string
	status MultiSpinnerServiceStatus
	detail string
	dur    time.Duration
	err    error
}

// MultiSpinnerModel is a Bubble Tea model that shows per-service build progress.
//
//	⠋ Building 3 services...
//	  ✓ postgres   2.1s
//	  ⠋ api        building...
//	  ⠋ frontend   building...
type MultiSpinnerModel struct {
	title   string
	rows    []multiSpinnerRow
	byName  map[string]int
	spinner spinner.Model
	done    bool
	err     error
}

// NewMultiSpinner creates a MultiSpinnerModel with the given title and service
// names listed in display order.
func NewMultiSpinner(title string, names []string) MultiSpinnerModel {
	rows := make([]multiSpinnerRow, len(names))
	byName := make(map[string]int, len(names))
	for i, n := range names {
		rows[i] = multiSpinnerRow{name: n, status: MultiSpinnerPending}
		byName[n] = i
	}

	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = lipgloss.NewStyle().Foreground(ColorPrimary)

	return MultiSpinnerModel{
		title:   title,
		rows:    rows,
		byName:  byName,
		spinner: s,
	}
}

// Init implements tea.Model.
func (m MultiSpinnerModel) Init() tea.Cmd {
	return m.spinner.Tick
}

// Update implements tea.Model.
func (m MultiSpinnerModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			m.done = true
			m.err = ErrCancelled
			return m, tea.Quit
		}

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd

	case MultiSpinnerStartMsg:
		if i, ok := m.byName[msg.Name]; ok {
			m.rows[i].status = MultiSpinnerRunning
		}

	case MultiSpinnerDetailMsg:
		if i, ok := m.byName[msg.Name]; ok {
			m.rows[i].detail = msg.Detail
		}

	case MultiSpinnerDoneMsg:
		if i, ok := m.byName[msg.Name]; ok {
			m.rows[i].dur = msg.Dur
			if msg.Err != nil {
				m.rows[i].status = MultiSpinnerFailed
				m.rows[i].err = msg.Err
			} else {
				m.rows[i].status = MultiSpinnerDone
				m.rows[i].detail = ""
			}
		}

	case MultiSpinnerAllDoneMsg:
		m.done = true
		return m, tea.Quit
	}

	return m, nil
}

var (
	msCheckStyle = lipgloss.NewStyle().Foreground(ColorAccent).Bold(true)
	msCrossStyle = lipgloss.NewStyle().Foreground(ColorError).Bold(true)
	msNameStyle  = lipgloss.NewStyle().Width(12)
	msDimStyle   = lipgloss.NewStyle().Foreground(ColorDim)
	msErrorStyle = lipgloss.NewStyle().Foreground(ColorError)
	msTitleStyle = lipgloss.NewStyle().Foreground(ColorPrimary)
)

// View implements tea.Model.
func (m MultiSpinnerModel) View() string {
	if m.done {
		return ""
	}

	running := 0
	for _, r := range m.rows {
		if r.status == MultiSpinnerRunning || r.status == MultiSpinnerPending {
			running++
		}
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("%s %s\n", m.spinner.View(), msTitleStyle.Render(m.title)))

	for _, r := range m.rows {
		switch r.status {
		case MultiSpinnerPending:
			sb.WriteString(fmt.Sprintf("  %s %s%s\n",
				msDimStyle.Render("·"),
				msDimStyle.Render(msNameStyle.Render(r.name)),
				msDimStyle.Render("waiting"),
			))

		case MultiSpinnerRunning:
			detail := r.detail
			if detail == "" {
				detail = "building..."
			}
			sb.WriteString(fmt.Sprintf("  %s %s%s\n",
				m.spinner.View(),
				msNameStyle.Render(r.name),
				msDimStyle.Render(detail),
			))

		case MultiSpinnerDone:
			sb.WriteString(fmt.Sprintf("  %s %s%s\n",
				msCheckStyle.Render("✓"),
				msNameStyle.Render(r.name),
				msDimStyle.Render(fmt.Sprintf("built (%s)", r.dur.Round(time.Millisecond))),
			))

		case MultiSpinnerFailed:
			sb.WriteString(fmt.Sprintf("  %s %s%s\n",
				msCrossStyle.Render("✗"),
				msNameStyle.Render(r.name),
				msErrorStyle.Render("failed"),
			))
		}
	}

	return sb.String()
}

// Err returns the first error recorded, if any.
func (m MultiSpinnerModel) Err() error {
	return m.err
}
