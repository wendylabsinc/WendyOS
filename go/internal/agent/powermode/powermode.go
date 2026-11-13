// Package powermode reads and changes the NVIDIA Jetson power mode through
// nvpmodel. Mode names and IDs are the device's own, exactly as its
// /etc/nvpmodel.conf declares them; nothing is translated or aliased.
package powermode

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultConfPath is where nvpmodel's mode table lives on L4T.
const DefaultConfPath = "/etc/nvpmodel.conf"

// commandTimeout bounds every nvpmodel invocation so a hung tool cannot pin an
// RPC (and the lock held across a switch) forever.
const commandTimeout = 30 * time.Second

var (
	// ErrUnavailable means nvpmodel or its configuration is not present.
	ErrUnavailable = errors.New("nvpmodel is not available on this device")
	// ErrUnknownMode means the requested mode is not in the configuration.
	ErrUnknownMode = errors.New("unknown power mode")
)

// Mode is one nvpmodel power mode.
type Mode struct {
	ID   int32
	Name string
}

// State is the device's current mode and everything it offers.
type State struct {
	Current Mode
	// Available lists every configured mode in file order.
	Available []Mode
	// BootDefault is the mode nvpmodel applies at boot; nil when the
	// configuration does not declare one.
	BootDefault *Mode
}

// SetResult describes the outcome of a mode switch.
type SetResult struct {
	// Mode is the mode that was requested.
	Mode Mode
	// RebootRequired reports that the mode only fully takes effect after a
	// reboot. nvpmodel was told not to reboot.
	RebootRequired bool
}

// Runner executes a command with the given stdin and returns its combined
// output. Tests substitute it; the real one never goes through a shell.
type Runner func(ctx context.Context, stdin string, name string, args ...string) (string, error)

// Controller reads and changes the power mode. Switches are serialized.
type Controller struct {
	// ConfPath is the nvpmodel mode table.
	ConfPath string
	// Run executes nvpmodel.
	Run Runner

	mu sync.Mutex
}

// New returns a Controller wired to the real nvpmodel.
func New() *Controller {
	return &Controller{ConfPath: DefaultConfPath, Run: execRunner}
}

func execRunner(ctx context.Context, stdin string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

var (
	modeLine    = regexp.MustCompile(`^<\s*POWER_MODEL\s+ID=(\d+)\s+NAME=(\S+)\s*>`)
	defaultLine = regexp.MustCompile(`^<\s*PM_CONFIG\s+DEFAULT=(\d+)\s*>`)
	currentName = regexp.MustCompile(`(?i)^NV Power Mode:\s*(\S+)`)
	digitsOnly  = regexp.MustCompile(`^\d+$`)
)

// ParseConfig reads the modes and the boot default from an nvpmodel.conf. It
// ignores everything else (per-mode parameter blocks, comments).
func ParseConfig(r io.Reader) (modes []Mode, bootDefault *int32, err error) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if m := modeLine.FindStringSubmatch(line); m != nil {
			id, convErr := parseID(m[1])
			if convErr != nil {
				return nil, nil, fmt.Errorf("mode id %q: %w", m[1], convErr)
			}
			modes = append(modes, Mode{ID: id, Name: m[2]})
		} else if m := defaultLine.FindStringSubmatch(line); m != nil {
			id, convErr := parseID(m[1])
			if convErr != nil {
				return nil, nil, fmt.Errorf("default mode id %q: %w", m[1], convErr)
			}
			bootDefault = &id
		}
	}
	if err := sc.Err(); err != nil {
		return nil, nil, err
	}
	return modes, bootDefault, nil
}

// parseQuery extracts the current mode ID (and, when present, its name) from
// `nvpmodel -q`, which prints "NV Power Mode: NAME" followed by the ID on its
// own line, possibly with warning lines around them.
func parseQuery(out string) (id int32, name string, err error) {
	found := false
	for _, raw := range strings.Split(out, "\n") {
		line := strings.TrimSpace(raw)
		if m := currentName.FindStringSubmatch(line); m != nil {
			name = m[1]
			continue
		}
		if !found && digitsOnly.MatchString(line) {
			id, err = parseID(line)
			if err != nil {
				return 0, "", err
			}
			found = true
		}
	}
	if !found {
		return 0, "", fmt.Errorf("could not find the current mode in nvpmodel output %q", strings.TrimSpace(out))
	}
	return id, name, nil
}

