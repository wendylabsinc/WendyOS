// Package audio talks to the PipeWire graph on the device.
//
// Every sink and source is a PipeWire node, including sound cards (via its ALSA
// monitor) and Bluetooth endpoints, which have no card and so are invisible to
// aplay, arecord and amixer.
//
// The agent runs as root while PipeWire runs in the wendy user's session, so
// every helper here points the client tools at that session's runtime
// directory. Root can open the socket regardless of the directory's mode.
package audio

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// SocketGlob locates the per-user PipeWire socket. Behind a var so tests can
// redirect it into a tempdir.
var SocketGlob = "/run/user/*/pipewire-0"

// expectedUID resolves the "wendy" user's UID, the only session whose socket
// root will trust. Without this, RuntimeDir would hand a root-run process to
// whichever local UID's session socket happened to glob first on a
// multi-user host. Behind a var so tests can stub it without requiring a real
// "wendy" account on the test host.
var expectedUID = func() (uint32, bool) {
	u, err := user.Lookup("wendy")
	if err != nil {
		return 0, false
	}
	uid, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		return 0, false
	}
	return uint32(uid), true
}

// queryTimeout bounds a single audio-query subprocess — pw-dump or wpctl here,
// aplay/arecord/amixer on the ALSA fallback — so a wedged PipeWire or sound
// card cannot hold an RPC open indefinitely.
const queryTimeout = 5 * time.Second

// Node is a PipeWire sink or source.
type Node struct {
	// ID is object.id, which wpctl takes for volume and default selection.
	ID uint32
	// Serial is object.serial, which pw-record's --target takes. They are
	// different numbering schemes and are not interchangeable.
	Serial uint64
	// Name is node.name, the stable identifier the default-device metadata
	// refers to (e.g. "bluez_output.78_2B_64_76_F3_CE.1").
	Name string
	// Description is node.description, meant for humans.
	Description string
	IsSink      bool
	// StableName names the endpoint in a way that survives a reboot or a
	// replug, which ID does not: PipeWire hands out object ids from a free list
	// that reuses the most recently released slot, and the ALSA fallback's ID
	// encodes a card index the kernel assigns in probe order. It is node.name on
	// PipeWire (WirePlumber derives that from the USB vendor, product and
	// serial number, or the board's device path, plus the active profile),
	// "alsa:<card id>:<device>" on the ALSA fallback, and
	// "sensor-link:<pairing>:<sensor>" for a paired microphone. Empty where no
	// stable identity is known.
	StableName string
}

// Defaults names the nodes PipeWire currently routes to by default.
type Defaults struct {
	SinkName   string
	SourceName string
}

// IsDefault reports whether n is the current default for its direction.
func (d Defaults) IsDefault(n Node) bool {
	if n.IsSink {
		return n.Name != "" && n.Name == d.SinkName
	}
	return n.Name != "" && n.Name == d.SourceName
}

