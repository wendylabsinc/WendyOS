package commands

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/cli/vm"
)

func stubVMCreatePrompts(t *testing.T) *[]string {
	t.Helper()
	originalInteractive, originalName, originalProfile := isInteractiveTerminalFn, promptVMCreateNameFn, pickSimulatorProfileFn
	originalJSON, originalYes := jsonOutput, vmAssumeYes
	t.Cleanup(func() {
		isInteractiveTerminalFn, promptVMCreateNameFn, pickSimulatorProfileFn = originalInteractive, originalName, originalProfile
		jsonOutput, vmAssumeYes = originalJSON, originalYes
	})
	jsonOutput, vmAssumeYes = false, false
	isInteractiveTerminalFn = func() bool { return true }
	var calls []string
	promptVMCreateNameFn = func() (string, error) {
		calls = append(calls, "name")
		return "prompted-vm", nil
	}
	pickSimulatorProfileFn = func() (string, error) {
		calls = append(calls, "profile")
		return "go2", nil
	}
	return &calls
}

func TestResolveVMCreateInputsPromptsOnlyForOmittedValues(t *testing.T) {
	for _, tt := range []struct {
		name        string
		args        []string
		profile     string
		profileSet  bool
		wantName    string
		wantProfile string
		wantCalls   string
	}{
		{name: "both omitted", profile: "generic", wantName: "prompted-vm", wantProfile: "go2", wantCalls: "name,profile"},
		{name: "explicit name", args: []string{"named-vm"}, profile: "generic", wantName: "named-vm", wantProfile: "go2", wantCalls: "profile"},
		{name: "explicit generic", profile: "generic", profileSet: true, wantName: "prompted-vm", wantProfile: "generic", wantCalls: "name"},
		{name: "explicit go2", profile: "go2", profileSet: true, wantName: "prompted-vm", wantProfile: "go2", wantCalls: "name"},
		{name: "explicit so101", args: []string{"arm-sim"}, profile: "so101", profileSet: true, wantName: "arm-sim", wantProfile: "so101"},
		{name: "explicit g1", profile: "g1", profileSet: true, wantName: "prompted-vm", wantProfile: "g1", wantCalls: "name"},
		{name: "both explicit", args: []string{"named-vm"}, profile: "g1", profileSet: true, wantName: "named-vm", wantProfile: "g1"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls := stubVMCreatePrompts(t)
			name, profile, err := resolveVMCreateInputs(tt.args, tt.profile, tt.profileSet)
			if err != nil || name != tt.wantName || profile != tt.wantProfile {
				t.Fatalf("got (%q, %q, %v), want (%q, %q, nil)", name, profile, err, tt.wantName, tt.wantProfile)
			}
			if got := strings.Join(*calls, ","); got != tt.wantCalls {
				t.Fatalf("prompt order = %q, want %q", got, tt.wantCalls)
			}
		})
	}
}

func TestResolveVMCreateInputsKeepsNoninteractiveUsageScriptable(t *testing.T) {
	for _, mode := range []string{"no terminal", "json", "yes"} {
		t.Run(mode, func(t *testing.T) {
			calls := stubVMCreatePrompts(t)
			switch mode {
			case "no terminal":
				isInteractiveTerminalFn = func() bool { return false }
			case "json":
				jsonOutput = true
			case "yes":
				vmAssumeYes = true
			}
			name, profile, err := resolveVMCreateInputs([]string{"named-vm"}, "generic", false)
			if err != nil || name != "named-vm" || profile != "generic" {
				t.Fatalf("default profile: got (%q, %q, %v)", name, profile, err)
			}
			name, profile, err = resolveVMCreateInputs([]string{"named-vm"}, "g1", true)
			if err != nil || name != "named-vm" || profile != "g1" {
				t.Fatalf("explicit profile: got (%q, %q, %v)", name, profile, err)
			}
			_, _, err = resolveVMCreateInputs(nil, "generic", false)
			if err == nil || !strings.Contains(err.Error(), "wendy vm create <name>") {
				t.Fatalf("missing-name error = %v, want argument guidance", err)
			}
			if len(*calls) != 0 {
				t.Fatalf("noninteractive input unexpectedly prompted: %v", *calls)
			}
		})
	}
}

func TestResolveVMCreateInputsRejectsInvalidValuesBeforeFurtherPrompts(t *testing.T) {
	for _, tt := range []struct {
		name      string
		args      []string
		profile   string
		prompted  string
		wantCalls string
	}{
		{name: "invalid explicit profile", profile: "unknown", prompted: "prompted-vm"},
		{name: "empty explicit profile", prompted: "prompted-vm"},
		{name: "invalid explicit name", args: []string{"../outside"}, profile: "generic", prompted: "prompted-vm"},
		{name: "invalid prompted name", profile: "generic", prompted: "Bad Name", wantCalls: "name"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls := stubVMCreatePrompts(t)
			promptVMCreateNameFn = func() (string, error) {
				*calls = append(*calls, "name")
				return tt.prompted, nil
			}
			_, _, err := resolveVMCreateInputs(tt.args, tt.profile, tt.profile != "generic")
			if err == nil {
				t.Fatal("invalid input was accepted")
			}
			if got := strings.Join(*calls, ","); got != tt.wantCalls {
				t.Fatalf("prompt order = %q, want %q", got, tt.wantCalls)
			}
		})
	}
}

