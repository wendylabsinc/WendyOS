package twinrobot

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestLocalizationMatchComputeTimeoutRetainsDeadlineIdentity(t *testing.T) {
	err := localizationMatchError(context.Background(), context.Background(), context.DeadlineExceeded)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "five-second compute limit") || !strings.Contains(err.Error(), "no alignment was confirmed") {
		t.Fatalf("misclassified compute timeout: %v", err)
	}
	if strings.Contains(err.Error(), "odometry") || strings.Contains(err.Error(), "LiDAR") {
		t.Fatal("compute limit invented a sensor fault")
	}
}
func TestLocalizationMatchDoesNotRelabelOuterCancellationOrObservationFault(t *testing.T) {
	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	runtime, stop := context.WithCancel(context.Background())
	stop()
	observed := errors.New("waiting for fresh observations")
	for _, test := range []struct {
		request, runtime context.Context
		err              error
	}{
		{expired, context.Background(), context.DeadlineExceeded},
		{context.Background(), runtime, context.DeadlineExceeded},
		{context.Background(), context.Background(), context.Canceled},
		{context.Background(), context.Background(), observed},
	} {
		if got := localizationMatchError(test.request, test.runtime, test.err); got != test.err {
			t.Fatalf("replaced outer cancellation or sensor error: %v", got)
		}
	}
}
