package cliupdate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/env"
	"github.com/wendylabsinc/wendy/go/internal/shared/version"
	"golang.org/x/term"
)

// Plan describes the installation actually running, rather than assuming the
// host's preferred installer owns it. Guidance-only plans never execute commands.
type Plan struct {
	Method     string     `json:"method"`
	Channel    string     `json:"channel"`
	Executable string     `json:"executable"`
	Supported  bool       `json:"supported"`
	Commands   [][]string `json:"commands,omitempty"`
	Guidance   string     `json:"guidance,omitempty"`
}

type Result struct {
	Version    string `json:"version"`
	Executable string `json:"executable"`
}

// ErrorCode returns a bounded product code, suitable for structured telemetry.
func ErrorCode(err error) string {
	var e *updateError
	if errors.As(err, &e) {
		return e.code
	}
	return "update_failed"
}

type updateError struct {
	code string
	err  error
}

func (e *updateError) Error() string { return e.err.Error() }
func (e *updateError) Unwrap() error { return e.err }
func failure(code, format string, args ...any) error {
	return &updateError{code, fmt.Errorf(format, args...)}
}

type dependencies struct {
	goos              string
	geteuid           func() int
	interactive       func() bool
	ownsMSIExecutable func(string) bool
	executable        func() (string, error)
	lookPath          func(string) (string, error)
	resolve           func(string) (string, error)
	output            func(context.Context, string, ...string) ([]byte, error)
	run               func(context.Context, io.Reader, io.Writer, io.Writer, string, ...string) error
}

func defaults() dependencies {
	return dependencies{goos: runtime.GOOS, geteuid: os.Geteuid, ownsMSIExecutable: ownsMSIExecutable, interactive: func() bool { return !env.IsCI() && term.IsTerminal(int(os.Stdin.Fd())) }, executable: os.Executable, lookPath: exec.LookPath, resolve: filepath.EvalSymlinks,
		output: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, name, args...)
			var out boundedOutput
			cmd.Stdout, cmd.Stderr = &out, io.Discard
			cmd.WaitDelay = 250 * time.Millisecond
			err := cmd.Run()
			return out.Bytes(), err
		},
		run: func(ctx context.Context, in io.Reader, out, errout io.Writer, name string, args ...string) error {
			cmd := exec.CommandContext(ctx, name, args...)
			cmd.Stdin, cmd.Stdout, cmd.Stderr = in, out, errout
			cmd.WaitDelay = time.Second
			return cmd.Run()
		},
	}
}

// maxOutput must hold one formula's brew info JSON: about 5 KB plus 1 KB per
// installed keg, and kegs pile up when Homebrew's cleanup is turned off.
const maxOutput = 4 << 20

type boundedOutput struct{ bytes.Buffer }

func (b *boundedOutput) Write(p []byte) (int, error) {
	n := len(p)
	if remaining := maxOutput - b.Len(); remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		_, _ = b.Buffer.Write(p)
	}
	return n, nil
}

func Detect(ctx context.Context) (Plan, error) { return detect(ctx, defaults()) }

