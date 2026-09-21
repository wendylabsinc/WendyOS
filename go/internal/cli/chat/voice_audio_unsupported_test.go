//go:build !cgo || (!darwin && !linux && !windows)

package chat

import (
	"strings"
	"testing"
)

func TestVoiceAudioUnsupportedIsActionable(t *testing.T) {
	capabilityError := VoiceSupportError()
	if capabilityError == nil || !strings.Contains(capabilityError.Error(), "CGO_ENABLED=1") {
		t.Fatalf("build capability check did not explain unavailable audio: %v", capabilityError)
	}
	audio, err := openVoiceAudio()
	if audio != nil || err == nil || !strings.Contains(err.Error(), "CGO_ENABLED=1") || !strings.Contains(err.Error(), "text chat") {
		t.Fatalf("unsupported build did not explain how to continue: %v", err)
	}
	if err.Error() != capabilityError.Error() {
		t.Fatalf("opening audio and capability preflight disagree: %v", err)
	}
}
