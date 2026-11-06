package commands

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// transportForDevice returns the sensor pairing transport to use for a
// discovered source: "wendycom" for a Wendy Lite board (WendyCom, never
// gRPC or raw sensorlink), "grpc" for a WendyOS mTLS agent advertising the
// "sensors" capability, "tcp" for a legacy/MCU sensorlink device.
func transportForDevice(d models.DiscoveredDevice) string {
	if d.WendyLite {
		return "wendycom"
	}
	if d.IsMTLS {
		for _, c := range d.Caps {
			if c == "sensors" {
				return "grpc"
			}
		}
	}
	return "tcp"
}

// orgAllowed permits sensor pairing when the source device's org is one the
// CLI holds an authenticated session in. The user may be logged into several
// orgs at once (e.g. org 2 and org 77); pairing must succeed for a device in
// ANY of them, not just whichever session happens to be the default — the mTLS
// connection to the consumer already used the right org's cert.
func orgAllowed(cliOrgs map[int32]bool, sourceOrg int32) error {
	if cliOrgs[sourceOrg] {
		return nil
	}
	have := make([]int, 0, len(cliOrgs))
	for org := range cliOrgs {
		have = append(have, int(org))
	}
	sort.Ints(have)
	labels := make([]string, len(have))
	for i, org := range have {
		labels[i] = orgLabel("", int32(org))
	}
	yours := "no organizations"
	if len(labels) == 1 {
		yours = "organization " + labels[0]
	} else if len(labels) > 1 {
		yours = "organizations " + strings.Join(labels, ", ")
	}
	return fmt.Errorf("device is in organization %s, but you are logged in to %s; pairing is only allowed within an organization you belong to", orgLabel("", sourceOrg), yours)
}

// discoverLocalSensorSources scans this computer's network for sensor sources:
// WendyOS agents (_wendyos._udp) and Wendy Lite boards (_wendy-lite._tcp). The
// SensorLink tab uses it only when the device's agent predates
// DiscoverSensorSources. Its results may name sources the device cannot
// reach, so the tab labels them.
func discoverLocalSensorSources(ctx context.Context) ([]models.DiscoveredDevice, error) {
	lan, err := discovery.DiscoverSensorSources(ctx, 0, nil)
	if err != nil {
		return nil, fmt.Errorf("discovering devices: %w", err)
	}
	return (&models.DevicesCollection{LANDevices: lan}).MergedDevices(), nil
}

// sensorSourcesFromDevice maps a DiscoverSensorSources reply into the shape
// the SensorLink tab consumes, exactly as if the device's sightings had been
// found locally. The tenant and principal hints are rechecked with the
// helpers discovery.LANDeviceFromWendyLiteService uses, so the tenant gate in
// sensorPairHandler.pair always compares the tenant the principal names.
func sensorSourcesFromDevice(sources []*agentpbv2.DiscoveredSensorSource) []models.DiscoveredDevice {
	lan := make([]models.LANDevice, 0, len(sources))
	for _, s := range sources {
		if s == nil {
			continue
		}
		d := models.LANDevice{
			ID:            s.GetId(),
			DisplayName:   s.GetDisplayName(),
			Hostname:      s.GetHostname(),
			IPAddress:     s.GetIpAddress(),
			Addresses:     s.GetAddresses(),
			Port:          int(s.GetPort()),
			IsMTLS:        s.GetIsMtls(),
			Caps:          s.GetCaps(),
			WendyLite:     s.GetWendyLite(),
			InterfaceType: string(models.InterfaceLAN),
			IsWendyDevice: true,
		}
		d.Sensorlink = slices.Contains(d.Caps, "sensors")
		if s.GetAssetId() > 0 {
			d.AssetID = s.GetAssetId()
		}
		if s.GetOrgId() > 0 {
			d.OrgID = s.GetOrgId()
		}
		d.TenantUUID = discovery.CanonicalTenantUUID(s.GetTenantUuid())
		if d.TenantUUID != "" && discovery.DevicePrincipalInTenant(s.GetPrincipalUri(), d.TenantUUID) {
			d.PrincipalURI = s.GetPrincipalUri()
		}
		lan = append(lan, d)
	}
	return (&models.DevicesCollection{LANDevices: lan}).MergedDevices()
}

