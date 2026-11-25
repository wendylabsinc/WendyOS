package cliupdate

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// brewInfoWendy is the verbatim output of brew info --json=v2 wendy from
// Homebrew 7.0.11, for a wendy keg installed from wendylabsinc/tap.
//
//go:embed testdata/brew-info-wendy.json
var brewInfoWendy []byte

// fakeBrew answers brew info --json=v2 the way Homebrew does: a bare name
// reports the tap its installed keg came from, a tap-qualified name reports
// the tap it names, and --installed ignores names and prints every installed
// formula. Output goes through the same capture buffer as a real subprocess.
type fakeBrew struct {
	installed map[string]string // formula -> tap its keg was installed from
	kegs      int               // installed versions of each formula, kept when cleanup is off
}

// newFakeBrew installs name from tap among a typical set of 276 core formulae.
func newFakeBrew(name, tap string) *fakeBrew {
	b := &fakeBrew{installed: map[string]string{name: tap}, kegs: 1}
	for i := range 276 {
		b.installed[fmt.Sprintf("core-formula-%03d", i)] = "homebrew/core"
	}
	return b
}

func (b *fakeBrew) output(_ context.Context, _ string, args ...string) ([]byte, error) {
	data, err := b.info(args...)
	if err != nil {
		return nil, err
	}
	var out boundedOutput
	_, _ = out.Write(data)
	return out.Bytes(), nil
}

func (b *fakeBrew) info(args ...string) ([]byte, error) {
	if len(args) < 2 || args[0] != "info" || args[1] != "--json=v2" {
		return nil, fmt.Errorf("unexpected brew %v", args)
	}
	var names []string
	if slices.Contains(args, "--installed") {
		names = slices.Sorted(maps.Keys(b.installed))
	} else {
		names = args[2:]
	}
	formulae := []map[string]any{}
	for _, arg := range names {
		name, tap := arg, b.installed[arg]
		if i := strings.LastIndex(arg, "/"); i >= 0 {
			name, tap = arg[i+1:], arg[:i]
		}
		if tap == "" { // brew: No available formula
			return nil, errors.New("exit status 1")
		}
		formulae = append(formulae, b.formula(name, tap))
	}
	return json.MarshalIndent(map[string]any{"formulae": formulae, "casks": []any{}}, "", "  ")
}

func (b *fakeBrew) formula(name, tap string) map[string]any {
	var fixture struct{ Formulae []map[string]any }
	if err := json.Unmarshal(brewInfoWendy, &fixture); err != nil || len(fixture.Formulae) != 1 {
		panic(fmt.Sprintf("bad brew fixture: %v", err))
	}
	f := fixture.Formulae[0]
	f["name"], f["tap"], f["full_name"] = name, tap, tap+"/"+name
	if tap == "homebrew/core" {
		f["full_name"] = name
	}
	keg := f["installed"].([]any)[0].(map[string]any)
	kegs := []any{}
	if _, ok := b.installed[name]; ok {
		for i := range b.kegs {
			k := maps.Clone(keg)
			if i > 0 {
				k["version"] = fmt.Sprintf("2026.01.01-%06d", i)
			}
			kegs = append(kegs, k)
		}
	}
	f["installed"] = kegs
	return f
}

func TestFakeBrewMatchesHomebrew(t *testing.T) {
	brew := newFakeBrew("wendy", "wendylabsinc/tap")
	got, err := brew.info("info", "--json=v2", "wendy")
	var gotJSON, wantJSON any
	if err != nil || json.Unmarshal(got, &gotJSON) != nil || json.Unmarshal(brewInfoWendy, &wantJSON) != nil || !reflect.DeepEqual(gotJSON, wantJSON) {
		t.Fatalf("fake brew differs from Homebrew: err=%v\n%s", err, got)
	}
	// Homebrew 7.0.11 printed 277 formulae (1.4 MB) here on a developer Mac.
	all, err := brew.info("info", "--json=v2", "--installed", "wendy")
	var installed struct{ Formulae []struct{ Name string } }
	if err != nil || json.Unmarshal(all, &installed) != nil || len(installed.Formulae) != 277 || len(all) < 1<<20 {
		t.Fatalf("--installed must list every installed formula: err=%v formulae=%d bytes=%d", err, len(installed.Formulae), len(all))
	}
}

