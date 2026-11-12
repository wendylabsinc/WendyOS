package litebuild

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/wendylabsinc/wendy/go/internal/cli/espidftoolchain"
)

// TestPythonParity checks that the Go configurator gives the same
// sdkconfig.wendy text as the Python configurator of wendy-lite
// (tools/configurator/solve.py), with the live master configuration of
// wendy-lite. It compares:
//
//   - migration/<old_conf>: each entry of
//     tools/configurator/conf_migration.yaml, solved directly and through
//     the questions;
//   - all/<NAME=VALUE ...>: each combination of answers that the questions
//     allow.
//
// It needs git, network and a Python with PyYAML, so it runs only with
// WENDY_LITE_PARITY=1. Then a missing tool is a failure, not a skip:
//
//	WENDY_LITE_PARITY=1 go test ./internal/cli/litebuild -run TestPythonParity
//
// By default it clones wendy-lite at DefaultRef, the ref that the CLI
// builds, and makes a Python virtual environment with uv. Two optional
// variables replace them:
//
//   - WENDY_LITE_DIR: a wendy-lite checkout (an absolute path, or relative
//     to this package directory);
//   - WENDY_LITE_PYTHON: a Python with PyYAML, for example the ESP-IDF
//     Python.
func TestPythonParity(t *testing.T) {
	if os.Getenv("WENDY_LITE_PARITY") != "1" {
		t.Skip("set WENDY_LITE_PARITY=1 to compare with the Python configurator of wendy-lite")
	}
	dir := wendyLiteSources(t)
	py := pythonSolver{python: pythonWithYAML(t), dir: dir}
	if _, err := os.Stat(py.script()); err != nil {
		t.Fatalf("wendy-lite has no Python configurator: %v", err)
	}
	m, err := LoadFile(MasterConfPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	c, err := NewConfigurator(m, nil)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("migration", func(t *testing.T) {
		for _, mc := range migrationCases(t, dir, m) {
			t.Run(mc.name, func(t *testing.T) {
				// The Go work runs before t.Parallel: one at a time.
				sol, err := c.solver.Evaluate(mc.inputs, nil)
				if err != nil {
					t.Fatal(err)
				}
				if !sol.Allowed() {
					t.Fatal(NotAllowedText(sol.Forbidden))
				}
				if missing := c.solver.FindMissing(sol.Values, c.required); len(missing) > 0 {
					t.Fatalf("missing: %v", missing)
				}
				answers := map[string]string{}
				for name, v := range mc.inputs {
					answers[name] = m.Attributes[name].Key(v)
				}
				res, err := c.Run(newScripted(t, answers))
				if err != nil {
					t.Fatalf("questions: %v", err)
				}

				t.Parallel()
				want := py.sdkconfig(t, m.Assignments(mc.inputs))
				checkSdkconfig(t, "solver", goSdkconfig(m, mc.inputs, sol), want)
				// An only allowed value is an input of the questions, but
				// not of the case: then only the rule lines are the same.
				checkSdkconfig(t, "questions: sdkconfig lines", sdkconfigLines(res.Solution.Sdkconfig), ruleLines(t, want))
				if len(res.Inputs) == len(mc.inputs) {
					checkSdkconfig(t, "questions", goSdkconfig(m, res.Inputs, res.Solution), want)
				}
			})
		}
	})

	t.Run("all", func(t *testing.T) {
		results := combinations(t, c)
		t.Logf("%d answer combinations", len(results))
		for _, res := range results {
			args := m.Assignments(res.Inputs)
			t.Run(strings.Join(args, " "), func(t *testing.T) {
				t.Parallel()
				checkSdkconfig(t, "questions", goSdkconfig(m, res.Inputs, res.Solution), py.sdkconfig(t, args))
			})
		}
	})
}

// wendyLiteSources returns the wendy-lite checkout: WENDY_LITE_DIR, else a
// shallow clone of DefaultRef.
func wendyLiteSources(t *testing.T) string {
	t.Helper()
	if dir := os.Getenv("WENDY_LITE_DIR"); dir != "" {
		abs, err := filepath.Abs(dir)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("wendy-lite: %s, commit %s", abs, gitCommit(abs))
		return abs
	}
	dir := filepath.Join(t.TempDir(), "wendy-lite")
	cmd := exec.Command("git", "clone", "--quiet", "--depth", "1", "--branch", DefaultRef, espidftoolchain.WendyLiteGitURL, dir)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("clone wendy-lite %s: %v\n%s", DefaultRef, err, out)
	}
	t.Logf("wendy-lite: %s, commit %s", DefaultRef, gitCommit(dir))
	return dir
}