func detect(ctx context.Context, d dependencies) (Plan, error) {
	p := Plan{Method: "unknown", Channel: "stable"}
	path, err := d.executable()
	if err != nil {
		return p, failure("update_installation_unknown", "cannot locate the running Wendy executable: %w", err)
	}
	real, err := d.resolve(path)
	if err != nil {
		return p, failure("update_installation_unknown", "cannot resolve the running Wendy executable: %w", err)
	}
	p.Executable = real
	pathCLI, pathErr := d.lookPath("wendy")
	pathReal, resolveErr := d.resolve(pathCLI)
	if pathErr != nil || resolveErr != nil || !samePath(real, pathReal, d.goos) {
		p.Guidance = fmt.Sprintf("The running CLI (%s) differs from wendy on PATH (%s). Choose the installation you want, fix PATH, then run wendy update from that installation.", real, pathCLI)
		return p, nil
	}
	normalized := strings.ReplaceAll(real, "\\", "/")
	if i := strings.LastIndex(normalized, "/Cellar/"); i >= 0 {
		parts := strings.Split(normalized[i+len("/Cellar/"):], "/")
		if len(parts) >= 4 && (parts[0] == "wendy" || parts[0] == "wendy-nightly") {
			p.Method = "brew"
			if parts[0] == "wendy-nightly" {
				p.Channel = "nightly"
			}
			brew := normalized[:i] + "/bin/brew"
			if _, err := d.lookPath(brew); err != nil {
				p.Guidance = "Restore the Homebrew installation that owns this executable, then run wendy update."
				return p, nil
			}
			// A bare name reports the tap the installed keg came from. --installed
			// ignores the name and lists every installed formula, and a
			// tap-qualified name always reports the tap it names.
			info, infoErr := d.output(ctx, brew, "info", "--json=v2", parts[0])
			var metadata struct {
				Formulae []struct{ Name, Tap string } `json:"formulae"`
			}
			if infoErr != nil || json.Unmarshal(info, &metadata) != nil || len(metadata.Formulae) != 1 || metadata.Formulae[0].Name != parts[0] || metadata.Formulae[0].Tap != "wendylabsinc/tap" {
				p.Guidance = "The Homebrew formula's Wendy tap ownership could not be verified. Inspect brew info " + parts[0] + " and upgrade it through its existing tap."
				return p, nil
			}
			p.Supported = true
			p.Commands = [][]string{{brew, "update"}, {brew, "upgrade", "wendylabsinc/tap/" + parts[0]}}
			p.Guidance = "If Homebrew reports an untrusted tap, run brew trust wendylabsinc/tap, then retry."
			return p, nil
		}
	}
	if d.goos == "windows" {
		// Wendy's published WinGet package is an MSI. Verify its registered
		// executable component; an arbitrary path must not create a second copy.
		if d.ownsMSIExecutable(real) {
			p.Method = "winget"
			if winget, err := d.lookPath("winget"); err == nil {
				p.Supported = true
				p.Commands = [][]string{{winget, "upgrade", "--id", "WendyLabs.Wendy", "--exact", "--source", "winget", "--accept-source-agreements", "--accept-package-agreements"}}
				return p, nil
			}
		}
		p.Guidance = "Update using the installer that owns this executable. For a WinGet installation: winget upgrade --id WendyLabs.Wendy --exact --source winget. Then verify wendy --version and check Get-Command wendy -All for older copies."
		return p, nil
	}
	if d.goos == "linux" {
		for _, owner := range []struct {
			tool, method string
			args         []string
		}{
			{"dpkg-query", "apt", []string{"-S", real}},
			{"rpm", "rpm", []string{"-qf", "--qf", "%{NAME}\n", real}},
			{"pacman", "pacman", []string{"-Qqo", real}},
		} {
			if ctx.Err() != nil {
				return p, ctx.Err()
			}
			tool, err := d.lookPath(owner.tool)
			if err != nil {
				continue
			}
			out, err := d.output(ctx, tool, owner.args...)
			if err != nil {
				continue
			}
			pkg := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
			if owner.method == "apt" {
				pkg = strings.SplitN(pkg, ": ", 2)[0]
			}
			if !packageName.MatchString(pkg) {
				continue
			}
			p.Method = owner.method
			switch owner.method {
			case "apt":
				p.Guidance = "This executable belongs to the apt package " + pkg + ". Run: sudo apt-get update && sudo apt-get install --only-upgrade " + pkg
				if apt, err := d.lookPath("apt-get"); err == nil {
					p.Commands = [][]string{{apt, "update"}, {apt, "install", "--only-upgrade", "-y", pkg}}
				}
			case "rpm":
				p.Guidance = "This executable belongs to the rpm package " + pkg + ". Run: sudo dnf upgrade " + pkg + " (or sudo yum update " + pkg + " on older systems)."
				if dnf, err := d.lookPath("dnf"); err == nil {
					p.Commands = [][]string{{dnf, "upgrade", "-y", pkg}}
				} else if yum, err := d.lookPath("yum"); err == nil {
					p.Commands = [][]string{{yum, "update", "-y", pkg}}
				}
			case "pacman":
				p.Guidance = "This executable belongs to the pacman package " + pkg + ". Upgrade it with your configured package source or AUR helper, for example: yay -S " + pkg
			}
			p.Guidance = strings.TrimSuffix(p.Guidance, ".") + ". Then run wendy update --check to verify. A manually downloaded package may require a newer package from https://github.com/wendylabsinc/wendy-agent/releases."
			if len(p.Commands) > 0 {
				if d.geteuid() != 0 {
					sudo, err := d.lookPath("sudo")
					if err != nil {
						p.Commands = nil
						return p, nil
					}
					for i, command := range p.Commands {
						prefix := []string{sudo}
						if !d.interactive() {
							prefix = append(prefix, "-n")
						}
						p.Commands[i] = append(append(prefix, "--"), command...)
					}
				}
				p.Supported = true
			}
			return p, nil
		}
	}
	p.Method = "direct"
	p.Guidance = fmt.Sprintf("This executable (%s) is not owned by a recognized updater. Replace this installation using its original installation method or a matching binary from https://github.com/wendylabsinc/wendy-agent/releases. Then run wendy --version and wendy update --check. Check PATH for older copies; the generic installer may choose a different location or package manager.", real)
	return p, nil
}

