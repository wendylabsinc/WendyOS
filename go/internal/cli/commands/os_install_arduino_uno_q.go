package commands

// Arduino UNO Q: a Qualcomm QRB2210 board flashed over EDL by the Dragonwing
// flow. Its registry entry and its wording (EDL pins, eMMC) live here.

import "github.com/wendylabsinc/wendy/go/internal/cli/qdl"

const arduinoUnoQDeviceType = "arduino-uno-q"

var arduinoUnoQBoard = dragonwingBoard{
	deviceType: arduinoUnoQDeviceType,
	msmID:      0x001c80e1,
	storage:    qdl.StorageEMMC,
	guide:      arduinoUnoQGuide,
	// Its smaller rootfs slots make for a smaller bundle and a lower ratio.
	extractedFactor: 11,
}

func init() {
	dragonwingBoards = append(dragonwingBoards, arduinoUnoQBoard)
}

// arduinoUnoQGuide is the UNO Q's wording: EDL is a pin short, held only for
// the boot it was made on.
func arduinoUnoQGuide() edlGuide {
	name := humanReadableDeviceType(arduinoUnoQDeviceType)
	return edlGuide{
		storage:  "eMMC",
		rewrites: "the board's eMMC, boot firmware included",
		intro: []string{
			"  WendyOS installs to the board's internal eMMC — no external drive is used.",
			"  The boot firmware, both A/B slots, the config partition and " + briefKey.Render("/data"),
			"  are rewritten.",
		},
		entry: []string{
			briefSection("USB cabling"),
			"  Connect this computer to the board's " + briefPort.Render("USB-C port") + ".",
			"",
			briefSection("Entering EDL mode"),
			"  EDL means Emergency Download Mode, used to flash the board.",
			"  Find the small " + briefKey.Render("10-pin JCTL header") + " near the USB-C socket.",
			"  The " + briefKey.Render("EDL pins") + " are the pair at the end of JCTL " + briefKey.Render("farthest from USB-C") + ".",
			"  Connect that pair together while powering up to enter EDL.",
			"",
			"  Pin-location photo (orange outline) in Arduino's guide:",
			"  https://docs.arduino.cc/software/app-lab/configure/flash/",
			"",
			briefStep(1, "Unplug the board."),
			briefStep(2, "Connect the two "+briefKey.Render("EDL pins")+" with a jumper cap or a"),
			"       female-to-female jumper wire.",
			briefStep(3, "Keep them shorted and plug in the USB-C cable. The board shows"),
			"       no sign of life in EDL — that is expected.",
			"",
			"  " + briefDim.Render("The short can come off once the board is in EDL. After flashing,"),
			"  " + briefDim.Render("unplug the board, remove any short and plug it back in."),
		},
		ready: "plugged in with the EDL pins shorted",
		hints: recoveryWaitHints{
			label:       name,
			family:      name,
			mode:        "EDL mode",
			cablingLine: "the USB-C cable is plugged into the board's " + briefPort.Render("USB-C port"),
			buttonLine:  "the " + briefKey.Render("EDL pins") + " at JCTL's end farthest from USB-C were shorted at power-up",
		},
		done: []string{
			"  Now unplug the board, remove the " + briefKey.Render("EDL pins") +
				" short if it is still there, and plug it back in.",
			"  " + briefDim.Render("With the pins still shorted, it boots into EDL again instead of WendyOS."),
		},
		retry: "  Replug the board without the " + briefKey.Render("EDL pins") +
			" short to boot it, or shorted to retry.",
		reenter: []string{
			"  Short the " + briefKey.Render("EDL pins") + ", unplug and replug the board to",
			"  re-enter EDL, and run the same command again.",
		},
	}
}
