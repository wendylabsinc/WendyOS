package tui

import (
	"math/rand"
	"strings"
	"sync/atomic"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// hintInterval is how often a running progress indicator rotates to a new hint.
const hintInterval = 7 * time.Second

// ProgressHints are short, educational "did you know" tips surfaced while a
// progress indicator is running. They give users a heads-up on what the Wendy
// CLI can do during otherwise-idle wait time. Edit this list freely.
var ProgressHints = []string{
	"Tip: Stream live app output with 'wendy device logs'",
	"Tip: 'wendy run --watch' rebuilds and redeploys automatically as you edit files",
	"Tip: Inspect a device's hardware and capabilities with 'wendy device info'",
	"Tip: Update the device agent with 'wendy device update'",
	"Tip: Discover devices on your network with 'wendy discover'",
	"Tip: List and manage running apps with 'wendy device apps'",
	"Tip: Watch live CPU, memory, and GPU usage with 'wendy device top'",
	"Tip: Scaffold a new project in seconds with 'wendy init'",
	"Tip: Grant hardware access (GPU, camera, GPIO) via entitlements in wendy.json",
	"Tip: Validate your wendy.json against the schema with 'wendy json validate'",
	"Tip: Reach a device behind NAT by forwarding a port with 'wendy cloud tunnel'",
	"Tip: Let Claude/Codex drive and debug your devices - set up the MCP server with 'wendy mcp setup'",
}

// hintTickMsg is emitted on each hint rotation tick.
type hintTickMsg struct{}

var hintStyle = lipgloss.NewStyle().Foreground(ColorDim)

// hintRotator holds the currently displayed hint and rotates through a list.
type hintRotator struct {
	hints   []string
	current string
}

// runningCommand holds the path of the command this process runs, such as
// "wendy device info"; see SetRunningCommand.
var runningCommand atomic.Value

// SetRunningCommand records the running command's path, so a progress tip
// never recommends the very command the user is already running.
func SetRunningCommand(path string) { runningCommand.Store(path) }

// newHintRotator builds a rotator over ProgressHints with a random first hint.
func newHintRotator() hintRotator {
	running, _ := runningCommand.Load().(string)
	r := hintRotator{hints: hintsBesides(ProgressHints, running)}
	if len(r.hints) > 0 {
		r.current = r.hints[rand.Intn(len(r.hints))]
	}
	return r
}

// hintsBesides drops the tips that name running as a whole command, e.g.
// 'wendy device info' while it runs. A variant is kept: 'wendy run --watch'
// is worth knowing during a plain 'wendy run'.
func hintsBesides(hints []string, running string) []string {
	if running == "" {
		return hints
	}
	quoted := "'" + running + "'"
	kept := make([]string, 0, len(hints))
	for _, hint := range hints {
		if !strings.Contains(hint, quoted) {
			kept = append(kept, hint)
		}
	}
	return kept
}

// tick returns a command that fires a hintTickMsg after hintInterval, or nil
// when there is nothing to rotate through (see the models' WithoutHints).
func (r hintRotator) tick() tea.Cmd {
	if len(r.hints) == 0 {
		return nil
	}
	return tea.Tick(hintInterval, func(time.Time) tea.Msg {
		return hintTickMsg{}
	})
}

// next advances to a different random hint. It is a no-op when there are fewer
// than two hints to choose from.
func (r *hintRotator) next() {
	if len(r.hints) < 2 {
		return
	}
	for {
		candidate := r.hints[rand.Intn(len(r.hints))]
		if candidate != r.current {
			r.current = candidate
			return
		}
	}
}

// view renders the current hint as a dimmed line, or "" when there is no hint.
//
// maxWidth is the terminal width in display columns. When it is > 0 the rendered
// line is truncated (ANSI-aware, accounting for the width-2 "💡 " prefix) so it
// occupies exactly one physical row. This matters because Bubble Tea redraws its
// frames in place by counting logical lines: a hint longer than the terminal
// width would soft-wrap onto a second physical row that Bubble Tea does not
// account for, desyncing the redraw and leaving garbled/duplicated spinner
// lines. A maxWidth <= 0 (no tea.WindowSizeMsg seen yet) disables truncation and
// preserves the original behavior.
func (r hintRotator) view(maxWidth int) string {
	if r.current == "" {
		return ""
	}
	rendered := hintStyle.Render("💡 " + r.current)
	if maxWidth > 0 {
		rendered = ansi.Truncate(rendered, maxWidth, "…")
	}
	return rendered
}