var packageName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9+_.:-]*$`)
var versionOutput = regexp.MustCompile(`^wendy version (v?[0-9]+(?:\.[0-9]+)+(?:-[0-9A-Za-z.-]+)?)$`)
var releaseVersion = regexp.MustCompile(`^(?:[0-9]{4}\.[0-9]{2}\.[0-9]{2}-[0-9]{6}|[0-9]+\.[0-9]+\.[0-9]+)$`)

func samePath(a, b, goos string) bool {
	if goos == "windows" {
		return strings.EqualFold(strings.ReplaceAll(a, "\\", "/"), strings.ReplaceAll(b, "\\", "/"))
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

// Apply only executes an already-confirmed plan. Every installer error is
// returned, and a successful installer exit is insufficient without checking
// the executable a subsequent shell invocation will actually select.
func Apply(ctx context.Context, p Plan, current, target string, in io.Reader, out, errout io.Writer) (Result, error) {
	return apply(ctx, defaults(), p, current, target, in, out, errout)
}

func apply(ctx context.Context, d dependencies, p Plan, current, target string, in io.Reader, out, errout io.Writer) (Result, error) {
	if !p.Supported || len(p.Commands) == 0 {
		return Result{}, failure("update_method_unsupported", "%s", p.Guidance)
	}
	for _, command := range p.Commands {
		if len(command) == 0 {
			return Result{}, failure("update_method_unsupported", "empty update command")
		}
		if err := d.run(ctx, in, out, errout, command[0], command[1:]...); err != nil {
			if ctx.Err() != nil {
				return Result{}, ctx.Err()
			}
			return Result{}, failure("update_process_failed", "CLI update failed: %w. %s", err, p.Guidance)
		}
	}
	path, err := d.lookPath("wendy")
	if err != nil {
		return Result{}, failure("update_verification_failed", "update finished but wendy is not on PATH: %w", err)
	}
	output, err := d.output(ctx, path, "--version")
	if err != nil {
		if ctx.Err() != nil {
			return Result{}, ctx.Err()
		}
		return Result{}, failure("update_verification_failed", "update finished but wendy --version failed: %w", err)
	}
	match := versionOutput.FindStringSubmatch(strings.TrimSpace(string(output)))
	if len(match) != 2 {
		return Result{}, failure("update_verification_failed", "update finished but wendy --version did not return a recognized release version")
	}
	installed := match[1]
	if version.IsDev(installed) {
		return Result{}, failure("update_verification_failed", "update finished but wendy on PATH (%s) is a development build; check for a shadowed release installation", path)
	}
	parsed := strings.TrimPrefix(installed, "v")
	if p.Channel == "nightly" {
		parsed = strings.TrimSuffix(parsed, "-nightly")
	}
	valid := releaseVersion.MatchString(parsed)
	if valid && strings.Contains(parsed, "-") {
		_, err := time.Parse("2006.01.02-150405", parsed)
		valid = err == nil
	}
	if !valid {
		return Result{}, failure("update_verification_failed", "update finished but wendy on PATH reports an invalid release version")
	}
	if version.CompareVersions(installed, current) <= 0 || (target != "" && version.CompareVersions(installed, target) < 0) {
		return Result{}, failure("update_version_unchanged", "update did not reach the expected version: wendy on PATH (%s) reports %s. Check for a pinned package, a package repository awaiting the release, or an older executable earlier on PATH", path, installed)
	}
	return Result{Version: installed, Executable: path}, nil
}