// runPicker shows an interactive picker over items and returns the selection,
// mirroring the tea.NewProgram/Selected() pattern in pickCamera (camera_picker.go).
func runPicker(title string, items []tui.PickerItem) (*tui.PickerItem, error) {
	picker := tui.NewPickerWithTitle(title)
	p := tea.NewProgram(picker)

	go func() {
		p.Send(tui.PickerAddMsg{Items: items})
		p.Send(tui.PickerDoneMsg{})
	}()

	finalModel, err := p.Run()
	if err != nil {
		return nil, fmt.Errorf("picker: %w", err)
	}
	pm, ok := finalModel.(tui.PickerModel)
	if !ok {
		return nil, fmt.Errorf("picker: unexpected model type")
	}
	if pm.Cancelled() {
		return nil, ErrUserCancelled
	}
	sel := pm.Selected()
	if sel == nil {
		return nil, ErrUserCancelled
	}
	return sel, nil
}

// cliOrgIDs returns the set of organization IDs the CLI holds an authenticated
// certificate for, across EVERY stored session. Pairing is allowed with a
// device in any of them, so unlike currentCLIOrgID (which read only the default
// session) this never rejects a device the user is legitimately logged in to.
func cliOrgIDs() (map[int32]bool, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, fmt.Errorf("loading config: %w", err)
	}
	orgs := make(map[int32]bool)
	for i := range cfg.Auth {
		for _, c := range cfg.Auth[i].Certificates {
			if c.OrganizationID > 0 {
				orgs[int32(c.OrganizationID)] = true
			}
		}
	}
	if len(orgs) == 0 {
		return nil, config.ErrNotLoggedIn
	}
	return orgs, nil
}

// cleanRPCError maps a gRPC error to a human-readable message so a raw
// "rpc error: code = ..." string never reaches the user.
func cleanRPCError(err error) error {
	switch status.Code(err) {
	case codes.Unavailable:
		return fmt.Errorf("device is not reachable")
	case codes.PermissionDenied:
		return fmt.Errorf("not authorized to pair with this device")
	default:
		return fmt.Errorf("sensor pairing request failed: %s", userFacingGRPCError(err))
	}
}

// pairingName returns the user-supplied name, or a sensible default derived
// from the source device's display name.
func pairingName(name string, source *models.DiscoveredDevice) string {
	if name != "" {
		return name
	}
	return source.DisplayName
}

