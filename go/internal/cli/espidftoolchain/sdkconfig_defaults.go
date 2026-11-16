package espidftoolchain

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
)

// SdkconfigDefaultsList is the SDKCONFIG_DEFAULTS of a Wendy Lite native app
// project: the configuration of the Wendy Lite build on the device, then
// the app's own defaults. A later file wins, so a line of
// sdkconfig.defaults overrides a line of sdkconfig.defaults.wendy.
const SdkconfigDefaultsList = "sdkconfig.defaults.wendy;sdkconfig.defaults"

// SdkconfigDefaultsLine is the line of the project CMakeLists.txt that sets
// SdkconfigDefaultsList. It must come before the include of project.cmake,
// which reads SDKCONFIG_DEFAULTS.
const SdkconfigDefaultsLine = `set(SDKCONFIG_DEFAULTS "` + SdkconfigDefaultsList + `")`

const (
	sdkconfigDefaultsComment = "# Wendy Lite: the configuration of the device (sdkconfig.defaults.wendy), then the app's own defaults, which win."
	appDefaultsContent       = "# The app's own sdkconfig defaults. A line here wins over sdkconfig.defaults.wendy.\n"
)

var (
	// The include of project.cmake: SDKCONFIG_DEFAULTS must be set before it.
	projectInclude = regexp.MustCompile(`(?im)^[\t ]*include[\t ]*\([\t ]*"?\$ENV\{IDF_PATH\}/tools/cmake/project\.cmake"?[\t ]*\)`)
	// A set(SDKCONFIG_DEFAULTS ...) command, in comment-masked text.
	setSdkconfigDefaults = regexp.MustCompile(`(?i)\bset[\t ]*\([\t ]*SDKCONFIG_DEFAULTS\b[^)]*\)`)
	// The command that SdkconfigDefaultsLine gives, with any spacing.
	wantSdkconfigDefaults = regexp.MustCompile(`(?i)^set[\t ]*\([\t ]*SDKCONFIG_DEFAULTS[\t ]+"sdkconfig\.defaults\.wendy;sdkconfig\.defaults"[\t ]*\)$`)
	sdkconfigDefaultsWord = regexp.MustCompile(`\bSDKCONFIG_DEFAULTS\b`)
)

// SdkconfigDefaultsSetup holds the changes that make a project use
// SdkconfigDefaultsList, prepared before the user confirms them.
type SdkconfigDefaultsSetup struct {
	projectPath string
	original    []byte // CMakeLists.txt
	updated     []byte
	// CreateDefaults is true when the project has no sdkconfig.defaults:
	// ESP-IDF stops when a file of SDKCONFIG_DEFAULTS does not exist, so
	// Apply creates it with a comment line.
	CreateDefaults bool
}

// AddsLine reports whether the setup adds SdkconfigDefaultsLine to
// CMakeLists.txt.
func (s *SdkconfigDefaultsSetup) AddsLine() bool { return !bytes.Equal(s.original, s.updated) }

// PrepareSdkconfigDefaultsList checks that the top-level CMakeLists.txt of
// the project in projectPath sets SdkconfigDefaultsLine before the include
// of project.cmake, and that the files of the list exist. It returns nil
// when they do. Otherwise it prepares the changes without writing them.
//
// It does not guess: a CMakeLists.txt that uses SDKCONFIG_DEFAULTS in
// another way, or that has no include of project.cmake, gives an error that
// asks for the change by hand. sdkconfig.defaults.wendy is not checked
// here.
func PrepareSdkconfigDefaultsList(projectPath string) (*SdkconfigDefaultsSetup, error) {
	path := filepath.Join(projectPath, "CMakeLists.txt")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading CMakeLists.txt: %w", err)
	}
	manual := fmt.Errorf("add %s to CMakeLists.txt before include($ENV{IDF_PATH}/tools/cmake/project.cmake), and use SDKCONFIG_DEFAULTS nowhere else", SdkconfigDefaultsLine)

	masked := maskCMakeComments(data)
	include := projectInclude.FindIndex(masked)
	if include == nil {
		return nil, fmt.Errorf("CMakeLists.txt has no include($ENV{IDF_PATH}/tools/cmake/project.cmake): %w", manual)
	}
	found := false
	sets := setSdkconfigDefaults.FindAllIndex(masked, -1)
	for _, m := range sets {
		if !wantSdkconfigDefaults.Match(masked[m[0]:m[1]]) {
			return nil, fmt.Errorf("CMakeLists.txt sets SDKCONFIG_DEFAULTS to another value: %w", manual)
		}
		if m[0] > include[0] {
			return nil, fmt.Errorf("CMakeLists.txt sets SDKCONFIG_DEFAULTS after the include of project.cmake: %w", manual)
		}
		found = true
	}
	if words := sdkconfigDefaultsWord.FindAllIndex(masked, -1); len(words) != len(sets) {
		return nil, fmt.Errorf("CMakeLists.txt uses SDKCONFIG_DEFAULTS in another way: %w", manual)
	}

	s := &SdkconfigDefaultsSetup{projectPath: projectPath, original: data, updated: data}
	if !found {
		newline := "\n"
		if bytes.Contains(data, []byte("\r\n")) {
			newline = "\r\n"
		}
		lineStart := bytes.LastIndexByte(data[:include[0]], '\n') + 1
		var b bytes.Buffer
		b.Write(data[:lineStart])
		b.WriteString(sdkconfigDefaultsComment + newline + SdkconfigDefaultsLine + newline + newline)
		b.Write(data[lineStart:])
		s.updated = b.Bytes()
	}
	if _, err := os.Stat(filepath.Join(projectPath, "sdkconfig.defaults")); errors.Is(err, fs.ErrNotExist) {
		s.CreateDefaults = true
	} else if err != nil {
		return nil, err
	}
	if !s.AddsLine() && !s.CreateDefaults {
		return nil, nil
	}
	return s, nil
}

