package services

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
)

// Matches CheckReadiness for tegra234 in wendyos-update commit
// 1f160d407833783755af153a64f1b386e8f922ac, internal/connector/tegrauefi.
// See that revision's docs/cli-contract.md for the read-only check JSON contract.
// The updater owns boot-chain selection; the Agent must not infer it from SoC
// names, versions, or human-readable detail strings.
const readyRootfsJSON = `{"ok":true,"checks":[{"name":"rootfs A/B redundancy","ok":true,"detail":"not applicable: boot-chain A/B moves the rootfs slot"},{"name":"ESP capsule headroom","ok":true}]}`

func TestWendyOSInstallAcceptsUpdaterReadyVolatileOrin(t *testing.T) {
	trace := stubWendyOSPreflightBinary(t, `
case "$1" in
 check) [ "$#" = 2 ] && [ "$2" = --json ] || exit 9
 printf '%s\n' '`+readyRootfsJSON+`';;
 install) [ "$#" = 2 ] && [ "$2" = https://example.invalid/update.wendy ] || exit 9;;
 *) exit 9;;
esac
`)
	f := newJetsonPreparationFixture(t)
	f.seed(t, rootfsRedundancy, 6, 0)
	f.seed(t, rootfsRetryMax, 6, 3)
	// Demonstrate the exact incompatible legacy decision on the same state.
	if _, err := f.p.prepare(context.Background()); err == nil || !strings.Contains(err.Error(), "firmware-managed") {
		t.Fatalf("legacy volatile Orin reproduction: %v", err)
	}
	w := newWendyOSUpdater(zap.NewNop())
	w.prepareRootfs = func(ctx context.Context) error {
		_, err := f.p.prepare(ctx)
		if err != nil {
			return fmt.Errorf("cannot prepare Jetson rootfs A/B redundancy: %w", err)
		}
		return nil
	}
	if err := w.install(context.Background(), "https://example.invalid/update.wendy", func(string, int32) {}); err != nil {
		t.Fatalf("updater-ready Orin must reach install without legacy EFI repair: %v", err)
	}
	if len(f.writes) != 0 {
		t.Fatalf("unexpected firmware writes: %v", f.writes)
	}
	f.wantVariable(t, rootfsRedundancy, 6, 0)
	f.wantVariable(t, rootfsRetryMax, 6, 3)
	assertWendyOSPreflightCalls(t, trace, "check\ninstall\n")
}

func TestWendyOSReadinessFallbackPreservesLegacyPreparation(t *testing.T) {
	for _, tc := range []struct {
		name, response string
		exit           int
	}{
		{"unsupported old engine", "", 1},
		{"not ready", `{"ok":false,"checks":[{"name":"rootfs A/B redundancy","ok":false}]}`, 5},
		{"pending deployment", `{"ok":false,"checks":[{"name":"rootfs A/B redundancy","ok":true},{"name":"no pending update","ok":false}]}`, 5},
		{"other check failed", `{"ok":true,"checks":[{"name":"rootfs A/B redundancy","ok":true},{"name":"ESP capsule headroom","ok":false}]}`, 0},
		{"failed exit with ready output", readyRootfsJSON, 1},
		{"malformed", "not json", 0},
		{"empty verdict", `{}`, 0},
		{"missing rootfs", `{"ok":true,"checks":[{"name":"other","ok":true}]}`, 0},
		{"missing check ok", `{"ok":true,"checks":[{"name":"rootfs A/B redundancy"}]}`, 0},
		{"inconsistent overall", `{"ok":true,"checks":[{"name":"rootfs A/B redundancy","ok":false}]}`, 0},
		{"duplicate check", `{"ok":true,"checks":[{"name":"rootfs A/B redundancy","ok":true},{"name":"rootfs A/B redundancy","ok":true}]}`, 0},
		{"duplicate key", `{"ok":false,"ok":true,"checks":[{"name":"rootfs A/B redundancy","ok":true}]}`, 0},
		{"duplicate nested key", `{"ok":true,"checks":[{"name":"rootfs A/B redundancy","ok":false,"ok":true}]}`, 0},
		{"case alias", `{"ok":false,"OK":true,"checks":[{"name":"rootfs A/B redundancy","ok":true}]}`, 0},
		{"trailing json", readyRootfsJSON + ` {}`, 0},
		{"oversized", readyRootfsJSON + strings.Repeat(" ", wendyOSReadinessLimit), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			trace := stubWendyOSPreflightBinary(t, fmt.Sprintf("if [ \"$1\" = check ]; then\n printf '%%s' '%s'\n exit %d\nfi\nexit 0\n", tc.response, tc.exit))
			f := newJetsonPreparationFixture(t)
			f.seed(t, rootfsRedundancy, 6, 0)
			f.seed(t, rootfsRetryMax, 6, 3)
			w := newWendyOSUpdater(zap.NewNop())
			w.prepareRootfs = func(ctx context.Context) error {
				recordWendyOSPreflightCall(t, trace, "prepare")
				_, err := f.p.prepare(ctx)
				return err
			}
			if err := w.install(context.Background(), "https://example.invalid/update.wendy", func(string, int32) { t.Error("unready engine reached install") }); err == nil || !strings.Contains(err.Error(), "firmware-managed") {
				t.Fatalf("legacy refusal: %v", err)
			}
			assertWendyOSPreflightCalls(t, trace, "prepare\ncheck\n")
			f.wantVariable(t, rootfsRedundancy, 6, 0)
			f.wantVariable(t, rootfsRetryMax, 6, 3)
			if len(f.writes) != 0 {
				t.Fatalf("legacy repair writes: %v", f.writes)
			}
		})
	}
}

