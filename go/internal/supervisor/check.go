package supervisor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
)

// AgentContract is compiled into every agent that works under the supervisor.
// A binary without it is refused before it is ever run: an older agent would
// treat flags it does not know as a normal start.
const AgentContract = "wendy-agent-supervisor-contract-v1"

// Want is what a new agent binary must match before it may run.
type Want struct {
	SHA256 string
	Arch   string
}

// VersionRunner runs `<path> --version` in a sandbox and returns its output.
type VersionRunner func(ctx context.Context, path string) (string, error)

var elfMachines = map[string]elf.Machine{
	"amd64": elf.EM_X86_64,
	"arm64": elf.EM_AARCH64,
}

// ErrNoContract: the binary predates the agent supervisor.
var ErrNoContract = errors.New("this agent is too old to run under the agent supervisor")

// Check runs the tests that need no switch: integrity, file type, the
// supervisor contract, and a sandboxed start. It returns the version the
// binary reports.
func Check(ctx context.Context, path string, want Want, run VersionRunner) (string, error) {
	if want.SHA256 == "" {
		return "", errors.New("a sha256 is required for a new agent")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); !strings.EqualFold(got, want.SHA256) {
		return "", fmt.Errorf("sha256 mismatch: got %s, want %s", got, want.SHA256)
	}

	f, err := elf.NewFile(bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("not an ELF binary: %w", err)
	}
	class, machine := f.Class, f.Machine
	if wantMachine, ok := elfMachines[want.Arch]; !ok || class != elf.ELFCLASS64 || machine != wantMachine {
		return "", fmt.Errorf("built for a different CPU architecture (%s, %s), device is %s", class, machine, want.Arch)
	}
	if !bytes.Contains(data, []byte(AgentContract)) {
		return "", ErrNoContract
	}

	out, err := run(ctx, path)
	if err != nil {
		return "", fmt.Errorf("%s --version failed: %w", path, err)
	}
	v, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
	v = strings.TrimSpace(v)
	if v == "" {
		return "", fmt.Errorf("%s --version printed no version", path)
	}
	return v, nil
}
