package arduinotoolchain

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// Artifacts are the two images Arduino's upload writes, from one compile.
type Artifacts struct {
	LoaderPath  string
	SketchPath  string
	CoreVersion string
	Layout      string
}

// sketchYAMLExample is what a missing sketch.yaml error suggests: a pinned
// platform, plus the Bridge library the zephyr core ships only as a stub.
const sketchYAMLExample = `profiles:
  default:
    fqbn: arduino:zephyr:unoq
    platforms:
      - platform: arduino:zephyr (1.1.0)
    libraries:
      - Arduino_RouterBridge (0.4.4)
      - dependency: Arduino_RPClite (0.3.1)
      - dependency: ArxContainer (0.7.0)
      - dependency: ArxTypeTraits (0.3.2)
      - dependency: DebugLog (0.8.4)
      - dependency: MsgPack (0.4.2)
default_profile: default`

type sketchProfile struct {
	FQBN      string `yaml:"fqbn"`
	Platforms []struct {
		Platform string `yaml:"platform"`
	} `yaml:"platforms"`
}

type sketchYAML struct {
	Profiles       map[string]sketchProfile `yaml:"profiles"`
	DefaultProfile string                   `yaml:"default_profile"`
}

var pinnedPlatform = regexp.MustCompile(`^\S+ \(\d+\.\d+\.\d+\)$`)

// LoadProfile returns the profile to build and the arduino:zephyr version it
// pins. It refuses a profile that would not build the same way twice.
func LoadProfile(sketchDir, profile string) (name, core string, err error) {
	name, p, err := loadProfile(sketchDir, profile)
	if err != nil {
		return "", "", err
	}
	for _, pl := range p.Platforms {
		if id, rest, ok := strings.Cut(pl.Platform, " ("); ok && id == "arduino:zephyr" {
			return name, strings.TrimSuffix(rest, ")"), nil
		}
	}
	return "", "", fmt.Errorf("profile %q does not pin arduino:zephyr, the core of the board's microcontroller", name)
}

func loadProfile(sketchDir, profile string) (string, sketchProfile, error) {
	data, err := os.ReadFile(filepath.Join(sketchDir, "sketch.yaml"))
	if errors.Is(err, os.ErrNotExist) {
		return "", sketchProfile{}, fmt.Errorf("no sketch.yaml in %s; add one that pins the Arduino platform, for example:\n\n%s", sketchDir, sketchYAMLExample)
	}
	if err != nil {
		return "", sketchProfile{}, err
	}
	var y sketchYAML
	if err := yaml.Unmarshal(data, &y); err != nil {
		return "", sketchProfile{}, fmt.Errorf("reading %s: %w", filepath.Join(sketchDir, "sketch.yaml"), err)
	}
	name := profile
	if name == "" {
		name = y.DefaultProfile
	}
	if name == "" && len(y.Profiles) == 1 {
		for only := range y.Profiles {
			name = only
		}
	}
	if name == "" {
		return "", sketchProfile{}, fmt.Errorf("sketch.yaml has several profiles and no default_profile; set mcu.profile in wendy.json")
	}
	p, ok := y.Profiles[name]
	if !ok {
		names := make([]string, 0, len(y.Profiles))
		for n := range y.Profiles {
			names = append(names, n)
		}
		sort.Strings(names)
		return "", sketchProfile{}, fmt.Errorf("profile %q is not in sketch.yaml (has: %s)", name, strings.Join(names, ", "))
	}
	if !strings.HasPrefix(p.FQBN, "arduino:zephyr:unoq") {
		return "", sketchProfile{}, fmt.Errorf("profile %q builds for %q; the board's microcontroller needs arduino:zephyr:unoq", name, p.FQBN)
	}
	if len(p.Platforms) == 0 {
		return "", sketchProfile{}, fmt.Errorf("profile %q lists no platform; pin one, e.g. arduino:zephyr (1.1.0)", name)
	}
	for _, pl := range p.Platforms {
		if !pinnedPlatform.MatchString(pl.Platform) {
			return "", sketchProfile{}, fmt.Errorf("profile %q: pin the platform version, e.g. arduino:zephyr (1.1.0), got %q", name, pl.Platform)
		}
	}
	return name, p, nil
}

