package audio

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// Shape taken from real aplay -l / arecord -l output on a Raspberry Pi 4: two
// HDMI playback devices and one USB capture device.
const aplayFixture = `**** List of PLAYBACK Hardware Devices ****
card 0: vc4hdmi0 [vc4-hdmi-0], device 0: MAI PCM i2s-hifi-0 [MAI PCM i2s-hifi-0]
  Subdevices: 1/1
  Subdevice #0: subdevice #0
card 1: vc4hdmi1 [vc4-hdmi-1], device 0: MAI PCM i2s-hifi-0 [MAI PCM i2s-hifi-0]
  Subdevices: 1/1
  Subdevice #0: subdevice #0
`

const arecordFixture = `**** List of CAPTURE Hardware Devices ****
card 2: C920 [HD Pro Webcam C920], device 0: USB Audio [USB Audio]
  Subdevices: 1/1
  Subdevice #0: subdevice #0
`

func TestListAlsaNodes(t *testing.T) {
	origAplay, origArecord := AplayListRun, ArecordListRun
	t.Cleanup(func() { AplayListRun, ArecordListRun = origAplay, origArecord })
	AplayListRun = func(context.Context) ([]byte, error) { return []byte(aplayFixture), nil }
	ArecordListRun = func(context.Context) ([]byte, error) { return []byte(arecordFixture), nil }

	nodes, err := ListAlsaNodes(context.Background())
	if err != nil {
		t.Fatalf("ListAlsaNodes() error = %v", err)
	}
	if len(nodes) != 3 {
		t.Fatalf("got %d nodes, want 3 (two HDMI outputs, one USB input); got %+v", len(nodes), nodes)
	}

	hdmi0, ok := FindNode(nodes, EncodeAlsaID(0, 0, true))
	if !ok {
		t.Fatal("card 0 device 0 missing")
	}
	if !hdmi0.IsSink || hdmi0.Name != "plughw:0,0" {
		t.Errorf("hdmi0 = %+v, want a sink named plughw:0,0", hdmi0)
	}

	mic, ok := FindNode(nodes, EncodeAlsaID(2, 0, false))
	if !ok {
		t.Fatal("card 2 device 0 missing")
	}
	if mic.IsSink {
		t.Error("USB capture device marked as a sink")
	}

	// Sorted by ID so a listing is stable.
	for i := 1; i < len(nodes); i++ {
		if nodes[i-1].ID >= nodes[i].ID {
			t.Errorf("nodes are not sorted by id: %d before %d", nodes[i-1].ID, nodes[i].ID)
		}
	}
}

func TestListAlsaNodesNoCards(t *testing.T) {
	origAplay, origArecord := AplayListRun, ArecordListRun
	t.Cleanup(func() { AplayListRun, ArecordListRun = origAplay, origArecord })
	failure := errors.New("no soundcards found")
	AplayListRun = func(context.Context) ([]byte, error) { return nil, failure }
	ArecordListRun = func(context.Context) ([]byte, error) { return nil, failure }

	if _, err := ListAlsaNodes(context.Background()); err == nil {
		t.Fatal("expected an error when neither aplay nor arecord finds a card")
	}
}

func TestEncodeDecodeAlsaID(t *testing.T) {
	for _, tc := range []struct {
		card, device uint64
		isSink       bool
	}{
		{0, 0, true}, {0, 0, false}, {0, 1, true}, {2, 0, false},
		{255, 255, true}, {255, 255, false}, {65535, 255, false},
	} {
		id := EncodeAlsaID(tc.card, tc.device, tc.isSink)
		if id == 0 {
			t.Fatalf("EncodeAlsaID(%d, %d, %v) = 0, which collides with the unspecified sentinel", tc.card, tc.device, tc.isSink)
		}
		gotCard, gotDevice, gotIsSink := DecodeAlsaID(id)
		if gotCard != tc.card || gotDevice != tc.device || gotIsSink != tc.isSink {
			t.Errorf("DecodeAlsaID(EncodeAlsaID(%d, %d, %v)) = (%d, %d, %v)",
				tc.card, tc.device, tc.isSink, gotCard, gotDevice, gotIsSink)
		}
	}
	if card, device, _ := DecodeAlsaID(0); card != 0 || device != 0 {
		t.Errorf("DecodeAlsaID(0) = (%d, %d), want (0, 0)", card, device)
	}
}