func newDevicePairCmd() *cobra.Command {
	var (
		listOnly                    bool
		name                        string
		sensors                     []string
		sensorSource                string
		unitreeTab                  bool
		unitreeIP                   string
		unitreeRegion               string
		usbArms                     bool
		bindArm, forgetArm, armName string
		allowLocation               bool
	)
	cmd := &cobra.Command{
		Use:   "pair",
		Short: "Pair or forget Bluetooth, SensorLink, IP cameras, USB arms, and Unitree robots",
		Long:  "Discover, pair, and forget devices in the Bluetooth, SensorLink, IP Cameras, USB Arms, and Unitree tabs. Bluetooth, SensorLink sources, IP cameras, and Unitree Go2 robots are discovered on the selected WendyOS device's network, so you can pair from anywhere; if the device's agent is too old to scan for SensorLink sources, that tab scans your local network instead and says so. Unitree login retrieves your robot's key, which the selected WendyOS device stores and uses to connect. Use --ip to enter a robot's IPv4 address. USB arm pairing saves an unverified host-local binding on the target Agent; it does not bond Bluetooth or start telemetry/control. SensorLink devices must be in an organization you belong to.\n\nTo pair one SensorLink source without the tabs, name it with --sensorlink: its name, source asset ID, or principal URI. An exact asset ID or principal URI wins over a name. Only the device's own scan is searched. --list prints saved pairings, --forget removes one by the pairing name --list shows, its source asset ID, or principal URI, and --json prints each result as JSON. Without a terminal, or with --json, use these or --usb-arms; the tabs need a terminal.",
		Example: "  wendy device pair --device my-wendy\n" +
			"  wendy --json device pair --device my-wendy --list\n" +
			"  wendy --json device pair --device my-wendy --sensorlink garage-cam --name \"Garage camera\"\n" +
			"  wendy --json device pair --device my-wendy --forget \"Garage camera\"\n" +
			"  wendy --json device pair --device my-wendy --usb-arms --list",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			sensorOptions := cmd.Flags().Changed("name") || cmd.Flags().Changed("sensors")
			unitreeFlags := unitreeTab || cmd.Flags().Changed("ip") || cmd.Flags().Changed("unitree-region")
			pairSensor := cmd.Flags().Changed("sensorlink")
			// Without --usb-arms, --forget removes a SensorLink pairing.
			forgetSensor := !usbArms && cmd.Flags().Changed("forget")
			armFlags := usbArms || bindArm != "" || armName != "" || allowLocation
			sensorQuery := sensorSource
			if forgetSensor {
				sensorQuery = forgetArm
			}
			// Before the USB checks, so a --sensorlink conflict is a usage error.
			if err := validateSensorPairFlags(pairSensor, forgetSensor, sensorQuery, listOnly, armFlags, unitreeFlags, sensorOptions); err != nil {
				return err
			}
			if err := validateArmPairFlags(usbArms, listOnly, bindArm, forgetArm, armName, allowLocation, sensorOptions, jsonOutput); err != nil {
				return err
			}
			if usbArms && unitreeFlags {
				return fmt.Errorf("--usb-arms cannot be combined with Unitree flags")
			}
			if cmd.Flags().Changed("ip") {
				var err error
				unitreeIP, err = unitreePairIPv4(unitreeIP)
				if err != nil {
					return fmt.Errorf("--ip: %w", err)
				}
			}
			if unitreeRegion != "global" && unitreeRegion != "cn" {
				return fmt.Errorf("--unitree-region must be global or cn")
			}
			if listOnly && unitreeFlags {
				return fmt.Errorf("--list prints SensorLink pairings and cannot be combined with Unitree flags")
			}
			armScript := usbArms && (listOnly || bindArm != "" || forgetArm != "" || jsonOutput)
			if !armScript && !listOnly && !pairSensor && !forgetSensor && (jsonOutput || !isInteractiveTerminal()) {
				return usageErrorf("the pairing tabs need an interactive terminal; with --json or without one, use --list, --sensorlink <source> or --forget <source> for SensorLink, or --usb-arms")
			}
			ctx := cmd.Context()
			conn, err := connectToAgent(ctx, SuppressProvisioningHint())
			if err != nil {
				return err
			}
			defer conn.Close()

			if armScript {
				return runArmPair(ctx, conn.RobotArmService, bindArm, forgetArm, armName, allowLocation, jsonOutput, cmd.OutOrStdout())
			}
			if listOnly {
				return runSensorList(ctx, conn.SensorPairingService, jsonOutput, cmd.OutOrStdout())
			}
			if pairSensor || forgetSensor {
				sensor := &sensorPairHandler{
					ctx: ctx, client: conn.SensorPairingService,
					orgIDs: cliOrgIDs, tenantIDs: cliTenantIDs,
					name: name, sensors: sensors,
				}
				if forgetSensor {
					return runSensorForget(ctx, sensor, forgetArm, jsonOutput, cmd.OutOrStdout())
				}
				return runSensorPair(ctx, sensor, sensorSource, jsonOutput, cmd.OutOrStdout())
			}

			pairCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			model := newDevicePairModel(
				&btTUIHandler{ctx: pairCtx, agent: conn.AgentService},
				&sensorPairHandler{
					ctx: pairCtx, client: conn.SensorPairingService,
					discoverLocal: discoverLocalSensorSources, orgIDs: cliOrgIDs, tenantIDs: cliTenantIDs,
					name: name, sensors: sensors,
				},
				&cameraPairHandler{ctx: pairCtx, client: conn.VideoService, preflight: func(ctx context.Context) error {
					return cameraServicePreflight(ctx, conn, cameraNetworkManagement)
				}},
				&unitreePairHandler{ctx: pairCtx, client: conn.UnitreeService, region: unitreeRegion, initialIP: unitreeIP, transportError: unitreePairTransportError(conn)},
			)
			model.arm = newArmPairModel(&armPairHandler{ctx: pairCtx, client: conn.RobotArmService, name: armName})
			if usbArms {
				model.active = pairArmTab
			}
			if sensorOptions {
				model.active = pairSensorLinkTab
			}
			if unitreeFlags {
				model.active = pairUnitreeTab
			}
			if _, err := tea.NewProgram(model, tea.WithContext(pairCtx)).Run(); err != nil {
				return fmt.Errorf("device pairing: %w", err)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&listOnly, "list", false, "list current SensorLink pairings (or USB inventory/bindings with --usb-arms)")
	cmd.Flags().StringVar(&name, "name", "", "friendly name for a SensorLink pairing")
	cmd.Flags().StringSliceVar(&sensors, "sensors", nil, "limit a SensorLink pairing to these sensor names (default: all)")
	cmd.Flags().StringVar(&sensorSource, "sensorlink", "", "pair this SensorLink source without the tabs: its name, source asset ID, or principal URI")
	cmd.Flags().BoolVar(&unitreeTab, "unitree", false, "open the Unitree Go2 pairing tab")
	cmd.Flags().StringVar(&unitreeIP, "ip", "", "Unitree Go2 IPv4 address on the selected WendyOS device's network")
	cmd.Flags().StringVar(&unitreeRegion, "unitree-region", "global", "Unitree account region: global or cn")
	cmd.Flags().BoolVar(&usbArms, "usb-arms", false, "select USB arm bindings on the target Agent")
	cmd.Flags().StringVar(&bindArm, "bind", "", "with --usb-arms, bind this inventory ID as an unverified SO-101")
	cmd.Flags().StringVar(&forgetArm, "forget", "", "forget this saved SensorLink pairing by its pairing name, source asset ID, or principal URI (or a USB binding ID with --usb-arms)")
	cmd.Flags().StringVar(&armName, "arm-name", "", "friendly name for a USB arm binding")
	cmd.Flags().BoolVar(&allowLocation, "allow-location-binding", false, "acknowledge a USB binding without serial ID follows its port, including replacements")
	return cmd
}

func validateArmPairFlags(usb, list bool, bind, forget, name string, allowLocation, sensorFlags, jsonOut bool) error {
	// Without --usb-arms, --forget removes a SensorLink pairing.
	if !usb && (bind != "" || name != "" || allowLocation) {
		return fmt.Errorf("USB arm options require --usb-arms")
	}
	if !usb {
		return nil
	}
	if sensorFlags {
		return fmt.Errorf("--usb-arms cannot be combined with SensorLink --name or --sensors")
	}
	if bind != "" && forget != "" || list && (bind != "" || forget != "") {
		return fmt.Errorf("choose one USB arm operation: --list, --bind or --forget")
	}
	if allowLocation && bind == "" {
		return fmt.Errorf("--allow-location-binding requires --bind")
	}
	// --json without --bind lists, which has nothing to name.
	if name != "" && bind == "" && (list || forget != "" || jsonOut) {
		return fmt.Errorf("--arm-name applies to binding only")
	}
	return nil
}

// newDeviceUnpairCmd removes a sensor pairing. With no argument it shows a
// picker of the device's current pairings; with a source asset id it removes
// that pairing directly (handy for scripts).
func newDeviceUnpairCmd() *cobra.Command {
	cmd := &cobra.Command{
		Hidden: true,
		Use:    "unpair [source-asset-id-or-principal]",
		Short:  "Remove a sensor-source pairing",
		Args:   cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			conn, err := connectToAgent(ctx, SuppressProvisioningHint())
			if err != nil {
				return err
			}
			defer conn.Close()

			var assetID int32
			var principal string
			if len(args) == 1 {
				if strings.HasPrefix(args[0], "spiffe://") {
					if _, err := certs.ParseDevicePrincipal(args[0]); err != nil {
						return err
					}
					principal = args[0]
				} else if _, err := fmt.Sscanf(args[0], "%d", &assetID); err != nil {
					return fmt.Errorf("invalid source asset id %q: %w", args[0], err)
				}
			} else {
				// No id given: pick from the device's current pairings.
				resp, err := conn.SensorPairingService.ListSensorPairings(ctx, &agentpbv2.ListSensorPairingsRequest{})
				if err != nil {
					return cleanRPCError(err)
				}
				if len(resp.Pairings) == 0 {
					return fmt.Errorf("no sensor pairings to remove")
				}
				items := make([]tui.PickerItem, 0, len(resp.Pairings))
				for _, p := range resp.Pairings {
					state := "disconnected"
					if p.Connected {
						state = "connected"
					}
					items = append(items, tui.PickerItem{
						Name:     p.Name,
						Address:  fmt.Sprintf("%s · %s", sensorPairLabel(p.SourceAssetId, p.SourcePrincipalUri), state),
						DedupKey: sensorPairKey(p.SourceAssetId, p.SourcePrincipalUri),
						Value:    p,
					})
				}
				sel, err := runPicker("Select a pairing to remove", items)
				if err != nil {
					return err
				}
				selected := sel.Value.(*agentpbv2.SensorPairing)
				assetID, principal = selected.SourceAssetId, selected.SourcePrincipalUri
			}

			if _, err := conn.SensorPairingService.RemoveSensorPairing(ctx, &agentpbv2.RemoveSensorPairingRequest{
				SourceAssetId: assetID, SourcePrincipalUri: principal,
			}); err != nil {
				return cleanRPCError(err)
			}
			cliSuccess("Unpaired %s.", sensorPairLabel(assetID, principal))
			return nil
		},
	}
	return cmd
}

func cliTenantIDs() (map[string]bool, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	tenants := make(map[string]bool)
	for _, auth := range cfg.Auth {
		for _, cert := range auth.Certificates {
			if tenant := cert.TenantUUID(); tenant != "" {
				tenants[tenant] = true
			}
		}
	}
	if len(tenants) == 0 {
		return nil, config.ErrNotLoggedIn
	}
	return tenants, nil
}