func TestVMCreateCommandStopsOnPromptCancellation(t *testing.T) {
	for _, tt := range []struct {
		name      string
		args      []string
		nameErr   error
		wantCalls string
	}{
		{name: "name cancelled", nameErr: tui.ErrCancelled, wantCalls: "name"},
		{name: "profile cancelled", wantCalls: "name,profile"},
		{name: "explicit name only", args: []string{"named-vm"}, wantCalls: "profile"},
		{name: "explicit generic only", args: []string{"--profile", "generic"}, nameErr: tui.ErrCancelled, wantCalls: "name"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls := stubVMCreatePrompts(t)
			promptVMCreateNameFn = func() (string, error) {
				*calls = append(*calls, "name")
				return "prompted-vm", tt.nameErr
			}
			pickSimulatorProfileFn = func() (string, error) {
				*calls = append(*calls, "profile")
				return "", ErrUserCancelled
			}
			cmd := newVMCreateCmd()
			// A command that falls through after cancellation must fail to open
			// this image, rather than download or create anything on the host.
			cmd.SetArgs(append(append([]string{}, tt.args...), "--image", t.TempDir()+"/missing.wic"))
			cmd.SetOut(&strings.Builder{})
			cmd.SetErr(&strings.Builder{})
			if err := cmd.Execute(); !errors.Is(err, ErrUserCancelled) {
				t.Fatalf("cancelled create returned %v, want ErrUserCancelled", err)
			}
			if got := strings.Join(*calls, ","); got != tt.wantCalls {
				t.Fatalf("prompt order = %q, want %q", got, tt.wantCalls)
			}
		})
	}
}

func TestVMCreateCommandHonorsExplicitGenericProfile(t *testing.T) {
	calls := stubVMCreatePrompts(t)
	cmd := newVMCreateCmd()
	// Conflicting image flags stop creation before accessing the VM store.
	cmd.SetArgs([]string{"--profile", "generic", "--image", "unused.wic", "--version", "1.2.3"})
	cmd.SetOut(&strings.Builder{})
	cmd.SetErr(&strings.Builder{})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "--image cannot be combined") {
		t.Fatalf("create returned %v, want image flag conflict", err)
	}
	if got := strings.Join(*calls, ","); got != "name" {
		t.Fatalf("prompt order = %q, want only name for explicit --profile generic", got)
	}
}

func stubDefaultGuestArch(t *testing.T, arch vm.Arch) {
	t.Helper()
	saved := defaultGuestArchFn
	t.Cleanup(func() { defaultGuestArchFn = saved })
	defaultGuestArchFn = func() vm.Arch { return arch }
}

func TestResolveCreateArch(t *testing.T) {
	for _, def := range []vm.Arch{vm.ArchAMD64, vm.ArchARM64} {
		stubDefaultGuestArch(t, def)
		if got, err := resolveCreateArch("", "", "linux"); err != nil || got != def {
			t.Errorf("default %s, no flag or image: %q, %v", def, got, err)
		}
	}
	stubDefaultGuestArch(t, vm.ArchAMD64)
	for _, tc := range []struct {
		flag, image, goos string
		want              vm.Arch
	}{
		{"", "./x.wic", "linux", vm.ArchARM64},
		{"x86_64", "./x.wic", "linux", vm.ArchAMD64},
		{"aarch64", "", "linux", vm.ArchARM64},
		{"arm64", "", "windows", vm.ArchARM64},
	} {
		if got, err := resolveCreateArch(tc.flag, tc.image, tc.goos); err != nil || got != tc.want {
			t.Errorf("resolveCreateArch(%q, %q, %q) = %q, %v; want %q", tc.flag, tc.image, tc.goos, got, err, tc.want)
		}
	}
	if _, err := resolveCreateArch("riscv64", "", "linux"); err == nil || !strings.Contains(err.Error(), "arm64") || !strings.Contains(err.Error(), "amd64") {
		t.Errorf("riscv64: %v, want an error naming arm64 and amd64", err)
	}
	const windows = "--arch amd64 is not available on native Windows: the simulator there runs the ARM64 guest"
	if _, err := resolveCreateArch("amd64", "", "windows"); err == nil || err.Error() != windows {
		t.Errorf("amd64 on windows: %v, want %q", err, windows)
	}
}

func TestVMCreateHasAnArchFlag(t *testing.T) {
	flag := newVMCreateCmd().Flags().Lookup("arch")
	if flag == nil || flag.DefValue != "" {
		t.Fatalf("vm create --arch = %+v, want a flag with an empty default", flag)
	}
	const usage = "Guest architecture: arm64 or amd64 (default: this computer's default; a local --image is treated as arm64)"
	if flag.Usage != usage {
		t.Errorf("--arch help = %q, want %q", flag.Usage, usage)
	}
}

