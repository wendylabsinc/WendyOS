// Package commands - shell completion script generation and installation.
package commands

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"
)

// completionRcSentinel marks Wendy-managed lines in a user's shell rc file so
// repeat installs are idempotent.
const completionRcSentinel = "# wendy-completion"

type installPlan struct {
	scriptPath string
	rcPath     string
	rcBlock    string
	notes      []string
}

func newCompletionCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "completion",
		Short: "Generate or install shell completion scripts",
		Long: "Generate shell completion scripts, or install them automatically for the current user " +
			"with `wendy completion install`.\n\n" +
			"Supported shells: bash, zsh, fish, powershell.",
		Args: cobra.NoArgs,
	}

	cmd.AddCommand(
		shellGenCmd("bash", "Print bash completion script", func(root *cobra.Command, w io.Writer) error {
			return root.GenBashCompletionV2(w, true)
		}),
		shellGenCmd("zsh", "Print zsh completion script", func(root *cobra.Command, w io.Writer) error {
			return root.GenZshCompletion(w)
		}),
		shellGenCmd("fish", "Print fish completion script", func(root *cobra.Command, w io.Writer) error {
			return root.GenFishCompletion(w, true)
		}),
		shellGenCmd("powershell", "Print PowerShell completion script", func(root *cobra.Command, w io.Writer) error {
			return root.GenPowerShellCompletionWithDesc(w)
		}),
		newCompletionInstallCmd(),
	)

	return cmd
}

func shellGenCmd(name, short string, fn func(*cobra.Command, io.Writer) error) *cobra.Command {
	return &cobra.Command{
		Use:   name,
		Short: short,
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return fn(c.Root(), c.OutOrStdout())
		},
	}
}

func newCompletionInstallCmd() *cobra.Command {
	var (
		shellOverride string
		outputDir     string
		printPath     bool
		toStdout      bool
	)

	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install shell completions into the user's standard config locations",
		Long: "Detect the current shell and write its completion script to the conventional " +
			"location, appending an idempotent sourcing line to your shell rc file when needed.\n\n" +
			"Use --shell to override shell detection, --print-path for a dry run, or --stdout " +
			"to print the script to stdout (useful for `brew install` completion staging).",
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			if toStdout && printPath {
				return errors.New("--stdout and --print-path are mutually exclusive")
			}
			shell, err := detectShell(shellOverride, runtime.GOOS, os.Getenv)
			if err != nil {
				return err
			}
			if toStdout {
				return writeShellScript(c.Root(), shell, c.OutOrStdout())
			}
			home, err := resolveHome(outputDir)
			if err != nil {
				return err
			}
			plan, err := computeInstallPlan(shell, runtime.GOOS, home, os.Getenv, fileExists)
			if err != nil {
				return err
			}

			if printPath {
				fmt.Fprintln(c.OutOrStdout(), plan.scriptPath)
				if plan.rcPath != "" {
					fmt.Fprintln(c.OutOrStdout(), plan.rcPath)
				}
				return nil
			}

			return performInstall(c.Root(), c.ErrOrStderr(), shell, plan)
		},
	}

	cmd.Flags().StringVar(&shellOverride, "shell", "", "Override shell (bash|zsh|fish|powershell)")
	cmd.Flags().StringVar(&outputDir, "output-dir", "", "Use this directory as $HOME (for testing)")
	cmd.Flags().BoolVar(&printPath, "print-path", false, "Print install paths without writing")
	cmd.Flags().BoolVar(&toStdout, "stdout", false, "Print the completion script to stdout instead of installing")
	// --output-dir is a test seam, not a user-facing knob; hide from --help.
	_ = cmd.Flags().MarkHidden("output-dir")
	return cmd
}

