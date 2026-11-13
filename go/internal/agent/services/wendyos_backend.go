package services

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/go/internal/agent/oshealth"
)

// wendyOSUpdater drives the in-house wendyos-update engine
// (github.com/wendylabsinc/wendyos-update), the OS A/B update backend. Its CLI
// verbs are install/commit/rollback: `commit` reports "nothing pending" with
// exit 2, install streams structured JSON-lines progress on stdout, and a
// rejected artifact reports a richer reject code (exit 3). A verify failure
// surfaces as exit 4 from `commit` (not install), handled by the gate.
type wendyOSUpdater struct {
	logger        *zap.Logger
	prepareRootfs func(context.Context) error
}

func newWendyOSUpdater(logger *zap.Logger) wendyOSUpdater {
	return wendyOSUpdater{
		logger: logger,
		prepareRootfs: func(ctx context.Context) error {
			return prepareJetsonRootfsAB(ctx, logger)
		},
	}
}

func (w wendyOSUpdater) name() string { return updaterNameWendyOS }

// delegatesHealthcheck is true: wendyos-update runs /etc/wendyos-update/health.d
// inside `commit`, so the agent gate skips its own CheckAll and acts on the
// commit verdict instead.
func (w wendyOSUpdater) delegatesHealthcheck() bool { return true }

func (w wendyOSUpdater) commitCommand() string { return "wendyos-update" }

func (w wendyOSUpdater) available() bool {
	_, found := resolveWendyOSBinary()
	return found
}

// wendyOSDetectTimeout bounds the `status` connector probe used by detect().
const wendyOSDetectTimeout = 10 * time.Second

// detect reports whether wendyos-update can update this device. The binary
// must be present AND `wendyos-update status` must succeed: status resolves the
// platform connector, so a clean exit confirms a supported board. On
// unsupported boards it errors, so chooseUpdater (install-time selection)
// reports no backend available.
func (w wendyOSUpdater) detect() bool {
	binary, found := resolveWendyOSBinary()
	if !found {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), wendyOSDetectTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "status", "--json")
	cmd.Env = envWithPath("/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	if err := cmd.Run(); err != nil {
		w.logger.Debug("wendyos-update status probe failed; backend unavailable for this device",
			zap.Error(err))
		return false
	}
	return true
}

func (w wendyOSUpdater) install(ctx context.Context, artifactURL string, onProgress func(string, int32)) error {
	binary, found := resolveWendyOSBinary()
	if !found {
		return errors.New("wendyos-update binary not found")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := w.prepareRootfs(ctx); err != nil {
		return fmt.Errorf("OS update preparation failed: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	stale, err := w.runInstall(ctx, binary, artifactURL, onProgress)
	if err == nil || !stale {
		return err
	}

	// wendyos-update refused because a previous deployment is still recorded as
	// in flight ("run rollback or mark-good first"). The post-reboot gate reverts
	// the A/B slot on a failed update but can leave this terminal record behind,
	// and the agent exposes no remote verb to clear it — so a single failed
	// update would otherwise wedge the device out of every future OTA update.
	// Clear it with the rollback the tool itself prescribes (mark-good would
	// commit the failed slot, which we must not do), then retry the install once.
	w.logger.Warn("wendyos-update rejected the install: a previous deployment is stuck in flight; clearing it with rollback and retrying")
	if res := runUpdaterCommit(w.logger, binary, "rollback"); res.Status == oshealth.UpdaterError {
		w.logger.Error("failed to clear the stuck wendyos-update deployment; surfacing the original install error",
			zap.String("output", res.Output), zap.Error(res.Err))
		return err
	}
	_, retryErr := w.runInstall(ctx, binary, artifactURL, onProgress)
	return retryErr
}

// runInstall performs one `wendyos-update install` attempt, streaming progress
// and returning a user-facing error on failure. stale is true only when the
// failure was wendyos-update refusing because a prior deployment is stuck in
// flight — the one case install() clears with a rollback and retries.
func (w wendyOSUpdater) runInstall(ctx context.Context, binary, artifactURL string, onProgress func(string, int32)) (stale bool, err error) {
	onProgress("downloading", 0)
	cmd := exec.CommandContext(ctx, binary, "install", artifactURL)
	cmd.Env = envWithPath("/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")

	// stdout carries machine-readable JSON-lines progress; stderr carries
	// human logs, whose tail we keep so a non-zero exit reports the real cause.
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return false, fmt.Errorf("failed to create stdout pipe: %v", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return false, fmt.Errorf("failed to create stderr pipe: %v", err)
	}

	if err := cmd.Start(); err != nil {
		return false, fmt.Errorf("failed to start wendyos-update: %v", err)
	}

	outputTail := newLineRing(updaterOutputTailLines)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
		for scanner.Scan() {
			if phase, percent, ok := parseWendyOSProgress(scanner.Text()); ok {
				onProgress(phase, percent)
			}
		}
		if err := scanner.Err(); err != nil {
			w.logger.Warn("wendyos-update progress scan error", zap.Error(err))
		}
	}()
	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(stderr)
		scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			outputTail.push(line)
			w.logger.Debug("wendyos-update output", zap.String("line", line))
		}
		if err := scanner.Err(); err != nil {
			w.logger.Warn("wendyos-update output scan error", zap.Error(err))
		}
	}()
	wg.Wait()

	if err := cmd.Wait(); err != nil {
		code := exitCodeOf(err)
		tail := outputTail.tail()
		msg := wendyOSInstallErrorMessage(code, tail)
		w.logger.Error("wendyos-update install failed",
			zap.Int("exit_code", code), zap.Error(err), zap.Strings("output_tail", tail))
		return isStaleDeploymentError(tail), errors.New(msg)
	}
	return false, nil
}

