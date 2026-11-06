package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// BuildStepMsg delivers a parser event to the model.
type BuildStepMsg BuildStepEvent

// BuildAllDoneMsg signals the build finished (Err nil on success).
type BuildAllDoneMsg struct{ Err error }

type buildRow struct {
	id      string
	kind    BuildVertexKind
	display string
	status  BuildStepStatus
	dur     time.Duration
	detail  string
	bytes   ByteProgress
	// started is when the row last began running; it drives the elapsed time.
	started time.Time
}

// BuildStepsModel renders a live, collapsing list of buildx steps for a single
// service build.
type BuildStepsModel struct {
	title   string
	rows    []buildRow
	byID    map[string]int
	spinner spinner.Model
	hints   hintRotator
	width   int
	tally   BuildTallyCounter
	done    bool
	err     error
	now     func() time.Time
}

// NewBuildStepsModel returns a model with the given title (e.g. the
// "Building image for linux/amd64..." line).
func NewBuildStepsModel(title string) BuildStepsModel {
	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = lipgloss.NewStyle().Foreground(ColorPrimary)
	return BuildStepsModel{
		title:   title,
		byID:    map[string]int{},
		spinner: s,
		hints:   newHintRotator(),
		now:     time.Now,
	}
}

// WithoutHints drops the rotating "Tip:" line; see ProgressModel.WithoutHints.
func (m BuildStepsModel) WithoutHints() BuildStepsModel {
	m.hints = hintRotator{}
	return m
}

// Init implements tea.Model.
func (m BuildStepsModel) Init() tea.Cmd {
	return tea.Batch(m.spinner.Tick, m.hints.tick())
}

// Update implements tea.Model.
func (m BuildStepsModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
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
	case BuildStepMsg:
		m.applyEvent(BuildStepEvent(msg))
	case BuildAllDoneMsg:
		m.done = true
		m.err = msg.Err
		return m, tea.Quit
	}
	return m, nil
}

func (m *BuildStepsModel) applyEvent(e BuildStepEvent) {
	m.tally.Observe(e)
	i, ok := m.byID[e.ID]
	if !ok {
		m.rows = append(m.rows, buildRow{
			id: e.ID, kind: e.Kind, display: e.Display,
			status: e.Status, detail: e.Detail, bytes: e.Bytes,
			started: m.now(),
		})
		m.byID[e.ID] = len(m.rows) - 1
		return
	}
	if e.Status == BuildStepRunning && m.rows[i].status != BuildStepRunning {
		// A finished step BuildKit reopened (a lazily pulled base image) times
		// its new run from now.
		m.rows[i].started = m.now()
	}
	m.rows[i].status = e.Status
	m.rows[i].dur = e.Dur
	if e.Status == BuildStepRunning {
		// Repeated Running events carry progress updates; terminal events clear
		// the sub-line so a finished step collapses back to one row.
		m.rows[i].detail = e.Detail
		m.rows[i].bytes = e.Bytes
	} else {
		m.rows[i].detail = ""
		m.rows[i].bytes = ByteProgress{}
	}
}

var (
	bsCheck = lipgloss.NewStyle().Foreground(ColorAccent).Bold(true)
	bsCache = lipgloss.NewStyle().Foreground(ColorPrimary)
	bsCross = lipgloss.NewStyle().Foreground(ColorError).Bold(true)
	bsDim   = lipgloss.NewStyle().Foreground(ColorDim)
	bsTitle = lipgloss.NewStyle().Foreground(ColorPrimary)
)

const buildStepLabelWidth = 34

// maxVisibleFinishedRows bounds how many completed steps stay on screen. Running
// steps are never dropped — they each occupy two lines once they have a detail
// sub-line, and a 40-step build would otherwise scroll its own progress away.
const maxVisibleFinishedRows = 8