func gitCommit(dir string) string {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}

// pythonWithYAML returns a Python with PyYAML: WENDY_LITE_PYTHON, else the
// Python of a new uv virtual environment with PyYAML.
func pythonWithYAML(t *testing.T) string {
	t.Helper()
	if python := os.Getenv("WENDY_LITE_PYTHON"); python != "" {
		return python
	}
	uv, err := exec.LookPath("uv")
	if err != nil {
		t.Fatal("uv is not installed: install uv, or set WENDY_LITE_PYTHON to a Python with PyYAML")
	}
	venv := filepath.Join(t.TempDir(), "venv")
	python := filepath.Join(venv, "bin", "python")
	if runtime.GOOS == "windows" {
		python = filepath.Join(venv, "Scripts", "python.exe")
	}
	for _, args := range [][]string{
		{"venv", "--quiet", venv},
		{"pip", "install", "--quiet", "--python", python, "pyyaml"},
	} {
		cmd := exec.Command(uv, args...)
		cmd.Dir = filepath.Dir(venv) // not in a Python project
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("uv %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	return python
}

// pythonSolver runs solve.py of a wendy-lite checkout.
type pythonSolver struct {
	python string
	dir    string // wendy-lite checkout
}

func (p pythonSolver) script() string {
	return filepath.Join(p.dir, "tools", "configurator", "solve.py")
}

// sdkconfig returns the sdkconfig text of solve.py for the NAME=VALUE
// arguments, without the first header line (it has the master
// configuration path).
func (p pythonSolver) sdkconfig(t *testing.T, args []string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "sdkconfig.wendy")
	cmd := exec.Command(p.python, append([]string{p.script(), "--conf", MasterConfPath(p.dir), "-o", out}, args...)...)
	if report, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("solve.py %s: %v\n%s", strings.Join(args, " "), err, report)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	first, rest, _ := strings.Cut(string(data), "\n")
	if !strings.HasPrefix(first, "# Generated by") {
		t.Fatalf("solve.py: first line %q, want the \"# Generated by\" header", first)
	}
	return rest
}

// goSdkconfig returns the Go sdkconfig text, without the first header line,
// as pythonSolver.sdkconfig.
func goSdkconfig(m *Model, inputs map[string]any, sol *Solution) string {
	return m.SdkconfigText(m.Header(inputs)[1:], inputs, sol)
}

// ruleLines returns the rule lines of an sdkconfig text: without the header
// comments and the build profile line.
func ruleLines(t *testing.T, text string) string {
	t.Helper()
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	for len(lines) > 0 && (strings.HasPrefix(lines[0], "# Generated by") ||
		strings.HasPrefix(lines[0], "# Values file:") || strings.HasPrefix(lines[0], "# Command line:")) {
		lines = lines[1:]
	}
	if len(lines) == 0 || !strings.HasPrefix(lines[0], BuildProfileOption+"=") {
		t.Fatalf("no %s line after the header:\n%s", BuildProfileOption, text)
	}
	return sdkconfigLines(lines[1:])
}

func sdkconfigLines(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}

// checkSdkconfig reports the first different line and both texts.
func checkSdkconfig(t *testing.T, what, got, want string) {
	t.Helper()
	if got == want {
		return
	}
	g, w := strings.Split(got, "\n"), strings.Split(want, "\n")
	i := 0
	for i < len(g) && i < len(w) && g[i] == w[i] {
		i++
	}
	line := func(lines []string) string {
		if i < len(lines) {
			return fmt.Sprintf("%q", lines[i])
		}
		return "end of text"
	}
	t.Errorf("%s: differs from solve.py at line %d: Go %s, Python %s\n--- Go\n%s--- Python\n%s",
		what, i+1, line(g), line(w), got, want)
}

type migrationCase struct {
	name   string
	inputs map[string]any
}

// migrationCases reads tools/configurator/conf_migration.yaml: the new_conf
// values of each entry.
func migrationCases(t *testing.T, dir string, m *Model) []migrationCase {
	t.Helper()
	path := filepath.Join(dir, "tools", "configurator", "conf_migration.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var entries []struct {
		OldConf string         `yaml:"old_conf"`
		NewConf map[string]any `yaml:"new_conf"`
	}
	if err := yaml.Unmarshal(data, &entries); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	if len(entries) == 0 {
		t.Fatalf("%s: no entries", path)
	}
	cases := make([]migrationCase, 0, len(entries))
	for _, e := range entries {
		inputs := map[string]any{}
		for name, raw := range e.NewConf {
			attr := m.Attributes[name]
			if attr == nil {
				t.Fatalf("%s: %s: unknown attribute %s", path, e.OldConf, name)
			}
			v, err := attr.Normalize(raw)
			if err != nil {
				t.Fatalf("%s: %s: %v", path, e.OldConf, err)
			}
			inputs[name] = v
		}
		cases = append(cases, migrationCase{e.OldConf, inputs})
	}
	return cases
}

// maxCombinations stops combinations when the master configuration has
// more answer combinations than a test can check.
const maxCombinations = 5000

// combinations runs the questions once for each combination of answers: a
// depth-first walk over the choices, the last question first.
func combinations(t *testing.T, c *Configurator) []*Result {
	t.Helper()
	var results []*Result
	var prefix []int
	for {
		w := &walker{c: c, prefix: prefix}
		res, err := c.Run(w)
		if err != nil {
			t.Fatalf("answers %v: %v", w.chosen, err)
		}
		results = append(results, res)
		if len(results) > maxCombinations {
			t.Fatalf("more than %d answer combinations", maxCombinations)
		}
		i := len(w.chosen) - 1
		for i >= 0 && w.chosen[i] == w.counts[i]-1 {
			i--
		}
		if i < 0 {
			return results
		}
		prefix = append(w.chosen[:i:i], w.chosen[i]+1)
	}
}

// walker is a Prompter that gives the choices of prefix, then the first
// choice of each next question.
type walker struct {
	c      *Configurator
	prefix []int // the choices to give first
	chosen []int // the choice of each question
	counts []int // the number of choices of each question
}

func (w *walker) choose(n int) int {
	i := 0
	if k := len(w.chosen); k < len(w.prefix) {
		i = w.prefix[k]
	}
	w.chosen = append(w.chosen, i)
	w.counts = append(w.counts, n)
	return i
}

func (w *walker) Select(q Question) (int, error) { return w.choose(len(q.Options)), nil }

func (w *walker) Confirm(Question) (bool, error) { return w.choose(2) == 1, nil }

// Text chooses one of the values that the rules tell apart.
func (w *walker) Text(q Question, validate func(string) error) (string, error) {
	var texts []string
	for _, v := range w.c.representatives(q.Attr) {
		if text := q.Attr.Key(v); validate(text) == nil {
			texts = append(texts, text)
		}
	}
	if len(texts) == 0 {
		return "", fmt.Errorf("%s: no allowed value to try", q.Attr.Name)
	}
	return texts[w.choose(len(texts))], nil
}

func (w *walker) Info(*Attribute, any, string) {}
