//go:build darwin || linux || windows

package commands

import (
	"context"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	entry, target := strings.Index(view, "ESP32 platform via new configurator"), strings.Index(view, "ESP32-C5")
	if entry < 0 || target < 0 || entry > target {
		t.Fatalf("the configurator entry is not first in the tab:\n%s", view)
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

func TestLiteBuildRef(t *testing.T) {
	for _, tc := range []struct {
		nightly bool
		version string
		want    string
	}{
		{nightly: true, want: "nightly"},
		{version: "v1.2.3", want: "v1.2.3"},
		{want: litebuild.DefaultRef},
	} {
		if got := liteBuildRef(tc.nightly, tc.version); got != tc.want {
			t.Errorf("liteBuildRef(%v, %q) = %q, want %q", tc.nightly, tc.version, got, tc.want)
		}
	}
}

func TestCheckGit(t *testing.T) {
	orig := liteBuildLookPath
	t.Cleanup(func() { liteBuildLookPath = orig })

	liteBuildLookPath = func(string) (string, error) { return "", exec.ErrNotFound }
	err := checkGit()
	if err == nil || !strings.Contains(err.Error(), "git is not installed") || !strings.Contains(err.Error(), gitInstallHint(runtime.GOOS)) {
		t.Errorf("checkGit without git = %v, want the install hint", err)
	}
	liteBuildLookPath = func(string) (string, error) { return "/usr/bin/git", nil }
	if err := checkGit(); err != nil {
		t.Errorf("checkGit with git = %v", err)
	}
}

// TestCloneLiteSources clones a branch and a tag of a local repository
// through a file:// URL, as the flow clones GitHub.
func TestCloneLiteSources(t *testing.T) {
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
	commit := func(content string) string {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, "file.txt"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		git("add", "file.txt")
		git("commit", "--quiet", "-m", content)
		return git("rev-parse", "--short", "HEAD")
	}
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	git("init", "--quiet")
	tagged := commit("tagged")
	git("tag", "t1")
	git("checkout", "--quiet", "-b", "feature/x")
	branch := commit("branch")
	url := "file://" + filepath.ToSlash(repo)

	for _, tc := range []struct{ ref, content, commit string }{
		{"feature/x", "branch", branch},
		{"t1", "tagged", tagged},
	} {
		dir, got, err := cloneLiteSources(context.Background(), url, tc.ref)
		if err != nil {
			t.Fatalf("%s: %v", tc.ref, err)
		}
		t.Cleanup(func() { os.RemoveAll(dir) })
		if got != tc.commit {
			t.Errorf("%s: commit = %s, want %s", tc.ref, got, tc.commit)
		}
		data, err := os.ReadFile(filepath.Join(dir, "file.txt"))
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != tc.content {
			t.Errorf("%s: clone has %q, want %q", tc.ref, data, tc.content)
		}
	}

	// An unknown ref: an error, and no temporary folder left behind.
	before, _ := filepath.Glob(filepath.Join(os.TempDir(), "wendy-lite-build-*"))
	_, _, err := cloneLiteSources(context.Background(), url, "does-not-exist")
	if err == nil || !strings.Contains(err.Error(), "does-not-exist") {
		t.Errorf("clone of an unknown ref = %v, want an error naming it", err)
	}
	after, _ := filepath.Glob(filepath.Join(os.TempDir(), "wendy-lite-build-*"))
	if len(after) != len(before) {
		t.Errorf("temporary folders: %d before, %d after the failed clone", len(before), len(after))
	}
}
