package commands

import (
	"errors"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/qdl"
)

func TestArduinoUnoQIsRegistered(t *testing.T) {
	b, ok := dragonwingBoardFor("arduino-uno-q")
	if !ok {
		t.Fatal("arduino-uno-q is not in the EDL board registry")
	}
	// The programmer initialises this storage; the wrong one fails Configure.
	if b.storage != qdl.StorageEMMC {
		t.Errorf("storage = %q, want %q", b.storage, qdl.StorageEMMC)
	}
	if got := humanReadableDeviceType(b.deviceType); got != "Arduino UNO Q" {
		t.Errorf("display name = %q", got)
	}
	// Published without the dragonwing- prefix, it must still stay out of the
	// disk-image flows.
	if !isFlashBundleDeviceType(b.deviceType) || !installedFromFlashBundle(deviceInfo{Key: b.deviceType}) {
		t.Error("arduino-uno-q is not treated as a flash-bundle board")
	}
}

// The bundle extracts to about ten times its size: the IQ boards' estimate
// would refuse hosts with room to spare.
func TestArduinoUnoQDiskSpaceEstimate(t *testing.T) {
	unoq, _ := dragonwingBoardFor("arduino-uno-q")
	const bundle, extracted = 548e6, 5.29e9
	needed := bundle * (1 + unoq.extractedFactor)
	if needed < bundle+extracted || needed > 1.5*(bundle+extracted) {
		t.Errorf("estimate %.1f GB for a %.1f GB need", needed/1e9, (bundle+extracted)/1e9)
	}
}

func TestVerifyArduinoUnoQChip(t *testing.T) {
	unoq, _ := dragonwingBoardFor("arduino-uno-q")
	if caution, err := verifyDragonwingBoard(unoq, qdl.ChipID{MsmID: 0x001c80e1}, nil, ""); err != nil || caution != "" {
		t.Errorf("its own chip id gave %q / %v", caution, err)
	}
	_, err := verifyDragonwingBoard(unoq, qdl.ChipID{MsmID: 0x002e70e1}, nil, "")
	if err == nil || !strings.Contains(err.Error(), "--device-type dragonwing-iq-8275") {
		t.Errorf("an IQ-8275 chip was accepted for a UNO Q install: %v", err)
	}
	caution, _ := verifyDragonwingBoard(unoq, qdl.ChipID{MsmID: 0x00123456}, nil, "")
	if !strings.Contains(caution, "an Arduino UNO Q") {
		t.Errorf("caution %q does not name the board the install targets", caution)
	}
}

// Every step of the flow tells the UNO Q user about its EDL pins, never about
// the EVKs' DIP switch.
func TestArduinoUnoQWording(t *testing.T) {
	unoq, _ := dragonwingBoardFor("arduino-uno-q")

	brief := dragonwingEDLBriefingBox(unoq)
	for _, want := range []string{"eMMC", "EDL pins", "boot firmware"} {
		if !strings.Contains(brief, want) {
			t.Errorf("briefing is missing %q", want)
		}
	}
	if strings.Contains(brief, "DIP switch") {
		t.Error("briefing asks for a DIP switch the board does not have")
	}
	if h := unoq.guide().hints; h.label != "Arduino UNO Q" || !strings.Contains(h.buttonLine, "EDL pins") {
		t.Errorf("wait hints = %+v", h)
	}

	var done, untouched, partial strings.Builder
	if err := finishDragonwingFlash(&done, unoq, nil, "0.19.4", nil, false); err != nil {
		t.Fatalf("a completed flash returned %v", err)
	}
	reportDragonwingFailure(&untouched, unoq, errDragonwingNothingWritten, false)
	reportDragonwingFailure(&partial, unoq, errors.New("programming rootfsA failed"), true)
	for name, out := range map[string]string{
		"success": done.String(), "nothing written": untouched.String(), "part-written": partial.String(),
	} {
		if !strings.Contains(out, "EDL pins") || strings.Contains(out, "DIP switch") {
			t.Errorf("%s output %q gives the wrong EDL instructions", name, out)
		}
	}
}
