package commands

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/chat"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
)

func newChatCmd() *cobra.Command {
	var cfg chat.Config
	var directory string
	var autoApprove, setup, helpAll, voice bool
	var preferredDevice, headlessPrompt string
	cmd := &cobra.Command{
		Use:   "chat [prompt...]",
		Short: "Build apps and work with your devices through a conversation",
		Long: `Chat with Wendy to build apps, deploy them, and work with your devices.

Just run wendy chat. I'll help you choose a local AI or a cloud service,
connect it, and pick a model. Your choice is remembered for next time.

Use --setup whenever you want to change your AI or model.

Scripts and other programs can run one turn without the chat screen with
--prompt. Add --json for one event per line; add --yes to allow tools that
would otherwise ask a person.`,
		Example: `  wendy chat
  wendy chat "Help me get this project running"
  wendy chat --setup
  wendy chat -C ./my-app
  wendy chat --prompt "Which of my devices are online?"
  wendy chat --prompt "Why does app A on device X fail?" --json --yes`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if helpAll {
				return cmd.Help()
			}
			workspace, err := chatWorkspace(directory)
			if err != nil {
				return err
			}
			if headlessPrompt != "" {
				return runChatHeadless(cmd, chatHeadlessOptions{
					Config: cfg, Prompt: headlessPrompt, Workspace: workspace, Device: preferredDevice,
					AutoApprove: autoApprove, JSON: jsonOutput, Voice: voice, Setup: setup,
				})
			}
			if !isInteractiveTerminal() {
				return fmt.Errorf("wendy chat requires an interactive terminal (stdin and stdout must be TTYs)")
			}
			if jsonOutput {
				return fmt.Errorf("wendy chat is interactive; remove --json to start the chat")
			}
			resolved, err := chat.Setup(cmd.Context(), chat.SetupOptions{
				Config: cfg, Force: setup, Input: cmd.InOrStdin(), Output: cmd.OutOrStdout(),
			})
			if errors.Is(err, tui.ErrCancelled) || errors.Is(err, context.Canceled) {
				return ErrUserCancelled
			}
			if err != nil {
				return err
			}
			voiceKey := chat.VoiceKey(resolved)
			voiceSupportError := chat.VoiceSupportError()
			if voice && voiceKey == "" && voiceSupportError == nil {
				voiceKey, err = chat.SetupVoice(cmd.Context(), resolved, cmd.InOrStdin(), cmd.OutOrStdout())
				if errors.Is(err, tui.ErrCancelled) {
					voice = false
				} else if err != nil {
					return err
				}
			}
			executable, err := os.Executable()
			if err != nil {
				return fmt.Errorf("locating Wendy executable: %w", err)
			}
			ctx, cancel := context.WithCancel(cmd.Context())
			defer cancel()
			toolset, err := chat.NewTools(ctx, executable, workspace, preferredDevice)
			if err != nil {
				return err
			}
			defer toolset.Close()
			initialPrompt := strings.Join(args, " ")
			var engine *chat.Engine
			state := new(chat.UIState)
			for {
				if engine == nil {
					provider, err := chat.NewProvider(resolved)
					if err != nil {
						return err
					}
					engine = chat.NewEngine(provider, toolset, chat.SystemPrompt(workspace, preferredDevice))
				}
				var voiceFactory func(context.Context) (chat.VoiceSession, error)
				if voiceSupportError != nil {
					voiceFactory = func(context.Context) (chat.VoiceSession, error) { return nil, voiceSupportError }
				} else if voiceKey != "" {
					key := voiceKey
					voiceFactory = func(ctx context.Context) (chat.VoiceSession, error) { return chat.StartVoice(ctx, key) }
				}
				err = chat.Run(ctx, chat.UIOptions{
					Engine: engine, Provider: resolved.Provider, Model: resolved.Model,
					State:     state,
					Workspace: workspace, Device: preferredDevice,
					InitialPrompt: initialPrompt, AutoApprove: autoApprove,
					Voice: voice, VoiceFactory: voiceFactory,
					Input: cmd.InOrStdin(), Output: cmd.OutOrStdout(),
				})
				initialPrompt = ""
				voice = state.Voice
				if errors.Is(err, chat.ErrVoiceSetup) {
					key, setupErr := chat.SetupVoice(ctx, resolved, cmd.InOrStdin(), cmd.OutOrStdout())
					if errors.Is(setupErr, tui.ErrCancelled) {
						continue
					}
					if setupErr != nil {
						return setupErr
					}
					voiceKey, voice = key, true
					continue
				}
				if !errors.Is(err, chat.ErrReconfigure) {
					return err
				}
				nextConfig, err := chat.Setup(ctx, chat.SetupOptions{Config: cfg, Force: true, Input: cmd.InOrStdin(), Output: cmd.OutOrStdout()})
				if errors.Is(err, tui.ErrCancelled) {
					// Resume the current conversation if setup was canceled.
					initialPrompt = ""
					continue
				}
				if err != nil {
					return err
				}
				resolved = nextConfig
				engine = nil
				state = new(chat.UIState)
				initialPrompt = ""
			}
		},
	}
	cmd.Flags().BoolVar(&setup, "setup", false, "Choose or change your AI and model")
	cmd.Flags().BoolVar(&voice, "voice", false, "Listen and speak with GPT Live (or use /voice in chat)")
	cmd.Flags().StringVarP(&headlessPrompt, "prompt", "p", "", "Run one turn without the chat screen and exit; combine with --json and --yes")
	cmd.Flags().BoolVar(&helpAll, "help-all", false, "Show advanced connection options")
	cmd.Flags().StringVarP(&preferredDevice, "device", "d", "", "Preferred Wendy device (you can also choose while chatting)")
	cmd.Flags().StringVar(&cfg.Provider, "provider", "", "Model API: openai, anthropic, ollama, or local (WENDY_CHAT_PROVIDER)")
	cmd.Flags().StringVarP(&cfg.Model, "model", "m", "", "Model name served by your endpoint (WENDY_CHAT_MODEL)")
	cmd.Flags().StringVar(&cfg.BaseURL, "base-url", "", "API base URL, including /v1 for compatible servers (WENDY_CHAT_BASE_URL)")
	cmd.Flags().IntVar(&cfg.MaxTokens, "max-tokens", 0, "Maximum generated tokens per response (WENDY_CHAT_MAX_TOKENS)")
	cmd.Flags().StringVarP(&directory, "directory", "C", ".", "Project directory for local file and command tools")
	cmd.Flags().BoolVarP(&autoApprove, "yes", "y", false, "Approve all tool calls, including shell commands and device changes")
	advanced := []string{"provider", "model", "base-url", "max-tokens", "yes"}
	for _, name := range advanced {
		_ = cmd.Flags().MarkHidden(name)
	}
	defaultHelp := cmd.HelpFunc()
	cmd.SetHelpFunc(func(cmd *cobra.Command, args []string) {
		if helpAll {
			for _, name := range advanced {
				cmd.Flags().Lookup(name).Hidden = false
			}
		}
		defaultHelp(cmd, args)
	})
	// Only show local flags: the inherited --json option cannot be used by chat.
	cmd.SetHelpTemplate(`{{.Long}}

Usage:
  {{.UseLine}}

Examples:
{{.Example}}

Options:
{{.LocalFlags.FlagUsages}}`)
	_ = cmd.MarkFlagDirname("directory")
	return cmd
}

