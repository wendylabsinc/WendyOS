//go:build darwin || linux || windows

package commands

import (
	"context"
	"encoding/binary"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"google.golang.org/protobuf/proto"

	"github.com/wendylabsinc/wendy/go/internal/cli/litebuild"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/proto/gen/litepb"
)

// TestInstallPickerLiteBuildFirst checks that the configurator entry is the
// first entry of the Microcontrollers tab, before the catalog targets.
func TestInstallPickerLiteBuildFirst(t *testing.T) {
	items := []tui.PickerItem{
		{Name: "Raspberry Pi 5", Section: "WendyOS", SortKey: "0_wendyos_raspberry pi 5", Value: "raspberry-pi-5"},
		// The SortKey of runOSInstall's target entries.
		{Name: "ESP32-C5", Section: "Wendy Lite", SortKey: "1_lite_esp32-c5", Value: "wlite_target_esp32c5"},
		liteBuildPickerItem(),
	}
	m := newInstallPickerModel(items)
	m = updateInstallPicker(m, tea.KeyMsg{Type: tea.KeyShiftTab})
	if got := m.tabs[m.active].label; got != "Microcontrollers" {
		t.Fatalf("active tab = %q, want Microcontrollers", got)
	}
	view := m.View()
	entry, target := strings.Index(view, "ESP32 platform"), strings.Index(view, "ESP32-C5")
	if entry < 0 || target < 0 || entry > target {
		t.Fatalf("the ESP32 platform entry is not first in the tab:\n%s", view)
	}
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(tabbedPickerModel)
	if m.selected == nil || m.selected.Value != liteBuildValue {
		t.Fatalf("selection = %+v, want %q", m.selected, liteBuildValue)
	}
}

// liteBuildStepsConf has the attributes that liteBuildSteps reads: the chip,
// the ESP-IDF version and the --preview flag, each set by a rule.
const liteBuildStepsConf = `
attributes:
  board: {type: string, enum: [seeed_xiao, esp32s31_korvo_1]}
  chip: {type: string, enum: [esp32s3, esp32s31]}
  idf_version: {type: string, enum: ["6.0.3", "6.1"]}
  idf_preview: {type: boolean}
  app_type: {type: string, enum: [native, wasm]}
rules:
  - if: board == "seeed_xiao"
    set: {chip: esp32s3}
  - if: board == "esp32s31_korvo_1"
    set: {chip: esp32s31}
  - if: chip == "esp32s3"
    set: {idf_version: "6.0.3", idf_preview: false}
  - if: chip == "esp32s31"
    set: {idf_version: "6.1", idf_preview: true}
  - if: app_type == "native"
    sdkconfig: CONFIG_NATIVE=y
`

func TestLiteBuildSteps(t *testing.T) {
	m, err := litebuild.Load([]byte(liteBuildStepsConf), "test")
	if err != nil {
		t.Fatal(err)
	}
	solver, err := litebuild.NewSolver(m)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args       []string
		chip       string
		eimVersion string
		build      []string
	}{
		{[]string{"board=seeed_xiao", "app_type=native"}, "esp32s3", "v6.0.3", []string{"build"}},
		{[]string{"board=esp32s31_korvo_1", "app_type=native"}, "esp32s31", "v6.1", []string{"--preview", "build"}},
	} {
		t.Run(tc.chip, func(t *testing.T) {
			inputs, err := m.ParseAssignments(tc.args)
			if err != nil {
				t.Fatal(err)
			}
			sol, err := solver.Evaluate(inputs, nil)
			if err != nil {
				t.Fatal(err)
			}
			b, err := liteBuildSteps(m, &litebuild.Result{Inputs: inputs, Solution: sol})
			if err != nil {
				t.Fatal(err)
			}
			if b.chip != tc.chip || b.eimVersion != tc.eimVersion {
				t.Errorf("chip %s, ESP-IDF %s; want %s, %s", b.chip, b.eimVersion, tc.chip, tc.eimVersion)
			}
			if got := b.idfArgs("build"); !slices.Equal(got, tc.build) {
				t.Errorf("idf.py arguments = %v, want %v", got, tc.build)
			}
			if !slices.Equal(b.configure, tc.args) {
				t.Errorf("configure.py arguments = %v, want %v", b.configure, tc.args)
			}
			if _, err := chipModelForTarget(b.chip); err != nil {
				t.Errorf("the flasher does not know the chip: %v", err)
			}
		})
	}
}

