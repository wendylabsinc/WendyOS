//go:build darwin || linux || windows

package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"time"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/onboarding"
)

func installDrives() ([]onboarding.Drive, error) {
	drives, err := listAllDrives()
	if err != nil {
		return nil, err
	}
	out := make([]onboarding.Drive, 0, len(drives))
	for _, d := range drives {
		out = append(out, onboarding.Drive{ID: d.DevicePath, Name: d.Name, Capacity: d.SizeBytes, Removable: d.IsRemovable})
	}
	return out, nil
}

func planOSInstall(ctx context.Context, opts onboarding.Options) (*onboarding.Plan, error) {
	resolve := func(device, version string, nightly bool, pr int) (*deviceManifest, string, error) {
		return resolveDeviceArtifactContext(ctx, device, version, nightly, pr)
	}
	return buildInstallPlan(ctx, opts, resolve, installDrives)
}

func buildInstallPlan(ctx context.Context, o onboarding.Options, resolve func(string, string, bool, int) (*deviceManifest, string, error), drives func() ([]onboarding.Drive, error)) (*onboarding.Plan, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if o.DeviceType == "" {
		return nil, fmt.Errorf("device_type is required; identify the board and carrier before selecting an image")
	}
	p := &onboarding.Plan{DeviceType: o.DeviceType, HostOS: runtime.GOOS, Requirements: []string{}, NextSteps: []string{
		"After writing, follow the board's power/recovery instructions and allow first boot to finish.",
		"Discover the expected device with device_list(scan=true) or wendy discover --json; do not select the first unrelated device.",
		"Use os_install_verify or wendy install verify with an explicit address and the planned OS version and device type. Record the returned public key for subsequent identity checks.",
		"Check enrollment if requested, then deploy a non-motion app and verify its health or ROS interface. Flash completion alone does not verify boot or application behavior.",
	}}
	if o.DeviceType == "unitree-g1" || o.DeviceType == linuxDesktopValue {
		if o.RootfsOnly || o.Drive != "" || o.Storage != "" || o.Version != "" || o.Carrier != "" {
			return nil, fmt.Errorf("agent installation does not accept WendyOS image, carrier, storage, or drive options")
		}
		p.Method, p.EraseScope = "agent", "No OS image write; install the Wendy Agent on the existing Linux system."
		p.Documentation = "https://docs.wendy.dev/latest/installation/linux"
		p.Requirements = []string{"Confirm the target Linux host, SSH access, sudo access, and Internet connectivity.", "Run the agent installer on the target host: curl -fsSL https://install.wendy.dev/agent.sh | bash"}
		if o.DeviceType == "unitree-g1" {
			p.Documentation = "https://docs.wendy.dev/latest/installation/wendy-agent-unitree-g1"
			p.Requirements = append(p.Requirements, "Use the G1 PC2's vendor Ubuntu installation. Do not flash a generic Jetson developer-kit image onto its custom carrier.")
		}
		p.NextSteps = []string{"Discover and verify the explicit target with install verify; agent-only systems do not report a WendyOS version or device type.", "Verify the robot's existing ROS topics and DDS configuration before deploying an application."}
		return p, nil
	}
	jetson := isT234RecoveryDevice(o.DeviceType) || o.DeviceType == thorDeviceType
	if jetson && o.Carrier != "developer-kit" {
		return nil, fmt.Errorf("Jetson image planning requires carrier=developer-kit; for a robot/custom carrier use its vendor installation guide or install the agent on its existing OS")
	}
	if !jetson && o.Carrier != "" {
		return nil, fmt.Errorf("carrier applies only to Jetson developer kits")
	}
	if !jetson && o.DeviceType != "raspberry-pi-3" && o.DeviceType != "raspberry-pi-4" && o.DeviceType != "raspberry-pi-5" {
		return nil, fmt.Errorf("installation planning does not yet cover %q; use wendy install --help and the board-specific installation guide", o.DeviceType)
	}
	if o.RootfsOnly && !isT234RecoveryDevice(o.DeviceType) {
		return nil, fmt.Errorf("rootfs_only applies only to Orin")
	}
	if o.Storage != "" && o.Storage != "sd" && o.Storage != "nvme" && o.Storage != "emmc" {
		return nil, fmt.Errorf("storage must be sd, nvme, or emmc")
	}
	dm, ver, err := resolve(o.DeviceType, o.Version, false, 0)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", onboarding.ErrArtifactUnavailable, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	v, ok := dm.Versions[ver]
	if !ok {
		return nil, fmt.Errorf("version %s not found for %s", ver, o.DeviceType)
	}
	if err := checkInstallMode(ver, v.InstallMode); err != nil {
		return nil, err
	}
	p.Version, p.Storage = ver, o.Storage
	p.Command = []string{"wendy", "install", "--device-type", o.DeviceType, "--version", ver}
	p.Requirements = append(p.Requirements, "Run the returned argument array in a terminal; the installer handles disk/USB elevation and progress. Recheck target identity immediately before writing.", "Choose a device name and first-boot networking. Add --device-name and WiFi options, or --no-wifi for Ethernet. Cloud pre-enrollment is optional and requires login.")
	p.Documentation = "https://docs.wendy.dev/latest/installation/wendyos-raspberry-pi-5"
	if jetson {
		p.Documentation = "https://docs.wendy.dev/latest/installation/wendyos-nvidia-jetson-orin-nano"
	}
	if o.DeviceType == thorDeviceType || (isT234RecoveryDevice(o.DeviceType) && !o.RootfsOnly) {
		if o.Drive != "" {
			return nil, fmt.Errorf("USB recovery targets the connected Jetson, not a host drive; remove drive or explicitly choose rootfs_only")
		}
		p.Method, p.EraseScope = "usb-recovery", "Replaces boot firmware and OS storage on the selected Jetson; existing data is erased."
		p.Requirements = append(p.Requirements, "Put the intended board into force recovery using its board-specific instructions. Attach a USB data cable and disconnect other recovery-mode Jetsons before a non-interactive install.")
		if o.DeviceType == thorDeviceType {
			if o.Storage != "" {
				return nil, fmt.Errorf("Thor flashpacks choose storage; omit storage")
			}
			if v.FlashpackPath == "" || v.FlashpackChecksum == "" || v.FlashpackSizeBytes <= 0 {
				return nil, fmt.Errorf("version %s has no complete Thor flashpack", ver)
			}
			p.ArtifactURL, p.ArtifactSHA256 = gcsBaseURL+"/"+v.FlashpackPath, v.FlashpackChecksum
			p.Documentation = "https://docs.wendy.dev/latest/installation/wendyos-nvidia-jetson-agx-thor"
		} else {
			if v.InstallMode != "recovery" {
				return nil, fmt.Errorf("version %s has no full recovery installation; choose a recovery release or explicitly use rootfs_only with compatible boot firmware", ver)
			}
			if p.Storage == "" && o.DeviceType == orinNanoDeviceType {
				p.Storage = "nvme"
			}
			if p.Storage == "" {
				return nil, fmt.Errorf("choose nvme or emmc storage for AGX Orin")
			}
			if p.Storage != "nvme" && !(o.DeviceType == orinDeviceType && p.Storage == "emmc") {
				return nil, fmt.Errorf("unsupported recovery storage %q for %s", p.Storage, o.DeviceType)
			}
			artifact, err := getRecoveryFlashpackInfo(dm, o.DeviceType, ver, p.Storage)
			if err != nil {
				return nil, err
			}
			p.ArtifactURL, p.ArtifactSHA256 = artifact.URL, artifact.Checksum
			p.Command = append(p.Command, "--storage", p.Storage, "--rootfs-only=false")
		}
		return p, nil
	}
	p.Method, p.EraseScope = "removable-media", "Erases all partitions and data on the selected drive."
	if p.Storage == "" {
		p.Storage = "sd"
		if jetson {
			p.Storage = "nvme"
		}
	}
	if p.Storage == "emmc" || (o.DeviceType == orinDeviceType && p.Storage != "nvme") {
		return nil, fmt.Errorf("unsupported raw-media storage %q for %s", p.Storage, o.DeviceType)
	}
	var artifact *imageInfo
	if o.RootfsOnly {
		p.Method = "rootfs-only"
		p.EraseScope += " Jetson QSPI boot firmware is unchanged."
		p.Requirements = append(p.Requirements, "Verify installed QSPI/boot firmware is compatible with the selected release; a successful rootfs write cannot establish this.")
		p.Command = append(p.Command, "--rootfs-only")
	}
	if o.RootfsOnly && v.InstallMode == "recovery" {
		artifact, err = getRootfsOnlyImageInfo(dm, ver, p.Storage)
	} else {
		artifact, err = getImageInfo(dm, ver, p.Storage)
	}
	if err != nil {
		return nil, err
	}
	p.ArtifactURL, p.ArtifactSHA256 = artifact.DownloadURL, artifact.Checksum
	if p.ArtifactURL == "" {
		p.ArtifactURL, p.ArtifactSHA256 = artifact.ZstURL, artifact.ZstChecksum
	}
	p.Command = append(p.Command, "--storage", p.Storage)
	if o.Drive == "" {
		p.Command = nil
		p.Requirements = append(p.Requirements, "List host drives using os_list_drives or wendy os list-drives --all --json; select by path, model and capacity, then request a plan with drive set.")
		return p, nil
	}
	available, err := drives()
	if err != nil {
		return nil, err
	}
	for _, d := range available {
		if d.ID != o.Drive {
			continue
		}
		p.Target = &d
		if d.Capacity <= 0 || (artifact.ImageSize > 0 && d.Capacity < artifact.ImageSize) {
			return nil, fmt.Errorf("drive %s is too small or its capacity is unknown", o.Drive)
		}
		p.Command = append(p.Command, "--drive", d.ID)
		if !d.Removable {
			p.Requirements = append(p.Requirements, "This drive is reported non-removable. The installer requires its internal-drive confirmation; do not add --yes-overwrite-internal unless wiping this exact drive was authorized.")
		}
		return p, nil
	}
	return nil, fmt.Errorf("drive %q is not in the current writable-drive inventory; relist drives", o.Drive)
}

