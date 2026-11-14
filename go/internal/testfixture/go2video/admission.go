package go2video

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// KernelAdmission is checked again at the kernel mutation boundary. The VM
// caller already rejected container execution before entering its owned PID
// namespace; identity and the actual hypervisor are checked again here.
func KernelAdmission() error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("kernel fixture requires guest root")
	}
	if os.Getenv("GITHUB_ACTIONS") == "true" && os.Getenv("RUNNER_ENVIRONMENT") == "github-hosted" {
		return nil
	}
	uuid := regexp.MustCompile(`^[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}$`)
	previous := ""
	for _, pair := range [][2]string{{"WENDY_GO2_VM_BOOT_ID", "/proc/sys/kernel/random/boot_id"}, {"WENDY_GO2_VM_PRODUCT_UUID", "/sys/class/dmi/id/product_uuid"}} {
		expected := strings.ToLower(os.Getenv(pair[0]))
		actual, err := os.ReadFile(pair[1])
		if err != nil || !uuid.MatchString(expected) || strings.Trim(expected, "0-") == "" || strings.Trim(expected, "f-") == "" || expected == previous || strings.ToLower(strings.TrimSpace(string(actual))) != expected {
			return fmt.Errorf("kernel fixture VM identity not confirmed")
		}
		previous = expected
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	actual, err := exec.CommandContext(ctx, "systemd-detect-virt", "--vm").Output()
	if err != nil {
		return fmt.Errorf("cannot verify replay VM: %w", err)
	}
	switch strings.TrimSpace(string(actual)) {
	case "kvm", "qemu", "vmware", "oracle", "microsoft", "xen", "parallels":
		return nil
	}
	return fmt.Errorf("unrecognized replay VM")
}
