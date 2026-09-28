package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
)

const maxBuildFailureCauseLen = 220

// The failing step's own output is the cause when BuildKit logged only its
// wrapper error: enough lines for a compiler error with context, few enough
// that the summary stays a summary (the full log is saved alongside).
const (
	maxBuildFailureOutputLines   = 20
	maxBuildFailureOutputLineLen = 240
)

var (
	buildFailureANSIRe   = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)
	buildFailurePrefixRe = regexp.MustCompile(`^#\d+(?:\s+\d+(?:\.\d+)?)?\s+`)
	buildElapsedPrefixRe = regexp.MustCompile(`^\d+(?:\.\d+)?\s+`)
	buildSourceRe        = regexp.MustCompile(`^([^\s:]*Dockerfile[^:]*):(\d+)$`)
	buildStepRe          = regexp.MustCompile(`^>?\s*\[([^\]]+)]\s+(.+?)(?::)?$`)
	githubPackageRe      = regexp.MustCompile(`github\.com/([^/\s]+)/([^/@\s]+?)(?:\.git)?@`)
	// buildVertexLineRe splits a `--progress plain` line into its vertex number
	// and the rest: "#9 [api 3/3] RUN …", "#9 0.908 <output>", "#9 ERROR: …".
	buildVertexLineRe = regexp.MustCompile(`^#(\d+) (.*)$`)
	// buildVertexOutputRe matches a step's own output, which plain progress
	// prefixes with the seconds since the step started.
	buildVertexOutputRe = regexp.MustCompile(`^\d+\.\d+(?: (.*))?$`)
	buildExitCodeRe     = regexp.MustCompile(`exit code: (-?\d+)`)

	persistBuildFailureLog = writeBuildFailureLog
)

type buildFailureSummary struct {
	step       string
	stage      string // the step the build failed in, "stage n/m", when the log names it
	command    string // and that step's command, compacted
	cause      string
	output     []string // the failing step's last output lines, when they are the cause
	source     string
	detailsURL string
	fallback   string
}

// buildVertex accumulates one plain-progress step: its "[stage n/m] CMD"
// header and the lines it printed.
type buildVertex struct {
	header string
	output []string
}

// summarizeBuildFailure extracts the useful part of a BuildKit failure. The
// raw stream repeats the failed command and dependency diagnostics several
// times; presenting those repetitions makes the actual cause hard to find.
func summarizeBuildFailure(raw string, buildErr error) buildFailureSummary {
	clean := buildFailureANSIRe.ReplaceAllString(raw, "")
	lines := strings.Split(clean, "\n")
	var summary buildFailureSummary
	var errorsSeen []string
	vertices := map[string]*buildVertex{}
	failedVertex, failedMessage := "", ""

	for _, rawLine := range lines {
		// Track each step's output by vertex so interleaved parallel steps
		// stay apart. Only trailing space is trimmed: indentation is output.
		if m := buildVertexLineRe.FindStringSubmatch(strings.TrimRight(rawLine, " \t\r")); m != nil {
			v := vertices[m[1]]
			if v == nil {
				v = &buildVertex{}
				vertices[m[1]] = v
			}
			switch rest := m[2]; {
			case strings.HasPrefix(rest, "ERROR: "):
				failedVertex, failedMessage = m[1], strings.TrimPrefix(rest, "ERROR: ")
			case strings.HasPrefix(rest, "["):
				v.header = rest
			default:
				if om := buildVertexOutputRe.FindStringSubmatch(rest); om != nil {
					v.output = append(v.output, om[1])
				}
			}
		}

		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}

		if m := buildSourceRe.FindStringSubmatch(line); m != nil {
			summary.source = m[1] + ":" + m[2]
		}
		if value, ok := strings.CutPrefix(line, "View build details:"); ok {
			summary.detailsURL = strings.TrimSpace(value)
		}

		withoutVertex := buildFailurePrefixRe.ReplaceAllString(line, "")
		withoutTime := buildElapsedPrefixRe.ReplaceAllString(withoutVertex, "")
		if m := buildStepRe.FindStringSubmatch(withoutVertex); m != nil && isBuildCommand(m[2]) {
			summary.step = m[1] + " — " + compactBuildCommand(strings.TrimSuffix(m[2], ":"))
			if strings.HasPrefix(withoutVertex, ">") {
				// BuildKit's failure summary (" > [stage n/m] CMD:") names
				// the step that failed.
				summary.setFailingStep(m[1], m[2])
			}
		}

		if strings.HasPrefix(withoutTime, "ERROR:") {
			message := strings.TrimSpace(strings.TrimPrefix(withoutTime, "ERROR:"))
			if !isBuildkitWrapperError(message) {
				errorsSeen = appendUnique(errorsSeen, compactBuildFailureText(message))
			}
		}
	}

	if strings.Contains(clean, "conflicting dependencies") && strings.Contains(clean, "ResolutionImpossible") {
		packages := githubPackages(clean)
		if len(packages) >= 2 {
			summary.cause = "pip dependency conflict: " + strings.Join(packages, " and ")
			if strings.Contains(clean, "unknown 0.0.0") {
				summary.cause += " both report package metadata as unknown 0.0.0"
			}
		} else {
			summary.cause = "pip could not resolve the requested package dependencies"
		}
	} else if len(errorsSeen) > 0 {
		summary.cause = errorsSeen[0]
	} else if v := vertices[failedVertex]; v != nil {
		// BuildKit logged only its wrapper (`process "…" did not complete
		// successfully: exit code: 1`), so the real cause — a compiler error,
		// a traceback — is whatever the failing step itself printed (WDY-1832).
		summary.cause = "step failed"
		if m := buildExitCodeRe.FindStringSubmatch(failedMessage); m != nil {
			summary.cause = "step failed with exit code " + m[1]
		}
		summary.output = lastBuildOutputLines(v.output, maxBuildFailureOutputLines)
		if m := buildStepRe.FindStringSubmatch(v.header); m != nil && isBuildCommand(m[2]) {
			summary.step = m[1] + " — " + compactBuildCommand(strings.TrimSuffix(m[2], ":"))
		}
	}
	// Without BuildKit's failure summary, the failing step is the vertex that
	// logged the failure, when that is a build step. The last step the scan
	// saw (summary.step) may not be: a failed push, for one, comes after
	// every step succeeded.
	if v := vertices[failedVertex]; v != nil && summary.stage == "" {
		if m := buildStepRe.FindStringSubmatch(v.header); m != nil && isBuildCommand(m[2]) {
			summary.setFailingStep(m[1], m[2])
		}
	}

	if buildErr != nil {
		summary.fallback = compactBuildFailureText(buildErr.Error())
	}
	return summary
}

