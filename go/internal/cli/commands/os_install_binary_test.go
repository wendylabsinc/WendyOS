//go:build darwin || linux || windows

package commands

import (
	"bytes"
	"context"
	"debug/elf"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/diskfs/go-diskfs/partition/gpt"
	"github.com/wendylabsinc/wendy/go/internal/shared/wendyconf"
)

func installTestELF(machine elf.Machine) []byte {
	h := elf.Header64{Type: uint16(elf.ET_EXEC), Machine: uint16(machine), Version: 1, Ehsize: 64}
	copy(h.Ident[:], []byte{0x7f, 'E', 'L', 'F', byte(elf.ELFCLASS64), byte(elf.ELFDATA2LSB), 1})
	var b bytes.Buffer
	_ = binary.Write(&b, binary.LittleEndian, h)
	return b.Bytes()
}

func writeInstallTestBinary(t *testing.T, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "wendy-agent")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadInstallAgent(t *testing.T) {
	for _, tc := range []struct {
		name string
		data []byte
		arch string
	}{
		{"arm64", installTestELF(elf.EM_AARCH64), "arm64"},
		{"x86_64", installTestELF(elf.EM_X86_64), "amd64"},
		{"unsupported machine", installTestELF(elf.EM_RISCV), ""},
		{"script", []byte("#!/bin/sh\n"), ""},
		{"Mach-O", []byte{0xcf, 0xfa, 0xed, 0xfe}, ""},
		{"zip", []byte("PK\x03\x04"), ""},
		{"truncated", installTestELF(elf.EM_AARCH64)[:20], ""},
		{"empty", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeInstallTestBinary(t, tc.data)
			agent, err := readInstallAgent(path)
			if tc.arch == "" {
				if err == nil {
					t.Fatal("accepted invalid binary")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if agent.arch != tc.arch || !bytes.Equal(agent.data, tc.data) {
				t.Fatalf("unexpected local agent: %v", agent)
			}
			if err := os.WriteFile(path, []byte("replaced during download"), 0o600); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(agent.data, tc.data) {
				t.Fatal("local bytes changed after validation")
			}
		})
	}
	for _, field := range []int{4, 5, 7, 16} {
		data := installTestELF(elf.EM_AARCH64)
		data[field] = 0xff // invalid class, byte order, OS ABI, or executable type
		if _, err := readInstallAgent(writeInstallTestBinary(t, data)); err == nil {
			t.Errorf("accepted invalid ELF header field %d", field)
		}
	}
	if _, err := readInstallAgent(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("accepted missing binary")
	}
}

func TestInstallAgentImageArchitecture(t *testing.T) {
	for _, tc := range []struct {
		name, device, binaryArch string
		parts                    []gpt.Type
		wantErr                  string
	}{
		{"arm64", "", "arm64", []gpt.Type{gpt.LinuxRootArm64}, ""},
		{"x86_64", "", "amd64", []gpt.Type{gpt.LinuxRootX86_64}, ""},
		{"wrong arm64", "", "arm64", []gpt.Type{gpt.LinuxRootX86_64}, "image is amd64"},
		{"wrong x86_64", "", "amd64", []gpt.Type{gpt.LinuxRootArm64}, "image is arm64"},
		{"GPT takes precedence", "raspberry-pi-5", "arm64", []gpt.Type{gpt.LinuxRootX86_64}, "image is amd64"},
		{"device fallback", "raspberry-pi-5", "arm64", nil, ""},
		{"x86 device fallback", "x86_64", "amd64", nil, ""},
		{"unknown local image", "", "arm64", nil, "cannot determine"},
		{"unknown device", "new-device", "arm64", nil, "cannot determine"},
		{"mixed root architectures", "", "arm64", []gpt.Type{gpt.LinuxRootArm64, gpt.LinuxRootX86_64}, "multiple root architectures"},
		{"A/B slots", "", "arm64", []gpt.Type{gpt.LinuxRootArm64, gpt.LinuxRootArm64}, ""},
		{"32-bit image", "raspberry-pi-3", "arm64", []gpt.Type{gpt.LinuxRootArm}, "does not support"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var layout *imageGPT
			if tc.parts != nil {
				layout = &imageGPT{table: &gpt.Table{}}
				for _, typ := range tc.parts {
					layout.table.Partitions = append(layout.table.Partitions, &gpt.Partition{Type: typ})
				}
			}
			err := (&localInstallAgent{arch: tc.binaryArch}).checkImage(layout, tc.device)
			if tc.wantErr == "" && err != nil || tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("checkImage = %v; want %q", err, tc.wantErr)
			}
		})
	}
}

