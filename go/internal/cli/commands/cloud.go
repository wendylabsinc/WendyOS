package commands

import (
	"context"
	"os"

	"github.com/spf13/cobra"
)

type cloudDeviceConfig struct {
	CloudGRPC  string
	DeviceName string
	BrokerURL  string
}

type cloudDeviceContextKey struct{}

func newCloudCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cloud",
		Short: "Manage Wendy Cloud resources",
	}

	cmd.AddGroup(
		&cobra.Group{ID: "auth", Title: "Authentication:"},
		&cobra.Group{ID: "applications", Title: "Applications:"},
		&cobra.Group{ID: "devices", Title: "Devices:"},
		&cobra.Group{ID: "connectivity", Title: "Connectivity:"},
	)

	addToGroup := func(groupID string, cmds ...*cobra.Command) {
		for _, c := range cmds {
			c.GroupID = groupID
			cmd.AddCommand(c)
		}
	}

	// Authentication: the common Wendy Cloud auth flow surfaced under
	// `wendy cloud`. These reuse the same constructors as the (now hidden)
	// top-level `wendy auth` command; the advanced session commands
	// (refresh-certs, use, default) remain reachable only via `wendy auth`.
	addToGroup("auth",
		newAuthLoginCmd(),
		newAuthLogoutCmd(),
		newAuthStatusCmd(),
	)
	addToGroup("applications", newCloudAppSettingsCmd())
	// Devices: enroll, discover, and operate cloud-connected devices. The
	// hidden `run` command stays registered (and runnable) but off the help
	// menu; it is hidden via its own constructor.
	addToGroup("devices",
		newCloudEnrollDeviceCmd(),
		newCloudDiscoverCmd(),
		newCloudDeviceCmd(),
		newCloudRunCmd(),
	)
	// Connectivity: networking helpers that reach a device through the cloud.
	addToGroup("connectivity",
		newCloudTunnelCmd(),
		newCloudPingCmd(),
	)
	return cmd
}

func newCloudDeviceCmd() *cobra.Command {
	var cloudGRPC string
	var brokerURL string

	cmd := newDeviceCmd()
	cmd.Short = "Manage WendyOS devices through Wendy Cloud"
	cmd.Long = "Mirror of 'wendy device', but connects to the target device through the Wendy Cloud tunnel broker."
	cmd.PersistentFlags().StringVar(&cloudGRPC, "cloud-grpc", "", "Cloud gRPC endpoint (optional when a default session is set via 'wendy auth use')")
	cmd.PersistentFlags().StringVar(&brokerURL, "broker-url", os.Getenv("WENDY_BROKER_URL"), "Tunnel broker host:port (default: cloud :443 endpoint, otherwise <cloud-host>:50052)")

	wrapCloudDeviceCommands(cmd, func(executing *cobra.Command) cloudDeviceConfig {
		return cloudDeviceConfig{
			CloudGRPC:  resolveCloudGRPCFlag(executing, cloudGRPC),
			DeviceName: deviceFlag,
			BrokerURL:  brokerURL,
		}
	})
	return cmd
}

// resolveCloudGRPCFlag reads --cloud-grpc from the command actually being run,
// falling back to the value bound to this group's persistent flag.
//
// `enroll`, `unenroll`, and `rename` declare their own local --cloud-grpc for
// the cloud-side cleanup they perform. A local flag shadows an inherited
// persistent one of the same name, so under `wendy cloud device` those three
// bound the user's value to their own variable and left this group's persistent
// flag empty. The endpoint documented on the parent was therefore impossible to
// set on exactly the three subcommands that talk to the cloud most.
//
// Looking the flag up on the executing command resolves whichever definition is
// in effect, so one --cloud-grpc means one thing regardless of where it is
// declared.
func resolveCloudGRPCFlag(executing *cobra.Command, fallback string) string {
	if executing == nil {
		return fallback
	}
	if f := executing.Flags().Lookup("cloud-grpc"); f != nil && f.Value.String() != "" {
		return f.Value.String()
	}
	return fallback
}

// effectiveDeviceName prefers a command's own --device value and falls back to the
// root's persistent --device.
//
// `cloud run` and `cloud tunnel` each declare a local --device bound to their own
// variable, which shadows the persistent one. Without this fallback the flag is
// position-dependent: `wendy cloud tunnel --device thor` is honoured while
// `wendy --device thor cloud tunnel` is silently ignored and falls through to the
// interactive picker.
func effectiveDeviceName(local string) string {
	if local != "" {
		return local
	}
	return deviceFlag
}

func wrapCloudDeviceCommands(cmd *cobra.Command, cfg func(*cobra.Command) cloudDeviceConfig) {
	if cmd.RunE != nil {
		runE := cmd.RunE
		cmd.RunE = func(cmd *cobra.Command, args []string) error {
			cmd.SetContext(context.WithValue(cmd.Context(), cloudDeviceContextKey{}, cfg(cmd)))
			return runE(cmd, args)
		}
	}
	if cmd.Run != nil {
		run := cmd.Run
		cmd.Run = func(cmd *cobra.Command, args []string) {
			cmd.SetContext(context.WithValue(cmd.Context(), cloudDeviceContextKey{}, cfg(cmd)))
			run(cmd, args)
		}
	}
	for _, child := range cmd.Commands() {
		wrapCloudDeviceCommands(child, cfg)
	}
}

func cloudDeviceConfigFromContext(ctx context.Context) (cloudDeviceConfig, bool) {
	cfg, ok := ctx.Value(cloudDeviceContextKey{}).(cloudDeviceConfig)
	return cfg, ok
}

func newCloudEnrollDeviceCmd() *cobra.Command {
	var name string
	var cloudGRPC string
	var orgID int32

	cmd := &cobra.Command{
		Use:   "enroll-device",
		Short: "Enroll the connected device with Wendy Cloud or a local pki-core",
		Long:  "Alias for 'wendy device enroll'. Creates an enrollment token using your stored auth session and provisions the connected device with mTLS certificates.",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()

			conn, err := connectToAgent(ctx, SuppressProvisioningHint())
			if err != nil {
				return err
			}
			defer conn.Close()

			promptWifiIfNeeded(ctx, conn)

			auth, err := pickAuthEntry(cloudGRPC)
			if err != nil {
				return err
			}

			return runEnrollDevice(ctx, conn, auth, name, orgID)
		},
	}

	cmd.Flags().StringVar(&name, "name", "", "Device name")
	cmd.Flags().StringVar(&cloudGRPC, "cloud-grpc", "", "Cloud/pki-core gRPC endpoint to use (optional when a default session is set via 'wendy auth use')")
	cmd.Flags().Int32Var(&orgID, "org", 0, "Organization ID to enroll into; skips the interactive org picker (required in non-interactive/--json runs when you belong to multiple orgs)")
	return cmd
}
