package commands

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// Minor image tags float to the latest patch. Only accept constraints for
// which the entire chosen minor line is compatible. Patch/prerelease/exact
// constraints need an explicit Dockerfile rather than an invented runtime.
var pythonMinorConstraint = regexp.MustCompile(`^(>=|<=|==|!=|>|<|~=)\s*(\d+)(?:\.(\d+))?(\.\*)?$`)

func pythonDockerBase(dir string) (string, error) {
	var project struct {
		Project struct {
			RequiresPython string `toml:"requires-python"`
		} `toml:"project"`
	}
	_, err := toml.DecodeFile(filepath.Join(dir, "pyproject.toml"), &project)
	if os.IsNotExist(err) {
		return "3.11", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading pyproject.toml: %w", err)
	}
	requires := strings.TrimSpace(project.Project.RequiresPython)
	if requires == "" {
		return "3.11", nil
	}
	type constraint struct {
		op           string
		major, minor int
		hasMinor     bool
	}
	var constraints []constraint
	for _, part := range strings.Split(requires, ",") {
		m := pythonMinorConstraint.FindStringSubmatch(strings.TrimSpace(part))
		if m == nil || ((m[1] == "==" || m[1] == "!=") && m[4] == "") || (m[1] == "~=" && m[3] == "") || (m[4] != "" && m[1] != "==" && m[1] != "!=") {
			return "", fmt.Errorf("cannot safely select a floating Python image for requires-python %q; provide a Dockerfile with a compatible Python version", requires)
		}
		major, parseErr := strconv.Atoi(m[2])
		if parseErr != nil {
			return "", parseErr
		}
		minor := 0
		if m[3] != "" {
			minor, parseErr = strconv.Atoi(m[3])
			if parseErr != nil {
				return "", parseErr
			}
		}
		constraints = append(constraints, constraint{m[1], major, minor, m[3] != ""})
	}
	// Preserve the existing default when compatible; otherwise prefer the
	// nearest newer supported minor, then older lines for bounded projects.
	for _, minor := range []int{11, 12, 13, 14, 15, 10, 9} {
		ok := true
		for _, c := range constraints {
			cmp := 0
			if 3 < c.major || (3 == c.major && minor < c.minor) {
				cmp = -1
			}
			if 3 > c.major || (3 == c.major && minor > c.minor) {
				cmp = 1
			}
			switch c.op {
			case ">=":
				ok = cmp >= 0
			case ">":
				ok = cmp > 0
			case "<":
				ok = cmp < 0
			case "<=":
				ok = cmp < 0 // equal minor includes patches above the ceiling
			case "==":
				ok = 3 == c.major && (!c.hasMinor || minor == c.minor)
			case "!=":
				ok = !(3 == c.major && (!c.hasMinor || minor == c.minor))
			case "~=":
				ok = cmp >= 0 && 3 == c.major
			}
			if !ok {
				break
			}
		}
		if ok {
			return fmt.Sprintf("3.%d", minor), nil
		}
	}
	return "", fmt.Errorf("no supported Python minor image satisfies requires-python %q; provide a compatible Dockerfile", requires)
}
