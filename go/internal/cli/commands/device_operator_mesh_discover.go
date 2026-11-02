package commands

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"time"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/netmesh/carriers"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

// Command tests replace these dependencies without a radio or profile mutation.
var discoverOperatorBLE = carriers.DiscoverBLE
var operatorBLEDiscoveryIdentity = func() (meshid.Device, error) {
	cfg, err := config.Load()
	if err != nil {
		return meshid.Device{}, err
	}
	auth, err := config.ResolveAuth(cfg, "", nil)
	if err != nil {
		return meshid.Device{}, err
	}
	if len(auth.Certificates) == 0 {
		return meshid.Device{}, errors.New("current context has no primary operator certificate")
	}
	chain, err := certs.ParseCertsFromPEM([]byte(auth.Certificates[0].PemCertificate))
	if err != nil || len(chain) == 0 {
		return meshid.Device{}, errors.New("selected primary operator certificate is missing")
	}
	peer, err := meshid.PeerFromCert(chain[0])
	if err != nil || !peer.IsOperator() {
		return meshid.Device{}, errors.New("selected certificate has no operator principal")
	}
	return peer, nil
}

type operatorBLEHint struct {
	Address       string `json:"address"`
	AddressKind   string `json:"addressKind"`
	PSM           uint16 `json:"psm"`
	DeviceHint    string `json:"deviceHint"`
	NetworkHint   string `json:"networkHint"`
	Capabilities  byte   `json:"capabilities"`
	Authenticated bool   `json:"authenticated"`
}

func newDeviceOperatorMeshDiscoverCmd() *cobra.Command {
	var transport string
	var timeout time.Duration
	cmd := &cobra.Command{Use: "discover", Short: "List unverified native BLE mesh candidates", Long: "Scan the current tenant's native BLE mesh advertisements without connecting or starting an operator router. Listed addresses are platform-native radio identifiers: CoreBluetooth peripheral UUIDs on macOS and LE addresses on Linux. A short advertised device hint is not the enrolled device UUID or an identity proof. Attachments still verify the exact --router device UUID and primary TLS identity. Some candidates may not support the WendyOS operator-router role.", Args: cobra.NoArgs}
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		if transport != carriers.BLE {
			return errors.New("mesh candidate discovery currently supports --transport ble")
		}
		if timeout <= 0 || timeout > 30*time.Second {
			return errors.New("--timeout must be greater than zero and at most 30s")
		}
		self, err := operatorBLEDiscoveryIdentity()
		if err != nil {
			return err
		}
		candidates, err := discoverOperatorBLE(cmd.Context(), self, timeout)
		if err != nil {
			return err
		}
		kind := "le-address"
		if runtime.GOOS == "darwin" {
			kind = "corebluetooth-peripheral-uuid"
		}
		rows := make([]operatorBLEHint, 0, len(candidates))
		for _, candidate := range candidates {
			rows = append(rows, operatorBLEHint{Address: candidate.Address, AddressKind: kind, PSM: candidate.PSM, DeviceHint: hex.EncodeToString(candidate.Hint[:]), NetworkHint: hex.EncodeToString(candidate.NetworkHash[:]), Capabilities: candidate.Capabilities})
		}
		if jsonOutput {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(struct {
				Version    int               `json:"version"`
				Transport  string            `json:"transport"`
				Tenant     string            `json:"tenant"`
				HintsOnly  bool              `json:"hintsOnly"`
				Candidates []operatorBLEHint `json:"candidates"`
			}{1, carriers.BLE, self.Tenant().String(), true, rows})
		}
		for _, row := range rows {
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s  PSM 0x%04x  device hint %s (unverified)\n", discovery.SanitiseDisplayName(row.Address), row.PSM, row.DeviceHint); err != nil {
				return err
			}
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), "Use the listed --address and --psm with connect --transport ble and the enrolled device's full --router UUID. Radio hints are not authenticated device identities.")
		return err
	}
	cmd.Flags().StringVar(&transport, "transport", carriers.BLE, "Candidate carrier to scan (ble)")
	cmd.Flags().DurationVar(&timeout, "timeout", 5*time.Second, "Bounded BLE observation window (maximum 30s)")
	return cmd
}