type chatHeadlessOptions struct {
	Config      chat.Config
	Prompt      string
	Workspace   string
	Device      string
	AutoApprove bool
	JSON        bool
	Voice       bool
	Setup       bool
}

// runChatHeadless is wendy chat for a caller that is not a person at a
// terminal: a script, a CI job, or another agent. It uses the saved setup or
// the connection flags, never the guided setup screens, and it writes either
// the final answer or one JSON event per line to stdout.
func runChatHeadless(cmd *cobra.Command, opts chatHeadlessOptions) error {
	if opts.Setup {
		return fmt.Errorf("--setup is interactive; run wendy chat --setup once, then use --prompt")
	}
	if opts.Voice {
		return fmt.Errorf("--voice needs a microphone and a person; remove it to use --prompt")
	}
	saved, err := chat.LoadSettings()
	if err != nil {
		saved = chat.Config{}
	}
	resolved, err := chat.ResolveConfig(chat.MergeSettings(opts.Config, saved))
	if err != nil {
		return fmt.Errorf("chat is not set up for headless use: %w\nRun wendy chat --setup once, or pass --provider, --model and --base-url", err)
	}
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locating Wendy executable: %w", err)
	}
	ctx, cancel := context.WithCancel(cmd.Context())
	defer cancel()
	toolset, err := chat.NewTools(ctx, executable, opts.Workspace, opts.Device)
	if err != nil {
		return err
	}
	defer toolset.Close()
	provider, err := chat.NewProvider(resolved)
	if err != nil {
		return err
	}
	engine := chat.NewEngine(provider, toolset, chat.SystemPrompt(opts.Workspace, opts.Device))
	err = chat.RunHeadless(ctx, chat.HeadlessOptions{
		Engine: engine, Prompt: opts.Prompt, JSON: opts.JSON,
		Output: cmd.OutOrStdout(), AutoApprove: opts.AutoApprove,
	})
	if errors.Is(err, context.Canceled) {
		return ErrUserCancelled
	}
	return err
}

func chatWorkspace(directory string) (string, error) {
	workspace, err := filepath.Abs(directory)
	if err == nil {
		workspace, err = filepath.EvalSymlinks(workspace)
	}
	if err != nil {
		return "", fmt.Errorf("opening chat directory: %w", err)
	}
	info, err := os.Stat(workspace)
	if err != nil {
		return "", fmt.Errorf("opening chat directory: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("chat directory %q is not a directory", workspace)
	}
	return workspace, nil
}