// Apply writes the prepared changes. It refuses to write CMakeLists.txt
// when the file changed since PrepareSdkconfigDefaultsList, and it does not
// replace a sdkconfig.defaults created in the meantime.
func (s *SdkconfigDefaultsSetup) Apply() error {
	path := filepath.Join(s.projectPath, "CMakeLists.txt")
	if s.AddsLine() {
		current, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !bytes.Equal(current, s.original) {
			return errors.New("CMakeLists.txt changed during setup; run 'wendy run' again")
		}
		if err := os.WriteFile(path, s.updated, 0o644); err != nil {
			return fmt.Errorf("writing CMakeLists.txt: %w", err)
		}
	}
	if s.CreateDefaults {
		f, err := os.OpenFile(filepath.Join(s.projectPath, "sdkconfig.defaults"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if errors.Is(err, fs.ErrExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("creating sdkconfig.defaults: %w", err)
		}
		_, err = f.WriteString(appDefaultsContent)
		return errors.Join(err, f.Close())
	}
	return nil
}

// maskCMakeComments replaces the CMake comments of data with spaces, keeping
// the offsets and the newlines: # to the end of the line, and bracket
// comments #[[ ... ]] (or #[==[ ... ]==]). A # in a quoted argument is not
// a comment.
func maskCMakeComments(data []byte) []byte {
	masked := bytes.Clone(data)
	blank := func(from, to int) {
		for i := from; i < to; i++ {
			if masked[i] != '\n' && masked[i] != '\r' {
				masked[i] = ' '
			}
		}
	}
	inQuote := false
	for i := 0; i < len(data); i++ {
		c := data[i]
		switch {
		case inQuote && c == '\\':
			i++
		case c == '"':
			inQuote = !inQuote
		case !inQuote && c == '#':
			end := -1
			if eq, ok := bracketOpen(data[i+1:]); ok {
				closing := []byte("]" + string(bytes.Repeat([]byte("="), eq)) + "]")
				if j := bytes.Index(data[i+1:], closing); j >= 0 {
					end = i + 1 + j + len(closing)
				} else {
					end = len(data)
				}
			} else {
				end = bytes.IndexByte(data[i:], '\n')
				if end < 0 {
					end = len(data)
				} else {
					end += i
				}
			}
			blank(i, end)
			i = end - 1
		}
	}
	return masked
}

// bracketOpen reports whether data starts with a bracket opening [[ or
// [=...=[, and its number of =.
func bracketOpen(data []byte) (int, bool) {
	if len(data) == 0 || data[0] != '[' {
		return 0, false
	}
	eq := 0
	for eq+1 < len(data) && data[eq+1] == '=' {
		eq++
	}
	if eq+1 < len(data) && data[eq+1] == '[' {
		return eq, true
	}
	return 0, false
}

// SdkconfigStale reports whether the sdkconfig of the project in dir must
// be generated again from its defaults: it does not exist, or it is older
// than one of inputs (paths relative to dir). An input that does not exist
// does not count.
func SdkconfigStale(dir string, inputs ...string) bool {
	config, err := os.Stat(filepath.Join(dir, "sdkconfig"))
	if err != nil {
		return true
	}
	for _, input := range inputs {
		info, err := os.Stat(filepath.Join(dir, input))
		if err == nil && info.ModTime().After(config.ModTime()) {
			return true
		}
	}
	return false
}