// setFailingStep records the step the build failed in, for the error that
// names it: its "stage n/m" and its command, compacted.
func (s *buildFailureSummary) setFailingStep(stage, command string) {
	s.stage, s.command = stage, compactBuildCommand(strings.TrimSuffix(command, ":"))
}

// buildFailureCauseError is a build failure whose message names the failing
// step and its cause instead of the builder's exit status. It unwraps to that
// failure, so errors.Is and errors.As (isImageBuildFailure, the build_failed
// error class, the registry-fallback decisions) see the same chain.
type buildFailureCauseError struct {
	message string
	err     error
}

func (e *buildFailureCauseError) Error() string { return e.message }
func (e *buildFailureCauseError) Unwrap() error { return e.err }

// buildFailureError returns buildErr with the failing step and the cause as
// its message — "build failed at [<stage n/m>] <command>: <cause>", or "build
// failed: <cause>" when the log does not name the step that failed — so the
// run's final error line, a --wait-ready "failed" object and the JSON error
// envelope say what failed. The cause is the last line of the failing step's
// output when that output is the cause, else the summarized cause. Without a
// cause, and for a cancellation, buildErr is returned unchanged.
func buildFailureError(summary buildFailureSummary, buildErr error) error {
	if buildErr == nil || errors.Is(buildErr, ErrUserCancelled) || errors.Is(buildErr, context.Canceled) {
		return buildErr
	}
	cause := summary.cause
	for i := len(summary.output) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(summary.output[i]); line != "" {
			cause = line
			break
		}
	}
	if cause == "" {
		return buildErr
	}
	cause = compactBuildFailureText(cause)
	message := "build failed: " + cause
	if summary.stage != "" {
		message = fmt.Sprintf("build failed at [%s] %s: %s", summary.stage, summary.command, cause)
	}
	return &buildFailureCauseError{message: message, err: buildErr}
}

