package containerd

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
)

var (
	go2VMNamePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)
	go2SourcePattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
)

// Kernel modules are host resources, like the existing camera/audio loopback
// modules. An app must never select the executable, module names or arguments.
// Dependencies are loaded by modprobe from the VM host's own /lib/modules.
var go2LegacyFirewallModules = []string{
	"ip_tables", "iptable_filter", "ip6_tables", "ip6table_filter",
	"xt_string", "xt_comment", "xt_tcpudp",
}

type go2KernelDeps struct {
	goos     string
	readFile func(string) ([]byte, error)
	modprobe func(context.Context, []string) error
}

func prepareGo2KernelModulesForStart(ctx context.Context, labels map[string]string, spec *oci.Spec) error {
	return prepareGo2KernelModules(ctx, labels, spec, go2KernelDeps{
		goos: runtime.GOOS, readFile: os.ReadFile,
		modprobe: func(ctx context.Context, modules []string) error {
			// The one-module form works with both kmod and minimal VM images'
			// modprobe implementations, matching the camera/audio precedent.
			for _, module := range modules {
				output, err := exec.CommandContext(ctx, "modprobe", module).CombinedOutput()
				if err != nil {
					return fmt.Errorf("modprobe %s: %w (%s)", module, err, strings.TrimSpace(string(output)))
				}
			}
			return nil
		},
	})
}

func prepareGo2KernelModules(ctx context.Context, labels map[string]string, spec *oci.Spec, deps go2KernelDeps) error {
	if deps.goos != "linux" || labels[labelKeyServiceName] != "" || spec == nil || spec.Process == nil {
		return nil
	}
	var robot, prefix, otherPrefix string
	switch labels[labelKeyAppID] {
	case go2RuntimeAppID:
		robot, prefix, otherPrefix = "Go2", "GO2_", "G1_"
	case g1RuntimeAppID:
		robot, prefix, otherPrefix = "G1", "G1_", "GO2_"
	default:
		return nil
	}
	env := make(map[string]string)
	for _, entry := range spec.Process.Env {
		if key, value, ok := strings.Cut(entry, "="); ok {
			env[key] = value // OCI environment uses the last occurrence.
		}
	}
	if env[otherPrefix+"VM_NAME"] != "" || env[otherPrefix+"SOURCE_DIGEST"] != "" {
		return fmt.Errorf("managed %s runtime has another robot's VM/source identity", robot)
	}
	if env[prefix+"VM_NAME"] == "" && env[prefix+"SOURCE_DIGEST"] == "" {
		return nil // An unmanaged development runtime has no host preparation.
	}
	if !go2VMNamePattern.MatchString(env[prefix+"VM_NAME"]) || !go2SourcePattern.MatchString(env[prefix+"SOURCE_DIGEST"]) {
		return fmt.Errorf("managed %s runtime has invalid VM/source identity", robot)
	}
	if labels[labelKeyAppVersion] != "0.1.0-"+strings.TrimPrefix(env[prefix+"SOURCE_DIGEST"], "sha256:")[:12] {
		return fmt.Errorf("managed %s runtime version does not match its pinned source", robot)
	}
	hostAdmin := false
	for _, ent := range parseEntitlementsFromAnnotations(labels) {
		if ent.Type == appconfig.EntitlementNetwork && ent.Mode == "host-admin" {
			hostAdmin = true
		}
	}
	if !hostAdmin || spec.Linux == nil || spec.Process.Capabilities == nil {
		return fmt.Errorf("managed %s firewall preparation requires explicit host-admin networking", robot)
	}
	for _, ns := range spec.Linux.Namespaces {
		if ns.Type == "network" {
			return fmt.Errorf("managed %s firewall preparation requires the VM host network namespace", robot)
		}
	}
	for _, caps := range [][]string{spec.Process.Capabilities.Bounding, spec.Process.Capabilities.Effective, spec.Process.Capabilities.Permitted} {
		if !slices.Contains(caps, "CAP_NET_ADMIN") {
			return fmt.Errorf("managed %s firewall preparation requires NET_ADMIN for its bootstrap", robot)
		}
	}
	// Use the same host-owned identity file as AgentService.GetAgentVersion,
	// not an app environment variable, container mount or generic SBC guess.
	deviceType, err := deps.readFile("/etc/wendyos/device-type")
	if err != nil {
		return fmt.Errorf("verifying managed %s VM identity: %w", robot, err)
	}
	if !go2HostIsVM(string(deviceType)) {
		return fmt.Errorf("managed %s firewall preparation is only supported on WendyOS vm-arm64", robot)
	}
	compressed, err := deps.readFile("/proc/config.gz")
	if err != nil {
		return fmt.Errorf("reading VM kernel firewall support: %w", err)
	}
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return fmt.Errorf("reading VM kernel configuration: %w", err)
	}
	defer reader.Close()
	const maxKernelConfig = 4 << 20
	config, err := io.ReadAll(io.LimitReader(reader, maxKernelConfig+1))
	if err != nil || len(config) > maxKernelConfig {
		return fmt.Errorf("VM kernel configuration is unreadable or exceeds %d bytes: %v", maxKernelConfig, err)
	}
	legacy := false
	for _, line := range strings.Split(string(config), "\n") {
		if strings.TrimSpace(line) == "# CONFIG_NF_TABLES is not set" {
			legacy = true
			break
		}
	}
	if !legacy {
		return nil
	}
	// Run before every start, including boot recovery: /run and loaded modules
	// do not survive a VM reboot. modprobe is idempotent for loaded/built-in
	// modules. A missing IPv6 module is a hard failure, never partial isolation.
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := deps.modprobe(ctx, append([]string(nil), go2LegacyFirewallModules...)); err != nil {
		return fmt.Errorf("preparing managed %s legacy IPv4/IPv6 firewall; the VM image must include its kernel modules: %w", robot, err)
	}
	return nil
}

func go2HostIsVM(content string) bool {
	content = strings.TrimSpace(content)
	if !strings.Contains(content, "=") {
		return content == "vm-arm64"
	}
	var board, machine string
	for _, line := range strings.Split(content, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "BOARD":
			board = strings.TrimSpace(value)
		case "MACHINE":
			machine = strings.TrimSpace(value)
		}
	}
	if board != "" {
		return board == "vm-arm64"
	}
	return machine == "vm-arm64-wendyos"
}
