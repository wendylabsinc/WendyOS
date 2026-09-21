package chat

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
)

type scriptedSetupUI struct {
	t              *testing.T
	choices, texts []string
	notes          []string
	secretInputs   int
}

func (u *scriptedSetupUI) Choose(_ context.Context, title, _ string, choices []setupChoice) (string, error) {
	u.t.Helper()
	if len(u.choices) == 0 {
		u.t.Fatalf("unexpected choice: %s", title)
	}
	answer := u.choices[0]
	u.choices = u.choices[1:]
	if answer == "cancel" {
		return "", tui.ErrCancelled
	}
	for _, choice := range choices {
		if choice.ID == answer {
			return answer, nil
		}
	}
	u.t.Fatalf("answer %q unavailable for %s: %v", answer, title, choices)
	return "", nil
}

func (u *scriptedSetupUI) Text(_ context.Context, title, _, initial string, secret bool) (string, error) {
	u.t.Helper()
	if secret {
		u.secretInputs++
		if initial != "" {
			u.t.Fatal("secret prompt must not prefill a key")
		}
	}
	if len(u.texts) == 0 {
		u.t.Fatalf("unexpected text input: %s", title)
	}
	answer := u.texts[0]
	u.texts = u.texts[1:]
	return answer, nil
}

func (u *scriptedSetupUI) Work(ctx context.Context, _ string, fn func(context.Context, func(string)) error) error {
	return fn(ctx, func(string) {})
}

func (u *scriptedSetupUI) Note(s string) { u.notes = append(u.notes, s) }

func setupTestBackend(saved *Config) setupBackend {
	return setupBackend{
		load: func() (Config, error) { return *saved, nil },
		save: func(c Config) error { *saved = c; return nil },
		discover: func(context.Context) []LocalServer {
			return []LocalServer{{Name: "Ollama", Config: Config{Provider: "ollama", BaseURL: "http://localhost:11434/v1", MaxTokens: 4096}}}
		},
		models: func(context.Context, Config) ([]ModelOption, error) {
			return []ModelOption{{ID: "test-model", Name: "Test model"}}, nil
		},
		pull: func(context.Context, Config, string, func(string)) error {
			return errors.New("unexpected model download")
		},
	}
}

func TestSetupBareCommandGuidesLocalAndRemembersNextLaunch(t *testing.T) {
	clearProviderEnv(t)
	var saved Config
	backend := setupTestBackend(&saved)
	u := &scriptedSetupUI{t: t, choices: []string{"computer", "test-model"}}
	cfg, err := runSetup(context.Background(), SetupOptions{}, u, backend)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "ollama" || saved.Model != "test-model" || saved.APIKey != "" {
		t.Fatalf("configuration: %+v, saved: %+v", cfg, saved)
	}
	if len(u.choices) != 0 {
		t.Fatal("setup did not complete its choices")
	}
	backend.discover = func(context.Context) []LocalServer { t.Fatal("returning user should start directly"); return nil }
	backend.models = func(context.Context, Config) ([]ModelOption, error) {
		t.Fatal("returning user should start directly")
		return nil, nil
	}
	if _, err := runSetup(context.Background(), SetupOptions{}, &scriptedSetupUI{t: t}, backend); err != nil {
		t.Fatal(err)
	}
}

func TestSetupHostedPromptsForKeyThenAvailableModels(t *testing.T) {
	clearProviderEnv(t)
	var saved Config
	backend := setupTestBackend(&saved)
	backend.models = func(_ context.Context, cfg Config) ([]ModelOption, error) {
		if cfg.APIKey != "pasted-private-key" {
			t.Fatal("key must be entered before model lookup")
		}
		return []ModelOption{{ID: "available-model"}}, nil
	}
	u := &scriptedSetupUI{t: t, choices: []string{"available-model"}, texts: []string{"pasted-private-key"}}
	cfg, err := runSetup(context.Background(), SetupOptions{Config: Config{Provider: "openai", Model: "luna"}}, u, backend)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Model != "available-model" || saved.APIKey != "pasted-private-key" || u.secretInputs != 1 {
		t.Fatal("guided hosted setup did not complete")
	}
	notes := strings.Join(u.notes, "\n")
	if !strings.Contains(notes, "isn't available") || strings.Contains(notes, "pasted-private-key") {
		t.Fatalf("incorrect user guidance: %s", notes)
	}
}

