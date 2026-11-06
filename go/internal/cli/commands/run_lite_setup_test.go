package commands

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/espidftoolchain"
	"github.com/wendylabsinc/wendy/go/internal/cli/providers"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
)

type coreSetupProvider struct {
	fakeProvider
	builds   int
	buildErr error
	retryErr error
}

func (p *coreSetupProvider) Build(context.Context, models.ExternalDevice, string, string, string, bool) (*providers.BuiltApp, error) {
	p.builds++
	if p.builds == 1 {
		return nil, p.buildErr
	}
	if p.retryErr != nil {
		return nil, p.retryErr
	}
	return &providers.BuiltApp{}, nil
}

func TestProviderBuildOffersWendyCoreSetup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake eim uses a shell script")
	}
	missing := &providers.MissingWendyCoreError{}
	other := errors.New("unrelated build failure")
	for _, tc := range []struct {
		name                    string
		interactive, accept     bool
		opts                    runOptions
		buildErr, retryErr      error
		wantPrompts, wantBuilds int
		wantSetup               bool
		wantErr                 error
	}{
		{name: "accept", interactive: true, accept: true, buildErr: missing, wantPrompts: 1, wantBuilds: 2, wantSetup: true},
		{name: "decline", interactive: true, buildErr: missing, wantPrompts: 1, wantBuilds: 1, wantErr: ErrUserCancelled},
		{name: "noninteractive", buildErr: missing, wantBuilds: 1, wantErr: missing},
		{name: "yes", opts: runOptions{yes: true}, buildErr: missing, wantBuilds: 2, wantSetup: true},
		{name: "watch", interactive: true, opts: withWatchInvariants(runOptions{}), buildErr: missing, wantBuilds: 1, wantErr: missing},
		{name: "other error", interactive: true, buildErr: other, wantBuilds: 1, wantErr: other},
		{name: "already configured", interactive: true, wantBuilds: 1},
		{name: "retry only once", interactive: true, accept: true, buildErr: missing, retryErr: missing, wantPrompts: 1, wantBuilds: 2, wantSetup: true, wantErr: missing},
		{name: "wrapped missing error", interactive: true, accept: true, buildErr: fmt.Errorf("configuration: %w", missing), wantPrompts: 1, wantBuilds: 2, wantSetup: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Mkdir(filepath.Join(dir, "main"), 0o755); err != nil {
				t.Fatal(err)
			}
			const original = "void app_main(void) { work(); }\n"
			sourcePath := filepath.Join(dir, "main", "app.c")
			if err := os.WriteFile(sourcePath, []byte(original), 0o644); err != nil {
				t.Fatal(err)
			}
			// Record IDF commands without downloading a toolchain or dependency.
			script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> idf-commands\n"
			if err := os.WriteFile(filepath.Join(dir, "eim"), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			oldInteractive, oldConfirm := isInteractiveTerminalFn, confirmFn
			t.Cleanup(func() { isInteractiveTerminalFn, confirmFn = oldInteractive, oldConfirm })
			isInteractiveTerminalFn = func() bool { return tc.interactive }
			prompts := 0
			confirmFn = func(question string) bool {
				prompts++
				if !strings.Contains(question, "wendy_core") || !strings.Contains(question, "initialization") {
					t.Fatalf("prompt does not describe changes: %q", question)
				}
				return tc.accept
			}
			p := &coreSetupProvider{buildErr: tc.buildErr, retryErr: tc.retryErr}
			_, err := providerBuild(context.Background(), p, models.ExternalDevice{}, dir, "esp-idf", "test", tc.opts)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("error = %v, want %v", err, tc.wantErr)
			}
			if prompts != tc.wantPrompts || p.builds != tc.wantBuilds {
				t.Fatalf("prompts=%d builds=%d, want %d, %d", prompts, p.builds, tc.wantPrompts, tc.wantBuilds)
			}
			source, err := os.ReadFile(sourcePath)
			if err != nil {
				t.Fatal(err)
			}
			commands, commandsErr := os.ReadFile(filepath.Join(dir, "idf-commands"))
			if tc.wantSetup {
				if !strings.Contains(string(source), "{\n    ESP_ERROR_CHECK(wendy_core_init());\n work(); }") {
					t.Fatalf("initialization was not first: %s", source)
				}
				if commandsErr != nil || !strings.Contains(string(commands), "idf.py add-dependency") || !strings.Contains(string(commands), "idf.py reconfigure") {
					t.Fatalf("setup commands missing: %s, %v", commands, commandsErr)
				}
			} else if string(source) != original || !os.IsNotExist(commandsErr) {
				t.Fatalf("unexpected setup: source=%s commands=%s", source, commands)
			}
		})
	}
}

func TestProviderBuildSetsUpWendyCoreWithProjectToolchain(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake docker uses a shell script")
	}
	dir := t.TempDir()
	image := "espressif/idf@sha256:" + strings.Repeat("c", 64)
	for name, content := range map[string]string{
		"main/app.c":     "void app_main(void) {}\n",
		"wendy-idf.json": `{"image":"` + image + `","target":"esp32s31"}`,
		// Host EIM must not run for a container-pinned project.
		"eim":    "#!/bin/sh\nexit 1\n",
		"docker": "#!/bin/sh\nprintf '%s\\n' \"$*\" >> docker-commands\n",
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	toolchain, err := espidftoolchain.LoadProjectToolchain(dir, "esp32s31")
	if err != nil {
		t.Fatal(err)
	}
	p := &coreSetupProvider{buildErr: &providers.MissingWendyCoreError{Toolchain: toolchain}}
	if _, err := providerBuild(context.Background(), p, models.ExternalDevice{}, dir, "esp-idf", "test", runOptions{yes: true}); err != nil {
		t.Fatal(err)
	}
	commands, err := os.ReadFile(filepath.Join(dir, "docker-commands"))
	if err != nil || !strings.Contains(string(commands), image+" idf.py add-dependency") || !strings.Contains(string(commands), image+" idf.py reconfigure") {
		t.Fatalf("setup did not use the project container: %s, %v", commands, err)
	}
}