// session locates the user session's PipeWire socket directory. It returns
// either a directory and an empty reason, or an empty directory and a reason
// naming the precondition that failed.
//
// The reason exists because "no session" is three different situations — no
// "wendy" account, no socket, or a socket owned by someone else — and a caller
// that sees only "" reports the same thing for all three. Each is a distinct
// misconfiguration, and an operator can only act on the one that applies.
//
// Only the socket owned by the "wendy" user is trusted: on a multi-user host,
// any local UID can create a session under /run/user/<uid>, and root blindly
// following the first glob match would let that UID influence the agent's
// audio graph operations.
func session() (dir, reason string) {
	uid, ok := expectedUID()
	if !ok {
		return "", `no local user "wendy" (PipeWire runs in that user's session)`
	}
	matches, _ := filepath.Glob(SocketGlob)
	// Tracked so a candidate that exists but is unusable can be reported for
	// what it is: a socket owned by the wrong UID is a very different fault
	// from no socket at all, and conflating them sends an operator looking in
	// the wrong place.
	var rejected string
	for _, m := range matches {
		// Lstat, not Stat: a symlink swapped in after the glob must not be
		// followed to a socket outside the expected session.
		fi, err := os.Lstat(m)
		if err != nil {
			if rejected == "" {
				rejected = fmt.Sprintf("%s could not be read: %v", m, err)
			}
			continue
		}
		if fi.Mode()&os.ModeSocket == 0 {
			if rejected == "" {
				rejected = fmt.Sprintf("%s is not a socket", m)
			}
			continue
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok {
			continue
		}
		if st.Uid != uid {
			if rejected == "" {
				rejected = fmt.Sprintf("%s is owned by uid %d, expected the \"wendy\" user (uid %d)", m, st.Uid, uid)
			}
			continue
		}
		return filepath.Dir(m), ""
	}
	if rejected != "" {
		return "", rejected
	}
	return "", fmt.Sprintf("no socket matching %s (is pipewire-user-setup.service running?)", SocketGlob)
}

// RuntimeDir returns the directory holding the user session's PipeWire socket,
// or "" when no session is up. Use UnavailableReason to find out why.
func RuntimeDir() string {
	dir, _ := session()
	return dir
}

// UnavailableReason explains why no PipeWire session was usable, or "" when one
// is. Callers put it in log lines and error messages so a fallback to ALSA
// states its cause rather than asserting, unprovably, that nothing is running.
// Behind a var so tests can pair it with a stubbed Available.
var UnavailableReason = func() string {
	_, reason := session()
	return reason
}

// Available reports whether a PipeWire user session is up. Callers use this
// to decide between the PipeWire and ALSA-fallback code paths. Behind a var
// so tests can force either path without a real socket or a "wendy" account.
var Available = func() bool { return RuntimeDir() != "" }

// Command builds a command pointed at the user session, or errors when no
// session is up. The system-wide instance is never used: it has no session
// manager, so its graph is empty.
func Command(ctx context.Context, name string, args ...string) (*exec.Cmd, error) {
	dir := RuntimeDir()
	if dir == "" {
		return nil, fmt.Errorf("no PipeWire session found (looked for %s)", SocketGlob)
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(),
		"PIPEWIRE_RUNTIME_DIR="+dir,
		"XDG_RUNTIME_DIR="+dir,
	)
	return cmd, nil
}

// DumpRun returns the raw pw-dump output. Behind a var so tests can supply a
// fixture instead of running PipeWire.
var DumpRun = func(ctx context.Context) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	cmd, err := Command(ctx, "pw-dump")
	if err != nil {
		return nil, err
	}
	return cmd.Output()
}

// WpctlRun runs wpctl against the user session. Behind a var for tests.
var WpctlRun = func(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	cmd, err := Command(ctx, "wpctl", args...)
	if err != nil {
		return nil, err
	}
	return cmd.CombinedOutput()
}

// pwObject is the subset of a pw-dump entry this package reads. Node properties
// live under info.props; the default-device metadata object carries its entries
// in a top-level metadata array.
type pwObject struct {
	ID   uint32 `json:"id"`
	Type string `json:"type"`
	Info struct {
		Props map[string]json.RawMessage `json:"props"`
	} `json:"info"`
	Props    map[string]json.RawMessage `json:"props"`
	Metadata []struct {
		Key   string          `json:"key"`
		Value json.RawMessage `json:"value"`
	} `json:"metadata"`
}

// propString reads a property that may be encoded as a JSON string or a bare
// number — pw-dump uses both, depending on the property.
func propString(props map[string]json.RawMessage, key string) string {
	raw, ok := props[key]
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return strings.Trim(string(raw), `"`)
}

func propUint(props map[string]json.RawMessage, key string) uint64 {
	v, err := strconv.ParseUint(propString(props, key), 10, 64)
	if err != nil {
		return 0
	}
	return v
}

// decodeDump unmarshals pw-dump output into the objects both scans below walk.
func decodeDump(data []byte) ([]pwObject, error) {
	var objects []pwObject
	if err := json.Unmarshal(data, &objects); err != nil {
		return nil, fmt.Errorf("parsing pw-dump output: %w", err)
	}
	return objects, nil
}

// nodeProps returns o's properties and media.class if o is a Node of one of classes. Device
// and Port objects share the id space and carry media.class too, but only a Node is streamable.
func nodeProps(o pwObject, classes ...string) (map[string]json.RawMessage, string, bool) {
	if o.Type != "PipeWire:Interface:Node" {
		return nil, "", false
	}
	class := propString(o.Info.Props, "media.class")
	if !slices.Contains(classes, class) {
		return nil, "", false
	}
	return o.Info.Props, class, true
}

