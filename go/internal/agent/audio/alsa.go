// ALSA fallback: enumeration, volume and capture for when no PipeWire user
// session is up (see Available). Call these only in that situation.
//
// PipeWire's own ALSA monitor exposes every sound card as a node, so mixing
// this package's two enumerations in one listing would double-list cards and
// risks the exact bug the PipeWire-first design replaced: a Bluetooth
// endpoint reported as present by one surface and absent from another. This
// file exists only to keep basic sound-card playback and capture honest on a
// host with no session manager, not to run alongside PipeWire.

package audio

import (
	"bufio"
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// AplayListRun and ArecordListRun enumerate ALSA cards. Behind vars so tests
// can supply a fixture instead of running the real tools.
// Each of these is bounded by queryTimeout for the same reason the PipeWire
// queries are: the RPC context they inherit carries no deadline of its own, so
// without one here a wedged card holds the call open for as long as the caller
// is willing to wait — which, for the CLI, is forever.
var (
	AplayListRun = func(ctx context.Context) ([]byte, error) {
		ctx, cancel := context.WithTimeout(ctx, queryTimeout)
		defer cancel()
		return exec.CommandContext(ctx, "aplay", "-l").Output()
	}
	ArecordListRun = func(ctx context.Context) ([]byte, error) {
		ctx, cancel := context.WithTimeout(ctx, queryTimeout)
		defer cancel()
		return exec.CommandContext(ctx, "arecord", "-l").Output()
	}
)

// AmixerRun runs amixer for ALSA volume control. Behind a var for tests.
var AmixerRun = func(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	return exec.CommandContext(ctx, "amixer", args...).CombinedOutput()
}

// Layout of the Node.ID an ALSA endpoint encodes to: device in the low 8 bits,
// card in the next 16, direction in the bit above.
//
// Direction has to be part of the ID because a card/device pair does not
// identify one endpoint. A duplex device — a USB speakerphone, say — is
// reported as the same card and device by both aplay -l and arecord -l, so an
// ID built from card and device alone names its playback and capture halves at
// once, and FindNode resolves it to whichever one the sort happened to place
// first.
const (
	alsaDeviceBits = 8
	alsaCardBits   = 16
	alsaDeviceMask = 1<<alsaDeviceBits - 1
	alsaCardMask   = 1<<alsaCardBits - 1
	// alsaSourceFlag marks a capture endpoint; sinks leave it clear.
	alsaSourceFlag = uint64(1) << (alsaDeviceBits + alsaCardBits)
	// alsaSubdeviceShift packs a snd-aloop subdevice index above every field
	// EncodeAlsaID uses, so a per-subdevice Loopback capture row gets a unique,
	// non-colliding Node.ID that DecodeAlsaID still reads the right card/device
	// from (the subdevice sits above the card mask and source flag).
	alsaSubdeviceShift = alsaDeviceBits + alsaCardBits + 1
	// loopbackCaptureDevice is the snd-aloop device the consumer captures from:
	// writes to hw:Loopback,0,N surface as captures on hw:Loopback,1,N.
	loopbackCaptureDevice = 1
)

// EncodeAlsaID and DecodeAlsaID convert between an ALSA card/device/direction
// triple and the Node.ID used to address it, so 0 remains the "unspecified"
// sentinel callers already use for PipeWire node IDs.
func EncodeAlsaID(card, device uint64, isSink bool) uint32 {
	encoded := ((card & alsaCardMask) << alsaDeviceBits) | (device & alsaDeviceMask)
	if !isSink {
		encoded |= alsaSourceFlag
	}
	return uint32(encoded + 1)
}

// DecodeAlsaID reverses EncodeAlsaID. The zero sentinel decodes to card 0,
// device 0; its direction is meaningless and callers must not read it.
func DecodeAlsaID(id uint32) (card, device uint64, isSink bool) {
	if id == 0 {
		return 0, 0, false
	}
	encoded := uint64(id) - 1
	card = (encoded >> alsaDeviceBits) & alsaCardMask
	device = encoded & alsaDeviceMask
	return card, device, encoded&alsaSourceFlag == 0
}

// EncodeAlsaSubdeviceID builds a Node.ID for one snd-aloop capture subdevice
// (hw:Loopback,1,subdevice). These synthetic rows are addressed by Name
// (arecord -D), so the encoding only needs to be unique per subdevice, not a
// full round-trip; DecodeAlsaID still recovers the card and device from it.
func EncodeAlsaSubdeviceID(card, subdevice uint64) uint32 {
	return EncodeAlsaID(card, loopbackCaptureDevice, false) | uint32((subdevice+1)<<alsaSubdeviceShift)
}

// AlsaSubdevice returns the snd-aloop subdevice packed into id by
// EncodeAlsaSubdeviceID, and whether one is present. A plain card/device id
// from parseAlsaList (no subdevice) returns ok=false.
func AlsaSubdevice(id uint32) (uint64, bool) {
	s := uint64(id) >> alsaSubdeviceShift
	if s == 0 {
		return 0, false
	}
	return s - 1, true
}

// parseAlsaList parses the output of "aplay -l" or "arecord -l": lines of the
// form "card N: CardName [Desc], device M: DeviceName [Desc]".
func parseAlsaList(output string, isSink bool) []Node {
	var nodes []Node
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "card ") {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) < 2 {
			continue
		}
		card, err := strconv.ParseUint(strings.TrimSpace(strings.TrimPrefix(parts[0], "card ")), 10, 32)
		if err != nil {
			continue
		}
		var device uint64
		rest := parts[1]
		if idx := strings.Index(rest, ", device "); idx >= 0 {
			after := rest[idx+len(", device "):]
			if ci := strings.Index(after, ":"); ci >= 0 {
				if d, err := strconv.ParseUint(strings.TrimSpace(after[:ci]), 10, 32); err == nil {
					device = d
				}
			}
		}
		nodes = append(nodes, Node{
			ID: EncodeAlsaID(card, device, isSink),
			// plughw, not hw: the plug layer handles format conversion a
			// card's native rate/format may not support directly.
			Name:        fmt.Sprintf("plughw:%d,%d", card, device),
			Description: strings.TrimSpace(rest),
			IsSink:      isSink,
		})
	}
	return nodes
}

