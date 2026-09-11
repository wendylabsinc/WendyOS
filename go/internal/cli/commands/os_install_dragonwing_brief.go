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

// dragonwingEDLBriefingBox renders the steps needed before an IQ-8275 will show
// up in the EDL scan.
func dragonwingEDLBriefingBox() string {
	section := func(title string) string {
		return briefMarker.Render("●") + " " + briefTitle.Render(title)
	}
	step := func(n int, text string) string {
		return "    " + briefNum.Render(fmt.Sprintf("%d.", n)) + " " + text
	}
	lines := []string{
		section("Storage"),
		"  WendyOS installs to the board's internal UFS — no external drive is used.",
		"  Both A/B slots are rewritten. " + briefKey.Render("/data is left untouched") + ", so device",
		"  identity and saved Wi-Fi survive; this is not a factory reset.",
		"",
		section("USB cabling"),
		"  Connect this computer to the board's " + briefPort.Render("USB0 (USB-C) port") + ".",
		"",
		section("Entering EDL mode"),
		"  EDL is selected by a " + briefKey.Render("DIP switch") + ", not a button, so it stays set until",
		"  you move it back.",
		"",
		step(1, "Power the board "+briefDim.Render("off")+"."),
		step(2, "Set "+briefKey.Render("DIP switch 3")+" to "+briefKey.Render("ON")+"."),
		step(3, "Power the board on. It boots straight into EDL and shows no"),
		"       display output — that is expected.",
		"",
		"  " + briefDim.Render("After flashing, set DIP switch 3 back to OFF and power-cycle,"),
		"  " + briefDim.Render("otherwise the board will keep booting into EDL."),
	}
	if runtime.GOOS == "windows" {
		lines = append(lines, "", section("Windows USB driver"),
			"  Wendy checks the selected board's driver and installs or updates its",
			"  WinUSB binding if needed. Windows will ask for administrator access.",
			"  This replaces the selected EDL device's QDLoader binding if installed.")
	}
	return briefBorder.Render(strings.Join(lines, "\n"))
}

// confirmDragonwingReady prints the EDL briefing and confirms the board is
// ready before scanning USB.
func confirmDragonwingReady(version string, force bool) error {
	fmt.Println()
	fmt.Println(tui.Header("Flashing WendyOS " + version))
	fmt.Println(dragonwingEDLBriefingBox())
	if force {
		return nil
	}
	fmt.Println()
	ok, err := tui.Confirm("Is the target board connected and in EDL mode (DIP switch 3 ON)?")
	if errors.Is(err, tui.ErrCancelled) || (err == nil && !ok) {
		return ErrUserCancelled
	}
	return err
}

// dragonwingEDLHints is the wait-UI text shown while polling for the board.
func dragonwingEDLHints() recoveryWaitHints {
	return recoveryWaitHints{
		label:       "Dragonwing",
		family:      "Dragonwing",
		mode:        "EDL mode",
		cablingLine: "the USB-C cable is in the " + briefPort.Render("USB0 port"),
		buttonLine:  briefKey.Render("DIP switch 3") + " is " + briefKey.Render("ON") + ", and the board was power-cycled after setting it",
	}
}