func TestSetupInvalidCredentialCanRetryWithoutRestart(t *testing.T) {
	clearProviderEnv(t)
	var saved Config
	backend := setupTestBackend(&saved)
	backend.models = func(_ context.Context, cfg Config) ([]ModelOption, error) {
		if cfg.APIKey == "wrong" {
			return nil, fmt.Errorf("This API key wasn't accepted. Please check it and try again.")
		}
		if cfg.APIKey != "right" {
			t.Fatal("unexpected credential")
		}
		return []ModelOption{{ID: "available-model"}}, nil
	}
	u := &scriptedSetupUI{t: t, choices: []string{"key", "available-model"}, texts: []string{"wrong", "right"}}
	_, err := runSetup(context.Background(), SetupOptions{Config: Config{Provider: "openai"}}, u, backend)
	if err != nil || saved.APIKey != "right" || u.secretInputs != 2 {
		t.Fatalf("retry failed: %v", err)
	}
}

func TestSetupEmptyOllamaOffersExplicitDownload(t *testing.T) {
	clearProviderEnv(t)
	var saved Config
	backend := setupTestBackend(&saved)
	downloaded := false
	backend.models = func(context.Context, Config) ([]ModelOption, error) {
		if !downloaded {
			return nil, ErrNoModels
		}
		return []ModelOption{{ID: StarterLocalModel}}, nil
	}
	backend.pull = func(_ context.Context, cfg Config, name string, _ func(string)) error {
		if cfg.Provider != "ollama" || name != StarterLocalModel {
			t.Fatal("incorrect download")
		}
		downloaded = true
		return nil
	}
	u := &scriptedSetupUI{t: t, choices: []string{"computer", "download", StarterLocalModel}}
	_, err := runSetup(context.Background(), SetupOptions{}, u, backend)
	if err != nil || !downloaded || saved.Model != StarterLocalModel {
		t.Fatalf("download flow: %v", err)
	}
}

func TestSetupSwitchingServerNeverForwardsSavedKey(t *testing.T) {
	clearProviderEnv(t)
	saved := Config{Provider: "openai", BaseURL: "https://api.openai.com/v1", APIKey: "old-private-key", Model: "old"}
	backend := setupTestBackend(&saved)
	backend.models = func(_ context.Context, cfg Config) ([]ModelOption, error) {
		if cfg.APIKey != "" || cfg.BaseURL != "http://localhost:9999/v1" {
			t.Fatal("old key sent to another server")
		}
		return []ModelOption{{ID: "new-model"}}, nil
	}
	u := &scriptedSetupUI{t: t, choices: []string{"custom", "new-model"}, texts: []string{"http://localhost:9999/v1"}}
	_, err := runSetup(context.Background(), SetupOptions{Force: true}, u, backend)
	if err != nil || saved.APIKey != "" || saved.Provider != "local" {
		t.Fatalf("switching server: %v", err)
	}
}

func TestSetupEnvironmentKeyIsNotSavedAndTokenLimitSurvivesSelection(t *testing.T) {
	clearProviderEnv(t)
	t.Setenv("OPENAI_API_KEY", "environment-only")
	var saved Config
	backend := setupTestBackend(&saved)
	u := &scriptedSetupUI{t: t, choices: []string{"openai", "test-model"}}
	cfg, err := runSetup(context.Background(), SetupOptions{Config: Config{MaxTokens: 1234}}, u, backend)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.APIKey != "environment-only" || saved.APIKey != "" || cfg.MaxTokens != 1234 || saved.MaxTokens != 1234 {
		t.Fatal("incorrect environment/persistence precedence")
	}
}