// parseID reads a mode ID, bounded to int32 as on the wire.
func parseID(s string) (int32, error) {
	id, err := strconv.ParseInt(s, 10, 32)
	if err != nil {
		return 0, err
	}
	return int32(id), nil
}

func (c *Controller) loadConfig() ([]Mode, *int32, error) {
	f, err := os.Open(c.ConfPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, fmt.Errorf("%w: %s not found", ErrUnavailable, c.ConfPath)
		}
		return nil, nil, err
	}
	defer f.Close()
	modes, def, err := ParseConfig(f)
	if err != nil {
		return nil, nil, fmt.Errorf("parsing %s: %w", c.ConfPath, err)
	}
	if len(modes) == 0 {
		return nil, nil, fmt.Errorf("%w: %s declares no power modes", ErrUnavailable, c.ConfPath)
	}
	return modes, def, nil
}

func (c *Controller) run(ctx context.Context, stdin string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	out, err := c.Run(ctx, stdin, "nvpmodel", args...)
	if err != nil && errors.Is(err, exec.ErrNotFound) {
		return out, fmt.Errorf("%w: nvpmodel not found", ErrUnavailable)
	}
	return out, err
}

func find(modes []Mode, id int32) (Mode, bool) {
	for _, m := range modes {
		if m.ID == id {
			return m, true
		}
	}
	return Mode{}, false
}

// Resolve matches a user-supplied name or numeric ID to a configured mode. An
// exact name wins, then a numeric ID, then a case-insensitive name.
func Resolve(modes []Mode, query string) (Mode, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return Mode{}, fmt.Errorf("%w: no mode given", ErrUnknownMode)
	}
	for _, m := range modes {
		if m.Name == query {
			return m, nil
		}
	}
	if digitsOnly.MatchString(query) {
		if id, err := parseID(query); err == nil {
			if m, ok := find(modes, id); ok {
				return m, nil
			}
		}
	}
	for _, m := range modes {
		if strings.EqualFold(m.Name, query) {
			return m, nil
		}
	}
	names := make([]string, len(modes))
	for i, m := range modes {
		names[i] = fmt.Sprintf("%s (%d)", m.Name, m.ID)
	}
	return Mode{}, fmt.Errorf("%w %q; available: %s", ErrUnknownMode, query, strings.Join(names, ", "))
}

// State reports the current mode and the modes the device offers.
func (c *Controller) State(ctx context.Context) (State, error) {
	modes, def, err := c.loadConfig()
	if err != nil {
		return State{}, err
	}
	out, err := c.run(ctx, "", "-q")
	if err != nil {
		if errors.Is(err, ErrUnavailable) {
			return State{}, err
		}
		return State{}, fmt.Errorf("nvpmodel -q: %w: %s", err, strings.TrimSpace(out))
	}
	id, name, err := parseQuery(out)
	if err != nil {
		return State{}, err
	}
	current, ok := find(modes, id)
	if !ok {
		// A mode outside the table (a custom conf): report what nvpmodel said.
		current = Mode{ID: id, Name: name}
	}
	st := State{Current: current, Available: modes}
	if def != nil {
		if d, ok := find(modes, *def); ok {
			st.BootDefault = &d
		}
	}
	return st, nil
}

// Set switches to the named or numbered mode. When a mode needs a reboot,
// nvpmodel asks on stdin whether to go ahead, and records the mode only if the
// answer is yes (a declined prompt leaves the stored mode untouched). With
// reboot false the answer is no and the need is only reported (RebootRequired);
// with reboot true the answer is yes, so the mode is recorded and the device
// restarts into it.
func (c *Controller) Set(ctx context.Context, query string, reboot bool) (SetResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	modes, _, err := c.loadConfig()
	if err != nil {
		return SetResult{}, err
	}
	mode, err := Resolve(modes, query)
	if err != nil {
		return SetResult{}, err
	}
	answer := "n\n"
	if reboot {
		answer = "y\n"
	}
	out, err := c.run(ctx, answer, "-m", strconv.FormatInt(int64(mode.ID), 10))
	rebootRequired := strings.Contains(strings.ToLower(out), "reboot required")
	if err != nil && !rebootRequired {
		if errors.Is(err, ErrUnavailable) {
			return SetResult{}, err
		}
		return SetResult{}, fmt.Errorf("nvpmodel -m %d: %w: %s", mode.ID, err, strings.TrimSpace(out))
	}
	return SetResult{Mode: mode, RebootRequired: rebootRequired}, nil
}