// sequenceProvider fails each build with the next of errs, then succeeds.
type sequenceProvider struct {
	fakeProvider
	errs   []error
	builds int
}

func (p *sequenceProvider) Build(context.Context, models.ExternalDevice, string, string, string, bool) (*providers.BuiltApp, error) {
	p.builds++
	if p.builds <= len(p.errs) && p.errs[p.builds-1] != nil {
		return nil, p.errs[p.builds-1]
	}
	return &providers.BuiltApp{}, nil
}

func TestProviderBuildOffersEIMInstall(t *testing.T) {
	missingEIM := fmt.Errorf("%w; install it via 'brew install %s'", espidftoolchain.ErrEIMNotInstalled, espidftoolchain.EIMFormula)
	brewFailed := errors.New("brew failed")
	for _, tc := range []struct {
		name                    string
		interactive, accept     bool
		noBrew, stillMissing    bool
		opts                    runOptions
		errs                    []error
		installErr              error
		wantPrompts, wantBuilds int
		wantInstall             bool
		wantErr                 error
		wantMessage             string
	}{
		{name: "accept", interactive: true, accept: true, errs: []error{missingEIM}, wantPrompts: 1, wantInstall: true, wantBuilds: 2},
		{name: "decline", interactive: true, errs: []error{missingEIM}, wantPrompts: 1, wantBuilds: 1, wantErr: espidftoolchain.ErrEIMNotInstalled},
		{name: "noninteractive", errs: []error{missingEIM}, wantBuilds: 1, wantErr: espidftoolchain.ErrEIMNotInstalled},
		{name: "yes", opts: runOptions{yes: true}, errs: []error{missingEIM}, wantInstall: true, wantBuilds: 2},
		{name: "watch", interactive: true, accept: true, opts: withWatchInvariants(runOptions{}), errs: []error{missingEIM}, wantBuilds: 1, wantErr: espidftoolchain.ErrEIMNotInstalled},
		{name: "no Homebrew", interactive: true, accept: true, noBrew: true, errs: []error{missingEIM}, wantBuilds: 1, wantErr: espidftoolchain.ErrEIMNotInstalled},
		{name: "install failed", interactive: true, accept: true, installErr: brewFailed, errs: []error{missingEIM}, wantPrompts: 1, wantInstall: true, wantBuilds: 1, wantErr: brewFailed, wantMessage: "installing EIM via Homebrew"},
		{name: "not on PATH after install", interactive: true, accept: true, stillMissing: true, errs: []error{missingEIM}, wantPrompts: 1, wantInstall: true, wantBuilds: 1, wantMessage: "not yet on PATH"},
		{name: "other error", interactive: true, accept: true, errs: []error{errors.New("unrelated build failure")}, wantBuilds: 1, wantMessage: "unrelated build failure"},
		// The retried build can still need wendy_core, which keeps its own offer.
		{name: "then wendy_core", interactive: true, accept: true, errs: []error{missingEIM, &providers.MissingWendyCoreError{}}, wantPrompts: 2, wantInstall: true, wantBuilds: 2, wantErr: ErrUserCancelled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Mkdir(filepath.Join(dir, "main"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "main", "app.c"), []byte("void app_main(void) {}\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			var installed bool
			prompts := 0
			oldInteractive, oldConfirm := isInteractiveTerminalFn, confirmFn
			oldLook, oldInstall := eimLookPathFn, installEIMFn
			t.Cleanup(func() {
				isInteractiveTerminalFn, confirmFn = oldInteractive, oldConfirm
				eimLookPathFn, installEIMFn = oldLook, oldInstall
			})
			isInteractiveTerminalFn = func() bool { return tc.interactive }
			// Accept only the EIM prompt, so a later wendy_core offer declines.
			confirmFn = func(question string) bool {
				prompts++
				return tc.accept && question == "Do you want to install EIM?"
			}
			stubBrewLookPath(t, func(name string) (string, error) {
				if tc.noBrew {
					return "", exec.ErrNotFound
				}
				return "/opt/homebrew/bin/" + name, nil
			})
			installEIMFn = func(context.Context) error {
				installed = true
				return tc.installErr
			}
			eimLookPathFn = func(name string) (string, error) {
				if installed && !tc.stillMissing {
					return "/opt/homebrew/bin/" + name, nil
				}
				return "", exec.ErrNotFound
			}

			p := &sequenceProvider{errs: tc.errs}
			_, err := providerBuild(context.Background(), p, models.ExternalDevice{}, dir, "esp-idf", "test", tc.opts)
			if prompts != tc.wantPrompts || p.builds != tc.wantBuilds || installed != tc.wantInstall {
				t.Fatalf("prompts=%d builds=%d installed=%t, want %d, %d, %t", prompts, p.builds, installed, tc.wantPrompts, tc.wantBuilds, tc.wantInstall)
			}
			if tc.wantErr == nil && tc.wantMessage == "" {
				if err != nil {
					t.Fatalf("providerBuild() = %v, want nil", err)
				}
				return
			}
			if err == nil || (tc.wantErr != nil && !errors.Is(err, tc.wantErr)) || !strings.Contains(err.Error(), tc.wantMessage) {
				t.Fatalf("providerBuild() = %v, want %v containing %q", err, tc.wantErr, tc.wantMessage)
			}
		})
	}
}
