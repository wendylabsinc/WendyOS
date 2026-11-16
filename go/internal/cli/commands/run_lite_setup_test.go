package commands

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/espidftoolchain"
	"github.com/wendylabsinc/wendy/go/internal/cli/litebuild"
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

// liteMasterConf is a small master configuration in the shape of the
// wendy-lite one, for the sdkconfig.defaults.wendy repair.
const liteMasterConf = `
attributes:
  board: {type: string, enum: [c61_board]}
  chip: {type: string, enum: [esp32c61]}
  idf_version: {type: string}
  idf_preview: {type: boolean}
  cloud_tunnel: {type: boolean}
rules:
  - if: board == "c61_board"
    set: {chip: esp32c61, idf_version: "6.0.3", idf_preview: false}
  - if: cloud_tunnel
    sdkconfig: |
      CONFIG_WENDY_CLOUD_TUNNEL=y
      CONFIG_PARTITION_TABLE_CUSTOM=y
  - else:
    sdkconfig: |
      CONFIG_WENDY_CLOUD_TUNNEL=n
`

const (
	testLiteVersion = "6e2ae72d0b0433a2e65744b6f0821f09b4770541"
	testLiteProfile = "board: c61_board\ncloud_tunnel: true"
)

// stubLiteRepairs makes the prompts answer accept and records them, makes
// the terminal interactive or not, and replaces the wendy-lite fetch and
// the git check. It returns the prompts.
func stubLiteRepairs(t *testing.T, interactive bool, accept func(question string) bool, fetchErr error) *[]string {
	t.Helper()
	oldInteractive, oldConfirm, oldFetch := isInteractiveTerminalFn, confirmFn, fetchMasterConfFn
	t.Cleanup(func() {
		isInteractiveTerminalFn, confirmFn, fetchMasterConfFn = oldInteractive, oldConfirm, oldFetch
	})
	isInteractiveTerminalFn = func() bool { return interactive }
	var prompts []string
	confirmFn = func(question string) bool {
		prompts = append(prompts, question)
		return accept(question)
	}
	fetchMasterConfFn = func(_ context.Context, url, ref string, _ func(string, ...any)) (*litebuild.Model, error) {
		if url != espidftoolchain.WendyLiteGitURL || ref != testLiteVersion {
			t.Errorf("fetch %s %s, want the master configuration of %s", url, ref, testLiteVersion)
		}
		if fetchErr != nil {
			return nil, fetchErr
		}
		return litebuild.Load([]byte(liteMasterConf), "wendy-lite 6e2ae72:master_config.yaml")
	}
	return &prompts
}

