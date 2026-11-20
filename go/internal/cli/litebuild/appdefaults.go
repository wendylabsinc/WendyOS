package litebuild

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"gopkg.in/yaml.v3"
)

// AppDefaultsFile is the sdkconfig defaults file of a Wendy Lite native app
// project: the configuration of the Wendy Lite build that runs on the
// device. The project CMakeLists.txt lists it in SDKCONFIG_DEFAULTS.
const AppDefaultsFile = "sdkconfig.defaults.wendy"

// VersionOption is the line of AppDefaultsFile that gives the Wendy Lite
// version that the file is for: the wendy_core option WENDY_VERSION, which
// tools/configure.py of wendy-lite also writes, after the build profile.
const VersionOption = "CONFIG_WENDY_VERSION"

// partitionTablePrefix starts the options of the partition table. The
// firmware lines name a partition CSV of the wendy-lite repository, which an
// app project does not have, so AppDefaultsText leaves them out.
const partitionTablePrefix = "CONFIG_PARTITION_TABLE_"

// ParseBuildProfile reads a build profile, the JSON text that BuildProfile
// writes and CONFIG_WENDY_BUILD_PROFILE stores, into input values. It uses
// a YAML parser: JSON is YAML, and older Wendy Lite versions wrote flat
// YAML, one "name: value" per line. An unknown attribute or a value that the
// attribute does not accept is an error. An empty text gives no values.
func ParseBuildProfile(m *Model, text string) (map[string]any, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		return nil, confErrorf("build profile: %v", err)
	}
	inputs := map[string]any{}
	root := documentRoot(&doc)
	if isNull(root) {
		return inputs, nil
	}
	entries, err := mapping(root, "build profile: expected name: value lines")
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.key.Kind != yaml.ScalarNode {
			return nil, confErrorf("build profile: line %d: the name must be a text", e.key.Line)
		}
		name := e.key.Value
		attr := m.Attributes[name]
		if attr == nil {
			return nil, confErrorf("build profile: unknown attribute \"%s\"", name)
		}
		if isNull(e.value) {
			continue
		}
		if e.value.Kind != yaml.ScalarNode {
			return nil, confErrorf("build profile: %s must be a single value", name)
		}
		value, err := attr.Normalize(scalarValue(e.value))
		if err != nil {
			return nil, where("build profile", err)
		}
		inputs[name] = value
	}
	return inputs, nil
}

// ProfileSolution is the solution of the build profile of a device.
type ProfileSolution struct {
	Inputs   map[string]any
	Solution *Solution
	// IDFVersion is the ESP-IDF version of the build ("6.0.3", "6.1"), and
	// IDFPreview whether idf.py needs --preview for the chip.
	IDFVersion string
	IDFPreview bool
}

// SolveProfile solves the build profile of a device whose chip is target,
// with m, the master configuration of its Wendy Lite version. The profile
// must give an allowed configuration with every required value, the chip
// of the device and the ESP-IDF version, as configure.py requires them.
func SolveProfile(m *Model, profile, target string) (*ProfileSolution, error) {
	inputs, err := ParseBuildProfile(m, profile)
	if err != nil {
		return nil, err
	}
	solver, err := NewSolver(m)
	if err != nil {
		return nil, err
	}
	sol, err := solver.Evaluate(inputs, nil)
	if err != nil {
		return nil, where("build profile", err)
	}
	if !sol.Allowed() {
		return nil, confErrorf("build profile: %s", NotAllowedText(sol.Forbidden))
	}
	required, err := solver.Required(map[string][]string{
		Chip:       {"the device chip check"},
		IDFVersion: {"the ESP-IDF version"},
		IDFPreview: {"idf.py --preview"},
	})
	if err != nil {
		return nil, err
	}
	if missing := solver.FindMissing(sol.Values, required); len(missing) > 0 {
		names := make([]string, len(missing))
		for i, m := range missing {
			names[i] = m.Name
		}
		return nil, confErrorf("build profile: missing value(s): %s", strings.Join(names, ", "))
	}
	if chip, _ := sol.Values[Chip].(string); chip != target {
		return nil, confErrorf("the build profile is for the %s, but the device is a %s", chip, target)
	}
	ps := &ProfileSolution{Inputs: inputs, Solution: sol}
	ps.IDFVersion, _ = sol.Values[IDFVersion].(string)
	ps.IDFPreview, _ = sol.Values[IDFPreview].(bool)
	return ps, nil
}

