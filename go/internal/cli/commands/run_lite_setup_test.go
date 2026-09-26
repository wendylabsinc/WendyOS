package commands

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

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