func newOSInstallPlanCmd() *cobra.Command {
	var o onboarding.Options
	cmd := &cobra.Command{Use: "plan", Short: "Describe an installation without writing disks (JSON)", Args: cobra.NoArgs,
		// Planning needs only the public manifest and host inventory. Do not run
		// config migration, MCP auto-setup or analytics initialization here.
		PersistentPreRunE:  func(*cobra.Command, []string) error { return nil },
		PersistentPostRunE: func(*cobra.Command, []string) error { return nil },
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, err := planOSInstall(cmd.Context(), o)
			if err != nil {
				return err
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(p)
		},
	}
	cmd.Flags().StringVar(&o.DeviceType, "device-type", "", "Board type, unitree-g1, or linux-desktop")
	cmd.Flags().StringVar(&o.Carrier, "carrier", "", "Jetson carrier: developer-kit (custom carriers require their vendor guide)")
	cmd.Flags().StringVar(&o.Version, "version", "", "Exact WendyOS release (default latest stable)")
	cmd.Flags().StringVar(&o.Storage, "storage", "", "Target storage: sd, nvme, or emmc")
	cmd.Flags().StringVar(&o.Drive, "drive", "", "Exact host drive path from list-drives")
	cmd.Flags().BoolVar(&o.RootfsOnly, "rootfs-only", false, "Plan a rootfs write without changing Orin boot firmware")
	return cmd
}

