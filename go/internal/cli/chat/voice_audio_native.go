//go:build cgo && (darwin || linux || windows)

package chat

import (
	"fmt"
	"runtime"

	"github.com/gen2brain/malgo"
)

// VoiceSupportError checks this build's voice capability without opening an
// audio device or requesting microphone permission.
func VoiceSupportError() error { return nil }

func openVoiceAudio() (VoiceAudio, error) {
	return startVoiceAudio(func(audio *bufferedVoiceAudio) (voiceAudioDevice, error) {
		// Explicit real backends prevent miniaudio's null backend from reporting
		// success on a machine without usable microphone/speaker support.
		backends := map[string][]malgo.Backend{
			"darwin":  {malgo.BackendCoreaudio},
			"linux":   {malgo.BackendPulseaudio, malgo.BackendAlsa, malgo.BackendJack},
			"windows": {malgo.BackendWasapi, malgo.BackendDsound, malgo.BackendWinmm},
		}[runtime.GOOS]
		ctx, err := malgo.InitContext(backends, malgo.ContextConfig{}, nil)
		if err != nil {
			return nil, fmt.Errorf("audio is unavailable on this computer: %w", err)
		}
		config := malgo.DefaultDeviceConfig(malgo.Duplex)
		config.SampleRate = voiceSampleRate
		config.Capture.Format = malgo.FormatS16
		config.Capture.Channels = 1
		config.Playback.Format = malgo.FormatS16
		config.Playback.Channels = 1
		config.PeriodSizeInMilliseconds = 10
		config.Periods = 2
		config.Alsa.NoMMap = 1
		device, err := malgo.InitDevice(ctx.Context, config, malgo.DeviceCallbacks{
			Data: func(output, input []byte, _ uint32) { audio.samples(output, input) },
			Stop: audio.deviceStopped,
		})
		if err != nil {
			_ = ctx.Uninit()
			ctx.Free()
			return nil, fmt.Errorf("open microphone and speakers: %w; allow microphone access for your terminal and check your default audio devices", err)
		}
		return &nativeVoiceAudioDevice{device: device, context: ctx}, nil
	})
}

type nativeVoiceAudioDevice struct {
	device  *malgo.Device
	context *malgo.AllocatedContext
}

func (d *nativeVoiceAudioDevice) Start() error { return d.device.Start() }

func (d *nativeVoiceAudioDevice) Close() error {
	d.device.Uninit()
	err := d.context.Uninit()
	d.context.Free()
	return err
}
