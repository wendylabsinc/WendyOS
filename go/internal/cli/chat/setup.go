package chat

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
)

// SetupOptions controls guided configuration before the conversation starts.
type SetupOptions struct {
	Config Config
	Force  bool
	Input  io.Reader
	Output io.Writer
}

type setupChoice struct{ ID, Label, Detail string }

type setupUI interface {
	Choose(context.Context, string, string, []setupChoice) (string, error)
	Text(context.Context, string, string, string, bool) (string, error)
	Work(context.Context, string, func(context.Context, func(string)) error) error
	Note(string)
}

type setupBackend struct {
	load     func() (Config, error)
	save     func(Config) error
	discover func(context.Context) []LocalServer
	models   func(context.Context, Config) ([]ModelOption, error)
	pull     func(context.Context, Config, string, func(string)) error
}

// Setup fills missing settings through a guided flow and remembers the chosen
// connection. Complete saved/explicit configurations start without prompting.
func Setup(ctx context.Context, opts SetupOptions) (Config, error) {
	return runSetup(ctx, opts, &terminalSetupUI{input: opts.Input, output: opts.Output}, setupBackend{
		load: LoadSettings, save: SaveSettings, discover: DiscoverLocal,
		models: ListModels, pull: PullOllamaModel,
	})
}

