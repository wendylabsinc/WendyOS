package chat

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
)

func TestVoiceKeyIsIndependentOfLocalCredentials(t *testing.T) {
	clearProviderEnv(t)
	t.Setenv("WENDY_VOICE_API_KEY", "")
	t.Setenv("WENDY_CHAT_API_KEY", "custom-server-key")
	local := Config{Provider: "local", BaseURL: "http://localhost:1234/v1", APIKey: "local-only-key"}
	if got := resolveVoiceKey(local, Config{}); got != "" {
		t.Fatal("local credential must not be used for OpenAI voice")
	}
	cloud := Config{Provider: "openai", BaseURL: defaultBaseURL("openai"), APIKey: "backend-key"}
	if got := resolveVoiceKey(cloud, Config{}); got != "backend-key" {
		t.Fatal("official OpenAI backend credential should be reusable")
	}
	saved := Config{Provider: "openai", BaseURL: defaultBaseURL("openai"), APIKey: "saved-voice-key"}
	if got := resolveVoiceKey(cloud, saved); got != "saved-voice-key" {
		t.Fatal("a separately chosen voice key should override the backend key")
	}
	t.Setenv("OPENAI_API_KEY", "openai-env-key")
	if got := resolveVoiceKey(local, saved); got != "openai-env-key" {
		t.Fatal("OpenAI environment should override saved voice credentials")
	}
	t.Setenv("WENDY_VOICE_API_KEY", "voice-env-key")
	if got := resolveVoiceKey(local, saved); got != "voice-env-key" {
		t.Fatal("voice-specific environment should take precedence")
	}
}

func TestVoiceSetupPrivateKeyAndEnvironmentReuse(t *testing.T) {
	u := &scriptedSetupUI{t: t, texts: []string{"", "fake-test-voice-key"}}
	var saved string
	key, err := setupVoice(context.Background(), "", u, func(key string) error { saved = key; return nil })
	if err != nil || key != "fake-test-voice-key" || saved != key || u.secretInputs != 2 {
		t.Fatalf("private setup failed: err=%v, secret prompts=%d", err, u.secretInputs)
	}
	if strings.Contains(strings.Join(u.notes, "\n"), key) {
		t.Fatal("voice key leaked into setup notes")
	}
	for _, choice := range []string{"keep-key", "cancel"} {
		u := &scriptedSetupUI{t: t, choices: []string{choice}}
		_, err := setupVoice(context.Background(), "fake-env-key", u, func(string) error {
			t.Fatal("keeping an environment key or canceling must not save it")
			return nil
		})
		if choice == "cancel" && !errors.Is(err, tui.ErrCancelled) {
			t.Fatalf("cancel returned %v", err)
		}
		if choice == "keep-key" && err != nil {
			t.Fatal(err)
		}
	}
}