func writeLiteProject(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestProviderBuildOffersLiteAppDefaults(t *testing.T) {
	missing := &providers.LiteAppDefaultsError{
		Status:  litebuild.AppDefaultsStatus{State: litebuild.AppDefaultsMissing},
		Version: testLiteVersion, Profile: testLiteProfile, Target: "esp32c61",
	}
	outdated := &providers.LiteAppDefaultsError{
		Status:  litebuild.AppDefaultsStatus{State: litebuild.AppDefaultsOutdated, Version: "1234567", Profile: "board: c61_board"},
		Version: testLiteVersion, Profile: testLiteProfile, Target: "esp32c61",
	}
	otherChip := *missing
	otherChip.Target = "esp32s3"
	fetchFailed := errors.New("fetch failed")
	for _, tc := range []struct {
		name                string
		interactive, accept bool
		opts                runOptions
		buildErr            error
		fetchErr            error
		wantPrompt          string
		wantBuilds          int
		wantFile            bool
		wantErr             error
		wantMessage         string
	}{
		{name: "create", interactive: true, accept: true, buildErr: missing, wantPrompt: "Create sdkconfig.defaults.wendy now?", wantBuilds: 2, wantFile: true},
		{name: "update", interactive: true, accept: true, buildErr: outdated, wantPrompt: "Update sdkconfig.defaults.wendy now?", wantBuilds: 2, wantFile: true},
		{name: "decline", interactive: true, buildErr: missing, wantPrompt: "Create sdkconfig.defaults.wendy now?", wantBuilds: 1, wantErr: ErrUserCancelled},
		{name: "decline update", interactive: true, buildErr: outdated, wantPrompt: "Update sdkconfig.defaults.wendy now?", wantBuilds: 1, wantErr: ErrUserCancelled},
		{name: "noninteractive", buildErr: missing, wantBuilds: 1, wantErr: missing},
		{name: "yes", opts: runOptions{yes: true}, buildErr: missing, wantBuilds: 2, wantFile: true},
		{name: "fetch failed", opts: runOptions{yes: true}, buildErr: missing, fetchErr: fetchFailed, wantBuilds: 1, wantErr: fetchFailed},
		{name: "other chip", opts: runOptions{yes: true}, buildErr: &otherChip, wantBuilds: 1, wantMessage: "the build profile is for the esp32c61, but the device is a esp32s3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			prompts := stubLiteRepairs(t, tc.interactive, func(string) bool { return tc.accept }, tc.fetchErr)
			p := &sequenceProvider{errs: []error{tc.buildErr}}
			_, err := providerBuild(context.Background(), p, models.ExternalDevice{}, dir, "esp-idf", "test", tc.opts)
			if tc.wantErr == nil && tc.wantMessage == "" {
				if err != nil {
					t.Fatalf("providerBuild() = %v", err)
				}
			} else if err == nil || (tc.wantErr != nil && !errors.Is(err, tc.wantErr)) || !strings.Contains(err.Error(), tc.wantMessage) {
				t.Fatalf("providerBuild() = %v, want %v containing %q", err, tc.wantErr, tc.wantMessage)
			}
			if p.builds != tc.wantBuilds {
				t.Errorf("builds = %d, want %d", p.builds, tc.wantBuilds)
			}
			if tc.wantPrompt == "" && len(*prompts) != 0 || tc.wantPrompt != "" && (len(*prompts) != 1 || (*prompts)[0] != tc.wantPrompt) {
				t.Errorf("prompts = %q, want %q", *prompts, tc.wantPrompt)
			}
			data, readErr := os.ReadFile(filepath.Join(dir, litebuild.AppDefaultsFile))
			if !tc.wantFile {
				if readErr == nil {
					t.Fatalf("file written: %s", data)
				}
				return
			}
			if readErr != nil {
				t.Fatal(readErr)
			}
			for _, want := range []string{
				`CONFIG_WENDY_BUILD_PROFILE="{\"board\":\"c61_board\",\"cloud_tunnel\":true}"` + "\n" + `CONFIG_WENDY_VERSION="` + testLiteVersion + `"`,
				"CONFIG_WENDY_CLOUD_TUNNEL=y",
				"from wendy-lite 6e2ae72:master_config.yaml",
			} {
				if !strings.Contains(string(data), want) {
					t.Errorf("file lacks %q:\n%s", want, data)
				}
			}
			if strings.Contains(string(data), "CONFIG_PARTITION_TABLE_CUSTOM=y") {
				t.Errorf("file keeps the partition table line:\n%s", data)
			}
			status, err := litebuild.CheckAppDefaults(dir, testLiteVersion, testLiteProfile)
			if err != nil || status.State != litebuild.AppDefaultsOK {
				t.Errorf("written file does not check: %+v, %v", status, err)
			}
		})
	}
}

func TestProviderBuildOffersSdkconfigDefaultsList(t *testing.T) {
	const cmake = "cmake_minimum_required(VERSION 3.16)\ninclude($ENV{IDF_PATH}/tools/cmake/project.cmake)\nproject(app)\n"
	listErr := &providers.MissingSdkconfigDefaultsListError{}
	for _, tc := range []struct {
		name                string
		interactive, accept bool
		opts                runOptions
		hasDefaults         bool
		wantPrompt          string
		wantBuilds          int
		wantApplied         bool
		wantErr             error
	}{
		{name: "accept", interactive: true, accept: true, hasDefaults: true, wantPrompt: "Add it to CMakeLists.txt now?", wantBuilds: 2, wantApplied: true},
		{name: "accept without sdkconfig.defaults", interactive: true, accept: true, wantPrompt: "Add the line to CMakeLists.txt and create sdkconfig.defaults now?", wantBuilds: 2, wantApplied: true},
		{name: "decline", interactive: true, hasDefaults: true, wantPrompt: "Add it to CMakeLists.txt now?", wantBuilds: 1, wantErr: ErrUserCancelled},
		{name: "noninteractive", hasDefaults: true, wantBuilds: 1, wantErr: listErr},
		{name: "yes", opts: runOptions{yes: true}, wantBuilds: 2, wantApplied: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			files := map[string]string{"CMakeLists.txt": cmake}
			if tc.hasDefaults {
				files["sdkconfig.defaults"] = "CONFIG_MINE=y\n"
			}
			writeLiteProject(t, dir, files)
			prompts := stubLiteRepairs(t, tc.interactive, func(string) bool { return tc.accept }, nil)
			p := &sequenceProvider{errs: []error{listErr}}
			_, err := providerBuild(context.Background(), p, models.ExternalDevice{}, dir, "esp-idf", "test", tc.opts)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("providerBuild() = %v, want %v", err, tc.wantErr)
			}
			if p.builds != tc.wantBuilds {
				t.Errorf("builds = %d, want %d", p.builds, tc.wantBuilds)
			}
			if tc.wantPrompt == "" && len(*prompts) != 0 || tc.wantPrompt != "" && (len(*prompts) != 1 || (*prompts)[0] != tc.wantPrompt) {
				t.Errorf("prompts = %q, want %q", *prompts, tc.wantPrompt)
			}
			data, _ := os.ReadFile(filepath.Join(dir, "CMakeLists.txt"))
			if applied := strings.Contains(string(data), espidftoolchain.SdkconfigDefaultsLine); applied != tc.wantApplied {
				t.Errorf("CMakeLists.txt applied = %v, want %v:\n%s", applied, tc.wantApplied, data)
			}
			_, statErr := os.Stat(filepath.Join(dir, "sdkconfig.defaults"))
			if (statErr == nil) != (tc.hasDefaults || tc.wantApplied) {
				t.Errorf("sdkconfig.defaults exists = %v", statErr == nil)
			}
		})
	}
}