// LayoutForCore maps a zephyr core version to the flash layout the board helper
// knows. Cores 1.0 and 1.1 share one map; older ones put the sketch elsewhere.
func LayoutForCore(version string) (string, error) {
	parts := strings.Split(version, ".")
	if len(parts) >= 2 {
		major, err1 := strconv.Atoi(parts[0])
		minor, err2 := strconv.Atoi(parts[1])
		if err1 == nil && err2 == nil && major == 1 && (minor == 0 || minor == 1) {
			return "zephyr-1", nil
		}
	}
	return "", fmt.Errorf("unsupported Arduino core version %s; use 1.0 or 1.1", version)
}

// CheckHost refuses a host that cannot run Arduino's build tools. Arduino ships
// ctags, which every .ino build runs, only for Intel Macs.
func CheckHost(ctx context.Context) error {
	if hostPlatform() != "darwin/arm64" {
		return nil
	}
	if err := execCommandContext(ctx, "arch", "-x86_64", "/usr/bin/true").Run(); err != nil {
		return errors.New("compiling an Arduino sketch on Apple Silicon needs Rosetta 2, because Arduino's ctags tool is Intel-only; install it once with: softwareupdate --install-rosetta --agree-to-license")
	}
	return nil
}

// Compile builds the sketch with its profile, then asks arduino-cli where the
// loader and the sketch image are, exactly as its own upload would.
func Compile(ctx context.Context, cliPath, sketchDir, profile, buildDir string, stdout, stderr io.Writer) (*Artifacts, error) {
	root, err := cacheDir()
	if err != nil {
		return nil, err
	}
	args := []string{"--config-file", configPath(root), "compile", "--profile", profile, "--build-path", buildDir}
	cmd := execCommandContext(ctx, cliPath, append(args, sketchDir)...)
	log := &outputLog{}
	cmd.Stdout, cmd.Stderr = log.tee(stdout), log.tee(stderr)
	if err := cmd.Run(); err != nil {
		if lines := compilerErrors(log.buf.Bytes()); len(lines) > 0 {
			return nil, fmt.Errorf("the sketch does not compile:\n  %s", strings.Join(lines, "\n  "))
		}
		return nil, fmt.Errorf("compiling the sketch: %w", err)
	}

	var out, errOut bytes.Buffer
	props := execCommandContext(ctx, cliPath, append(args, "--show-properties=expanded", sketchDir)...)
	props.Stdout, props.Stderr = &out, &errOut
	if err := props.Run(); err != nil {
		return nil, fmt.Errorf("reading the sketch build properties: %w: %s", err, strings.TrimSpace(errOut.String()))
	}
	kv := parseProperties(out.Bytes())
	a := &Artifacts{
		LoaderPath:  kv["upload.artifacts.loader"],
		SketchPath:  kv["upload.artifacts.sketch"],
		CoreVersion: kv["version"],
	}
	for key, v := range map[string]string{"upload.artifacts.loader": a.LoaderPath, "upload.artifacts.sketch": a.SketchPath, "version": a.CoreVersion} {
		if v == "" {
			return nil, fmt.Errorf("arduino-cli did not report %s; is this an Arduino UNO Q profile?", key)
		}
	}
	if a.Layout, err = LayoutForCore(a.CoreVersion); err != nil {
		return nil, err
	}
	return a, nil
}

// outputLog keeps a copy of a command's output. os/exec writes stdout and
// stderr from two goroutines, so every write goes through one lock.
type outputLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *outputLog) tee(w io.Writer) io.Writer {
	return writerFunc(func(p []byte) (int, error) {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.buf.Write(p)
		if w == nil {
			return len(p), nil
		}
		return w.Write(p)
	})
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// compilerErrors picks the compiler's own error lines out of arduino-cli's
// output, where they sit above a long table of the libraries used.
func compilerErrors(out []byte) []string {
	const max = 5
	var lines []string
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() && len(lines) < max {
		line := strings.TrimSpace(sc.Text())
		if strings.Contains(line, ": error:") || strings.Contains(line, ": fatal error:") {
			lines = append(lines, line)
		}
	}
	return lines
}

func parseProperties(b []byte) map[string]string {
	kv := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		if k, v, ok := strings.Cut(sc.Text(), "="); ok {
			kv[k] = v
		}
	}
	return kv
}