func runSetup(ctx context.Context, opts SetupOptions, ui setupUI, backend setupBackend) (Config, error) {
	saved, err := backend.load()
	if err != nil {
		ui.Note("I couldn't read the saved chat setup. Let's choose your AI again.")
		saved = Config{}
	}
	cfg := MergeSettings(opts.Config, saved)
	// Invalid advanced token limits cannot be fixed by choosing another AI.
	// Validate them once so the guided flow cannot loop on the same setting.
	if _, err := resolveConnection(Config{Provider: "local", MaxTokens: cfg.MaxTokens, maxTokensEnvironment: cfg.maxTokensEnvironment}, false); err != nil {
		return Config{}, err
	}
	selectedConfig := func(selected Config) Config {
		return MergeSettings(Config{Provider: selected.Provider, BaseURL: selected.BaseURL, MaxTokens: opts.Config.MaxTokens}, saved)
	}
	if !opts.Force {
		if complete, err := ResolveConfig(cfg); err == nil {
			return complete, nil
		}
	}
	ui.Note("Welcome to Wendy Chat. Let's connect your AI — I'll remember your choice for next time.")
	var servers []LocalServer
	chooseProvider := opts.Force || cfg.Provider == ""
	for {
		if err := ctx.Err(); err != nil {
			return Config{}, err
		}
		if chooseProvider {
			if err := ui.Work(ctx, "Looking for local AI apps…", func(ctx context.Context, _ func(string)) error {
				servers = backend.discover(ctx)
				return nil
			}); err != nil {
				return Config{}, err
			}
			localDetail := "Private, on your computer. I'll help you connect Ollama or LM Studio."
			if len(servers) > 0 {
				localDetail = "Found " + servers[0].Name + " on this computer. No API key needed."
			}
			choice, err := ui.Choose(ctx, "Where would you like your AI to run?", "Wendy uses the AI you choose to help you build apps and work with devices.", []setupChoice{
				{"computer", "On this computer", localDetail},
				{"openai", "OpenAI", "Connect an OpenAI API account. Usage is billed by OpenAI."},
				{"anthropic", "Anthropic (Claude)", "Connect an Anthropic API account. Usage is billed by Anthropic."},
				{"custom", "Another AI server", "Connect a server on your network or an OpenAI-compatible service."},
			})
			if err != nil {
				return Config{}, err
			}
			switch choice {
			case "computer":
				selected, err := selectLocalServer(ctx, ui, backend, servers)
				if errors.Is(err, errSetupBack) {
					continue
				}
				if err != nil {
					return Config{}, err
				}
				cfg = selectedConfig(selected)
			case "custom":
				address, err := ui.Text(ctx, "AI server address", "Paste the API address from your server app, for example http://localhost:1234/v1.", "http://localhost:1234/v1", false)
				if err != nil {
					return Config{}, err
				}
				cfg = selectedConfig(Config{Provider: "local", BaseURL: address})
			default:
				cfg = selectedConfig(Config{Provider: choice, BaseURL: defaultBaseURL(choice)})
			}
			// --setup explicitly asks to choose the model again, even when the
			// provider matches the previous session.
			cfg.Model = ""
		}
		chooseProvider = false
		cfg, err = ResolveConnection(cfg)
		if err != nil {
			ui.Note("That connection needs another look: " + err.Error())
			chooseProvider = true
			continue
		}
		pastedKey := false
		if opts.Force && cfg.APIKey != "" {
			choice, choiceErr := ui.Choose(ctx, "Which API key should Wendy use?", "A key is already configured for this service. Keep it, or enter a replacement privately.", []setupChoice{
				{"keep-key", "Keep current key", "Continue with the key already configured for this service."},
				{"replace-key", "Enter a different API key", "Paste a replacement in the private key field."},
			})
			if choiceErr != nil {
				return Config{}, choiceErr
			}
			if choice == "replace-key" {
				cfg.APIKey, err = enterSetupKey(ctx, ui, cfg)
				if err != nil {
					return Config{}, err
				}
				pastedKey = true
			}
		}
		if RequiredAPIKeyEnv(cfg) != "" && cfg.APIKey == "" {
			cfg.APIKey, err = enterSetupKey(ctx, ui, cfg)
			if err != nil {
				return Config{}, err
			}
			pastedKey = true
		}
		for {
			var models []ModelOption
			err = ui.Work(ctx, "Finding models you can use…", func(ctx context.Context, _ func(string)) error {
				var err error
				models, err = backend.models(ctx, cfg)
				return err
			})
			if errors.Is(err, ErrNoModels) {
				err = nil
			}
			if errors.Is(err, tui.ErrCancelled) || errors.Is(err, context.Canceled) {
				return Config{}, err
			}
			if err != nil {
				choices := []setupChoice{{"retry", "Try again", "Check your connection or start the model server, then retry."}}
				choices = append(choices, setupChoice{"key", "Enter an API key", "Use a new key, or add the key required by this server."})
				if cfg.Provider == "local" || cfg.Provider == "ollama" {
					choices = append(choices, setupChoice{"address", "Change server address", "Use the API address shown in your local AI app."})
				}
				choices = append(choices, setupChoice{"back", "Choose another AI", "Go back to local and cloud options."})
				choice, choiceErr := ui.Choose(ctx, "Let's get you connected", err.Error(), choices)
				if choiceErr != nil {
					return Config{}, choiceErr
				}
				switch choice {
				case "key":
					cfg.APIKey, err = enterSetupKey(ctx, ui, cfg)
					if err != nil {
						return Config{}, err
					}
					pastedKey = true
				case "address":
					address, textErr := ui.Text(ctx, "AI server address", "Include the API path, usually /v1.", cfg.BaseURL, false)
					if textErr != nil {
						return Config{}, textErr
					}
					// A key belongs to its old endpoint; never carry it to a new URL.
					cfg.BaseURL, cfg.APIKey, cfg.Model = address, "", ""
					pastedKey = false
				case "back":
					chooseProvider = true
				}
				if chooseProvider {
					break
				}
				continue
			}
			if len(models) == 0 {
				choices := []setupChoice{{"retry", "Check again", "Load a model in your AI app, then return here."}, {"back", "Choose another AI", "Return to local and cloud options."}}
				description := "Your server is running, but it hasn't offered a chat model yet. In LM Studio, download a model that supports tools and load it in the server."
				if cfg.Provider == "ollama" {
					description = "Ollama is ready. Let's download a small model that can use Wendy's tools. This is a one-time download."
					choices = append([]setupChoice{{"download", "Download Qwen3 4B · 2.5 GB", "A small starter model. Runs on this computer, with no API key."}}, choices...)
				} else if cfg.Provider != "local" {
					description = "This account hasn't offered a supported chat model. Check model access and API billing with your provider, then try again."
				}
				choice, choiceErr := ui.Choose(ctx, "Choose a model to get started", description, choices)
				if choiceErr != nil {
					return Config{}, choiceErr
				}
				if choice == "back" {
					chooseProvider = true
					break
				}
				if choice == "download" {
					err := ui.Work(ctx, "Downloading your local model…", func(ctx context.Context, progress func(string)) error {
						return backend.pull(ctx, cfg, StarterLocalModel, progress)
					})
					if errors.Is(err, tui.ErrCancelled) || errors.Is(err, context.Canceled) {
						return Config{}, err
					}
					if err != nil {
						ui.Note("The download didn't finish: " + err.Error())
					}
				}
				continue
			}
			choices := make([]setupChoice, 0, len(models)+1)
			found := false
			for _, model := range models {
				label := model.Name
				if label == "" {
					label = model.ID
				}
				choices = append(choices, setupChoice{model.ID, label, model.Description})
				found = found || model.ID == cfg.Model
			}
			if cfg.Model != "" && !found {
				ui.Note(fmt.Sprintf("%q isn't available from this AI service. Choose one of the models below.", cfg.Model))
			}
			choices = append(choices, setupChoice{"__back", "Choose another AI", "Return to local and cloud options."})
			model, err := ui.Choose(ctx, "Which model would you like to use?", "These models are available from your chosen service. You can change this later with wendy chat --setup.", choices)
			if err != nil {
				return Config{}, err
			}
			if model == "__back" {
				chooseProvider = true
				break
			}
			cfg.Model = model
			resolved, err := ResolveConfig(cfg)
			if err != nil {
				ui.Note(err.Error())
				continue
			}
			toSave := resolved
			// Environment credentials remain in the environment. Only remember
			// keys explicitly pasted here or already saved for this connection.
			if !pastedKey {
				toSave.APIKey = ""
				if strings.EqualFold(saved.Provider, resolved.Provider) && sameEndpoint(saved.BaseURL, resolved.BaseURL) {
					toSave.APIKey = saved.APIKey
				}
			}
			if err := backend.save(toSave); err != nil {
				ui.Note("You're connected, but I couldn't remember this setup: " + err.Error())
			} else {
				ui.Note("Ready. Next time, just run wendy chat. Use --setup whenever you want to switch AI.")
			}
			return resolved, nil
		}
	}
}