// posixSingleQuote returns s wrapped in POSIX single quotes, suitable for
// embedding in bash/zsh rc files. Inside single quotes only the quote
// character itself is special; close-quote-literal-quote-open-quote escapes it.
func posixSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// powershellSingleQuote returns s wrapped in PowerShell single quotes, where
// single quotes are literal and embedded quotes are escaped by doubling.
func powershellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func detectShell(override, goos string, env func(string) string) (string, error) {
	if override != "" {
		return normalizeShell(override)
	}
	if goos == "windows" {
		return "powershell", nil
	}
	s := env("SHELL")
	if s == "" {
		return "", errors.New("could not detect shell from $SHELL; pass --shell <bash|zsh|fish|powershell>")
	}
	return normalizeShell(filepath.Base(s))
}

func normalizeShell(name string) (string, error) {
	switch name {
	case "bash", "zsh", "fish", "powershell":
		return name, nil
	case "pwsh":
		return "powershell", nil
	default:
		return "", fmt.Errorf("unsupported shell %q (supported: bash, zsh, fish, powershell)", name)
	}
}

func resolveHome(override string) (string, error) {
	if override != "" {
		return filepath.Abs(override)
	}
	return os.UserHomeDir()
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func computeInstallPlan(shell, goos, home string, env func(string) string, exists func(string) bool) (installPlan, error) {
	switch shell {
	case "bash":
		return bashPlan(goos, home, env, exists), nil
	case "zsh":
		return zshPlan(home, env), nil
	case "fish":
		return fishPlan(home, env), nil
	case "powershell":
		return powershellPlan(goos, home), nil
	default:
		return installPlan{}, fmt.Errorf("unsupported shell %q", shell)
	}
}

func bashPlan(goos, home string, env func(string) string, exists func(string) bool) installPlan {
	xdg := env("XDG_DATA_HOME")
	if xdg == "" {
		xdg = filepath.Join(home, ".local", "share")
	}
	standard := filepath.Join(xdg, "bash-completion", "completions", "wendy")

	bcPresent := exists(filepath.Join(xdg, "bash-completion")) ||
		exists("/etc/bash_completion") ||
		exists("/usr/local/etc/bash_completion") ||
		exists("/opt/homebrew/etc/bash_completion")

	if bcPresent {
		return installPlan{
			scriptPath: standard,
			notes:      []string{"bash-completion v2 detected; the script will be auto-loaded on next shell start."},
		}
	}

	scriptPath := filepath.Join(home, ".wendy", "completions", "wendy.bash")
	rcPath := filepath.Join(home, ".bashrc")
	quoted := posixSingleQuote(scriptPath)
	rcBlock := fmt.Sprintf("%s\n[ -f %s ] && source %s", completionRcSentinel, quoted, quoted)
	notes := []string{
		"bash-completion package not detected; using stand-alone install.",
		"Restart your shell (or `source ~/.bashrc`) for completions to take effect.",
	}
	if goos == "darwin" {
		notes = append(notes, "On macOS login shells, ensure ~/.bash_profile sources ~/.bashrc.")
	}
	return installPlan{
		scriptPath: scriptPath,
		rcPath:     rcPath,
		rcBlock:    rcBlock,
		notes:      notes,
	}
}

func zshPlan(home string, env func(string) string) installPlan {
	scriptDir := filepath.Join(home, ".zfunc")
	scriptPath := filepath.Join(scriptDir, "_wendy")

	rcDir := home
	if zd := env("ZDOTDIR"); zd != "" {
		rcDir = zd
	}
	rcPath := filepath.Join(rcDir, ".zshrc")
	rcBlock := fmt.Sprintf("%s\nfpath=(%s $fpath)\nautoload -U compinit && compinit", completionRcSentinel, posixSingleQuote(scriptDir))

	return installPlan{
		scriptPath: scriptPath,
		rcPath:     rcPath,
		rcBlock:    rcBlock,
		notes:      []string{"Restart your shell (or `source ~/.zshrc`) for completions to take effect."},
	}
}

func fishPlan(home string, env func(string) string) installPlan {
	cfg := env("XDG_CONFIG_HOME")
	if cfg == "" {
		cfg = filepath.Join(home, ".config")
	}
	return installPlan{
		scriptPath: filepath.Join(cfg, "fish", "completions", "wendy.fish"),
		notes:      []string{"Fish auto-loads completions on next shell start."},
	}
}

func powershellPlan(goos, home string) installPlan {
	var profileDir string
	switch goos {
	case "windows":
		profileDir = filepath.Join(home, "Documents", "PowerShell")
	default:
		profileDir = filepath.Join(home, ".config", "powershell")
	}
	scriptPath := filepath.Join(profileDir, "Completions", "wendy.ps1")
	rcPath := filepath.Join(profileDir, "Microsoft.PowerShell_profile.ps1")
	rcBlock := fmt.Sprintf("%s\n. %s", completionRcSentinel, powershellSingleQuote(scriptPath))
	notes := []string{"Restart PowerShell for completions to take effect."}
	if goos == "windows" {
		notes = append(notes, "Windows PowerShell 5.1 users: dot-source the script from your WindowsPowerShell profile manually.")
	}
	return installPlan{
		scriptPath: scriptPath,
		rcPath:     rcPath,
		rcBlock:    rcBlock,
		notes:      notes,
	}
}

func performInstall(root *cobra.Command, stderr io.Writer, shell string, plan installPlan) error {
	if err := os.MkdirAll(filepath.Dir(plan.scriptPath), 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(plan.scriptPath), err)
	}
	f, err := os.OpenFile(plan.scriptPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("creating %s: %w", plan.scriptPath, err)
	}
	if err := writeShellScript(root, shell, f); err != nil {
		if cerr := f.Close(); cerr != nil {
			return errors.Join(err, cerr)
		}
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	fmt.Fprintf(stderr, "Wrote %s\n", plan.scriptPath)

	if plan.rcPath != "" {
		added, err := ensureBlockInFile(plan.rcPath, completionRcSentinel, plan.rcBlock)
		if err != nil {
			return fmt.Errorf("editing %s: %w", plan.rcPath, err)
		}
		if added {
			fmt.Fprintf(stderr, "Updated %s\n", plan.rcPath)
		} else {
			fmt.Fprintf(stderr, "Already configured in %s\n", plan.rcPath)
		}
	}

	for _, note := range plan.notes {
		fmt.Fprintln(stderr, note)
	}
	return nil
}

func writeShellScript(root *cobra.Command, shell string, w io.Writer) error {
	switch shell {
	case "bash":
		return root.GenBashCompletionV2(w, true)
	case "zsh":
		return root.GenZshCompletion(w)
	case "fish":
		return root.GenFishCompletion(w, true)
	case "powershell":
		return root.GenPowerShellCompletionWithDesc(w)
	default:
		return fmt.Errorf("unsupported shell %q", shell)
	}
}

// ensureBlockInFile appends block to path unless sentinel is already present.
// Returns true when the file was modified. Surfaces close errors via a named
// return so buffered-write failures on close aren't silently dropped.
func ensureBlockInFile(path, sentinel, block string) (added bool, err error) {
	if err = os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}

	existing, readErr := os.ReadFile(path)
	switch {
	case errors.Is(readErr, os.ErrNotExist):
		existing = nil
	case readErr != nil:
		return false, readErr
	}

	if bytes.Contains(existing, []byte(sentinel)) {
		return false, nil
	}

	f, openErr := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if openErr != nil {
		return false, openErr
	}
	defer func() {
		if cerr := f.Close(); err == nil && cerr != nil {
			err = cerr
		}
	}()

	var buf strings.Builder
	if len(existing) > 0 && existing[len(existing)-1] != '\n' {
		buf.WriteByte('\n')
	}
	buf.WriteString(block)
	if !strings.HasSuffix(block, "\n") {
		buf.WriteByte('\n')
	}
	if _, err = f.WriteString(buf.String()); err != nil {
		return false, err
	}
	return true, nil
}