func TestWendyOSReadinessCancellationDoesNotRepairOrInstall(t *testing.T) {
	trace := stubWendyOSPreflightBinary(t, "exec /bin/sleep 60\n")
	w := newWendyOSUpdater(zap.NewNop())
	f := newJetsonPreparationFixture(t)
	f.seed(t, rootfsRedundancy, 6, 0)
	f.seed(t, rootfsRetryMax, 6, 3)
	w.prepareRootfs = func(ctx context.Context) error { _, err := f.p.prepare(ctx); return err }
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := w.install(ctx, "https://example.invalid/update.wendy", func(string, int32) { t.Error("cancelled probe reached install") })
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 2*time.Second {
		t.Fatalf("cancelled check: %v after %v", err, time.Since(start))
	}
	assertWendyOSPreflightCalls(t, trace, "check\n")
}

func TestWendyOSReadinessTimeoutRetainsLegacyRefusal(t *testing.T) {
	trace := stubWendyOSPreflightBinary(t, "exec /bin/sleep 60\n")
	f := newJetsonPreparationFixture(t)
	f.seed(t, rootfsRedundancy, 6, 0)
	f.seed(t, rootfsRetryMax, 6, 3)
	w := newWendyOSUpdater(zap.NewNop())
	w.prepareRootfs = func(ctx context.Context) error {
		recordWendyOSPreflightCall(t, trace, "prepare")
		_, err := f.p.prepare(ctx)
		return err
	}
	start := time.Now()
	err := w.install(context.Background(), "https://example.invalid/update.wendy", func(string, int32) { t.Error("unready engine reached install") })
	if err == nil || !strings.Contains(err.Error(), "firmware-managed") || time.Since(start) > wendyOSDetectTimeout+2*time.Second {
		t.Fatalf("bounded fallback: %v after %v", err, time.Since(start))
	}
	if len(f.writes) != 0 {
		t.Fatalf("unexpected firmware writes: %v", f.writes)
	}
	assertWendyOSPreflightCalls(t, trace, "prepare\ncheck\n")
}

func TestWendyOSLegacyRepairsRemainRequired(t *testing.T) {
	for _, tc := range []struct {
		name                                   string
		levelAttrs, level, retryAttrs, retries uint32
		writes                                 int
		wantErr                                bool
	}{
		{"ready old engine still needs retry repair", 7, 1, 7, 0, 1, false},
		{"missing persistent level still needs repair", 7, 0, 6, 3, 1, false},
		{"volatile level with missing retry budget", 6, 0, 7, 0, 0, true},
		{"volatile retry budget is not delegable", 7, 1, 6, 0, 0, true},
		{"invalid retry count is not delegable", 6, 0, 7, 4, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			trace := stubWendyOSPreflightBinary(t, "if [ \"$1\" = check ]; then printf '%s' '"+readyRootfsJSON+"'; fi\n")
			f := newJetsonPreparationFixture(t)
			f.seed(t, rootfsRedundancy, tc.levelAttrs, tc.level)
			f.seed(t, rootfsRetryMax, tc.retryAttrs, tc.retries)
			w := newWendyOSUpdater(zap.NewNop())
			w.prepareRootfs = func(ctx context.Context) error { _, err := f.p.prepare(ctx); return err }
			err := w.install(context.Background(), "https://example.invalid/update.wendy", func(string, int32) {})
			if (err != nil) != tc.wantErr {
				t.Fatalf("legacy preparation: %v", err)
			}
			wantCalls := "install\n"
			if tc.wantErr {
				wantCalls = ""
			}
			assertWendyOSPreflightCalls(t, trace, wantCalls)
			if len(f.writes) != tc.writes {
				t.Fatalf("writes %v, want %d", f.writes, tc.writes)
			}
			if !tc.wantErr {
				f.wantVariable(t, rootfsRedundancy, tc.levelAttrs, 1)
				f.wantVariable(t, rootfsRetryMax, tc.retryAttrs, 3)
			}
		})
	}
}
