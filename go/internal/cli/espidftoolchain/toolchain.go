package espidftoolchain

import (
	"bufio"
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
	"strconv"
	"strings"
)

// DefaultVersion is the ESP-IDF version used to build Wendy Lite native apps,
// named as eim (the ESP-IDF Installation Manager) names it.
const DefaultVersion = "v5.5.4"

var execCommandContext = exec.CommandContext

// IsEspIdfProject reports whether dir contains an ESP-IDF project.
//
// A directory is considered an ESP-IDF project when it contains an sdkconfig
// (or sdkconfig.defaults) file, or a top-level CMakeLists.txt that includes
// the IDF build system entry point (project.cmake resolved via IDF_PATH or
// an esp-idf directory). A plain CMakeLists.txt without such an include is
// not enough, so generic CMake projects are not misclassified.
func IsEspIdfProject(dir string) bool {
	for _, name := range []string{"sdkconfig", "sdkconfig.defaults"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return true
		}
	}
	content, err := os.ReadFile(filepath.Join(dir, "CMakeLists.txt"))
	if err != nil {
		return false
	}
	if !bytes.Contains(content, []byte("project.cmake")) {
		return false
	}
	// Require the include path to reference an IDF install, e.g.
	// include($ENV{IDF_PATH}/tools/cmake/project.cmake) or a path
	// containing an esp-idf directory.
	return bytes.Contains(content, []byte("IDF_PATH")) ||
		bytes.Contains(content, []byte("esp-idf"))
}

// projectNamePattern matches a CMake project() command and captures its first
// argument, the project name. CMake command names are case-insensitive.
var projectNamePattern = regexp.MustCompile(`(?i)^\s*project\s*\(\s*([A-Za-z0-9._-]+)`)

// ProjectName extracts the project name from the top-level CMakeLists.txt in
// dir, i.e. the first argument of the project(...) command. It returns "" if
// the file is missing or contains no project() declaration.
func ProjectName(dir string) string {
	f, err := os.Open(filepath.Join(dir, "CMakeLists.txt"))
	if err != nil {
		return ""
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		if m := projectNamePattern.FindStringSubmatch(line); m != nil {
			return m[1]
		}
	}
	return ""
}

// ProjectTarget returns the IDF target (SoC name, e.g. "esp32c6") the project
// in dir has been configured for, read from the IDF_TARGET property of
// build/config/sdkconfig.json. It returns "" if the project has not been
// configured yet (file missing, unparseable, or no IDF_TARGET property).
func ProjectTarget(dir string) string {
	content, err := os.ReadFile(filepath.Join(dir, "build", "config", "sdkconfig.json"))
	if err != nil {
		return ""
	}
	var config struct {
		IdfTarget string `json:"IDF_TARGET"`
	}
	if err := json.Unmarshal(content, &config); err != nil {
		return ""
	}
	return config.IdfTarget
}

// ReadSdkconfig reads the sdkconfig of the ESP-IDF project in dir and returns
// the value of each requested setting, typed: a boolean option comes back as a
// bool, an integer one as an int, and a string one with its quotes stripped.
//
// A setting that is not set is left out of the result rather than reported, so
// a lookup of it yields nil, which stays distinct from an option genuinely set
// to an empty string (CONFIG_WENDY_WIFI_SSID="" is a real one). Only a failure
// to read the file is an error.
//
// This reads the sdkconfig idf.py generates in the project directory, which
// exists only once the project has been configured — not sdkconfig.defaults,
// which merely seeds it. Its lines look like:
//
//	CONFIG_ESP_CONSOLE_UART_DEFAULT=y
//	# CONFIG_ESP_CONSOLE_USB_SERIAL_JTAG is not set
//	CONFIG_ESP_CONSOLE_UART_BAUDRATE=115200
//	CONFIG_IDF_TARGET="esp32c6"
func ReadSdkconfig(dir string, keys []string) (map[string]any, error) {
	f, err := os.Open(filepath.Join(dir, "sdkconfig"))
	if err != nil {
		return nil, err
	}
	defer f.Close()

	wanted := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		wanted[key] = struct{}{}
	}

	values := make(map[string]any, len(keys))
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		// Skipping every comment is what detects an unset option: Kconfig
		// writes those as "# CONFIG_X is not set" rather than leaving them
		// out, and either way the option never enters the map.
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		// The generated file never pads the assignment, but a hand-edited one
		// can, and a padded key would otherwise read as unset.
		key = strings.TrimSpace(key)
		if _, ok := wanted[key]; !ok {
			continue
		}
		// Last one wins, as Kconfig itself resolves it. IDF appends a
		// deprecated-alias section naming retired options
		// (CONFIG_CONSOLE_UART_DEFAULT for CONFIG_ESP_CONSOLE_UART_DEFAULT),
		// but never the same option twice, so this only matters for a
		// hand-edited file.
		values[key] = parseSdkconfigValue(strings.TrimSpace(value))
	}
	// Without this a truncated read would look like a project with every
	// option unset, i.e. a misconfiguration rather than a failure to read.
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return values, nil
}