// renderBuildFailure prints the build failure's details (the failing step,
// its cause, where it is, and the path of the full log, which it saves) and
// returns the error to report in buildErr's place: buildErr with the step
// and cause in its message (buildFailureError).
func renderBuildFailure(w io.Writer, label, raw string, buildErr error) error {
	summary := summarizeBuildFailure(raw, buildErr)
	heading := "Build failure details"
	if label != "" {
		heading += ": " + label
	}
	fmt.Fprintf(w, "\n%s\n", heading)
	if summary.step != "" {
		fmt.Fprintf(w, "  Step: %s\n", summary.step)
	}
	if summary.cause != "" && len(summary.output) > 0 {
		fmt.Fprintf(w, "  Cause: %s; its output ended with:\n", summary.cause)
		for _, line := range summary.output {
			fmt.Fprintf(w, "    %s\n", line)
		}
	} else if summary.cause != "" {
		fmt.Fprintf(w, "  Cause: %s\n", summary.cause)
	} else if summary.fallback != "" {
		fmt.Fprintf(w, "  Cause: %s\n", summary.fallback)
	}
	if summary.source != "" {
		fmt.Fprintf(w, "  At: %s\n", summary.source)
	}
	if summary.detailsURL != "" {
		fmt.Fprintf(w, "  Details: %s\n", summary.detailsURL)
	}

	if path, err := persistBuildFailureLog(label, raw); err == nil {
		fmt.Fprintf(w, "  Build log: %s\n", path)
	} else {
		// Never discard the only diagnostic when the temporary directory is
		// unavailable. This is rare, and the verbose fallback is preferable to
		// a tidy but unactionable error.
		fmt.Fprintf(w, "\n%s", raw)
		if raw != "" && !strings.HasSuffix(raw, "\n") {
			fmt.Fprintln(w)
		}
	}
	return buildFailureError(summary, buildErr)
}

func writeBuildFailureLog(label, raw string) (string, error) {
	name := sanitizeBuildLogLabel(label)
	f, err := os.CreateTemp("", "wendy-build-"+name+"-*.log")
	if err != nil {
		return "", err
	}
	path := f.Name()
	if _, err := io.WriteString(f, raw); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return "", err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return path, nil
}

func sanitizeBuildLogLabel(label string) string {
	if label == "" {
		return "image"
	}
	var b strings.Builder
	for _, r := range strings.ToLower(label) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	if b.Len() == 0 {
		return "image"
	}
	return b.String()
}

// lastBuildOutputLines returns up to max trailing lines of a step's output,
// without the blank lines around it, each cut to a readable width.
func lastBuildOutputLines(lines []string, max int) []string {
	start, end := 0, len(lines)
	for start < end && strings.TrimSpace(lines[start]) == "" {
		start++
	}
	for end > start && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}
	if end-start > max {
		start = end - max
	}
	out := make([]string, 0, end-start)
	for _, line := range lines[start:end] {
		out = append(out, truncateBuildFailureText(strings.TrimRight(line, " \t"), maxBuildFailureOutputLineLen))
	}
	return out
}

func githubPackages(text string) []string {
	var out []string
	for _, match := range githubPackageRe.FindAllStringSubmatch(text, -1) {
		repo := strings.TrimSuffix(match[2], ".git")
		out = appendUnique(out, match[1]+"/"+repo)
	}
	return out
}

func isBuildCommand(text string) bool {
	for _, prefix := range []string{"RUN ", "COPY ", "ADD ", "FROM ", "WORKDIR ", "ENV ", "ARG "} {
		if strings.HasPrefix(text, prefix) {
			return true
		}
	}
	return false
}

func compactBuildCommand(command string) string {
	if strings.HasPrefix(command, "RUN ") {
		for _, tool := range []string{"pip install", "apt-get", "npm ", "yarn ", "swift build", "go build", "cargo build"} {
			if strings.Contains(command, tool) && len(command) > 120 {
				return "RUN " + strings.TrimSpace(tool) + " …"
			}
		}
	}
	return truncateBuildFailureText(command, 140)
}

func compactBuildFailureText(text string) string {
	return truncateBuildFailureText(strings.Join(strings.Fields(text), " "), maxBuildFailureCauseLen)
}

func truncateBuildFailureText(text string, max int) string {
	if len(text) <= max {
		return text
	}
	return strings.TrimSpace(text[:max-1]) + "…"
}

func isBuildkitWrapperError(message string) bool {
	return strings.HasPrefix(message, "failed to build:") ||
		strings.HasPrefix(message, "failed to solve:") ||
		strings.HasPrefix(message, "process \"") ||
		strings.HasPrefix(message, "ResolutionImpossible:")
}

func appendUnique(values []string, value string) []string {
	if value == "" {
		return values
	}
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}
