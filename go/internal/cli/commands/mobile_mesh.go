package commands

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/providers"
	"github.com/wendylabsinc/wendy/go/internal/cli/sessionbroker"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
)

func mobileMeshDiscoveryStream(ctx context.Context) <-chan discovery.LANEvent {
	out := make(chan discovery.LANEvent)
	go func() {
		defer close(out)
		// A foreground app can outlive its operator certificate. Reuse CLI
		// renewal, including serialized OAuth rotation, alongside direct LAN
		// discovery so an offline CA cannot hold up local device browsing.
		// Broker startup reports unusable credentials through mesh status.
		refresh, cancel := context.WithTimeout(ctx, 5*time.Second)
		_, _, _ = prepareMeshCertificate(refresh, "")
		cancel()
		for event := range meshDiscoveryStream(ctx) {
			select {
			case out <- event:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out
}

// MobileUseAccount selects an exact saved context in the app's own sandbox.
// Call only after joining the previous embedded broker and device connections.
func MobileUseAccount(name, organization string) error {
	return config.Update(func(cfg *config.Config) (bool, error) {
		selected, ok := cfg.ContextByName(name)
		if !ok {
			return false, fmt.Errorf("this saved account is no longer available; sign in again")
		}
		if organization != "" && (len(selected.Certificates) == 0 || selected.Certificates[0].TenantUUID() != organization) {
			return false, fmt.Errorf("sign in to this organization before joining its Wendy Net")
		}
		changed := cfg.CurrentContext != name
		cfg.CurrentContext = name
		return changed, nil
	})
}

// DesktopMeshDevices uses the same fresh, authenticated directory as CLI
// discovery. In particular, Lite management targets must not be filtered out.
func DesktopMeshDevices(snapshot sessionbroker.OperatorDirectory) []DesktopDevice {
	rows, _ := meshCollection(snapshot)
	result := make([]DesktopDevice, 0, len(rows))
	for _, row := range rows {
		lite := row.Platform == "Lite"
		if lite {
			// Match the provider's signed native WendyCom admission requirements.
			var info *sessionbroker.OperatorTargetInfo
			for i := range snapshot.Devices {
				if snapshot.Devices[i].Selector == row.Selector {
					info = &snapshot.Devices[i]
					break
				}
			}
			if info == nil {
				continue
			}
			if _, err := providers.MeshLiteDevice(*info); err != nil {
				continue
			}
		}
		result = append(result, DesktopDevice{ID: row.Selector, Name: row.Name, Selector: row.Selector, Transport: "Mesh", Lite: lite})
	}
	return result
}

// MobileMeshSummary observes the existing broker without starting radios or
// logging in. Failure stays visible rather than looking like an empty mesh.
func MobileMeshSummary(ctx context.Context) (string, string) {
	cfg, err := config.Load()
	if err != nil {
		return "unavailable", err.Error()
	}
	if _, ok := selectedMeshCertificate(cfg, ""); !ok {
		return "signed-out", "Sign in to join your organization's Wendy Net."
	}
	if active, failure := sessionbroker.EmbeddedStatus(); !active {
		return "paused", "Wendy Net paused in the background."
	} else if failure != nil {
		return "unavailable", failure.Error()
	}
	view, err := sessionbroker.ReadOperatorDirectory(ctx)
	if err != nil {
		return "searching", "Searching for an authenticated Wendy Net peer over LAN or Bluetooth."
	}
	devices := DesktopMeshDevices(view)
	if view.State != "ready" {
		return view.State, "Waiting for authenticated mesh routes."
	}
	topology, err := sessionbroker.ReadOperatorTopology(ctx)
	if err != nil || len(topology.GetPeers()) == 0 {
		return "searching", "Searching for an authenticated Wendy Net peer over LAN or Bluetooth."
	}
	carriers := map[string]bool{}
	for _, peer := range topology.GetPeers() {
		switch peer.GetTransport() {
		case "tcp":
			carriers["LAN"] = true
		case "ble":
			carriers["Bluetooth"] = true
		}
	}
	names := make([]string, 0, len(carriers))
	for name := range carriers {
		names = append(names, name)
	}
	sort.Strings(names)
	return "ready", fmt.Sprintf("Wendy Net connected via %s · %d reachable devices", strings.Join(names, " + "), len(devices))
}