// A duplex device — one card exposing the same card/device number for both
// playback and capture — must get two distinct IDs. A USB speakerphone is the
// common case: aplay -l and arecord -l both report it as card 0, device 0, so
// an ID derived from card and device alone addresses two nodes at once and
// FindNode returns whichever the sort happened to place first.
func TestAlsaIDDistinguishesDirection(t *testing.T) {
	const duplex = "card 0: PowerConf [PowerConf], device 0: USB Audio [USB Audio]\n"

	origAplay, origArecord := AplayListRun, ArecordListRun
	t.Cleanup(func() { AplayListRun, ArecordListRun = origAplay, origArecord })
	AplayListRun = func(context.Context) ([]byte, error) { return []byte(duplex), nil }
	ArecordListRun = func(context.Context) ([]byte, error) { return []byte(duplex), nil }

	nodes, err := ListAlsaNodes(context.Background())
	if err != nil {
		t.Fatalf("ListAlsaNodes() error = %v", err)
	}
	if len(nodes) != 2 {
		t.Fatalf("got %d nodes, want 2 (one playback, one capture); got %+v", len(nodes), nodes)
	}
	if nodes[0].ID == nodes[1].ID {
		t.Fatalf("playback and capture share ID %d; FindNode(%d) is ambiguous", nodes[0].ID, nodes[0].ID)
	}

	sink, ok := FindNode(nodes, EncodeAlsaID(0, 0, true))
	if !ok || !sink.IsSink {
		t.Errorf("playback node not addressable by its own ID: %+v, ok=%v", sink, ok)
	}
	source, ok := FindNode(nodes, EncodeAlsaID(0, 0, false))
	if !ok || source.IsSink {
		t.Errorf("capture node not addressable by its own ID: %+v, ok=%v", source, ok)
	}

	// Both resolve back to the same underlying ALSA card and device.
	for _, n := range nodes {
		card, device, _ := DecodeAlsaID(n.ID)
		if card != 0 || device != 0 {
			t.Errorf("node %+v decodes to card %d device %d, want 0/0", n, card, device)
		}
	}
}

func TestParseMixerContents(t *testing.T) {
	input := "Simple mixer control 'PCM',0\n" +
		"  Capabilities: pvolume\n" +
		"  Mono: Playback 51 [22%] [on]\n" +
		"Simple mixer control 'Mic',0\n" +
		"  Capabilities: cvolume\n"
	got := parseMixerContents(input)
	want := []mixerContents{
		{name: "PCM", body: "  Capabilities: pvolume\n  Mono: Playback 51 [22%] [on]\n"},
		{name: "Mic", body: "  Capabilities: cvolume\n"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseMixerContents() = %#v, want %#v", got, want)
	}
}

// A Tegra APE card lists ~2200 simple mixer controls and does not reveal a
// playback volume until roughly control 1900. Resolving that with one `amixer
// sget` process per control took ~150s on a Jetson Thor — long enough that
// `wendy device audio` looked hung — so the whole mixer must come back in a
// single call regardless of how deep the first playback control sits.
func TestMixerControlReadsWholeMixerInOneCall(t *testing.T) {
	orig := AmixerRun
	t.Cleanup(func() { AmixerRun = orig })

	var mixer strings.Builder
	for i := 1; i <= 2000; i++ {
		fmt.Fprintf(&mixer, "Simple mixer control 'ADMAIF%d Mux',0\n", i)
		mixer.WriteString("  Capabilities: enum\n  Items: 'None' 'ADMAIF1' 'I2S1'\n  Item0: 'None'\n")
	}
	mixer.WriteString("Simple mixer control 'CVB-RT DAC1',0\n")
	mixer.WriteString("  Capabilities: pvolume pswitch\n")
	mixer.WriteString("  Front Left: Playback 51 [22%] [-20.04dB] [on]\n")

	var calls [][]string
	AmixerRun = func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		if strings.Join(args, " ") != "-c 2 scontents" {
			return nil, errors.New("unexpected amixer call: " + strings.Join(args, " "))
		}
		return []byte(mixer.String()), nil
	}

	control, volume, err := mixerControl(context.Background(), 2)
	if err != nil {
		t.Fatal(err)
	}
	if control != "CVB-RT DAC1" || volume != 22 {
		t.Fatalf("mixerControl() = %q, %d; want CVB-RT DAC1, 22", control, volume)
	}
	if len(calls) != 1 {
		t.Fatalf("amixer ran %d times, want exactly 1 (the scontents dump); calls = %v", len(calls), calls)
	}
}

func TestParsePlaybackVolume(t *testing.T) {
	input := "Simple mixer control 'PCM',0\n" +
		"  Capabilities: pvolume pvolume-joined pswitch pswitch-joined\n" +
		"  Playback channels: Mono\n" +
		"  Mono: Playback 231 [100%] [-2.50dB] [on]"
	volume, ok := parsePlaybackVolume(input)
	if !ok || volume != 100 {
		t.Fatalf("parsePlaybackVolume() = %d, %v; want 100, true", volume, ok)
	}

	if _, ok := parsePlaybackVolume("Mono: Capture 12 [22%]"); ok {
		t.Fatal("capture-only controls must not be treated as playback volume")
	}
}

