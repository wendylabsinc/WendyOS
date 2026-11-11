package main

import (
	"errors"
	"fmt"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/mesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/meshsession"
)

func TestApplicationUDPCloudFallbackOnlyForMissingRoute(t *testing.T) {
	cloudResult := errors.New("Cloud result")
	for _, tt := range []struct {
		name      string
		localErr  error
		wantCloud bool
	}{
		{"local success", nil, false},
		{"no initial route", meshsession.ErrNoRoute, true},
		{"route withdrawn during setup", fmt.Errorf("rechecking route: %w", meshsession.ErrNoRoute), true},
		{"port refused", meshsession.ErrDenied, false},
		{"transport unsupported", meshsession.ErrUDPUnsupported, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cloudCalled := false
			_, err := dialApplicationUDPWithCloudFallback(
				func() (mesh.UDPFlow, error) { return nil, tt.localErr },
				func() (mesh.UDPFlow, error) { cloudCalled = true; return nil, cloudResult },
			)
			wantErr := tt.localErr
			if tt.wantCloud {
				wantErr = cloudResult
			}
			if cloudCalled != tt.wantCloud || err != wantErr {
				t.Fatalf("Cloud called = %v, error = %v; want %v, %v", cloudCalled, err, tt.wantCloud, wantErr)
			}
		})
	}
}
