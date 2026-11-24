package commands

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
)

// Install status lines. Styling follows stdout's colour profile, so install-job
// logs and piped output stay plain.
var (
	installTickStyle   = lipgloss.NewStyle().Foreground(tui.ColorAccent)
	installResultStyle = lipgloss.NewStyle().Foreground(tui.ColorAccent).Bold(true)
	installHeadStyle   = lipgloss.NewStyle().Foreground(tui.ColorPrimary).Bold(true)
	installAskStyle    = lipgloss.NewStyle().Foreground(tui.ColorNotice)
	installDangerStyle = lipgloss.NewStyle().Foreground(tui.ColorError)
	installInfoStyle   = lipgloss.NewStyle().Foreground(tui.ColorInfo)
)

// printAnswer leaves an answered question on screen as one line; the prompt
// itself clears once answered.
func printAnswer(w io.Writer, question, answer string, danger bool) {
	mark := installAskStyle.Render("?")
	if danger {
		mark = installDangerStyle.Render("?")
	}
	sep := ": "
	if strings.HasSuffix(question, "?") {
		sep = " "
	}
	fmt.Fprintln(w, mark+" "+question+sep+tui.Value(answer))
}

func installHeader(w io.Writer, board, version, target, detail string) {
	line := installHeadStyle.Render("Installing WendyOS ") + tui.Value(version) + installHeadStyle.Render(" on ") +
		tui.Value(board) + installHeadStyle.Render(" → ") + tui.Value(target)
	if detail != "" {
		line += tui.Dim(" (" + detail + ")")
	}
	fmt.Fprintln(w, line)
}

// installImageDetail names the drive's size and the image variant going onto it.
func installImageDetail(d drive, storage string) string {
	var parts []string
	if d.SizeBytes > 0 {
		parts = append(parts, formatBytes(d.SizeBytes))
	}
	switch storage {
	case "nvme":
		parts = append(parts, "NVMe image")
	case "sd":
		parts = append(parts, "SD image")
	case "emmc":
		parts = append(parts, "eMMC image")
	}
	return strings.Join(parts, ", ")
}

func installDone(w io.Writer, text string, detail ...string) {
	line := installTickStyle.Render("✓") + " " + text
	if d := strings.Join(detail, " "); d != "" {
		line += " " + tui.Dim(d)
	}
	fmt.Fprintln(w, line)
}

func installResult(w io.Writer, text string) {
	fmt.Fprintln(w, installResultStyle.Render("✓ "+text))
}

// runProgress runs work behind prog's bar, or directly when no terminal can
// draw one (JSON, CI, services), noting title for the log instead.
func runProgress(title string, prog tui.ProgressModel, work func(send func(tui.ProgressUpdateMsg)) error) (workErr, uiErr error) {
	if jsonOutput || !isInteractiveTerminal() {
		installNote(os.Stdout, "%s...", title)
		return work(func(tui.ProgressUpdateMsg) {}), nil
	}
	p := tui.NewProgressProgram(prog)
	go func() {
		p.Send(tui.ProgressDoneMsg{Err: work(func(m tui.ProgressUpdateMsg) { p.Send(m) })})
	}()
	final, err := tui.RunProgram(p)
	if err != nil {
		return nil, fmt.Errorf("progress TUI: %w", err)
	}
	return final.(tui.ProgressModel).Err(), nil
}

// writtenDetail is the "Image written" detail; an unknown size is left out.
func writtenDetail(total int64, elapsed time.Duration) string {
	if total <= 0 {
		return "· in " + formatInstallDuration(elapsed)
	}
	return fmt.Sprintf("· %s in %s", tui.FormatBytes(total), formatInstallDuration(elapsed))
}

// formatInstallDuration is a whole-second duration such as "2m 14s".
func formatInstallDuration(d time.Duration) string {
	s := int(d.Round(time.Second) / time.Second)
	switch {
	case s < 1:
		return "1s"
	case s < 60:
		return fmt.Sprintf("%ds", s)
	case s < 3600:
		return fmt.Sprintf("%dm %ds", s/60, s%60)
	default:
		return fmt.Sprintf("%dh %dm", s/3600, s%3600/60)
	}
}

func installNote(w io.Writer, format string, args ...any) {
	fmt.Fprintln(w, tui.Dim(fmt.Sprintf(format, args...)))
}

func installWarn(w io.Writer, format string, args ...any) {
	fmt.Fprintln(w, tui.WarningMessage(fmt.Sprintf(format, args...)))
}

func installNext(w io.Writer, step, command string) {
	fmt.Fprintf(w, "%s %s %s\n", tui.Dim("→ Next:"), step, tui.Command(command))
}

// finishLiteInstall ends a Wendy Lite install; the next step is printed only
// once enrollment, when there is one, has succeeded.
func finishLiteInstall(w io.Writer, version string, enroll func() error) error {
	fmt.Fprintln(w)
	installResult(w, fmt.Sprintf("Flashed Wendy Lite %s.", version))
	if enroll != nil {
		if err := enroll(); err != nil {
			return err
		}
	}
	installNext(w, "the board restarts by itself, then run", "wendy discover")
	return nil
}
