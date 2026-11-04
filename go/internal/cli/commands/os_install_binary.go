//go:build darwin || linux || windows

package commands

import (
	"bytes"
	"debug/elf"
	"fmt"
	"os"

	"github.com/diskfs/go-diskfs/partition/gpt"
)

// localInstallAgent retains the validated bytes so a long download or a retry
// cannot replace them with a different build from the same path.
type localInstallAgent struct {
	path string
	data []byte
	arch string
}

func readInstallAgent(path string) (*localInstallAgent, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading --binary: %w", err)
	}
	f, err := elf.NewFile(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("--binary must be a Linux ELF executable: %w", err)
	}
	defer f.Close()
	if f.OSABI != elf.ELFOSABI_NONE && f.OSABI != elf.ELFOSABI_LINUX {
		return nil, fmt.Errorf("--binary must target Linux, but its ELF OS ABI is %s", f.OSABI)
	}
	if f.Class != elf.ELFCLASS64 || f.Data != elf.ELFDATA2LSB ||
		(f.Type != elf.ET_EXEC && f.Type != elf.ET_DYN) {
		return nil, fmt.Errorf("--binary must be a 64-bit little-endian Linux ELF executable")
	}
	var arch string
	switch f.Machine {
	case elf.EM_AARCH64:
		arch = "arm64"
	case elf.EM_X86_64:
		arch = "amd64"
	default:
		return nil, fmt.Errorf("--binary has unsupported ELF architecture %s; expected arm64 or x86_64", f.Machine)
	}
	return &localInstallAgent{path: path, data: data, arch: arch}, nil
}

func (a *localInstallAgent) checkArchitecture(arch string) error {
	if a == nil {
		return nil
	}
	if arch == "" {
		return fmt.Errorf("cannot determine image architecture for --binary; use a supported --device-type or an image with an architecture-specific GPT root partition")
	}
	if a.arch != arch {
		return fmt.Errorf("--binary is %s but the image is %s; provide a matching Linux ELF wendy-agent binary", a.arch, arch)
	}
	return nil
}

func installDeviceArchitecture(device string) string {
	switch device {
	case "raspberry-pi-3", "raspberry-pi-4", "raspberry-pi-5",
		"jetson-agx-orin", "jetson-orin-nano", "jetson-agx-thor", "vm-arm64":
		return "arm64"
	case "x86_64", "vm-x86-64":
		return "amd64"
	}
	if _, ok := dragonwingBoardFor(device); ok {
		return "arm64"
	}
	return ""
}

// Prefer the image's root partition type over the device catalog. Local images
// have no catalog entry, so an unknown or ambiguous architecture is an error.
func installImageArchitecture(layout *imageGPT, device string) (string, error) {
	arch := ""
	if layout != nil {
		for _, p := range layout.table.Partitions {
			var next string
			switch p.Type {
			case gpt.LinuxRootArm64:
				next = "arm64"
			case gpt.LinuxRootX86_64:
				next = "amd64"
			case gpt.LinuxRootArm, gpt.LinuxRootX86, gpt.LinuxRootIA64:
				return "", fmt.Errorf("--binary does not support image root partition architecture %s", p.Type)
			default:
				continue
			}
			if arch != "" && arch != next {
				return "", fmt.Errorf("--binary cannot be used with an image containing multiple root architectures")
			}
			arch = next
		}
	}
	if arch == "" {
		arch = installDeviceArchitecture(device)
	}
	return arch, nil
}

func (a *localInstallAgent) checkImage(layout *imageGPT, device string) error {
	if a == nil {
		return nil
	}
	arch, err := installImageArchitecture(layout, device)
	if err != nil {
		return err
	}
	return a.checkArchitecture(arch)
}