var errSetupBack = errors.New("return to AI selection")

func selectLocalServer(ctx context.Context, ui setupUI, backend setupBackend, servers []LocalServer) (Config, error) {
	for {
		if len(servers) == 1 {
			ui.Note("Found " + servers[0].Name + " on this computer.")
			return servers[0].Config, nil
		}
		if len(servers) > 1 {
			choices := make([]setupChoice, 0, len(servers))
			for _, server := range servers {
				choices = append(choices, setupChoice{server.Config.BaseURL, server.Name, fmt.Sprintf("%d available models", len(server.Models))})
			}
			selected, err := ui.Choose(ctx, "I found these local AI apps", "Choose the app you want Wendy to use.", choices)
			if err != nil {
				return Config{}, err
			}
			for _, server := range servers {
				if server.Config.BaseURL == selected {
					return server.Config, nil
				}
			}
		}
		choice, err := ui.Choose(ctx, "Let's set up local AI", "No local AI app is running yet.\n\n1. Install Ollama from https://ollama.com/download, then open it.\n2. Choose Check again below. I'll find it and help you download a starter model.\n\nAlready use LM Studio? Start its local server, then choose Check again.", []setupChoice{
			{"retry", "Check again", "I've opened Ollama or started my local server."},
			{"address", "My server is at another address", "Connect an AI server on your network."},
			{"back", "Use a cloud AI instead", "Return to the AI choices."},
		})
		if err != nil {
			return Config{}, err
		}
		switch choice {
		case "back":
			return Config{}, errSetupBack
		case "address":
			address, err := ui.Text(ctx, "AI server address", "Paste the API address shown in your AI app.", "http://localhost:1234/v1", false)
			return Config{Provider: "local", BaseURL: address}, err
		}
		if err := ui.Work(ctx, "Looking for local AI apps…", func(ctx context.Context, _ func(string)) error {
			servers = backend.discover(ctx)
			return nil
		}); err != nil {
			return Config{}, err
		}
	}
}

func enterSetupKey(ctx context.Context, ui setupUI, cfg Config) (string, error) {
	title, description := "API key", "Paste the key supplied by your AI service. It will be hidden as you type and saved privately for next time."
	switch cfg.Provider {
	case "openai":
		title = "Connect your OpenAI account"
		description = "Create an API key at https://platform.openai.com/api-keys, then paste it below.\nOpenAI API usage is billed separately from a ChatGPT subscription.\nYour key is hidden as you type and saved privately for next time."
	case "anthropic":
		title = "Connect your Anthropic account"
		description = "Create an API key in the Anthropic Console at https://platform.claude.com/settings/keys, then paste it below.\nAnthropic API usage is billed separately from a Claude subscription.\nYour key is hidden as you type and saved privately for next time."
	}
	for {
		value, err := ui.Text(ctx, title, description, "", true)
		if err != nil {
			return "", err
		}
		if value = strings.TrimSpace(value); value != "" && !strings.ContainsAny(value, "\r\n") {
			return value, nil
		}
		ui.Note("Paste a non-empty API key, or press Esc to cancel setup.")
	}
}
