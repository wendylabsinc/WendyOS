package network

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/go/internal/shared/nmcli"
)

var (
	osReleasePath = "/etc/os-release"
	// authRepairMarker lives in the persistent state dir, so the repair runs once
	// per device rather than once per boot.
	authRepairMarker = "/var/lib/wendy/wifi-auth-alg-cleared"
	authRepairRetry  = 10 * time.Second
)

// RepairForcedOpenAuthOnce clears auth-alg=open from saved WPA profiles once
// per WendyOS device, retrying until ctx ends while a pass is incomplete.
func (n *NMCLINetworkManager) RepairForcedOpenAuthOnce(ctx context.Context) {
	if !onWendyOS() {
		return
	}
	if _, err := os.Stat(authRepairMarker); err == nil {
		return
	}
	for !n.repairForcedOpenAuth(ctx) {
		select {
		case <-ctx.Done():
			n.logger.Warn("WiFi profile repair incomplete; retrying on the next start")
			return
		case <-time.After(authRepairRetry):
		}
	}
	if err := os.MkdirAll(filepath.Dir(authRepairMarker), 0o755); err != nil {
		n.logger.Warn("Recording the WiFi profile repair failed", zap.Error(err))
		return
	}
	if err := os.WriteFile(authRepairMarker, nil, 0o644); err != nil {
		n.logger.Warn("Recording the WiFi profile repair failed", zap.Error(err))
	}
}

// repairForcedOpenAuth clears auth-alg=open from saved WPA profiles and reports
// whether it read every profile and cleared every one it had to.
func (n *NMCLINetworkManager) repairForcedOpenAuth(ctx context.Context) bool {
	uuids, err := wifiProfileUUIDs(ctx, n.nmcliPath)
	if err != nil {
		n.logger.Warn("Listing WiFi profiles failed", zap.Error(err))
		return false
	}
	profiles, err := readWiFiProfiles(ctx, n.nmcliPath, uuids)
	if err != nil {
		n.logger.Warn("Reading WiFi profiles failed", zap.Error(err))
		return false
	}
	complete := true
	for _, uuid := range uuids {
		p, ok := profiles[uuid]
		if !ok || p.AuthAlg != "open" || (p.KeyMgmt != "wpa-psk" && p.KeyMgmt != "sae") {
			continue
		}
		cmd := nmcli.Command(ctx, n.nmcliPath, "connection", "modify", uuid, authAlgField, "")
		if out, err := cmd.CombinedOutput(); err != nil {
			n.logger.Warn("Clearing forced open authentication failed",
				zap.String("uuid", uuid),
				zap.String("output", strings.TrimSpace(string(out))),
				zap.Error(err))
			complete = false
			continue
		}
		n.logger.Info("Cleared forced open authentication from WiFi profile", zap.String("uuid", uuid))
	}
	return complete
}

// onWendyOS reports whether the host runs WendyOS, whose WiFi profiles the agent
// manages; on other hosts they belong to the user.
func onWendyOS() bool {
	data, err := os.ReadFile(osReleasePath)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "ID="); ok {
			return strings.Trim(v, `"'`) == "wendyos"
		}
	}
	return false
}
