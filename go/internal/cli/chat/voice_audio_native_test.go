//go:build cgo && (darwin || linux || windows)

package chat

import "testing"

func TestVoiceAudioNativeBuildCapability(t *testing.T) {
	// This is a pure build-capability check; it must never open the microphone.
	if err := VoiceSupportError(); err != nil {
		t.Fatalf("native build should include voice support: %v", err)
	}
}
