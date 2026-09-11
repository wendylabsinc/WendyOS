package commands

// Pre-scan briefing for Thor USB flashing. Cross-platform (no gousb) so both the
// macOS/Linux install path and the Windows path show the same cabling/recovery
// instructions; on Windows an extra note explains WinUSB driver installation
// install and its UAC prompt.

import (
	"errors"
	"fmt"
	"runtime"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
)

// Styles for the pre-flash briefing. Color carries the hierarchy: emerald
// section headers, amber for things the user physically presses/disconnects,
// sky for cabling/ports, so the box scans rather than reading as a wall of text.
var (
	briefBorder = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(tui.ColorBorder).
			Padding(1, 3)
	briefMarker = lipgloss.NewStyle().Foreground(tui.ColorAccent).Bold(true)
	briefTitle  = lipgloss.NewStyle().Foreground(tui.ColorPrimary).Bold(true)
	briefKey    = lipgloss.NewStyle().Foreground(tui.ColorNotice).Bold(true) // buttons / "disconnect"
	briefPort   = lipgloss.NewStyle().Foreground(tui.Sky500).Bold(true)      // cabling / ports
	briefNum    = lipgloss.NewStyle().Foreground(tui.ColorAccent).Bold(true)
	briefDim    = lipgloss.NewStyle().Foreground(tui.ColorDim)
)

// thorRecoveryBriefingBox renders the cabling and recovery-mode steps the user
// must complete before a Thor will appear in the USB recovery scan, as a colored,
// scannable box.
func thorRecoveryBriefingBox() string {
	section := func(title string) string {
		return briefMarker.Render("●") + " " + briefTitle.Render(title)
	}
	step := func(n int, text string) string {
		return "    " + briefNum.Render(fmt.Sprintf("%d.", n)) + " " + text
	}
	lines := []string{
		section("Storage"),
		"  WendyOS installs to the Thor's internal flash + NVMe — it uses no",
		"  external USB drive. " + briefKey.Render("Disconnect any USB drive now") + " and leave it out.",
		"",
		section("USB-C cabling"),
		"  Connect this computer to the " + briefPort.Render("USB-C port next to the HDMI port") + ".",
		"  The other USB-C port is power-only.",
		"",
		section("Entering recovery mode"),
		"  Front buttons, left → right:  " +
			briefKey.Render("Power") + briefDim.Render(" · ") +
			briefKey.Render("Force Recovery") + briefDim.Render(" · ") +
			briefKey.Render("Reset"),
		"",
		step(1, "Start with the Thor "+briefDim.Render("unplugged and powered off")+"."),
		step(2, "Plug in power."),
		step(3, "Briefly press "+briefKey.Render("Power")+" (left)."),
		step(4, "Hold "+briefKey.Render("Force Recovery")+" (middle); briefly tap "+briefKey.Render("Reset")+" (right),"),
		"       then release " + briefKey.Render("Force Recovery") + ".",
		step(5, "Connect the "+briefPort.Render("USB-C port next to HDMI")+" to this computer."),
	}
	return briefBorder.Render(strings.Join(lines, "\n"))
}

// thorWindowsDriverNote returns a short note (Windows only) explaining that wendy
// installs a WinUSB driver for the Thor and that the first install prompts for
// administrator approval. Empty string on other platforms.
func thorWindowsDriverNote() string {
	if runtime.GOOS != "windows" {
		return ""
	}
	lines := []string{
		briefMarker.Render("●") + " " + briefTitle.Render("Windows driver setup"),
		"  Wendy checks the selected Thor's " + briefKey.Render("WinUSB driver") + " and its flashing gadget support.",
		"  If installation or an update is needed, Windows asks for " + briefKey.Render("administrator approval") + ".",
		"  A compatible driver is reused without another setup prompt.",
		"  The flashing gadget is checked again after the board restarts.",
	}
	return briefBorder.Render(strings.Join(lines, "\n"))
}

// confirmThorReady prints a titled recovery-mode briefing and asks the user to
// confirm the target Thor is connected and in recovery mode before scanning.
// Returns ErrUserCancelled if the user declines or cancels.
func confirmThorReady(version string, force bool) error {
	fmt.Println()
	fmt.Println(tui.Header("Flashing WendyOS " + version))
	fmt.Println(thorRecoveryBriefingBox())
	if note := thorWindowsDriverNote(); note != "" {
		fmt.Println(note)
	}
	if force {
		return nil
	}
	fmt.Println()
	ok, err := tui.Confirm("Is the target Thor connected and in recovery mode?")
	if errors.Is(err, tui.ErrCancelled) || (err == nil && !ok) {
		return ErrUserCancelled
	}
	return err
}
