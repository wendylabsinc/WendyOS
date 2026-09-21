//go:build !cgo || (!darwin && !linux && !windows)

package chat

import "fmt"

func openVoiceAudio() (VoiceAudio, error) {
	return nil, VoiceSupportError()
}

// VoiceSupportError checks this build's voice capability without opening an
// audio device or requesting microphone permission.
func VoiceSupportError() error {
	return fmt.Errorf("this Wendy build does not include microphone support; use a native audio build for macOS, Linux, or Windows (built with CGO_ENABLED=1), or continue in text chat")
}
