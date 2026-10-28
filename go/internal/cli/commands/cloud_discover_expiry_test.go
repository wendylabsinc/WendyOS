package commands

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

// stubCertExpiry makes every stored certificate expire at notAfter, with the
// clock fixed one hour before (valid) or after (expired) it.
func stubCertExpiry(t *testing.T, expired bool) *config.AuthConfig {
	t.Helper()
	notAfter := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	now := notAfter.Add(-time.Hour)
	if expired {
		now = notAfter.Add(time.Hour)
	}
	oldNow, oldLeaf, oldFresh := timeNowFn, leafNotAfterFn, ensureFreshCertificateFn
	t.Cleanup(func() { timeNowFn, leafNotAfterFn, ensureFreshCertificateFn = oldNow, oldLeaf, oldFresh })
	timeNowFn = func() time.Time { return now }
	leafNotAfterFn = func(string) (time.Time, error) { return notAfter, nil }
	ensureFreshCertificateFn = func(context.Context, *config.AuthConfig) error {
		if expired {
			return errCertExpired
		}
		return nil
	}
	auth := config.AuthConfig{CloudGRPC: "localhost:50051", Certificates: []config.CertificateInfo{{PemCertificate: "stub"}}}
	seedConfig(t, &config.Config{Auth: []config.AuthConfig{auth}})
	return &auth
}

func TestCloudDiscoverExpiredCertificateExitsBeforeView(t *testing.T) {
	for _, interactive := range []bool{true, false} {
		t.Run(map[bool]string{true: "interactive", false: "plain"}[interactive], func(t *testing.T) {
			stubCertExpiry(t, true)
			oldInteractive, oldJSON := isInteractiveTerminalFn, jsonOutput
			t.Cleanup(func() { isInteractiveTerminalFn, jsonOutput = oldInteractive, oldJSON })
			isInteractiveTerminalFn = func() bool { return interactive }
			jsonOutput = false

			cmd := newCloudDiscoverCmd()
			cmd.SetContext(context.Background())
			err := cmd.RunE(cmd, nil)
			if interactive {
				var reported *ReportedExitError
				if !errors.As(err, &reported) {
					t.Fatalf("RunE = %v, want ReportedExitError before the view opens", err)
				}
				return
			}
			if !errors.Is(err, errCertExpired) || !strings.Contains(err.Error(), "wendy auth login") {
				t.Fatalf("RunE = %v, want the expiry error with its remedy", err)
			}
		})
	}
}

func TestCloudDiscoverViewExpiryBanner(t *testing.T) {
	for _, expired := range []bool{true, false} {
		auth := stubCertExpiry(t, expired)
		m := newCloudDiscoverModel(context.Background(), auth, "", false, false, nil)
		updated, _ := m.Update(cloudScanMsg{})
		view := updated.(cloudDiscoverModel).View()
		hasBanner := strings.Contains(view, "Your certificate has expired.") && strings.Contains(view, "wendy auth login")
		hasEmpty := strings.Contains(view, "No online devices found")
		if hasBanner != expired || hasEmpty == expired {
			t.Fatalf("expired=%v: banner=%v, empty-list line=%v in view:\n%s", expired, hasBanner, hasEmpty, view)
		}
	}
}
