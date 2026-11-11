package oci

import (
	"fmt"
	"os"
	"os/user"
	"strconv"
)

// ArduinoRouterDir holds Arduino's router socket on the UNO Q. Containers get
// the directory, not the socket: the router recreates the socket on each start.
var ArduinoRouterDir = "/run/arduino-router"

const (
	ctrArduinoRouterDir    = "/run/arduino-router"
	arduinoRouterAppSocket = "unix:///run/arduino-router/router.sock"
)

var lookupArduinoRouterGroup = user.LookupGroup

// ArduinoBridgeAvailable reports whether this device runs Arduino's router.
func ArduinoBridgeAvailable() bool {
	fi, err := os.Stat(ArduinoRouterDir)
	return err == nil && fi.IsDir()
}

func applyArduinoBridge(spec *Spec) error {
	if !ArduinoBridgeAvailable() {
		return fmt.Errorf("arduino-bridge entitlement requires %s: this device has no Arduino router", ArduinoRouterDir)
	}
	grp, err := lookupArduinoRouterGroup("arduino-router")
	if err != nil {
		return fmt.Errorf("arduino-bridge entitlement: %w", err)
	}
	gid, err := strconv.ParseUint(grp.Gid, 10, 32)
	if err != nil {
		return fmt.Errorf("arduino-bridge entitlement: group arduino-router has gid %q: %w", grp.Gid, err)
	}
	spec.Mounts = append(spec.Mounts, Mount{
		Destination: ctrArduinoRouterDir,
		Source:      ArduinoRouterDir,
		Type:        "bind",
		// Read-only still lets apps connect, but no app can replace the socket.
		Options: []string{"rbind", "nosuid", "nodev", "noexec", "ro"},
	})
	spec.Process.User.AdditionalGids = appendUnique(spec.Process.User.AdditionalGids, uint32(gid))
	if _, set := envValue(spec.Process.Env, "APP_SOCKET"); !set {
		spec.Process.Env = setEnvValue(spec.Process.Env, "APP_SOCKET", arduinoRouterAppSocket)
	}
	return nil
}
