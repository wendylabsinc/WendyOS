package chat

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// VoiceKey resolves credentials independently of the reasoning model. A local
// or custom backend credential must never be forwarded to OpenAI for voice.
func VoiceKey(backend Config) string {
	path, err := voiceSettingsPath()
	if err != nil {
		return resolveVoiceKey(backend, Config{})
	}
	saved, _ := loadSettings(path)
	return resolveVoiceKey(backend, saved)
}

func resolveVoiceKey(backend, saved Config) string {
	key := firstValue(os.Getenv("WENDY_VOICE_API_KEY"), os.Getenv("OPENAI_API_KEY"))
	if RequiredAPIKeyEnv(saved) == "OPENAI_API_KEY" {
		key = firstValue(key, saved.APIKey)
	}
	if RequiredAPIKeyEnv(backend) == "OPENAI_API_KEY" {
		key = firstValue(key, backend.APIKey)
	}
	return key
}

func voiceSettingsPath() (string, error) {
	path, err := settingsPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(path), "voice.json"), nil
}

// SetupVoice opens private credential entry outside the full-screen chat. Voice
// credentials are saved separately so local backend settings remain intact.
func SetupVoice(ctx context.Context, backend Config, input io.Reader, output io.Writer) (string, error) {
	path, err := voiceSettingsPath()
	if err != nil {
		return "", err
	}
	return setupVoice(ctx, VoiceKey(backend), &terminalSetupUI{input: input, output: output}, func(key string) error {
		return saveSettings(path, Config{Provider: "openai", BaseURL: defaultBaseURL("openai"), Model: "gpt-live-1", APIKey: key})
	})
}

func setupVoice(ctx context.Context, existing string, ui setupUI, save func(string) error) (string, error) {
	ui.Note("Voice uses your computer's microphone with OpenAI GPT Live. Your selected AI still handles reasoning and Wendy tools. Audio and brief task context go to OpenAI; API usage is billed separately.")
	if existing != "" {
		choice, err := ui.Choose(ctx, "OpenAI voice connection", "Keep the configured OpenAI key or enter a replacement privately.", []setupChoice{
			{"keep-key", "Keep current key", "Use the configured key for voice."},
			{"replace-key", "Enter a different API key", "Save a separate key for voice."},
		})
		if err != nil {
			return "", err
		}
		if choice == "keep-key" {
			return existing, nil
		}
	}
	for {
		key, err := ui.Text(ctx, "Connect GPT Live", "Create an OpenAI API key at https://platform.openai.com/api-keys, then paste it here.\nThe key is hidden and saved privately for voice. Your chat model stays the same.", "", true)
		if err != nil {
			return "", err
		}
		key = strings.TrimSpace(key)
		if key == "" || strings.ContainsAny(key, "\r\n") {
			ui.Note("Paste one API key to connect, or press Esc to keep using text chat.")
			continue
		}
		if err := save(key); err != nil {
			return "", fmt.Errorf("save voice connection: %w", err)
		}
		return key, nil
	}
}