func TestSetupForceCanReplaceWorkingSavedKey(t *testing.T) {
	clearProviderEnv(t)
	saved := Config{Provider: "openai", BaseURL: "https://api.openai.com/v1", APIKey: "fake-working-old-key", Model: "test-model"}
	backend := setupTestBackend(&saved)
	lookups := 0
	backend.models = func(_ context.Context, cfg Config) ([]ModelOption, error) {
		lookups++
		if cfg.APIKey != "fake-replacement-key" {
			t.Fatal("replacement key should be used before any model lookup")
		}
		return []ModelOption{{ID: "test-model"}}, nil
	}
	u := &scriptedSetupUI{t: t, choices: []string{"openai", "replace-key", "test-model"}, texts: []string{"fake-replacement-key"}}
	cfg, err := runSetup(context.Background(), SetupOptions{Force: true}, u, backend)
	if err != nil || cfg.APIKey != "fake-replacement-key" || saved.APIKey != "fake-replacement-key" || u.secretInputs != 1 || lookups != 1 {
		t.Fatalf("working key rotation failed: %v", err)
	}
	if strings.Contains(strings.Join(u.notes, "\n"), "fake-") {
		t.Fatal("key rotation exposed a credential in status output")
	}
}

func TestSetupForceKeepEnvironmentKeyDoesNotSaveIt(t *testing.T) {
	clearProviderEnv(t)
	t.Setenv("OPENAI_API_KEY", "fake-environment-key")
	saved := Config{Provider: "openai", BaseURL: "https://api.openai.com/v1", Model: "test-model"}
	u := &scriptedSetupUI{t: t, choices: []string{"openai", "keep-key", "test-model"}}
	cfg, err := runSetup(context.Background(), SetupOptions{Force: true}, u, setupTestBackend(&saved))
	if err != nil || cfg.APIKey != "fake-environment-key" || saved.APIKey != "" || u.secretInputs != 0 {
		t.Fatalf("keeping environment key persisted it or prompted for it: %v", err)
	}
}

func TestSetupForceCancelKeyChoicePreservesSavedKey(t *testing.T) {
	clearProviderEnv(t)
	saved := Config{Provider: "openai", BaseURL: "https://api.openai.com/v1", APIKey: "fake-working-old-key", Model: "test-model"}
	backend := setupTestBackend(&saved)
	backend.save = func(Config) error { t.Fatal("canceling key selection must not save"); return nil }
	u := &scriptedSetupUI{t: t, choices: []string{"openai", "cancel"}}
	_, err := runSetup(context.Background(), SetupOptions{Force: true}, u, backend)
	if !errors.Is(err, tui.ErrCancelled) || saved.APIKey != "fake-working-old-key" {
		t.Fatalf("canceling key selection did not preserve existing setup: %v", err)
	}
}

func TestSetupCancellationDoesNotSave(t *testing.T) {
	clearProviderEnv(t)
	var saved Config
	backend := setupTestBackend(&saved)
	backend.save = func(Config) error { t.Fatal("canceled setup must not save"); return nil }
	_, err := runSetup(context.Background(), SetupOptions{}, &scriptedSetupUI{t: t, choices: []string{"cancel"}}, backend)
	if !errors.Is(err, tui.ErrCancelled) {
		t.Fatalf("cancellation: %v", err)
	}
}

func TestSetupRejectsInvalidAdvancedLimitWithoutPromptLoop(t *testing.T) {
	clearProviderEnv(t)
	t.Setenv("WENDY_CHAT_MAX_TOKENS", "invalid")
	var saved Config
	_, err := runSetup(context.Background(), SetupOptions{}, &scriptedSetupUI{t: t}, setupTestBackend(&saved))
	if err == nil || !strings.Contains(err.Error(), "WENDY_CHAT_MAX_TOKENS") {
		t.Fatalf("invalid limit: %v", err)
	}
}

// runSetupTerminalFixture exercises real terminal rendering without reading or
// writing user settings or connecting to a model service. Used by PTY smoke QA.
func runSetupTerminalFixture() int {
	var saved Config
	backend := setupTestBackend(&saved)
	backend.models = func(context.Context, Config) ([]ModelOption, error) {
		return []ModelOption{{ID: "fixture-model", Name: "Fixture model", Description: "Local model with tool support"}}, nil
	}
	cfg, err := runSetup(context.Background(), SetupOptions{}, &terminalSetupUI{input: os.Stdin, output: os.Stdout}, backend)
	if errors.Is(err, tui.ErrCancelled) {
		fmt.Println("SETUP_CANCELED")
		return 0
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if _, err := runSetup(context.Background(), SetupOptions{}, &terminalSetupUI{input: os.Stdin, output: os.Stdout}, backend); err != nil {
		return 1
	}
	fmt.Println("SETUP_OK", cfg.Provider, cfg.Model)
	return 0
}