func TestInstallBinaryCommandPreflight(t *testing.T) {
	arm := writeInstallTestBinary(t, installTestELF(elf.EM_AARCH64))
	x86 := writeInstallTestBinary(t, installTestELF(elf.EM_X86_64))
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--binary="}, "requires a path"},
		{[]string{"--binary", filepath.Join(t.TempDir(), "missing")}, "reading --binary"},
		{[]string{"--binary", arm, "--device-type", "x86_64"}, "image is amd64"},
		{[]string{"--binary", x86, "--device-type", "raspberry-pi-5"}, "image is arm64"},
		{[]string{"--binary", x86, "--device-type", "jetson-agx-thor"}, "image is arm64"},
		{[]string{"--binary", x86, "--device-type", "dragonwing-iq-8275"}, "image is arm64"},
		{[]string{"--binary", arm, "--device-type", "linux-desktop"}, "only when installing a WendyOS image"},
		{[]string{"--binary", arm, "--device-type", "headless-mac"}, "only when installing a WendyOS image"},
	} {
		cmd := newOSInstallCmd()
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		cmd.SetArgs(tc.args)
		if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: got %v; want %q", tc.args, err, tc.want)
		}
	}
}

func TestInstallLocalAgentRecoveryConfig(t *testing.T) {
	data := installTestELF(elf.EM_AARCH64)
	agent, err := readInstallAgent(writeInstallTestBinary(t, data))
	if err != nil {
		t.Fatal(err)
	}
	for _, inject := range []struct {
		name string
		fn   func(string, []wendyconf.WifiCredential, string, []byte, io.Writer, func(string), *localInstallAgent) error
	}{
		{"Thor", injectConfigPartition},
		{"Orin", injectRecoveryConfig},
	} {
		t.Run(inject.name, func(t *testing.T) {
			img := newFATImage(t)
			if err := inject.fn(img, nil, "", nil, io.Discard, func(string) {}, agent); err != nil {
				t.Fatal(err)
			}
			got, err := readFATFile(t, img, "wendy-agent")
			if err != nil || !bytes.Equal(got, data) {
				t.Fatalf("config agent = %x, %v", got, err)
			}
			wrong := &localInstallAgent{arch: "amd64", data: installTestELF(elf.EM_X86_64)}
			if err := inject.fn(img, nil, "", nil, io.Discard, func(string) {}, wrong); err == nil {
				t.Fatal("accepted incompatible recovery agent")
			}
		})
	}
	got, warning := resolveSeedAgent(io.Discard, func(string) {}, agent)
	if warning != "" || !bytes.Equal(got, data) {
		t.Fatalf("seed agent = %x, %q", got, warning)
	}
}

func TestInstallLocalAgentProvisioningRetry(t *testing.T) {
	for name, succeeds := range map[string]bool{"retry succeeds": true, "retry fails": false} {
		t.Run(name, func(t *testing.T) {
			withStubbedProvisioning(t, nil, true, []bool{true, false})
			agent := &localInstallAgent{data: installTestELF(elf.EM_AARCH64), arch: "arm64"}
			failure := errors.New("config write failed")
			calls := 0
			provisionConfigPartitionFn = func(_ drive, _ []wendyconf.WifiCredential, _ string, _ []byte, got *localInstallAgent) error {
				calls++
				if got != agent {
					t.Fatal("retry lost the local agent")
				}
				if succeeds && calls == 2 {
					return nil
				}
				return failure
			}
			_, _, err := provisionConfigWithRetry(context.Background(), drive{}, nil, "", nil, true, agent)
			if calls != 2 || succeeds && err != nil || !succeeds && !errors.Is(err, failure) {
				t.Fatalf("calls = %d, error = %v", calls, err)
			}
		})
	}
}