// ListAlsaNodes enumerates ALSA cards directly via aplay/arecord. There is no
// notion of a default sink or source at this layer, so callers get a nil
// Defaults and must pick a device themselves (e.g. the first available).
func ListAlsaNodes(ctx context.Context) ([]Node, error) {
	var nodes []Node
	var firstErr error
	for _, s := range []struct {
		run    func(context.Context) ([]byte, error)
		isSink bool
	}{
		{AplayListRun, true},
		{ArecordListRun, false},
	} {
		out, err := s.run(ctx)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		nodes = append(nodes, parseAlsaList(string(out), s.isSink)...)
	}
	if len(nodes) == 0 && firstErr != nil {
		return nil, fmt.Errorf("querying ALSA: %w", firstErr)
	}
	// SliceStable, not Slice: an unstable sort leaves the order of any two
	// nodes that compare equal up to the algorithm, which is how a duplicate ID
	// used to resolve to an arbitrary one of the two nodes carrying it. IDs are
	// unique now, so this is belt and braces — but it costs nothing and keeps a
	// listing reproducible if a future card ever collides again.
	sort.SliceStable(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
	return nodes, nil
}

var (
	simpleMixerControlPattern = regexp.MustCompile(`^Simple mixer control '(.+)',\d+$`)
	playbackVolumePattern     = regexp.MustCompile(`\[(\d{1,3})%\]`)
	// preferredPlaybackControls covers the conventional ALSA simple-control
	// names. If none is present, mixerControl falls back to the first control
	// that exposes a playback volume, which keeps USB and board-specific
	// codecs usable.
	preferredPlaybackControls = []string{"Master", "PCM", "Speaker", "Headphone"}
)

// mixerContents is one simple mixer control as `amixer scontents` reports it:
// the name from its header line, and the indented body lines that follow
// carrying its capabilities and current values.
type mixerContents struct {
	name string
	body string
}

// maxMixerLine bounds a single scontents line. A routing mux on a Tegra APE
// card enumerates every selectable source on one "Items:" line, so the default
// 64 KiB scanner limit is nearer than it looks — and overrunning it would
// silently truncate the mixer rather than report anything.
const maxMixerLine = 1 << 20

// parseMixerContents splits `amixer scontents` output into per-control blocks,
// preserving the order amixer listed them in.
func parseMixerContents(output string) []mixerContents {
	var controls []mixerContents
	var body strings.Builder
	flush := func() {
		if len(controls) > 0 {
			controls[len(controls)-1].body = body.String()
		}
		body.Reset()
	}

	scanner := bufio.NewScanner(strings.NewReader(output))
	scanner.Buffer(make([]byte, 0, bufio.MaxScanTokenSize), maxMixerLine)
	for scanner.Scan() {
		line := scanner.Text()
		if match := simpleMixerControlPattern.FindStringSubmatch(strings.TrimSpace(line)); len(match) == 2 {
			flush()
			controls = append(controls, mixerContents{name: match[1]})
			continue
		}
		body.WriteString(line)
		body.WriteString("\n")
	}
	flush()
	return controls
}

func orderPlaybackControls(controls []string) []string {
	ordered := make([]string, 0, len(controls))
	used := make(map[int]bool, len(controls))
	for _, preferred := range preferredPlaybackControls {
		for i, control := range controls {
			if !used[i] && strings.EqualFold(control, preferred) {
				ordered = append(ordered, control)
				used[i] = true
			}
		}
	}
	for i, control := range controls {
		if !used[i] {
			ordered = append(ordered, control)
		}
	}
	return ordered
}

func parsePlaybackVolume(output string) (uint32, bool) {
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.Contains(line, "Playback") {
			continue
		}
		match := playbackVolumePattern.FindStringSubmatch(line)
		if len(match) != 2 {
			continue
		}
		volume, err := strconv.ParseUint(match[1], 10, 32)
		if err == nil && volume <= 100 {
			return uint32(volume), true
		}
	}
	return 0, false
}