// AppDefaultsText returns the text of AppDefaultsFile for a device: its
// Wendy Lite version, its build profile and its chip (target). m is the
// master configuration of that version.
//
// The text is the solution of the build profile (SolveProfile), as
// SdkconfigText gives it, with the VersionOption line after the build
// profile line, as configure.py writes it, and without the partition table
// lines.
func AppDefaultsText(m *Model, version, profile, target string) (string, error) {
	ps, err := SolveProfile(m, profile, target)
	if err != nil {
		return "", err
	}
	inputs, sol := ps.Inputs, ps.Solution

	header := m.Header(inputs)
	lines := []string{VersionOption + "=" + kconfigString(version)}
	for _, line := range sol.Sdkconfig {
		if strings.HasPrefix(line, partitionTablePrefix) {
			continue
		}
		lines = append(lines, line)
	}
	if len(lines) < 1+len(sol.Sdkconfig) {
		header = append(header, "# The partition table lines ("+partitionTablePrefix+"*) of the firmware are left out.")
	}
	app := *sol
	app.Sdkconfig = lines
	return m.SdkconfigText(header, inputs, &app), nil
}

// kconfigString returns s as a Kconfig string value: in double quotes, with
// \ and " escaped.
func kconfigString(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}

// AppDefaultsState tells whether AppDefaultsFile matches a device.
type AppDefaultsState int

const (
	// AppDefaultsOK: the file is for the version and the build profile of
	// the device.
	AppDefaultsOK AppDefaultsState = iota
	// AppDefaultsMissing: the project has no AppDefaultsFile.
	AppDefaultsMissing
	// AppDefaultsOutdated: the file is for another version or another build
	// profile.
	AppDefaultsOutdated
)

// AppDefaultsStatus is the result of CheckAppDefaults.
type AppDefaultsStatus struct {
	State AppDefaultsState
	// Version and Profile are the values in the file, "" when it has none.
	Version string
	Profile string
}

// CheckAppDefaults tells whether the AppDefaultsFile of the project in
// projectPath is for the Wendy Lite version and the build profile of a
// device. The build profiles are compared as YAML maps, so that the quoting
// of a value does not count.
func CheckAppDefaults(projectPath, version, profile string) (AppDefaultsStatus, error) {
	data, err := os.ReadFile(filepath.Join(projectPath, AppDefaultsFile))
	if errors.Is(err, fs.ErrNotExist) {
		return AppDefaultsStatus{State: AppDefaultsMissing}, nil
	}
	if err != nil {
		return AppDefaultsStatus{}, fmt.Errorf("reading %s: %w", AppDefaultsFile, err)
	}
	values := kconfigStrings(string(data))
	status := AppDefaultsStatus{
		State:   AppDefaultsOutdated,
		Version: values[VersionOption],
		Profile: storedProfile(values),
	}
	if status.Version == version && sameProfile(status.Profile, profile) {
		status.State = AppDefaultsOK
	}
	return status, nil
}

// storedProfile returns the build profile in the string options of
// sdkconfig text (kconfigStrings). Older Wendy Lite versions wrote flat YAML
// with the 2 characters \n between the lines. Configuration values have no
// \n, so this does not change a JSON profile.
func storedProfile(values map[string]string) string {
	return strings.ReplaceAll(values[BuildProfileOption], `\n`, "\n")
}

// kconfigStrings returns the string options of sdkconfig text, unescaped:
// the lines CONFIG_X="value".
func kconfigStrings(text string) map[string]string {
	values := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		name, raw, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || !strings.HasPrefix(name, "CONFIG_") || len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"' {
			continue
		}
		var b strings.Builder
		raw = raw[1 : len(raw)-1]
		for i := 0; i < len(raw); i++ {
			if raw[i] == '\\' && i+1 < len(raw) {
				i++
			}
			b.WriteByte(raw[i])
		}
		values[name] = b.String()
	}
	return values
}

// sameProfile tells whether two build profiles give the same YAML map. A
// text that is not a YAML map is never the same.
func sameProfile(a, b string) bool {
	var ma, mb map[string]any
	if yaml.Unmarshal([]byte(a), &ma) != nil || yaml.Unmarshal([]byte(b), &mb) != nil {
		return false
	}
	if len(ma) == 0 && len(mb) == 0 {
		return true
	}
	return reflect.DeepEqual(ma, mb)
}
