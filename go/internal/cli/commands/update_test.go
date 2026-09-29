package commands

import (
	"context"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/version"
)

// TestDueCLIUpdateCheckSkipsDevBuilds asserts that development builds — both the
// literal "dev" default and CI branch builds carrying a "-dev" suffix — never
// trigger the periodic CLI update check (WDY-1770).
func TestDueCLIUpdateCheckSkipsDevBuilds(t *testing.T) {
	original := version.Version
	t.Cleanup(func() { version.Version = original })

	for _, ver := range []string{"dev", "2026.06.30-133859-dev"} {
		t.Run(ver, func(t *testing.T) {
			version.Version = ver
			// Empty LastCLIUpdateCheck would otherwise mark the check as due.
			if dueCLIUpdateCheck(&config.Config{}) {
				t.Errorf("dueCLIUpdateCheck for dev build %q = true, want false", ver)
			}
		})
	}
}

func TestCLIUpdateCheckPreservesRotatedOAuthSession(t *testing.T) {
	originalVersion := version.Version
	version.Version = "2026.09.26-062348"
	t.Cleanup(func() { version.Version = originalVersion })

	auth, calls := rotatingOAuthSession(t)
	if err := ensureOAuthAccessToken(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	checkedAt := time.Date(2026, time.September, 27, 12, 0, 0, 0, time.FixedZone("test", 2*60*60))
	if err := persistCLIUpdateCheckResult(checkedAt, "2026.09.27-215032", nil); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || cfg.Auth[0].RefreshToken != "refresh-2" {
		t.Fatal("update check restored the consumed refresh token")
	}
	if cfg.LastCLIUpdateCheck != "2026-09-27T10:00:00Z" || cfg.AvailableCLIUpdate != "2026.09.27-215032" {
		t.Fatalf("update metadata = %q, %q", cfg.LastCLIUpdateCheck, cfg.AvailableCLIUpdate)
	}
}
