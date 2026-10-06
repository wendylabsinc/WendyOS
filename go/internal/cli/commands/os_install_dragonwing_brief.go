package commands

// Pre-scan briefing for Dragonwing EDL flashing: how to put the board into
// Emergency Download mode, which is a DIP switch rather than a button.

import (
	"errors"
	"fmt"
	"runtime"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
)

// edlGuide is a board's own wording for its storage and for entering and
// leaving EDL mode; the flash flow around it is shared.
type edlGuide struct {
	storage  string   // the flash as the board's documentation names it
	rewrites string   // what the destructive warning says is rewritten
	intro    []string // the briefing's storage lines
	entry    []string // the briefing's cabling and EDL-entry sections
	ready    string   // what the confirm prompt asks the user to have done
	hints    recoveryWaitHints
	done     []string // after a successful flash
	retry    string   // after a failure that wrote nothing
	reenter  []string // after a flash that did not finish
}

func briefSection(title string) string {
	return briefMarker.Render("●") + " " + briefTitle.Render(title)
}

func briefStep(n int, text string) string {
	return "    " + briefNum.Render(fmt.Sprintf("%d.", n)) + " " + text
}

// dipSwitch3Guide is the IQ EVKs' wording: EDL is a latching DIP switch.
func dipSwitch3Guide() edlGuide {
	return edlGuide{
		storage:  "UFS",
		rewrites: "the board's UFS",
		intro: []string{
			"  WendyOS installs to the board's internal UFS — no external drive is used.",
			"  Both A/B slots, the config partition and " + briefKey.Render("/data") + " are rewritten.",
		},
		entry: []string{
			briefSection("USB cabling"),
			"  Connect this computer to the board's " + briefPort.Render("USB0 (USB-C) port") + ".",
			"",
			briefSection("Entering EDL mode"),
			"  EDL is selected by a " + briefKey.Render("DIP switch") + ", not a button, so it stays set until",
			"  you move it back.",
			"",
			briefStep(1, "Power the board "+briefDim.Render("off")+"."),
			briefStep(2, "Set "+briefKey.Render("DIP switch 3")+" to "+briefKey.Render("ON")+"."),
			briefStep(3, "Power the board on. It boots straight into EDL and shows no"),
			"       display output — that is expected.",
			"",
			"  " + briefDim.Render("After flashing, set DIP switch 3 back to OFF and power-cycle,"),
			"  " + briefDim.Render("otherwise the board will keep booting into EDL."),
		},
		ready: "DIP switch 3 ON",
		hints: recoveryWaitHints{
			label:       "Dragonwing",
			family:      "Dragonwing",
			mode:        "EDL mode",
			cablingLine: "the USB-C cable is in the " + briefPort.Render("USB0 port"),
			buttonLine:  briefKey.Render("DIP switch 3") + " is " + briefKey.Render("ON") + ", and the board was power-cycled after setting it",
		},
		done: []string{
			"  Now set " + briefKey.Render("DIP switch 3") + " back to " + briefKey.Render("OFF") +
				" and power-cycle the board.",
			"  " + briefDim.Render("Left ON, it will boot into EDL again instead of WendyOS."),
		},
		retry: "  Set " + briefKey.Render("DIP switch 3") + " back to " + briefKey.Render("OFF") +
			" and power-cycle, or leave it ON to retry.",
		reenter: []string{
			"  Leave " + briefKey.Render("DIP switch 3") + " set to ON, power-cycle the board to",
			"  re-enter EDL, and run the same command again.",
		},
	}
}

// dragonwingEDLBriefingBox renders the steps needed before a board will show up
// in the EDL scan.
func dragonwingEDLBriefingBox(board dragonwingBoard) string {
	g := board.guide()
	lines := append([]string{briefSection("Storage")}, g.intro...)
	lines = append(lines,
		"  This "+briefKey.Render("is a factory reset")+": device identity, cloud enrollment,",
		"  saved Wi-Fi and app data are discarded and the board comes back as a new",
		"  device. The filesystem is recreated, not securely wiped.",
		"",
	)
	lines = append(lines, g.entry...)
	if runtime.GOOS == "windows" {
		lines = append(lines, "", briefSection("Windows USB driver"),
			"  Wendy checks the selected board's driver and installs or updates its",
			"  WinUSB binding if needed. Windows will ask for administrator access.",
			"  This replaces the selected EDL device's QDLoader binding if installed.")
	}
	return briefBorder.Render(strings.Join(lines, "\n"))
}

// confirmDragonwingReady prints the EDL briefing and confirms the board is
// ready before scanning USB.
func confirmDragonwingReady(board dragonwingBoard, version string, force bool) error {
	fmt.Println()
	fmt.Println(tui.Header("Flashing WendyOS " + version))
	fmt.Println(dragonwingEDLBriefingBox(board))
	if force {
		return nil
	}
	fmt.Println()
	ok, err := tui.Confirm("Is the target board connected and in EDL mode (" + board.guide().ready + ")?")
	if errors.Is(err, tui.ErrCancelled) || (err == nil && !ok) {
		return ErrUserCancelled
	}
	return err
}
