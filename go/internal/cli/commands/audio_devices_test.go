package commands

import (
	"errors"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

func mkDev(id uint32, name, desc string, t agentpb.AudioDeviceType) *agentpb.AudioDevice {
	return &agentpb.AudioDevice{Id: id, Name: name, Description: desc, Type: t}
}

const (
	inT  = agentpb.AudioDeviceType_AUDIO_DEVICE_TYPE_INPUT
	outT = agentpb.AudioDeviceType_AUDIO_DEVICE_TYPE_OUTPUT
)

func TestIsLikelyVirtualCapture(t *testing.T) {
	cases := []struct {
		name string
		dev  *agentpb.AudioDevice
		want bool
	}{
		{"tegra admaif", mkDev(257, "hw:1,0", "APE [NVIDIA Jetson Thor AGX APE], device 0: fe.admaif@9610000.ADMAIF1 (*) []", inT), true},
		{"hdmi capture", mkDev(4, "hw:0,3", "HDA [NVIDIA Jetson Thor AGX HDA], device 3: HDMI 0 [HDMI 0]", inT), true},
		{"dummy", mkDev(99, "hw:9,0", "Dummy [Dummy], device 0: Dummy PCM", inT), true},
		{"loopback", mkDev(98, "hw:8,0", "Loopback [Loopback], device 0: Loopback PCM", inT), true},
		{"usb webcam mic", mkDev(513, "hw:2,0", "C920 [HD Pro Webcam C920], device 0: USB Audio [USB Audio]", inT), false},
		{"realtek onboard", mkDev(1, "hw:0,0", "HDA Intel PCH [HDA Intel PCH], device 0: ALC892 Analog [ALC892 Analog]", inT), false},
		{"named i2s mic", mkDev(770, "hw:3,1", "tegrasndt210ref [tegra-snd-t210ref-mobile-rt5640], device 1: ICM-43434 Mic", inT), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isLikelyVirtualCapture(c.dev); got != c.want {
				t.Fatalf("isLikelyVirtualCapture(%q) = %v, want %v", c.dev.GetDescription(), got, c.want)
			}
		})
	}
}

// thorDevices mirrors the real `wendy device audio list` output on a Jetson Thor:
// 32 APE ADMAIF capture FIFOs, the C920 USB mic, and HDMI outputs.
func thorDevices() []*agentpb.AudioDevice {
	var devs []*agentpb.AudioDevice
	for i := uint32(0); i < 32; i++ {
		devs = append(devs, mkDev(257+i, "hw:1,"+itoa(i),
			"APE [NVIDIA Jetson Thor AGX APE], device "+itoa(i)+": fe.admaif@9610000.ADMAIF"+itoa(i+1)+" (*) []", inT))
	}
	devs = append(devs, mkDev(513, "hw:2,0", "C920 [HD Pro Webcam C920], device 0: USB Audio [USB Audio]", inT))
	devs = append(devs, mkDev(4, "hw:0,3", "HDA [NVIDIA Jetson Thor AGX HDA], device 3: HDMI 0 [HDMI 0]", outT))
	return devs
}

func itoa(n uint32) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func TestRealCaptureDevices_Thor(t *testing.T) {
	real := realCaptureDevices(thorDevices())
	if len(real) != 1 {
		t.Fatalf("expected exactly 1 real capture device, got %d", len(real))
	}
	if real[0].GetId() != 513 {
		t.Fatalf("expected C920 (id 513), got id %d (%s)", real[0].GetId(), real[0].GetName())
	}
}