func fakeDependencies(path string) dependencies {
	return dependencies{goos: "darwin", geteuid: func() int { return 0 }, interactive: func() bool { return false }, ownsMSIExecutable: func(string) bool { return false }, executable: func() (string, error) { return path, nil },
		lookPath: func(name string) (string, error) {
			if name == "wendy" {
				return path, nil
			}
			return name, nil
		},
		resolve: func(path string) (string, error) { return path, nil },
		output:  func(context.Context, string, ...string) ([]byte, error) { return nil, errors.New("not installed") },
	}
}

func TestDetectMSIRequiresExactComponentOwnership(t *testing.T) {
	for _, owned := range []bool{false, true} {
		d := fakeDependencies(`C:\Program Files\Wendy\wendy.exe`)
		d.goos = "windows"
		d.ownsMSIExecutable = func(path string) bool {
			if path != `C:\Program Files\Wendy\wendy.exe` {
				t.Fatalf("wrong MSI lookup %q", path)
			}
			return owned
		}
		p, err := detect(context.Background(), d)
		if err != nil || p.Supported != owned {
			t.Fatalf("owned=%v plan=%+v err=%v", owned, p, err)
		}
		if owned && !reflect.DeepEqual(p.Commands[0][:7], []string{"winget", "upgrade", "--id", "WendyLabs.Wendy", "--exact", "--source", "winget"}) {
			t.Fatalf("commands=%v", p.Commands)
		}
	}
}

func TestDetectPackageMethod(t *testing.T) {
	for _, tt := range []struct {
		name, path, goos, method, channel string
		supported                         bool
		command                           string
	}{
		{"brew stable", "/opt/homebrew/Cellar/wendy/2026.09.16/bin/wendy", "darwin", "brew", "stable", true, "wendylabsinc/tap/wendy"},
		{"brew nightly", "/opt/homebrew/Cellar/wendy-nightly/2026.10.06/bin/wendy", "darwin", "brew", "nightly", true, "wendylabsinc/tap/wendy-nightly"},
		{"linuxbrew", "/home/linuxbrew/.linuxbrew/Cellar/wendy/2026.09.16/bin/wendy", "linux", "brew", "stable", true, "wendylabsinc/tap/wendy"},
		{"unregistered winget-like path", `C:\Users\me\AppData\Local\Microsoft\WinGet\Packages\WendyLabs.Wendy_Microsoft.Winget.Source_8wekyb3d8bbwe\wendy.exe`, "windows", "unknown", "stable", false, ""},
		{"arbitrary windows", `C:\tools\wendy.exe`, "windows", "unknown", "stable", false, ""},
		{"manual mac", "/usr/local/bin/wendy", "darwin", "direct", "stable", false, ""},
		{"manual linux", "/home/me/.local/bin/wendy", "linux", "direct", "stable", false, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := fakeDependencies(tt.path)
			d.goos = tt.goos
			if tt.method == "brew" {
				d.output = newFakeBrew(strings.TrimPrefix(tt.command, "wendylabsinc/tap/"), "wendylabsinc/tap").output
			}
			p, err := detect(context.Background(), d)
			if err != nil || p.Method != tt.method || p.Channel != tt.channel || p.Supported != tt.supported {
				t.Fatalf("plan=%+v err=%v", p, err)
			}
			if tt.command != "" && !strings.Contains(strings.Join(p.Commands[len(p.Commands)-1], " "), tt.command) {
				t.Fatalf("wrong commands: %v", p.Commands)
			}
			if p.Method == "brew" && !reflect.DeepEqual(p.Commands[1][1:], []string{"upgrade", tt.command}) {
				t.Fatalf("brew must explicitly upgrade: %v", p.Commands)
			}
			if !p.Supported && len(p.Commands) != 0 {
				t.Fatal("unsupported plan may not execute commands")
			}
		})
	}
}

