package main

import (
	"context"
	"slices"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/meshsession"
	"github.com/wendylabsinc/wendy/go/internal/agent/services"
	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
)

// meshSensorSources consumes verified, live signed router manifests. The native
// WendyCom endpoint is discovered; SensorLink is its sensors capability. Host
// consumers use the exact signed native-agent endpoint, not an app VIP or an
// additional app TLS tunnel. WendyCom still authenticates the paired principal.
func meshSensorSources(snapshot localmesh.NodeSnapshot) []models.LANDevice {
	var sources []models.LANDevice
	if !snapshot.Self.Tenant().IsValid() {
		return nil
	}
	for _, manifest := range snapshot.Devices {
		if manifest.Platform != localmesh.PlatformLite || manifest.AgentTransport != localmesh.AgentMTLS ||
			!slices.Contains(manifest.Caps, localmesh.CapabilitySensors) || manifest.Withdraw || manifest.Expires <= time.Now().UnixMilli() {
			continue
		}
		owner := meshid.NewDevice(snapshot.Self.Tenant(), manifest.Device)
		if owner == snapshot.Self {
			continue
		}
		address, native, err := meshsession.ResolveAgentMTLS(snapshot, owner, manifest.AgentPort)
		if err != nil || !native {
			continue
		}
		name := manifest.Name
		if name == "" {
			name = owner.UUID().String()
		}
		sources = append(sources, models.LANDevice{
			ID: owner.UUID().String(), DisplayName: name, IPAddress: address.Addr().String(),
			Port: int(address.Port()), IsMTLS: true, Sensorlink: true, WendyLite: true,
			TenantUUID: owner.Tenant().String(), PrincipalURI: owner.Principal(),
			Caps: append([]string(nil), manifest.Caps...), IsWendyDevice: true,
			InterfaceType: string(models.InterfaceLAN),
		})
	}
	return discovery.MergeSensorSources(sources)
}

func browseSensorSources(meshSources func() []models.LANDevice) func(context.Context, time.Duration, func(models.LANDevice) bool) ([]models.LANDevice, error) {
	return browseSensorSourcesWithLAN(discovery.DiscoverSensorSources, meshSources)
}

func browseSensorSourcesWithLAN(browse services.SensorSourceBrowseFunc, meshSources func() []models.LANDevice) services.SensorSourceBrowseFunc {
	return func(ctx context.Context, timeout time.Duration, exclude func(models.LANDevice) bool) ([]models.LANDevice, error) {
		lan, err := browse(ctx, timeout, exclude)
		var mesh []models.LANDevice
		for _, source := range meshSources() {
			if exclude == nil || !exclude(source) {
				mesh = append(mesh, source)
			}
		}
		sources := discovery.MergeSensorSourcesWithMesh(lan, mesh)
		if len(sources) != 0 && ctx.Err() == nil {
			err = nil // A working mesh source remains useful without mDNS.
		}
		return sources, err
	}
}