func TestProviderBuildOffersWendyCoreVersion(t *testing.T) {
	const manifest = "dependencies:\n  wendy_core:\n    git: \"https://github.com/wendylabsinc/wendy-lite.git\"\n    path: \"components/wendy_core\"\n"
	versionErr := &providers.WendyCoreVersionError{Version: testLiteVersion}
	for _, tc := range []struct {
		name                string
		interactive, accept bool
		opts                runOptions
		wantBuilds          int
		wantPinned          bool
		wantErr             error
	}{
		{name: "accept", interactive: true, accept: true, wantBuilds: 2, wantPinned: true},
		{name: "decline", interactive: true, wantBuilds: 1, wantErr: ErrUserCancelled},
		{name: "noninteractive", wantBuilds: 1, wantErr: versionErr},
		{name: "yes", opts: runOptions{yes: true}, wantBuilds: 2, wantPinned: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeLiteProject(t, dir, map[string]string{"main/idf_component.yml": manifest, "dependencies.lock": "manifest_hash: old\n"})
			prompts := stubLiteRepairs(t, tc.interactive, func(string) bool { return tc.accept }, nil)
			p := &sequenceProvider{errs: []error{versionErr}}
			_, err := providerBuild(context.Background(), p, models.ExternalDevice{}, dir, "esp-idf", "test", tc.opts)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("providerBuild() = %v, want %v", err, tc.wantErr)
			}
			if p.builds != tc.wantBuilds {
				t.Errorf("builds = %d, want %d", p.builds, tc.wantBuilds)
			}
			if tc.interactive && (len(*prompts) != 1 || (*prompts)[0] != "Update main/idf_component.yml and rebuild dependencies.lock now?") {
				t.Errorf("prompts = %q", *prompts)
			}
			data, _ := os.ReadFile(filepath.Join(dir, "main/idf_component.yml"))
			if pinned := strings.Contains(string(data), `version: "`+testLiteVersion+`"`); pinned != tc.wantPinned {
				t.Errorf("pinned = %v, want %v:\n%s", pinned, tc.wantPinned, data)
			}
			// The pin deletes the lock, so that the build solves again.
			if _, err := os.Stat(filepath.Join(dir, "dependencies.lock")); os.IsNotExist(err) != tc.wantPinned {
				t.Errorf("dependencies.lock deleted = %v, want %v", os.IsNotExist(err), tc.wantPinned)
			}
		})
	}
}