func TestOrderPlaybackControls(t *testing.T) {
	got := orderPlaybackControls([]string{"Mic", "Speaker", "PCM", "Digital"})
	want := []string{"PCM", "Speaker", "Mic", "Digital"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("orderPlaybackControls() = %v, want %v", got, want)
	}
}

func TestMixerControlPrefersPCMAndSkipsCaptureOnly(t *testing.T) {
	orig := AmixerRun
	t.Cleanup(func() { AmixerRun = orig })

	var calls [][]string
	AmixerRun = func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		switch strings.Join(args, " ") {
		case "-c 2 scontents":
			return []byte("Simple mixer control 'Mic',0\n" +
				"  Mono: Capture 12 [22%]\n" +
				"Simple mixer control 'PCM',0\n" +
				"  Mono: Playback 51 [22%] [-20.04dB] [on]\n"), nil
		default:
			return nil, errors.New("unexpected amixer call")
		}
	}

	control, volume, err := mixerControl(context.Background(), 2)
	if err != nil {
		t.Fatal(err)
	}
	if control != "PCM" || volume != 22 {
		t.Fatalf("mixerControl() = %q, %d; want PCM, 22", control, volume)
	}
	wantCalls := [][]string{{"-c", "2", "scontents"}}
	if !reflect.DeepEqual(calls, wantCalls) {
		t.Fatalf("calls = %v, want %v", calls, wantCalls)
	}
}

func TestSetAlsaVolume(t *testing.T) {
	orig := AmixerRun
	t.Cleanup(func() { AmixerRun = orig })

	var calls [][]string
	AmixerRun = func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		switch strings.Join(args, " ") {
		case "-c 2 scontents":
			return []byte("Simple mixer control 'PCM',0\n  Mono: Playback 51 [22%] [on]\n"), nil
		case "-c 2 sset PCM 100% unmute":
			return []byte("Mono: Playback 231 [100%] [on]\n"), nil
		default:
			return nil, errors.New("unexpected amixer call")
		}
	}

	actual, err := SetAlsaVolume(context.Background(), 2, 100)
	if err != nil {
		t.Fatal(err)
	}
	if actual != 100 {
		t.Fatalf("actual volume = %d, want 100", actual)
	}
	wantLast := []string{"-c", "2", "sset", "PCM", "100%", "unmute"}
	if !reflect.DeepEqual(calls[len(calls)-1], wantLast) {
		t.Fatalf("set call = %v, want %v", calls[len(calls)-1], wantLast)
	}
}

func TestAlsaVolumeNoControl(t *testing.T) {
	orig := AmixerRun
	t.Cleanup(func() { AmixerRun = orig })
	AmixerRun = func(context.Context, ...string) ([]byte, error) {
		return []byte(""), nil
	}

	if _, ok := AlsaVolume(context.Background(), 0); ok {
		t.Error("AlsaVolume() ok = true with no mixer controls at all")
	}
}

func TestArecordCommand(t *testing.T) {
	cmd := ArecordCommand(context.Background(), "plughw:1,0", 44100, 2)
	want := []string{
		"arecord", "-q", "-D", "plughw:1,0", "-f", "S16_LE", "-r", "44100", "-c", "2", "-t", "raw",
		"--period-time=10000", "--buffer-time=40000", "-",
	}
	if !reflect.DeepEqual(cmd.Args, want) {
		t.Errorf("ArecordCommand args = %v, want %v", cmd.Args, want)
	}
}

// TestArecordCommandBoundsDeviceBacklog pins the buffering flags at every
// sample rate. Without them arecord accepts the driver default, which measured
// 208 ms of backlog on a Jetson Orin Nano: audio already that stale by the time
// a caller reads it, and far outside the delay envelope the audio capture
// adapter publishes as its uncertainty. The flags are the only thing keeping
// this path inside that envelope, and nothing else would notice their loss.
func TestArecordCommandBoundsDeviceBacklog(t *testing.T) {
	for _, rate := range []uint32{16000, 44100, 48000} {
		cmd := ArecordCommand(context.Background(), "plughw:0,0", rate, 1)
		for _, want := range []string{"--period-time=10000", "--buffer-time=40000"} {
			found := false
			for _, arg := range cmd.Args {
				if arg == want {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("ArecordCommand at %d Hz does not bound the device buffer: %v lacks %q", rate, cmd.Args, want)
			}
		}
	}
}