func TestDetectPackageOwnerGuidance(t *testing.T) {
	for _, tt := range []struct{ method, tool, output, hint string }{
		{"apt", "dpkg-query", "wendy:arm64: /usr/bin/wendy\n", "apt-get install --only-upgrade wendy:arm64"},
		{"rpm", "rpm", "wendy-cli\n", "dnf upgrade wendy-cli"},
		{"pacman", "pacman", "wendy-bin\n", "yay -S wendy-bin"},
	} {
		t.Run(tt.method, func(t *testing.T) {
			d := fakeDependencies("/usr/bin/wendy")
			d.goos = "linux"
			d.output = func(_ context.Context, name string, args ...string) ([]byte, error) {
				if name == tt.tool {
					return []byte(tt.output), nil
				}
				return nil, errors.New("not owned")
			}
			p, err := detect(context.Background(), d)
			if err != nil || p.Method != tt.method || p.Supported != (tt.method != "pacman") || !strings.Contains(p.Guidance, tt.hint) {
				t.Fatalf("plan=%+v err=%v", p, err)
			}
		})
	}
}

func TestLinuxOwnedPackageExecution(t *testing.T) {
	for _, tt := range []struct {
		name, owner, output                           string
		root, interactive, noSudo, noManager, yumOnly bool
		want                                          [][]string
	}{
		{name: "apt root", owner: "dpkg-query", output: "wendy: /usr/bin/wendy", root: true, want: [][]string{{"apt-get", "update"}, {"apt-get", "install", "--only-upgrade", "-y", "wendy"}}},
		{name: "apt no terminal", owner: "dpkg-query", output: "wendy:arm64: /usr/bin/wendy", want: [][]string{{"sudo", "-n", "--", "apt-get", "update"}, {"sudo", "-n", "--", "apt-get", "install", "--only-upgrade", "-y", "wendy:arm64"}}},
		{name: "rpm interactive", owner: "rpm", output: "wendy-cli", interactive: true, want: [][]string{{"sudo", "--", "dnf", "upgrade", "-y", "wendy-cli"}}},
		{name: "yum fallback", owner: "rpm", output: "wendy-cli", root: true, yumOnly: true, want: [][]string{{"yum", "update", "-y", "wendy-cli"}}},
		{name: "missing sudo", owner: "dpkg-query", output: "wendy: /usr/bin/wendy", noSudo: true},
		{name: "missing package manager", owner: "rpm", output: "wendy", noManager: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := fakeDependencies("/usr/bin/wendy")
			d.goos = "linux"
			d.geteuid = func() int {
				if tt.root {
					return 0
				}
				return 1000
			}
			d.interactive = func() bool { return tt.interactive }
			d.lookPath = func(name string) (string, error) {
				if name == "wendy" {
					return "/usr/bin/wendy", nil
				}
				if (tt.noSudo && name == "sudo") || (tt.noManager && (name == "dnf" || name == "yum")) || (tt.yumOnly && name == "dnf") {
					return "", errors.New("not installed")
				}
				return name, nil
			}
			d.output = func(_ context.Context, name string, args ...string) ([]byte, error) {
				if name == tt.owner {
					return []byte(tt.output), nil
				}
				if name == "/usr/bin/wendy" {
					return []byte("wendy version 2026.10.05-134249"), nil
				}
				return nil, errors.New("not owned")
			}
			p, err := detect(context.Background(), d)
			if err != nil || !reflect.DeepEqual(p.Commands, tt.want) || p.Supported != (tt.want != nil) {
				t.Fatalf("plan=%+v err=%v", p, err)
			}
			var calls [][]string
			d.run = func(_ context.Context, _ io.Reader, _, _ io.Writer, name string, args ...string) error {
				calls = append(calls, append([]string{name}, args...))
				return nil
			}
			_, err = apply(context.Background(), d, p, "2026.09.16-025644", "2026.10.05-134249", nil, io.Discard, io.Discard)
			if tt.want == nil {
				if err == nil || len(calls) > 0 {
					t.Fatalf("unsupported plan executed: calls=%v err=%v", calls, err)
				}
			} else if err != nil || !reflect.DeepEqual(calls, tt.want) {
				t.Fatalf("calls=%v err=%v", calls, err)
			}
		})
	}
}

