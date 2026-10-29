package services

import (
	"context"
	"crypto/x509"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/timesync"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The caller only gets a generic "keys retained" refusal, so the journal is
// the one place that can say which check failed. mesh-1's refusal on
// 2026-10-26 left no trace of its reason.
func TestCloudResetRefusalLogsItsReason(t *testing.T) {
	for name, tc := range map[string]struct {
		setup      func(*ProvisioningService)
		wantStatus string
		wantReason string
	}{
		"key-fetch": {
			setup: func(svc *ProvisioningService) {
				svc.verifyCloudReset = func(context.Context, []byte, string, string, string, []byte, *x509.Certificate, time.Time, time.Time) ([]byte, error) {
					return nil, errors.New("fetching Cloud grant keys: i/o timeout")
				}
			},
			wantStatus: "independently verified Cloud reset authorization required; keys retained",
			wantReason: "fetching Cloud grant keys: i/o timeout",
		},
		// Deleted one minute ago, but the authenticated window starts two
		// minutes ago: the deletion is not provably before it.
		"deletion-inside-window": {
			setup: func(svc *ProvisioningService) {
				svc.trustedTime = func(context.Context) (timesync.TimeWindow, error) {
					now := time.Now()
					return anchored(timesync.TimeWindow{Earliest: now.Add(-2 * time.Minute), Latest: now}), nil
				}
			},
			wantStatus: "independently verified Cloud reset authorization required; keys retained",
			wantReason: "Cloud deletion binding mismatch",
		},
		"time": {
			setup: func(svc *ProvisioningService) {
				svc.trustedTime = func(context.Context) (timesync.TimeWindow, error) {
					return timesync.TimeWindow{}, errors.New("timesync: fresh authenticated quorum required")
				}
			},
			wantStatus: "fresh authenticated time required; keys retained",
			wantReason: "fresh authenticated quorum required",
		},
	} {
		t.Run(name, func(t *testing.T) {
			svc, req, ctx := cloudResetFixture(t)
			core, logs := observer.New(zapcore.WarnLevel)
			svc.logger = zap.New(core)
			tc.setup(svc)
			unchanged := checkResetStateUnchanged(t, svc)
			_, err := NewProvisioningServiceV2(svc).Unprovision(ctx, req)
			if status.Code(err) != codes.FailedPrecondition || status.Convert(err).Message() != tc.wantStatus {
				t.Fatalf("reset = %v, want the unchanged %q refusal", err, tc.wantStatus)
			}
			unchanged()
			found := false
			for _, entry := range logs.All() {
				if errField, ok := entry.ContextMap()["error"].(string); ok && strings.Contains(errField, tc.wantReason) {
					found = true
				}
			}
			if !found {
				t.Fatalf("refusal reason %q not logged; logs: %v", tc.wantReason, logs.All())
			}
		})
	}
}
