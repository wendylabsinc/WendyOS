// Package payload provides the ARM64 bare-metal binary that zeroes the eMMC
// boot partitions when loaded onto the device via nv3p DL_BL.
//
// The binary (zero_emmc.bin) is compiled from zero_emmc.S and embedded here.
// Build it once with:
//
//	aarch64-linux-gnu-as -o zero_emmc.o zero_emmc.S
//	aarch64-linux-gnu-objcopy -O binary zero_emmc.o zero_emmc.bin
//
// Then embed with //go:embed zero_emmc.bin (uncomment the embed directive
// below once the binary has been compiled and tested on real hardware).
//
// TODO: compile zero_emmc.S, validate on real AGX Orin hardware, then embed.
package payload

import (
	_ "embed"
	"fmt"
	"os"
)

// T234 load and entry addresses for DL_BL.
// The applet maps SDRAM starting at 0x80000000 for T234.
// TODO: verify against T234 TRM / applet output on real hardware.
const (
	LoadAddress  = uint32(0x80000000)
	EntryAddress = uint32(0x80000000)
)

// ZeroEMMCBin holds the compiled zero_emmc.bin payload.
// Uncomment the embed directive after compiling and validating on hardware.
//
// //go:embed zero_emmc.bin
// var ZeroEMMCBin []byte

// ZeroEMMCBin is a placeholder until the binary is compiled and tested.
// Remove this and uncomment the //go:embed above once zero_emmc.bin exists.
var ZeroEMMCBin []byte

// Load returns the payload binary, either from the embedded bytes or from
// an external file (specified via --payload flag for pre-production testing).
func Load(override string) ([]byte, error) {
	if override != "" {
		data, err := os.ReadFile(override)
		if err != nil {
			return nil, fmt.Errorf("reading payload from %s: %w", override, err)
		}
		return data, nil
	}
	if len(ZeroEMMCBin) == 0 {
		return nil, fmt.Errorf(
			"payload not yet compiled — build zero_emmc.S to zero_emmc.bin " +
				"and embed it, or pass --payload <path> to specify the binary")
	}
	return ZeroEMMCBin, nil
}