// mixerControl resolves the ALSA simple mixer control that owns playback
// volume for a card and returns its current percentage.
//
// This reads the whole mixer with one `scontents` dump rather than listing the
// controls and running `sget` per control. The per-control scan is quadratic in
// the wrong thing: a Tegra APE card lists 2179 simple controls, nearly all of
// them routing muxes, and the first one exposing a playback volume sits at
// index 1872 — 1872 amixer processes, ~150s on a Jetson Thor, during which
// `wendy device audio` shows nothing at all. scontents answers the same
// question in a single process (~0.1s on the same card) because the values are
// already in the listing.
func mixerControl(ctx context.Context, card uint64) (string, uint32, error) {
	cardArg := strconv.FormatUint(card, 10)
	output, err := AmixerRun(ctx, "-c", cardArg, "scontents")
	if err != nil {
		return "", 0, fmt.Errorf("reading mixer controls: %w: %s", err, strings.TrimSpace(string(output)))
	}

	// Keep the first block of any repeated name: `sget <name>` used to resolve
	// to index 0, and amixer lists the indices in order.
	bodies := make(map[string]string)
	names := make([]string, 0)
	for _, control := range parseMixerContents(string(output)) {
		if _, seen := bodies[control.name]; seen {
			continue
		}
		bodies[control.name] = control.body
		names = append(names, control.name)
	}

	for _, name := range orderPlaybackControls(names) {
		if volume, ok := parsePlaybackVolume(bodies[name]); ok {
			return name, volume, nil
		}
	}
	return "", 0, fmt.Errorf("no playback volume control found on ALSA card %d", card)
}

// AlsaVolume reports a card's playback volume as a percentage, or ok=false
// when the card has no discoverable playback control.
func AlsaVolume(ctx context.Context, card uint64) (uint32, bool) {
	_, volume, err := mixerControl(ctx, card)
	return volume, err == nil
}

// SetAlsaVolume sets a card's playback volume from a percentage and unmutes
// it, returning the value the mixer actually landed on (a coarse hardware
// mixer may land on a nearby step rather than the exact request).
func SetAlsaVolume(ctx context.Context, card uint64, percent uint32) (uint32, error) {
	if percent > 100 {
		percent = 100
	}
	control, _, err := mixerControl(ctx, card)
	if err != nil {
		return 0, err
	}
	cardArg := strconv.FormatUint(card, 10)
	percentArg := fmt.Sprintf("%d%%", percent)
	output, err := AmixerRun(ctx, "-c", cardArg, "sset", control, percentArg, "unmute")
	if err != nil {
		return 0, fmt.Errorf("amixer sset %s: %w: %s", control, err, strings.TrimSpace(string(output)))
	}
	actual := percent
	if volume, ok := parsePlaybackVolume(string(output)); ok {
		actual = volume
	}
	return actual, nil
}

// ArecordCommand builds an arecord invocation that emits raw s16le PCM on
// stdout from device (an ALSA -D argument, e.g. "plughw:0,0"), matching
// pw-record's output shape so a caller can treat either source the same way
// once the process is running.
func ArecordCommand(ctx context.Context, device string, sampleRate, channels uint32) *exec.Cmd {
	return exec.CommandContext(ctx, "arecord",
		"-q",
		"-D", device,
		"-f", "S16_LE",
		"-r", strconv.FormatUint(uint64(sampleRate), 10),
		"-c", strconv.FormatUint(uint64(channels), 10),
		"-t", "raw",
		"-",
	)
}
