package services

import (
	"context"
	"net"
)

// SwitchingMeshDialer reads the local-mesh opt-in at dial time so an atomic
// CLI configuration change takes effect without restarting the app proxy.
type SwitchingMeshDialer struct {
	Legacy       *MeshDialer
	Local        *MeshAppDialer
	LocalEnabled func() bool
}

func (d *SwitchingMeshDialer) DialDevice(ctx context.Context, asset int32, port uint16) (net.Conn, string, error) {
	if d.LocalEnabled != nil && d.LocalEnabled() {
		return d.Local.DialDevice(ctx, asset, port)
	}
	return d.Legacy.DialDevice(ctx, asset, port)
}
