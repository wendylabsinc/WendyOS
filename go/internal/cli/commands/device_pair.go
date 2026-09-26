package commands

import (
	"context"
	"fmt"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
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
		labels[i] = fmt.Sprintf("%d", org)
	}
	yours := "no organizations"
	if len(labels) == 1 {
		yours = "organization " + labels[0]
	} else if len(labels) > 1 {
		yours = "organizations " + strings.Join(labels, ", ")
	}
	return fmt.Errorf("device is in organization %d, but you are logged in to %s; pairing is only allowed within an organization you belong to", sourceOrg, yours)
}

// discoverWendyLiteSensorSources browses _wendy-lite._tcp and returns every
// sighting as a models.LANDevice. discovery.Discover never finds these
// boards, since it only browses _wendyos._udp.
func discoverWendyLiteSensorSources(ctx context.Context) ([]models.LANDevice, error) {
	svcs, err := discovery.BrowseMDNSServices(ctx, discovery.WendyLiteServiceType, 0)
	if err != nil {
		return nil, err
	}
	devices := make([]models.LANDevice, 0, len(svcs))
	for _, svc := range svcs {
		devices = append(devices, discovery.LANDeviceFromWendyLiteService(svc))
	}
	return devices, nil
}

// discoverSensorSources runs LAN discovery and returns the merged device list
// for the caller to filter down to sensor sources. WendyOS agents
// (_wendyos._udp) and Wendy Lite boards (_wendy-lite._tcp) are two separate
// mDNS services, so Wendy Lite is browsed and merged in alongside the main
// LAN scan; a failure to browse it is non-fatal (the picker just shows
// whatever it found among WendyOS agents).
func discoverSensorSources(ctx context.Context) ([]models.DiscoveredDevice, error) {
	collection, err := discovery.Discover(ctx, discovery.DiscoveryOptions{
		Types: []models.InterfaceType{models.InterfaceLAN},
	})
	if err != nil {
		return nil, fmt.Errorf("discovering devices: %w", err)
	}
	if liteDevices, err := discoverWendyLiteSensorSources(ctx); err == nil {
		collection.LANDevices = append(collection.LANDevices, liteDevices...)
	}
	return collection.MergedDevices(), nil
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
		listOnly bool
		name     string
		sensors  []string
	)
	cmd := &cobra.Command{
		Use:   "pair",
		Short: "Pair or forget Bluetooth, SensorLink, and IP camera devices",
		Long:  "Discover, pair, and forget devices in the Bluetooth, SensorLink, and IP Cameras tabs. Bluetooth and IP cameras are discovered on the target device; SensorLink discovers sources on your local network. SensorLink devices must be in an organization you belong to.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			conn, err := connectToAgent(ctx, SuppressProvisioningHint())
			if err != nil {
				return err
			}
			defer conn.Close()

			if listOnly {
				resp, err := conn.SensorPairingService.ListSensorPairings(ctx, &agentpbv2.ListSensorPairingsRequest{})
				if err != nil {
					return cleanRPCError(err) // maps codes to human text, never raw "rpc error: code ="
				}
				for _, p := range resp.Pairings {
					state := "disconnected"
					if p.Connected {
						state = "connected"
					}
					cliLogln("%-20s asset %d  %s", p.Name, p.SourceAssetId, state)
				}
				return nil
			}

			pairCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			model := newDevicePairModel(
				&btTUIHandler{ctx: pairCtx, agent: conn.AgentService},
				&sensorPairHandler{
					ctx: pairCtx, client: conn.SensorPairingService,
					discover: discoverSensorSources, orgIDs: cliOrgIDs,
					name: name, sensors: sensors,
				},
				&cameraPairHandler{ctx: pairCtx, client: conn.VideoService},
			)
			if cmd.Flags().Changed("name") || cmd.Flags().Changed("sensors") {
				model.active = pairSensorLinkTab
			}
			if _, err := tea.NewProgram(model, tea.WithContext(pairCtx)).Run(); err != nil {
				return fmt.Errorf("device pairing: %w", err)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&listOnly, "list", false, "list current SensorLink pairings")
	cmd.Flags().StringVar(&name, "name", "", "friendly name for a SensorLink pairing")
	cmd.Flags().StringSliceVar(&sensors, "sensors", nil, "limit a SensorLink pairing to these sensor names (default: all)")
	return cmd
}

// newDeviceUnpairCmd removes a sensor pairing. With no argument it shows a
// picker of the device's current pairings; with a source asset id it removes
// that pairing directly (handy for scripts).
func newDeviceUnpairCmd() *cobra.Command {
	cmd := &cobra.Command{
		Hidden: true,
		Use:    "unpair [source-asset-id]",
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
			if len(args) == 1 {
				if _, err := fmt.Sscanf(args[0], "%d", &assetID); err != nil {
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
						Address:  fmt.Sprintf("asset %d · %s", p.SourceAssetId, state),
						DedupKey: fmt.Sprintf("asset-%d", p.SourceAssetId),
						Value:    p.SourceAssetId,
					})
				}
				sel, err := runPicker("Select a pairing to remove", items)
				if err != nil {
					return err
				}
				assetID = sel.Value.(int32)
			}

			if _, err := conn.SensorPairingService.RemoveSensorPairing(ctx, &agentpbv2.RemoveSensorPairingRequest{
				SourceAssetId: assetID,
			}); err != nil {
				return cleanRPCError(err)
			}
			cliSuccess("Unpaired asset %d.", assetID)
			return nil
		},
	}
	return cmd
}
