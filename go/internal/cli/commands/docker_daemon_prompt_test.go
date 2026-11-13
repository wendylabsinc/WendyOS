package commands

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
)

func stubDockerRuntimePrompt(t *testing.T, action string) {
	t.Helper()
	stubDockerDaemonSeams(t, time.Second)
	stubHumanPresent(t, true)
	isInteractiveTerminalFn = func() bool { return true }
	dockerVersionOKFn = func(context.Context) bool { return false }
	darwinDockerRuntimes = nil
	dockerLookPathFn = func(string) (string, error) { return "", os.ErrNotExist }
	if action == "open" {
		appPath := filepath.Join(t.TempDir(), "Docker.app")
		if err := os.Mkdir(appPath, 0700); err != nil {
			t.Fatal(err)
		}
		darwinDockerRuntimes = []dockerRuntime{{name: "Docker Desktop", app: appPath}}
		dockerLookPathFn = func(string) (string, error) { return "/usr/local/bin/docker", nil }
	}
	dockerInstallRuntimeFn = func(context.Context) error {
		t.Fatal("installed Docker after prompt cancellation or refusal")
		return nil
	}
	dockerOpenRuntimeFn = func(context.Context, string) error {
		t.Fatal("opened Docker after prompt cancellation or refusal")
		return nil
	}
}

func TestEnsureDockerDaemon_PromptCancellation(t *testing.T) {
	for _, action := range []string{"install", "open"} {
		for _, input := range []struct{ name, key string }{{"q", "q"}, {"ctrl-c", "\x03"}} {
			t.Run(action+"/"+input.name, func(t *testing.T) {
				stubDockerRuntimePrompt(t, action)
				prompts := 0
				dockerConfirmFn = func(ctx context.Context, question string) (bool, error) {
					prompts++
					return tui.ConfirmDefaultYes(question, tea.WithContext(ctx), tea.WithInput(strings.NewReader(input.key)), tea.WithOutput(io.Discard), tea.WithoutRenderer(), tea.WithoutSignals())
				}
				if err := ensureDockerDaemonForHostOS(context.Background(), dockerHostOSDarwin); !errors.Is(err, ErrUserCancelled) {
					t.Fatalf("error = %v, want user cancellation", err)
				}
				if prompts != 1 {
					t.Fatalf("prompts = %d, want 1", prompts)
				}
			})
		}
	}
}

func TestEnsureDockerDaemon_CancelledAcceptedPromptDoesNotInstallOrOpen(t *testing.T) {
	for _, action := range []string{"install", "open"} {
		t.Run(action, func(t *testing.T) {
			stubDockerRuntimePrompt(t, action)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			dockerConfirmFn = func(context.Context, string) (bool, error) {
				cancel()
				return true, nil
			}
			if err := ensureDockerDaemonForHostOS(ctx, dockerHostOSDarwin); !errors.Is(err, context.Canceled) {
				t.Fatalf("error = %v, want context cancellation", err)
			}
		})
	}
}

func TestEnsureDockerDaemon_DeclinedPromptRetainsManualInstructions(t *testing.T) {
	for _, action := range []string{"install", "open"} {
		t.Run(action, func(t *testing.T) {
			stubDockerRuntimePrompt(t, action)
			dockerConfirmFn = func(context.Context, string) (bool, error) { return false, nil }
			err := ensureDockerDaemonForHostOS(context.Background(), dockerHostOSDarwin)
			instruction := "install Docker Desktop"
			if action == "open" {
				instruction = "start Docker Desktop"
			}
			if err == nil || !strings.Contains(err.Error(), instruction) || errors.Is(err, ErrUserCancelled) {
				t.Fatalf("error = %v, want manual instruction %q", err, instruction)
			}
		})
	}
}