func TestWendyConfPayload(t *testing.T) {
	conf := &litepb.WendyConf{DeviceName: proto.String("lite-1")}
	payload, err := wendyConfPayload(conf)
	if err != nil {
		t.Fatal(err)
	}
	if string(payload[:4]) != "WYC0" {
		t.Fatalf("magic = %q, want WYC0", payload[:4])
	}
	if n := binary.LittleEndian.Uint32(payload[4:8]); int(n) != len(payload)-8 {
		t.Fatalf("length = %d, want %d", n, len(payload)-8)
	}
	var got litepb.WendyConf
	if err := proto.Unmarshal(payload[8:], &got); err != nil {
		t.Fatal(err)
	}
	if got.GetDeviceName() != "lite-1" {
		t.Errorf("device name = %q, want lite-1", got.GetDeviceName())
	}
}

// liteTestCommits are the commits of liteTestRepo.
type liteTestCommits struct {
	old     string // tag v0.9.0, without master configuration
	release string // tag v1.0.0, the latest release
	nightly string // tag nightly
	branch  string // the tip of the branch feature/x
	future  string // the tip of the branch future: master configuration format 2.0
}

// liteTestRepo makes a local wendy-lite repository, which the tests clone
// through a file:// URL, as the flow clones GitHub. Its commits change
// file.txt to "old", "release", "nightly", "branch" and "future"; all but
// the first have a master configuration, the last one in format 2.0. It
// returns the URL and the commits.
func liteTestRepo(t *testing.T) (string, liteTestCommits) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	repo := filepath.Join(t.TempDir(), "wendy-lite")
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false"}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		git("add", name)
	}
	commit := func(content string) string {
		t.Helper()
		write("file.txt", content)
		git("commit", "--quiet", "-m", content)
		return git("rev-parse", "HEAD")
	}
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	git("init", "--quiet")
	// As GitHub: fetches of a commit hash, and without the file contents.
	git("config", "uploadpack.allowAnySHA1InWant", "true")
	git("config", "uploadpack.allowFilter", "true")
	var c liteTestCommits
	c.old = commit("old")
	git("tag", "v0.9.0")
	write(litebuild.MasterConfFile, "attributes: {}\n")
	c.release = commit("release")
	git("tag", "v1.0.0")
	c.nightly = commit("nightly")
	git("tag", "nightly")
	git("checkout", "--quiet", "-b", "feature/x")
	c.branch = commit("branch")
	git("checkout", "--quiet", "-b", "future")
	write(litebuild.MasterConfFile, "format_version: \"2.0\"\nattributes: {}\nnew_section: {}\n")
	c.future = commit("future")
	return "file://" + filepath.ToSlash(repo), c
}

// setLiteBuildRoot makes the build cache a temporary folder.
func setLiteBuildRoot(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "wendy-lite-build")
	orig := liteBuildRoot
	t.Cleanup(func() { liteBuildRoot = orig })
	liteBuildRoot = func() (string, error) { return root, nil }
	return root
}

