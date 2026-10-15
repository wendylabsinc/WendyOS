package services

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/meshsession"
)

func TestSwitchingMeshDialerUsesLiveOptIn(t *testing.T) {
	legacy, probes := testDialer()
	legacy.SetLocalMeshDialer(func(context.Context, int32, uint16) (net.Conn, error) {
		return nil, meshsession.ErrDenied
	})
	enabled := true
	d := &SwitchingMeshDialer{Legacy: legacy, Local: NewMeshAppDialer(legacy), LocalEnabled: func() bool { return enabled }}
	if _, _, err := d.DialDevice(context.Background(), 215, 8080); !errors.Is(err, meshsession.ErrDenied) {
		t.Fatalf("local opt-in denial = %v", err)
	}
	if probes.lookups != 0 || probes.lanDials != 0 || probes.brokerDials != 0 {
		t.Fatalf("local mode used a legacy fallback: %+v", probes)
	}
	enabled = false
	_, _, _ = d.DialDevice(context.Background(), 215, 8080)
	if probes.lookups == 0 {
		t.Fatal("opt-out did not select legacy path")
	}
}