func newOSInstallVerifyCmd() *cobra.Command {
	o := onboarding.VerifyOptions{Timeout: 15 * time.Second}
	cmd := &cobra.Command{Use: "verify", Short: "Check first boot at an explicit address (JSON)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if os.Getenv("WENDY_AGENT_SOCKET") != "" {
				return fmt.Errorf("run installation verification from the host, without WENDY_AGENT_SOCKET overriding the target")
			}
			v, err := onboarding.Verify(cmd.Context(), o, connectMCPDevice)
			if err != nil {
				return err
			}
			if err := json.NewEncoder(cmd.OutOrStdout()).Encode(v); err != nil {
				return err
			}
			if !v.Verified {
				return fmt.Errorf("installation verification failed; see the JSON problems field")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&o.Address, "address", "", "Explicit device hostname, IP:port, or cloud selector")
	cmd.Flags().StringVar(&o.OSVersion, "expected-os-version", "", "Require the installed WendyOS version")
	cmd.Flags().StringVar(&o.DeviceType, "expected-device-type", "", "Require the reported WendyOS board type")
	cmd.Flags().StringVar(&o.PublicKey, "expected-public-key", "", "Require a previously recorded device identity")
	cmd.Flags().BoolVar(&o.RequireEnrollment, "require-enrollment", false, "Fail unless cloud enrollment is verified")
	cmd.Flags().DurationVar(&o.Timeout, "timeout", o.Timeout, "RPC deadline, at most one minute")
	return cmd
}

func printFirstBootVerification(deviceType, version string) {
	fmt.Println("First boot has not been verified. Run wendy discover --json and identify the expected device.")
	fmt.Printf("Then run: wendy install verify --address <expected-name-or-IP> --expected-os-version %q --expected-device-type %q\n", version, deviceType)
	fmt.Println("Add --require-enrollment if requested. Application readiness needs a separate deployment and health check.")
}