// View implements tea.Model.
func (m BuildStepsModel) View() string {
	if m.done {
		return ""
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("%s %s\n", m.spinner.View(), bsTitle.Render(m.title)))

	labelWidth := m.labelWidth()
	visible, elided := m.visibleRows()
	if elided > 0 {
		sb.WriteString(fmt.Sprintf("  %s\n", bsDim.Render(fmt.Sprintf("… %d earlier steps", elided))))
	}
	now := m.now()
	for _, r := range visible {
		label := truncateDetail(r.display, labelWidth)
		switch r.status {
		case BuildStepRunning:
			if el := runningElapsed(r.started, now); el != "" {
				sb.WriteString(fmt.Sprintf("  %s %s %s\n", m.spinner.View(), label, bsDim.Render(el)))
			} else {
				sb.WriteString(fmt.Sprintf("  %s %s\n", m.spinner.View(), label))
			}
			if d := r.progressLine(); d != "" {
				sb.WriteString(fmt.Sprintf("      %s\n", bsDim.Render(d)))
			}
		case BuildStepCached:
			sb.WriteString(fmt.Sprintf("  %s %s %s\n", bsCache.Render("⚡"), label, bsDim.Render("cached")))
		case BuildStepDone:
			sb.WriteString(fmt.Sprintf("  %s %s %s\n", bsCheck.Render("✓"), label, bsDim.Render(r.dur.Round(time.Millisecond).String())))
		case BuildStepFailed:
			sb.WriteString(fmt.Sprintf("  %s %s\n", bsCross.Render("✗"), label))
		}
	}
	if hint := m.hints.view(m.width); hint != "" {
		sb.WriteString(hint)
		sb.WriteString("\n")
	}
	return sb.String()
}

// labelWidth grows the step label to fit the terminal, leaving room for the
// spinner and the trailing duration. A step label like "RUN apt-get update &&
// apt-get install …" is not worth much cut at 34 columns, and it now sits above
// a detail line up to maxDetailLen wide.
func (m BuildStepsModel) labelWidth() int {
	if m.width <= 0 {
		return buildStepLabelWidth
	}
	w := m.width - 24
	if w < buildStepLabelWidth {
		return buildStepLabelWidth
	}
	if w > maxDetailLen {
		return maxDetailLen
	}
	return w
}

// visibleRows keeps every running/failed row plus the most recent finished ones,
// returning how many older finished rows were dropped.
func (m BuildStepsModel) visibleRows() ([]buildRow, int) {
	finished := 0
	for _, r := range m.rows {
		if r.status == BuildStepCached || r.status == BuildStepDone {
			finished++
		}
	}
	drop := finished - maxVisibleFinishedRows
	if drop <= 0 {
		return m.rows, 0
	}
	out := make([]buildRow, 0, len(m.rows)-drop)
	dropped := 0
	for _, r := range m.rows {
		if dropped < drop && (r.status == BuildStepCached || r.status == BuildStepDone) {
			dropped++
			continue
		}
		out = append(out, r)
	}
	return out, dropped
}

// runningElapsed is how long a running row has been going, in the same
// Duration format a finished row shows but whole seconds: the view redraws on
// every spinner frame, and ticking milliseconds would be noise. It stays empty
// for the first second so quick steps do not flash "0s".
func runningElapsed(started, now time.Time) string {
	if started.IsZero() {
		return ""
	}
	d := now.Sub(started).Truncate(time.Second)
	if d < time.Second {
		return ""
	}
	return d.String()
}

// progressLine is the dim sub-line under a running step: the tool's own progress
// ("[525/1027] 51% Compiling WendyKit") and/or BuildKit's transfer counters.
func (r buildRow) progressLine() string { return joinProgress(r.detail, r.bytes) }

// Err returns the terminal error (ErrCancelled on ctrl+c, the build error from
// BuildAllDoneMsg, or nil).
func (m BuildStepsModel) Err() error { return m.err }

// Tally returns the cached/rebuilt counts accumulated from step events.
func (m BuildStepsModel) Tally() BuildTally { return m.tally.Tally() }