// isStaleDeploymentError reports whether a failed `wendyos-update install` was
// refused because a previous deployment is still recorded as in flight — e.g.
// `an update is already in flight (phase "failed", …); run rollback or mark-good
// first`. Matching the tool's stable "already in flight" wording keeps this a
// pure recovery heuristic: if the wording ever changes we simply fall back to
// today's behavior of surfacing the failure unchanged.
func isStaleDeploymentError(tail []string) bool {
	for _, line := range tail {
		if strings.Contains(strings.ToLower(line), "already in flight") {
			return true
		}
	}
	return false
}

func (w wendyOSUpdater) commit() oshealth.UpdaterResult {
	binary, found := resolveWendyOSBinary()
	if !found {
		return oshealth.UpdaterResult{Status: oshealth.UpdaterUnavailable}
	}
	return runUpdaterCommit(w.logger, binary, "commit")
}

func (w wendyOSUpdater) rollback() oshealth.UpdaterResult {
	binary, found := resolveWendyOSBinary()
	if !found {
		return oshealth.UpdaterResult{Status: oshealth.UpdaterUnavailable}
	}
	res := runUpdaterCommit(w.logger, binary, "rollback")
	if res.Status == oshealth.UpdaterOK {
		res.RebootRequired = parseWendyOSRebootRequired(res.Output)
	}
	return res
}

// wendyOSRollbackLine is the JSON-lines summary `wendyos-update rollback`
// prints on stdout: {"phase":"rollback","origin_slot":"...","reboot_required":bool}.
type wendyOSRollbackLine struct {
	Phase          string `json:"phase"`
	RebootRequired bool   `json:"reboot_required"`
}

// parseWendyOSRebootRequired scans a successful rollback's combined output for
// its JSON summary line and reports whether a reboot is actually needed to
// finish the rollback, as opposed to the firmware having already fallen back
// to the previous slot on its own (rollback was then pure bookkeeping). Fails
// safe to true — the old always-reboot behavior — when the line is missing or
// malformed, e.g. an older wendyos-update that predates this field.
func parseWendyOSRebootRequired(output string) bool {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var rec wendyOSRollbackLine
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if rec.Phase == "rollback" {
			return rec.RebootRequired
		}
	}
	return true
}

// resolveWendyOSBinary finds the wendyos-update binary. It checks PATH via
// exec.LookPath and then probes absolute paths directly. The os.Stat fallback
// is restricted to absolute paths to avoid accidentally executing a file from
// the current working directory.
func resolveWendyOSBinary() (string, bool) {
	candidates := []string{
		"wendyos-update",
		"/usr/local/sbin/wendyos-update",
		"/usr/local/bin/wendyos-update",
		"/usr/sbin/wendyos-update",
		"/usr/bin/wendyos-update",
		"/sbin/wendyos-update",
		"/bin/wendyos-update",
	}
	for _, c := range candidates {
		if path, err := exec.LookPath(c); err == nil {
			return path, true
		}
		if filepath.IsAbs(c) {
			if _, err := os.Stat(c); err == nil {
				return c, true
			}
		}
	}
	return "", false
}

// wendyOSProgressLine is one JSON-lines progress record on wendyos-update's
// stdout, e.g. {"phase":"write","percent":42,"msg":"..."}.
type wendyOSProgressLine struct {
	Phase   string `json:"phase"`
	Percent int32  `json:"percent"`
}

// parseWendyOSProgress decodes one stdout line into (phase, percent). It
// returns ok=false for blank lines, non-JSON lines, the `status --json` object
// (no phase), and malformed JSON, so callers can scan stdout uniformly.
func parseWendyOSProgress(line string) (string, int32, bool) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "{") {
		return "", 0, false
	}
	var p wendyOSProgressLine
	if err := json.Unmarshal([]byte(line), &p); err != nil {
		return "", 0, false
	}
	if p.Phase == "" {
		return "", 0, false
	}
	return p.Phase, p.Percent, true
}

// wendyOSInstallErrorMessage builds the user-facing error for a failed
// wendyos-update install and appends the captured tail of the tool's stderr.
// install rejects an artifact (incompatible device, bad checksum/digest, size
// mismatch, or malformed) with exit 3; every other failure uses the generic
// message. Exit 4 ("verify failed") is a commit-time code that install never
// emits — the gate's commit path handles it — so it is not special-cased here.
func wendyOSInstallErrorMessage(exitCode int, tail []string) string {
	reason := "wendyos-update install failed"
	if exitCode == 3 {
		reason = "wendyos-update install failed: the artifact was rejected (incompatible device, bad checksum, or malformed)"
	}
	if len(tail) > 0 {
		return reason + "\nwendyos-update output:\n" + strings.Join(tail, "\n")
	}
	return reason
}
