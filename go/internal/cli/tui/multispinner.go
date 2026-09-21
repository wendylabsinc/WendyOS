package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
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
	Name    string
	Err     error
	Dur     time.Duration
	Cached  int
	Rebuilt int
}

// MultiSpinnerAllDoneMsg signals that all service builds have completed.
type MultiSpinnerAllDoneMsg struct{}

type multiSpinnerRow struct {
	name    string
	status  MultiSpinnerServiceStatus
	detail  string
	dur     time.Duration
	cached  int
	rebuilt int
	err     error
}

// MultiSpinnerModel is a Bubble Tea model that shows per-service build progress.
//
//	⠋ Building 3 services...
//	  ✓ postgres   2.1s
//	  ⠋ api        building...
//	  ⠋ frontend   building...
type MultiSpinnerModel struct {
	title  string
	rows   []multiSpinnerRow
	byName map[string]int
	// nameWidth is wide enough for the longest service name plus a separator.
	// Keeping it on the model avoids lipgloss wrapping names longer than the
	// old fixed 12-cell column into extra physical lines, which confuses the
	// Bubble Tea renderer's line accounting.
	nameWidth int
	spinner   spinner.Model
	hints     hintRotator
	width     int
	done      bool
	err       error
}

func NewMultiSpinner(title string, names []string) MultiSpinnerModel {
	rows := make([]multiSpinnerRow, len(names))
	byName := make(map[string]int, len(names))
	nameWidth := 12
	for i, n := range names {
		rows[i] = multiSpinnerRow{name: n, status: MultiSpinnerPending}
		byName[n] = i
		if w := lipgloss.Width(n) + 1; w > nameWidth {
			nameWidth = w
		}
	}

	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = lipgloss.NewStyle().Foreground(ColorPrimary)

	return MultiSpinnerModel{
		title:     title,
		rows:      rows,
		byName:    byName,
		nameWidth: nameWidth,
		spinner:   s,
		hints:     newHintRotator(),
	}
}

// Init implements tea.Model.
func (m MultiSpinnerModel) Init() tea.Cmd {
	return tea.Batch(m.spinner.Tick, m.hints.tick())
}

// Update implements tea.Model.
func (m MultiSpinnerModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		return m, nil

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

	case hintTickMsg:
		m.hints.next()
		return m, m.hints.tick()

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
				m.rows[i].cached = msg.Cached
				m.rows[i].rebuilt = msg.Rebuilt
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
	msDimStyle   = lipgloss.NewStyle().Foreground(ColorDim)
	msErrorStyle = lipgloss.NewStyle().Foreground(ColorError)
	msTitleStyle = lipgloss.NewStyle().Foreground(ColorPrimary)
)

// View implements tea.Model.
func (m MultiSpinnerModel) View() string {
	if m.done {
		return ""
	}

	var sb strings.Builder
	singleLine := strings.NewReplacer("\r", " ", "\n", " ", "\t", " ")
	nameWidth := m.nameWidth
	if m.width > 0 {
		// Reserve at least half of the row after the status icon for detail.
		nameWidth = min(nameWidth, max(1, (m.width-4)/2))
	}
	nameStyle := lipgloss.NewStyle().Width(nameWidth)
	renderName := func(name string) string {
		return nameStyle.Render(ansi.Truncate(singleLine.Replace(name), nameWidth-1, "…"))
	}
	writeLine := func(line string) {
		// Details come from build output. Embedded newlines or long rows must
		// not change the number of physical lines occupied by a service.
		line = singleLine.Replace(line)
		if m.width > 0 {
			line = ansi.Truncate(line, m.width, "…")
		}
		sb.WriteString(line)
		sb.WriteByte('\n')
	}
	writeLine(fmt.Sprintf("%s %s", m.spinner.View(), msTitleStyle.Render(m.title)))

	for _, r := range m.rows {
		switch r.status {
		case MultiSpinnerPending:
			writeLine(fmt.Sprintf("  %s %s%s",
				msDimStyle.Render("·"),
				msDimStyle.Render(renderName(r.name)),
				msDimStyle.Render("waiting"),
			))

		case MultiSpinnerRunning:
			detail := r.detail
			if detail == "" {
				detail = "building..."
			}
			writeLine(fmt.Sprintf("  %s %s%s",
				m.spinner.View(),
				renderName(r.name),
				msDimStyle.Render(detail),
			))

		case MultiSpinnerDone:
			note := fmt.Sprintf("built (%d cached, %d rebuilt) %s",
				r.cached, r.rebuilt, r.dur.Round(time.Millisecond))
			writeLine(fmt.Sprintf("  %s %s%s",
				msCheckStyle.Render("✓"),
				renderName(r.name),
				msDimStyle.Render(note),
			))

		case MultiSpinnerFailed:
			writeLine(fmt.Sprintf("  %s %s%s",
				msCrossStyle.Render("✗"),
				renderName(r.name),
				msErrorStyle.Render("failed"),
			))
		}
	}

	if hint := m.hints.view(m.width); hint != "" {
		sb.WriteString(hint)
		sb.WriteString("\n")
	}

	return sb.String()
}

func (m MultiSpinnerModel) Err() error {
	return m.err
}