// parseDump extracts audio sinks and sources, and the current defaults.
func parseDump(data []byte) ([]Node, Defaults, error) {
	objects, err := decodeDump(data)
	if err != nil {
		return nil, Defaults{}, err
	}

	var nodes []Node
	var defaults Defaults
	for _, o := range objects {
		if len(o.Metadata) > 0 && propString(o.Props, "metadata.name") == "default" {
			for _, m := range o.Metadata {
				// The value is an object, {"name": "<node.name>"}.
				var v struct {
					Name string `json:"name"`
				}
				if json.Unmarshal(m.Value, &v) != nil {
					continue
				}
				switch m.Key {
				case "default.audio.sink":
					defaults.SinkName = v.Name
				case "default.audio.source":
					defaults.SourceName = v.Name
				}
			}
			continue
		}

		// Exactly "Audio/Sink" or "Audio/Source": monitor and virtual nodes carry a
		// further suffix and are not devices anyone chose to install.
		props, class, ok := nodeProps(o, "Audio/Sink", "Audio/Source")
		if !ok {
			continue
		}
		name := propString(props, "node.name")
		if name == "" {
			continue
		}
		description := propString(props, "node.description")
		if description == "" {
			description = name
		}
		nodes = append(nodes, Node{
			ID:          o.ID,
			Serial:      propUint(props, "object.serial"),
			Name:        name,
			Description: description,
			IsSink:      class == "Audio/Sink",
			StableName:  name,
		})
	}
	// pw-dump emits graph order, which shifts as devices come and go.
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
	return nodes, defaults, nil
}

// ListNodes enumerates the sinks and sources PipeWire currently has.
func ListNodes(ctx context.Context) ([]Node, Defaults, error) {
	out, err := DumpRun(ctx)
	if err != nil {
		return nil, Defaults{}, fmt.Errorf("querying PipeWire: %w", err)
	}
	return parseDump(out)
}

// FindNode returns the node with the given object id.
func FindNode(nodes []Node, id uint32) (Node, bool) {
	for _, n := range nodes {
		if n.ID == id {
			return n, true
		}
	}
	return Node{}, false
}

// parseVolume reads the "Volume: 0.50" line wpctl get-volume prints, and
// returns it as a percentage. A muted node prints a trailing "[MUTED]", which
// does not change the underlying volume and is ignored here.
func parseVolume(output string) (uint32, bool) {
	for _, line := range strings.Split(output, "\n") {
		_, after, found := strings.Cut(strings.TrimSpace(line), "Volume:")
		if !found {
			continue
		}
		fields := strings.Fields(after)
		if len(fields) == 0 {
			continue
		}
		v, err := strconv.ParseFloat(fields[0], 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
			continue
		}
		// wpctl reports a fraction that exceeds 1.0 on a boosted node.
		v = math.Min(math.Max(v, 0), 1)
		return uint32(v*100 + 0.5), true
	}
	return 0, false
}

// NodeVolume reports a node's volume as a percentage.
func NodeVolume(ctx context.Context, id uint32) (uint32, bool) {
	out, err := WpctlRun(ctx, "get-volume", strconv.FormatUint(uint64(id), 10))
	if err != nil {
		return 0, false
	}
	return parseVolume(string(out))
}

// SetNodeVolume sets a node's volume from a percentage and unmutes it. Mute is
// a separate flag that the reported volume does not reflect.
func SetNodeVolume(ctx context.Context, id, percent uint32) error {
	if percent > 100 {
		percent = 100
	}
	idArg := strconv.FormatUint(uint64(id), 10)
	arg := strconv.FormatFloat(float64(percent)/100, 'f', 2, 64)
	out, err := WpctlRun(ctx, "set-volume", idArg, arg)
	if err != nil {
		return fmt.Errorf("wpctl set-volume: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if out, err := WpctlRun(ctx, "set-mute", idArg, "0"); err != nil {
		return fmt.Errorf("wpctl set-mute: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// SetDefaultNode makes a node the default sink or source for its direction.
func SetDefaultNode(ctx context.Context, id uint32) error {
	out, err := WpctlRun(ctx, "set-default", strconv.FormatUint(uint64(id), 10))
	if err != nil {
		return fmt.Errorf("wpctl set-default: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