func TestVMCreateRefusesArchForWendyLite(t *testing.T) {
	stubVMCreatePrompts(t)
	configDir := t.TempDir()
	t.Setenv("WENDY_CONFIG_DIR", configDir)
	cmd := newVMCreateCmd()
	cmd.SetArgs([]string{"lite1", "--profile", "wendy-lite", "--arch", "amd64"})
	cmd.SetOut(&strings.Builder{})
	cmd.SetErr(&strings.Builder{})
	const want = "--arch applies to WendyOS simulators; wendy-lite runs ESP32-C6 firmware"
	if err := cmd.Execute(); err == nil || err.Error() != want {
		t.Fatalf("create = %v, want %q", err, want)
	}
	if _, err := os.Stat(filepath.Join(configDir, "vms")); !os.IsNotExist(err) {
		t.Errorf("a refused Wendy Lite create touched the VM store: %v", err)
	}
}

func TestVMCreateRecordsTheArchitecture(t *testing.T) {
	stubDefaultGuestArch(t, vm.ArchAMD64)
	configDir := t.TempDir()
	t.Setenv("WENDY_CONFIG_DIR", configDir)
	saved := vmDiskAvailBytes
	t.Cleanup(func() { vmDiskAvailBytes = saved })
	vmDiskAvailBytes = func(string) (int64, bool) { return 1 << 50, true }
	image := filepath.Join(t.TempDir(), "source.wic")
	if err := os.WriteFile(image, []byte("image data"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := &vm.Store{Root: filepath.Join(configDir, "vms")}
	const hint = "Treating the local image as ARM64; pass --arch amd64 for an x86-64 image."

	var out strings.Builder
	err := createVMContext(context.Background(), &out, "x86", image, "", 1, false, 0, "amd64")
	if runtime.GOOS == "windows" {
		// Native Windows runs only the ARM64 guest; TestResolveCreateArch pins the message.
		if err == nil {
			t.Error("--arch amd64 created a VM on native Windows")
		}
		if _, statErr := os.Stat(store.MetaPath("x86")); !os.IsNotExist(statErr) {
			t.Errorf("a refused --arch amd64 create wrote metadata: %v", statErr)
		}
	} else {
		if err != nil {
			t.Fatal(err)
		}
		if m, _ := store.ReadMeta("x86"); m.Arch != vm.ArchAMD64 {
			t.Errorf("--arch amd64 recorded %q", m.Arch)
		}
		if _, err := os.Stat(store.VarsPath("x86")); !os.IsNotExist(err) {
			t.Errorf("x86-64 VM has a variable store at create: %v", err)
		}
		if strings.Contains(out.String(), hint) {
			t.Errorf("explicit --arch printed the local-image hint: %q", out.String())
		}
	}

	out.Reset()
	if err := createVMContext(context.Background(), &out, "arm", image, "", 1, false, 0, ""); err != nil {
		t.Fatal(err)
	}
	if m, _ := store.ReadMeta("arm"); m.Arch != vm.ArchARM64 {
		t.Errorf("a local image without --arch recorded %q, want arm64", m.Arch)
	}
	if _, err := os.Stat(store.VarsPath("arm")); err != nil {
		t.Errorf("ARM64 VM has no variable store: %v", err)
	}
	if !strings.Contains(out.String(), hint) {
		t.Errorf("output %q does not carry the local-image hint", out.String())
	}
}

func TestPublishedImageUsesTheBoardForTheArchitecture(t *testing.T) {
	isolateVMImageCache(t)
	previous := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = previous })
	master := `{"devices":{"vm-arm64":{"latest":"0.21.0","manifest_path":"manifests/vm-arm64.json"}}}`
	var requested []string
	http.DefaultTransport = vmImageTestTransport(func(req *http.Request) (*http.Response, error) {
		requested = append(requested, req.URL.Path)
		body := `{"versions":{}}`
		if strings.HasSuffix(req.URL.Path, "/master.json") {
			body = master
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}, Request: req}, nil
	})

	if _, _, _, err := fetchPublishedVMImageContext(context.Background(), vm.ArchAMD64, "", false, 0); err == nil || !strings.Contains(err.Error(), "vm-x86-64") {
		t.Fatalf("manifest without vm-x86-64: %v, want an error naming vm-x86-64", err)
	}

	master = `{"devices":{"vm-arm64":{"latest":"0.21.0","manifest_path":"manifests/vm-arm64.json"},` +
		`"vm-x86-64":{"latest":"0.20.9","manifest_path":"manifests/vm-x86-64.json"}}}`
	requested = nil
	_, _, _, err := fetchPublishedVMImageContext(context.Background(), vm.ArchAMD64, "", false, 0)
	if err == nil || !strings.Contains(err.Error(), "0.20.9") {
		t.Fatalf("amd64 resolution: %v, want vm-x86-64's latest 0.20.9", err)
	}
	if len(requested) != 2 || !strings.HasSuffix(requested[1], "/manifests/vm-x86-64.json") {
		t.Errorf("fetched %v, want the vm-x86-64 device manifest", requested)
	}
}