func TestResolveListenDeviceID(t *testing.T) {
	usb := mkDev(513, "hw:2,0", "C920 USB Audio", inT)
	usb2 := mkDev(514, "hw:4,0", "Blue Yeti USB Audio", inT)
	admaif := mkDev(257, "hw:1,0", "APE ADMAIF1", inT)

	neverPick := func([]*agentpb.AudioDevice) (uint32, error) {
		t.Helper()
		t.Fatal("picker should not have been called")
		return 0, nil
	}

	t.Run("--id wins, no pick, no list needed", func(t *testing.T) {
		id, _, err := resolveListenDeviceID([]*agentpb.AudioDevice{admaif}, 999, false, true, neverPick)
		if err != nil || id != 999 {
			t.Fatalf("got id=%d err=%v, want 999/nil", id, err)
		}
	})

	t.Run("single real device used silently", func(t *testing.T) {
		id, chosen, err := resolveListenDeviceID([]*agentpb.AudioDevice{usb, admaif}, 0, false, true, neverPick)
		if err != nil || id != 513 || chosen.GetId() != 513 {
			t.Fatalf("got id=%d chosen=%v err=%v", id, chosen, err)
		}
	})

	t.Run("multiple real, non-interactive picks first", func(t *testing.T) {
		id, chosen, err := resolveListenDeviceID([]*agentpb.AudioDevice{usb, usb2, admaif}, 0, false, false, neverPick)
		if err != nil || id != 513 || chosen.GetId() != 513 {
			t.Fatalf("got id=%d chosen=%v err=%v", id, chosen, err)
		}
	})

	t.Run("multiple real, interactive uses picker", func(t *testing.T) {
		pick := func(cands []*agentpb.AudioDevice) (uint32, error) {
			if len(cands) != 2 {
				t.Fatalf("expected 2 candidates, got %d", len(cands))
			}
			return 514, nil
		}
		id, chosen, err := resolveListenDeviceID([]*agentpb.AudioDevice{usb, usb2, admaif}, 0, false, true, pick)
		if err != nil || id != 514 || chosen.GetId() != 514 {
			t.Fatalf("got id=%d chosen=%v err=%v", id, chosen, err)
		}
	})

	t.Run("picker cancel propagates ErrUserCancelled", func(t *testing.T) {
		pick := func([]*agentpb.AudioDevice) (uint32, error) { return 0, ErrUserCancelled }
		_, _, err := resolveListenDeviceID([]*agentpb.AudioDevice{usb, usb2}, 0, false, true, pick)
		if !errors.Is(err, ErrUserCancelled) {
			t.Fatalf("want ErrUserCancelled, got %v", err)
		}
	})

	t.Run("no real device without --all errors and mentions --all", func(t *testing.T) {
		_, _, err := resolveListenDeviceID([]*agentpb.AudioDevice{admaif}, 0, false, true, neverPick)
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "--all") {
			t.Fatalf("error should mention --all: %v", err)
		}
	})

	t.Run("--all includes virtual devices", func(t *testing.T) {
		// Only virtual inputs present; --all makes them eligible.
		id, _, err := resolveListenDeviceID([]*agentpb.AudioDevice{admaif}, 0, true, false, neverPick)
		if err != nil || id != 257 {
			t.Fatalf("got id=%d err=%v, want 257/nil", id, err)
		}
	})

	loop0 := mkDev(200, "plughw:2,0", "Loopback [Loopback], device 0: Loopback PCM", inT)
	loop1 := mkDev(201, "plughw:2,1", "Loopback [Loopback], device 1: Loopback PCM", inT)

	t.Run("loopback-only auto-selects without --all", func(t *testing.T) {
		// A remote-sensor mic exposed via snd-aloop is the only input; it must be
		// used without --all rather than erroring "no microphone detected".
		id, chosen, err := resolveListenDeviceID([]*agentpb.AudioDevice{admaif, loop1}, 0, false, false, neverPick)
		if err != nil || id != 201 || chosen.GetId() != 201 {
			t.Fatalf("got id=%d chosen=%v err=%v, want 201", id, chosen, err)
		}
	})

	t.Run("physical mic preferred over loopback when both exist", func(t *testing.T) {
		id, chosen, err := resolveListenDeviceID([]*agentpb.AudioDevice{loop0, usb}, 0, false, true, neverPick)
		if err != nil || id != 513 || chosen.GetId() != 513 {
			t.Fatalf("got id=%d chosen=%v err=%v, want physical 513", id, chosen, err)
		}
	})

	t.Run("--id overrides even when a physical mic is present", func(t *testing.T) {
		id, _, err := resolveListenDeviceID([]*agentpb.AudioDevice{usb, loop1}, 201, false, true, neverPick)
		if err != nil || id != 201 {
			t.Fatalf("got id=%d err=%v, want explicit 201", id, err)
		}
	})

	t.Run("multiple loopback fallback, non-interactive picks first", func(t *testing.T) {
		id, _, err := resolveListenDeviceID([]*agentpb.AudioDevice{loop0, loop1}, 0, false, false, neverPick)
		if err != nil || id != 200 {
			t.Fatalf("got id=%d err=%v, want first loopback 200", id, err)
		}
	})
}

func TestJitterDepthChunks(t *testing.T) {
	cases := []struct {
		bufferMs, chunkMs uint32
		want              int
	}{
		{30, 10, 3},
		{10, 10, 1},
		{5, 10, 1}, // floor at 1
		{50, 20, 2},
		{30, 0, 30}, // guard against div-by-zero (chunkMs treated as 1)
	}
	for _, c := range cases {
		if got := jitterDepthChunks(c.bufferMs, c.chunkMs); got != c.want {
			t.Fatalf("jitterDepthChunks(%d,%d) = %d, want %d", c.bufferMs, c.chunkMs, got, c.want)
		}
	}
}