// TestOpenLiteBuildFolder opens the build folders of the versions in the
// cache, refuses a version without master configuration, and checks the
// cache lock.
func TestOpenLiteBuildFolder(t *testing.T) {
	url, commits := liteTestRepo(t)
	root := setLiteBuildRoot(t)
	ctx := context.Background()

	for _, tc := range []struct {
		name    string
		nightly bool
		version string
		content string
		want    litebuild.Source
	}{
		// A commit that is not the tip of a branch.
		{name: "hash", version: commits.release, content: "release", want: litebuild.Source{Name: commits.release, Version: commits.release, Commit: commits.release}},
		{name: "latest release", content: "release", want: litebuild.Source{Name: "1.0.0", Version: "1.0.0", Commit: commits.release}},
		{name: "release", version: "1.0.0", content: "release", want: litebuild.Source{Name: "1.0.0", Version: "1.0.0", Commit: commits.release}},
		{name: "nightly", nightly: true, content: "nightly", want: litebuild.Source{Name: "nightly", Version: commits.nightly, Commit: commits.nightly}},
		{name: "branch", version: "feature/x", content: "branch", want: litebuild.Source{Name: "feature/x", Version: commits.branch, Commit: commits.branch}},
	} {
		dir, src, release, err := openLiteBuildFolder(ctx, io.Discard, url, tc.nightly, tc.version)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		release()
		if src != tc.want || dir != filepath.Join(root, litebuild.BuildFolderName(tc.want.Commit)) {
			t.Errorf("%s: folder %s of %+v, want the folder of %+v", tc.name, dir, src, tc.want)
		}
		data, err := os.ReadFile(filepath.Join(dir, "file.txt"))
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != tc.content {
			t.Errorf("%s: the folder has %q, want %q", tc.name, data, tc.content)
		}
	}

	// A version without master configuration: an error that names it, and
	// the cache does not change.
	before := liteBuildFolders(t, root)
	if _, _, _, err := openLiteBuildFolder(ctx, io.Discard, url, false, "0.9.0"); err == nil || err.Error() != "Wendy Lite 0.9.0 is not compatible with this version of the CLI. Specify a Wendy Lite version with --version" {
		t.Errorf("version without master configuration: %v", err)
	}
	if after := liteBuildFolders(t, root); !slices.Equal(after, before) {
		t.Errorf("the build folders changed from %q to %q", before, after)
	}

	// A newer master configuration format: update the CLI or choose
	// another version, and the cache does not change.
	newer := "Wendy Lite future (" + commits.future[:7] + ") needs a newer version of the CLI. Update the CLI or specify a Wendy Lite version with --version"
	if _, _, _, err := openLiteBuildFolder(ctx, io.Discard, url, false, "future"); err == nil || err.Error() != newer {
		t.Errorf("newer format: %v", err)
	}
	if after := liteBuildFolders(t, root); !slices.Equal(after, before) {
		t.Errorf("the build folders changed from %q to %q", before, after)
	}

	// While a build holds the cache, another one stops.
	_, _, release, err := openLiteBuildFolder(ctx, io.Discard, url, false, "")
	if err != nil {
		t.Fatalf("the cache stayed locked after the failure: %v", err)
	}
	if _, _, _, err := openLiteBuildFolder(ctx, io.Discard, url, false, ""); err == nil || !strings.Contains(err.Error(), "another Wendy Lite build is running") {
		t.Errorf("with the cache locked: %v", err)
	}
	release()

	// An unknown ref: an error naming it, and the cache is unlocked.
	if _, _, _, err := openLiteBuildFolder(ctx, io.Discard, url, false, "does-not-exist"); err == nil || !strings.Contains(err.Error(), "does-not-exist") {
		t.Errorf("unknown ref: %v, want an error naming it", err)
	}
	_, _, release, err = openLiteBuildFolder(ctx, io.Discard, url, false, "")
	if err != nil {
		t.Fatalf("the cache stayed locked after the failure: %v", err)
	}
	release()
}

// liteBuildFolders returns the names of the folders in the build cache root.
func liteBuildFolders(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	return names
}

func TestLiteBuildConfigureReason(t *testing.T) {
	dir := t.TempDir()
	const profile = `{"board":"seeed_xiao"}`
	write := func(name, content string) {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	reason := func() string {
		t.Helper()
		r, err := liteBuildConfigureReason(dir, profile, "1.2.3", "v6.0.3")
		if err != nil {
			t.Fatal(err)
		}
		return r
	}

	if r := reason(); !strings.Contains(r, "not configured") {
		t.Errorf("without sdkconfig: %q", r)
	}
	write("sdkconfig", litebuild.BuildProfileLine(profile)+"\n"+litebuild.VersionOption+"=\"1.2.3\"\n")
	if r := reason(); !strings.Contains(r, "CMake") {
		t.Errorf("without CMakeCache.txt: %q", r)
	}
	write("build/CMakeCache.txt", "")
	if r := reason(); r != "" {
		t.Errorf("configured: %q", r)
	}
	write("build/project_description.json", `{"git_revision": "v6.0.3-dirty"}`)
	if r := reason(); r != "" {
		t.Errorf("configured with the same ESP-IDF: %q", r)
	}
	write("build/project_description.json", `{"git_revision": "v6.1"}`)
	if r := reason(); !strings.Contains(r, "ESP-IDF v6.1") {
		t.Errorf("configured with another ESP-IDF: %q", r)
	}
}

// TestResetLiteBuildFolder checks that the reset before configure.py deletes
// the ESP-IDF build folder and restores the tracked files, and keeps the
// untracked files.
func TestResetLiteBuildFolder(t *testing.T) {
	url, _ := liteTestRepo(t)
	setLiteBuildRoot(t)
	dir, _, release, err := openLiteBuildFolder(context.Background(), io.Discard, url, false, "feature/x")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	for name, content := range map[string]string{
		"file.txt":             "changed by the build",
		"sdkconfig":            "untracked",
		"build/CMakeCache.txt": "",
	} {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := resetLiteBuildFolder(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "file.txt")); string(data) != "branch" {
		t.Errorf("tracked file = %q, want it restored", data)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "sdkconfig")); string(data) != "untracked" {
		t.Errorf("untracked file = %q, want it kept", data)
	}
	if _, err := os.Stat(filepath.Join(dir, "build")); !os.IsNotExist(err) {
		t.Errorf("the build folder stays: %v", err)
	}
}