// parseSdkconfigValue converts the right-hand side of an sdkconfig assignment
// to the Go type the option holds:
//
//	y      -> true
//	n      -> false
//	"text" -> "text"
//	115200 -> 115200
//	0x8000 -> 32768
//
// An unrecognized form is returned as its raw string, so an option this does
// not know how to type stays visible to the caller instead of vanishing.
func parseSdkconfigValue(value string) any {
	switch value {
	case "y":
		return true
	case "n":
		// The generated sdkconfig writes "# CONFIG_X is not set" instead, but
		// a sdkconfig.defaults does use =n.
		return false
	}
	// A matched pair only: strings.Trim(value, `"`), the idiom used elsewhere
	// in the repo, would also eat an unbalanced quote belonging to the value.
	if len(value) >= 2 && strings.HasPrefix(value, `"`) && strings.HasSuffix(value, `"`) {
		return value[1 : len(value)-1]
	}
	// Base 0 so hex values (CONFIG_PARTITION_TABLE_OFFSET=0x8000) parse too.
	// int rather than int64, so an untyped constant in a caller's comparison
	// — which boxes into any as an int — actually matches.
	if n, err := strconv.ParseInt(value, 0, strconv.IntSize); err == nil {
		return int(n)
	}
	return value
}

// EnsureVersion verifies that eim is installed and that the DefaultVersion
// ESP-IDF toolchain is available, installing the toolchain via eim when it
// is missing.
func EnsureVersion(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	checkCmd := execCommandContext(ctx, "eim", "--version")
	checkCmd.Stdout = io.Discard
	checkCmd.Stderr = io.Discard
	if err := checkCmd.Run(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return errors.New("eim (ESP-IDF Installation Manager) is not installed; " +
				"install it via 'brew install espressif/eim/eim' or see https://dl.espressif.com/dl/eim/")
		}
		return fmt.Errorf("running 'eim --version': %w", err)
	}

	listCmd := execCommandContext(ctx, "eim", "list")
	out, err := listCmd.Output()
	if err != nil {
		details := strings.TrimSpace(string(out))
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			if stderr := strings.TrimSpace(string(exitErr.Stderr)); stderr != "" {
				if details != "" {
					details += "\n"
				}
				details += stderr
			}
		}
		if details != "" {
			return fmt.Errorf("running 'eim list': %w: %s", err, details)
		}
		return fmt.Errorf("running 'eim list': %w", err)
	}
	for _, v := range parseInstalledVersions(string(out)) {
		if v == DefaultVersion {
			return nil
		}
	}

	fmt.Fprintf(os.Stdout, "Installing ESP-IDF %s (this may take a while)...\n", DefaultVersion)
	installCmd := execCommandContext(ctx, "eim", "install", "-i", DefaultVersion, "-n", "true")
	installCmd.Stdout = os.Stdout
	installCmd.Stderr = os.Stderr
	if err := installCmd.Run(); err != nil {
		return fmt.Errorf("installing ESP-IDF %s via eim: %w", DefaultVersion, err)
	}
	return nil
}

// parseInstalledVersions extracts the version names from 'eim list' output,
// which reports installed versions as lines like:
//
//   - v5.5.4 (selected) [/Users/me/.espressif/v5.5.4/esp-idf]
func parseInstalledVersions(output string) []string {
	var versions []string
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "- ") {
			continue
		}
		if fields := strings.Fields(line[2:]); len(fields) > 0 {
			versions = append(versions, fields[0])
		}
	}
	return versions
}

// IdfCommandContext returns a command that runs idf.py with the given
// arguments inside the DefaultVersion ESP-IDF environment. 'eim run' performs
// the activation (including the toolchain's Python venv) itself, runs the
// command in the caller's working directory and propagates its exit code.
// eim takes the command as a single string and interprets it with a POSIX
// shell, so each argument is quoted to survive spaces and other shell
// metacharacters.
func IdfCommandContext(ctx context.Context, args ...string) *exec.Cmd {
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, "idf.py")
	for _, arg := range args {
		parts = append(parts, shellQuote(arg))
	}
	return execCommandContext(ctx, "eim", "run", strings.Join(parts, " "), DefaultVersion)
}

// shellSafePattern matches arguments that need no quoting in a POSIX shell.
var shellSafePattern = regexp.MustCompile(`^[A-Za-z0-9@%+=:,./_-]+$`)

// shellQuote returns arg quoted for a POSIX shell, leaving it untouched when
// it contains only safe characters so typical commands stay readable in logs.
func shellQuote(arg string) string {
	if shellSafePattern.MatchString(arg) {
		return arg
	}
	return "'" + strings.ReplaceAll(arg, "'", `'"'"'`) + "'"
}