func TestDetectShadowedRunningExecutable(t *testing.T) {
	d := fakeDependencies("/opt/homebrew/Cellar/wendy/old/bin/wendy")
	d.lookPath = func(string) (string, error) { return "/usr/local/bin/wendy", nil }
	p, err := detect(context.Background(), d)
	if err != nil || p.Supported || !strings.Contains(p.Guidance, "differs from wendy on PATH") {
		t.Fatalf("plan=%+v err=%v", p, err)
	}
}

func TestDetectDoesNotSwitchHomebrewTap(t *testing.T) {
	// A tap-qualified query would report wendylabsinc/tap here and pass.
	d := fakeDependencies("/opt/homebrew/Cellar/wendy/2026.09.16/bin/wendy")
	d.output = newFakeBrew("wendy", "another/vendor").output
	p, err := detect(context.Background(), d)
	if err != nil || p.Supported || len(p.Commands) != 0 || !strings.Contains(p.Guidance, "ownership") {
		t.Fatalf("plan=%+v err=%v", p, err)
	}
}

func TestDetectHomebrewFormulaWithManyKegs(t *testing.T) {
	// With cleanup off, every upgrade leaves a keg in the formula's JSON;
	// 200 kegs is about 180 KB.
	brew := newFakeBrew("wendy-nightly", "wendylabsinc/tap")
	brew.kegs = 200
	d := fakeDependencies("/opt/homebrew/Cellar/wendy-nightly/2026.10.06/bin/wendy")
	d.output = brew.output
	p, err := detect(context.Background(), d)
	if err != nil || !p.Supported || p.Channel != "nightly" {
		t.Fatalf("plan=%+v err=%v", p, err)
	}
}

func TestDetectResolvesHomebrewSymlink(t *testing.T) {
	d := fakeDependencies("/opt/homebrew/bin/wendy")
	d.resolve = func(path string) (string, error) {
		if path == "/opt/homebrew/bin/wendy" {
			return "/opt/homebrew/Cellar/wendy/2026.09.16/bin/wendy", nil
		}
		return path, nil
	}
	d.output = newFakeBrew("wendy", "wendylabsinc/tap").output
	p, err := detect(context.Background(), d)
	if err != nil || !p.Supported || p.Method != "brew" {
		t.Fatalf("plan=%+v err=%v", p, err)
	}
}