// A new project needs one repair per build, in the order of the checks.
func TestProviderBuildRepairsInSequence(t *testing.T) {
	dir := t.TempDir()
	writeLiteProject(t, dir, map[string]string{
		"CMakeLists.txt":         "include($ENV{IDF_PATH}/tools/cmake/project.cmake)\nproject(app)\n",
		"main/idf_component.yml": "dependencies:\n  wendy_core:\n    git: https://github.com/wendylabsinc/wendy-lite.git\n",
	})
	prompts := stubLiteRepairs(t, true, func(string) bool { return true }, nil)
	p := &sequenceProvider{errs: []error{
		&providers.LiteAppDefaultsError{Status: litebuild.AppDefaultsStatus{State: litebuild.AppDefaultsMissing}, Version: testLiteVersion, Profile: testLiteProfile, Target: "esp32c61"},
		&providers.MissingSdkconfigDefaultsListError{},
		&providers.WendyCoreVersionError{Version: testLiteVersion},
	}}
	if _, err := providerBuild(context.Background(), p, models.ExternalDevice{}, dir, "esp-idf", "test", runOptions{}); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"Create sdkconfig.defaults.wendy now?",
		"Add the line to CMakeLists.txt and create sdkconfig.defaults now?",
		"Update main/idf_component.yml and rebuild dependencies.lock now?",
	}
	if p.builds != 4 || !reflect.DeepEqual(*prompts, want) {
		t.Fatalf("builds = %d, prompts = %q; want 4, %q", p.builds, *prompts, want)
	}
	// The project now passes the three checks.
	if status, err := litebuild.CheckAppDefaults(dir, testLiteVersion, testLiteProfile); err != nil || status.State != litebuild.AppDefaultsOK {
		t.Errorf("sdkconfig.defaults.wendy: %+v, %v", status, err)
	}
	if setup, err := espidftoolchain.PrepareSdkconfigDefaultsList(dir); err != nil || setup != nil {
		t.Errorf("CMakeLists.txt: %+v, %v", setup, err)
	}
	if setup, _, err := espidftoolchain.PrepareWendyCoreVersion(dir, testLiteVersion); err != nil || setup != nil {
		t.Errorf("wendy_core version: %+v, %v", setup, err)
	}
}

// A repair that does not take effect is not offered again.
func TestProviderBuildRepairsOnce(t *testing.T) {
	dir := t.TempDir()
	writeLiteProject(t, dir, map[string]string{"CMakeLists.txt": "include($ENV{IDF_PATH}/tools/cmake/project.cmake)\n", "sdkconfig.defaults": ""})
	prompts := stubLiteRepairs(t, true, func(string) bool { return true }, nil)
	listErr := &providers.MissingSdkconfigDefaultsListError{}
	p := &sequenceProvider{errs: []error{listErr, listErr}}
	_, err := providerBuild(context.Background(), p, models.ExternalDevice{}, dir, "esp-idf", "test", runOptions{})
	if !errors.Is(err, listErr) || p.builds != 2 || len(*prompts) != 1 {
		t.Fatalf("err = %v, builds = %d, prompts = %q; want the error after 2 builds and 1 prompt", err, p.builds, *prompts)
	}
}

func TestProviderBuildOffersFullclean(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake eim uses a shell script")
	}
	for _, tc := range []struct {
		name                string
		interactive, accept bool
		opts                runOptions
		wantBuilds          int
		wantClean           bool
		wantErr             bool
		wantCancelled       bool
	}{
		{name: "accept", interactive: true, accept: true, wantBuilds: 2, wantClean: true},
		{name: "decline", interactive: true, wantBuilds: 1, wantErr: true, wantCancelled: true},
		{name: "noninteractive", wantBuilds: 1, wantErr: true},
		{name: "yes", opts: runOptions{yes: true}, wantBuilds: 2, wantClean: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			// Record the IDF commands.
			script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> idf-commands\n"
			writeLiteProject(t, dir, map[string]string{"eim": script})
			if err := os.Chmod(filepath.Join(dir, "eim"), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			prompts := stubLiteRepairs(t, tc.interactive, func(string) bool { return tc.accept }, nil)

			toolchain := &espidftoolchain.ProjectToolchain{}
			toolchain.UseIDF("v6.1", true)
			changed := &providers.IDFVersionChangedError{Configured: "v6.0.3", Version: "v6.1", Toolchain: toolchain}
			p := &sequenceProvider{errs: []error{changed}}
			_, err := providerBuild(context.Background(), p, models.ExternalDevice{}, dir, "esp-idf", "test", tc.opts)
			if (err != nil) != tc.wantErr || (tc.wantCancelled && !errors.Is(err, ErrUserCancelled)) || (tc.wantErr && !tc.wantCancelled && !errors.Is(err, changed)) {
				t.Fatalf("providerBuild() = %v", err)
			}
			if p.builds != tc.wantBuilds {
				t.Errorf("builds = %d, want %d", p.builds, tc.wantBuilds)
			}
			if tc.interactive && (len(*prompts) != 1 || (*prompts)[0] != "Clean the build folder with idf.py fullclean now?") {
				t.Errorf("prompts = %q", *prompts)
			}
			commands, _ := os.ReadFile(filepath.Join(dir, "idf-commands"))
			if cleaned := strings.Contains(string(commands), "run idf.py --preview fullclean v6.1"); cleaned != tc.wantClean {
				t.Errorf("fullclean ran = %v, want %v; commands: %s", cleaned, tc.wantClean, commands)
			}
		})
	}
}
