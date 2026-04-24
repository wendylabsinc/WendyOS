package commands

import (
	"fmt"
	"io"
	"os"
)

const (
	elfDataLittle = 1 // ELFDATA2LSB
	elfDataBig    = 2 // ELFDATA2MSB

	emX86_64  = 62  // EM_X86_64  → amd64
	emAArch64 = 183 // EM_AARCH64 → arm64
)

// detectELFArchitecture returns the Go architecture name encoded in an ELF
// header and whether the input looked like an ELF file at all.
func detectELFArchitecture(data []byte) (arch string, isELF bool) {
	// ELF magic + header fields up to e_machine occupy 20 bytes.
	if len(data) < 20 {
		return "", false
	}
	if data[0] != 0x7f || data[1] != 'E' || data[2] != 'L' || data[3] != 'F' {
		return "", false
	}

	var machine uint16
	switch data[5] {
	case elfDataLittle:
		machine = uint16(data[18]) | uint16(data[19])<<8
	case elfDataBig:
		machine = uint16(data[18])<<8 | uint16(data[19])
	default:
		return "", true
	}

	switch machine {
	case emX86_64:
		return "amd64", true
	case emAArch64:
		return "arm64", true
	default:
		return "", true
	}
}

func validateELFFileArchitecture(path, expectedArch string) error {
	switch expectedArch {
	case "amd64", "arm64":
		// supported, continue
	default:
		return fmt.Errorf("unsupported target architecture %q; only amd64 and arm64 are supported", expectedArch)
	}

	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("opening %s: %w", path, err)
	}
	defer file.Close()

	header := make([]byte, 64)
	n, err := io.ReadFull(file, header)
	if err != nil && err != io.ErrUnexpectedEOF {
		return fmt.Errorf("reading ELF header from %s: %w", path, err)
	}
	header = header[:n]

	actualArch, isELF := detectELFArchitecture(header)
	if !isELF {
		return fmt.Errorf("swift build output %s is not an ELF executable; expected a linux/%s binary", path, expectedArch)
	}
	if actualArch == "" {
		return fmt.Errorf("swift build output %s has an unsupported ELF architecture; expected linux/%s", path, expectedArch)
	}
	if actualArch != expectedArch {
		return fmt.Errorf("swift build output %s is linux/%s, expected linux/%s", path, actualArch, expectedArch)
	}
	return nil
}