func TestApplyVerifiesActualVersion(t *testing.T) {
	for _, tt := range []struct{ name, output, target, code string }{
		{"updated", "wendy version 2026.10.05-134249\n", "2026.10.05-134249", ""},
		{"newer", "wendy version 2026.10.06-134249\n", "2026.10.05-134249", ""},
		{"no op", "wendy version 2026.09.16-025644\n", "2026.10.05-134249", "update_version_unchanged"},
		{"partial", "wendy version 2026.09.28-142251\n", "2026.10.05-134249", "update_version_unchanged"},
		{"invalid", "unrelated tool 2026.10.05-134249", "2026.10.05-134249", "update_verification_failed"},
		{"dev shadow", "wendy version dev\n", "2026.10.05-134249", "update_verification_failed"},
		{"versioned dev shadow", "wendy version 2026.10.06-134249-dev\n", "2026.10.05-134249", "update_verification_failed"},
		{"invalid date shadow", "wendy version 2026.99.99-134249\n", "2026.10.05-134249", "update_verification_failed"},
		{"unknown branch shadow", "wendy version 2026.10.06-134249-feature\n", "2026.10.05-134249", "update_verification_failed"},
		{"nightly advanced", "wendy version 2026.10.06-134249-nightly\n", "", ""},
		{"nightly no op", "wendy version 2026.09.16-025644\n", "", "update_version_unchanged"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := fakeDependencies("/opt/homebrew/bin/wendy")
			var calls []string
			d.run = func(_ context.Context, _ io.Reader, _, _ io.Writer, name string, args ...string) error {
				calls = append(calls, name+" "+strings.Join(args, " "))
				return nil
			}
			d.output = func(_ context.Context, name string, args ...string) ([]byte, error) {
				calls = append(calls, name+" "+strings.Join(args, " "))
				return []byte(tt.output), nil
			}
			p := Plan{Supported: true, Commands: [][]string{{"brew", "update"}, {"brew", "upgrade", "wendylabsinc/tap/wendy"}}}
			if tt.target == "" {
				p.Channel = "nightly"
			}
			r, err := apply(context.Background(), d, p, "2026.09.16-025644", tt.target, nil, io.Discard, io.Discard)
			if (err != nil) != (tt.code != "") {
				t.Fatalf("result=%+v err=%v", r, err)
			}
			if err != nil && ErrorCode(err) != tt.code {
				t.Fatalf("code=%s err=%v", ErrorCode(err), err)
			}
			if len(calls) != 3 || calls[2] != "/opt/homebrew/bin/wendy --version" {
				t.Fatalf("calls=%v", calls)
			}
		})
	}
}

func TestApplyStopsOnUpdaterFailure(t *testing.T) {
	d := fakeDependencies("/opt/homebrew/bin/wendy")
	sentinel := errors.New("installer download failed")
	calls := 0
	d.run = func(context.Context, io.Reader, io.Writer, io.Writer, string, ...string) error {
		calls++
		return sentinel
	}
	d.output = func(context.Context, string, ...string) ([]byte, error) {
		t.Fatal("must not verify after failed update")
		return nil, nil
	}
	_, err := apply(context.Background(), d, Plan{Supported: true, Commands: [][]string{{"brew", "update"}, {"brew", "upgrade", "wendy"}}}, "old", "new", nil, io.Discard, io.Discard)
	if calls != 1 || !errors.Is(err, sentinel) || ErrorCode(err) != "update_process_failed" {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}

func TestApplyPreservesSubprocessCancellation(t *testing.T) {
	for _, stage := range []string{"installer", "verification"} {
		t.Run(stage, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			d := fakeDependencies("/opt/homebrew/bin/wendy")
			// CommandContext can return an exit error without wrapping ctx.Err().
			processErr := errors.New("signal: killed")
			var calls []string
			d.run = func(context.Context, io.Reader, io.Writer, io.Writer, string, ...string) error {
				calls = append(calls, "install")
				if stage == "installer" {
					cancel()
					return processErr
				}
				return nil
			}
			d.output = func(context.Context, string, ...string) ([]byte, error) {
				calls = append(calls, "verify")
				cancel()
				return nil, processErr
			}
			p := Plan{Supported: true, Commands: [][]string{{"brew", "update"}, {"brew", "upgrade", "wendy"}}}
			_, err := apply(ctx, d, p, "old", "new", nil, io.Discard, io.Discard)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("subprocess cancellation became an updater failure: %v", err)
			}
			want := []string{"install"}
			if stage == "verification" {
				want = []string{"install", "install", "verify"}
			}
			if !reflect.DeepEqual(calls, want) {
				t.Fatalf("calls=%v, want %v", calls, want)
			}
		})
	}
}

func TestApplyUnsupportedCannotRun(t *testing.T) {
	d := fakeDependencies("/usr/bin/wendy")
	d.run = func(context.Context, io.Reader, io.Writer, io.Writer, string, ...string) error {
		t.Fatal("unsupported updater executed")
		return nil
	}
	_, err := apply(context.Background(), d, Plan{Guidance: "Use apt."}, "old", "new", nil, io.Discard, io.Discard)
	if ErrorCode(err) != "update_method_unsupported" {
		t.Fatalf("err=%v", err)
	}
}
