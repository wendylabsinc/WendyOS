//go:build regression_camera_fixture

package ipcam

import (
	"context"

	"go.uber.org/zap"
)

// NewRegressionDiscoverer exposes only discovery's host transport seams to the
// service integration test. The normal build does not contain this constructor.
// Parsing, admission, ONVIF requests, registry updates and liveness policy still
// run through Discoverer.Once; no camera admission decisions are substituted.
func NewRegressionDiscoverer(reg *Registry, logger *zap.Logger, probe func(context.Context) ([][]byte, error), arp string, reachable func(string) bool) *Discoverer {
	d := NewDiscoverer(reg, logger)
	d.probe = probe
	d.arpTable = func() (string, error) { return arp, nil }
	d.reachable = reachable
	return d
}
