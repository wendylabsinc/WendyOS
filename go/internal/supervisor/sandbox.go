//go:build linux

package supervisor

import (
	"context"
	"errors"
	"os/exec"
	"time"
)

// SandboxedVersion runs `<path> --version` with no capabilities, no sockets
// of any kind, no devices and a read-only system, so a binary that ignores
// --version still cannot change the device. Without systemd-run it refuses.
func SandboxedVersion(ctx context.Context, path string) (string, error) {
	if _, err := exec.LookPath("systemd-run"); err != nil {
		return "", errors.New("systemd-run is not available to sandbox the check")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	args := []string{"--quiet", "--wait", "--pipe", "--collect", "--service-type=exec"}
	for _, p := range sandboxProperties {
		args = append(args, "-p", p)
	}
	out, err := exec.CommandContext(ctx, "systemd-run", append(args, "--", path, "--version")...).Output()
	return string(out), err
}

var sandboxProperties = []string{
	"CapabilityBoundingSet=",
	"NoNewPrivileges=yes",
	"PrivateNetwork=yes",
	// No sockets of any kind, the agent's own unix socket included.
	// systemd-run cannot send RestrictAddressFamilies=none.
	"SystemCallFilter=~@network-io",
	"SystemCallErrorNumber=EPERM",
	"PrivateDevices=yes",
	"PrivateTmp=yes",
	"ProtectSystem=strict",
	"ProtectHome=yes",
	"ProtectKernelTunables=yes",
	"ProtectKernelModules=yes",
	"ProtectControlGroups=yes",
	"RestrictNamespaces=yes",
	"RuntimeMaxSec=5",
}
