package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/protobuf/proto"
)

// Scripted SensorLink operations: --list, --sensorlink <source> and
// --forget <source>. They run the SensorLink tab's discovery, checks and RPCs
// without its screen, and change at most the one source they name.

// validateSensorPairFlags checks the scripted SensorLink operations. pair is
// whether --sensorlink was given and forget whether --forget was given
// without --usb-arms; source is the value of the one given. arms is whether
// --usb-arms or a USB arm option was given. options is whether --name or
// --sensors was given; they configure a pair.
func validateSensorPairFlags(pair, forget bool, source string, list, arms, unitree, options bool) error {
	switch {
	case !pair && !forget:
		return nil
	case pair && arms:
		return usageErrorf("--sensorlink cannot be combined with --usb-arms or USB arm options")
	case unitree:
		return usageErrorf("--sensorlink and SensorLink --forget cannot be combined with Unitree flags")
	case pair && forget, list:
		return usageErrorf("choose one SensorLink operation: --list, --sensorlink or --forget")
	case strings.TrimSpace(source) == "":
		return usageErrorf("name a SensorLink source by its name, source asset ID or principal URI")
	case forget && options:
		return usageErrorf("--name and --sensors apply to --sensorlink, not --forget")
	}
	return nil
}

// runSensorList prints the target's saved SensorLink pairings, one line
// each, or with jsonOut the agent's ListSensorPairings reply.
func runSensorList(ctx context.Context, client agentpbv2.WendySensorPairingServiceClient, jsonOut bool, out io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	resp, err := client.ListSensorPairings(ctx, &agentpbv2.ListSensorPairingsRequest{})
	if err != nil {
		return cleanRPCError(err) // maps codes to human text, never raw "rpc error: code ="
	}
	if jsonOut {
		return writePairProtoJSON(out, resp)
	}
	for _, p := range resp.GetPairings() {
		state := "disconnected"
		if p.Connected {
			state = "connected"
		}
		if _, err := fmt.Fprintf(out, "%-20s %s  %s\n", tui.StripControl(p.Name), tui.StripControl(sensorPairLabel(p.SourceAssetId, p.SourcePrincipalUri)), state); err != nil {
			return err
		}
	}
	return nil
}

// runSensorPair pairs the one SensorLink source that query names, as Enter
// does in the SensorLink tab. Only the device scans: an agent too old to scan
// is an error, never a scan of this computer's network, whose sources the
// device may be unable to reach.
func runSensorPair(ctx context.Context, h *sensorPairHandler, query string, jsonOut bool, out io.Writer) error {
	scripted := *h
	scripted.discoverLocal = nil
	query = strings.TrimSpace(query)
	if !jsonOut {
		cliLogln("Scanning the device's network for SensorLink sources...")
	}
	scan := scripted.load(ctx)
	if errors.Is(scan.discoveryErr, errSensorDiscoveryUnsupported) {
		return withNextSteps(scan.discoveryErr,
			"Update the device's agent with 'wendy device update', or pair in a terminal with 'wendy device pair', which then lists sources on this computer's network.")
	}
	if scan.discoveryErr != nil {
		return fmt.Errorf("SensorLink discovery failed: %w", scan.discoveryErr)
	}
	if scan.pairingsErr != nil {
		return fmt.Errorf("could not load SensorLink pairings: %w", scan.pairingsErr)
	}

	candidates := sensorPairCandidates(scan.devices, scan.pairings)
	matched := matchingSensorPairRows(candidates, query)
	switch {
	case len(candidates) == 0:
		return fmt.Errorf("no SensorLink source matches %q: the device found none on its network", query)
	case len(matched) == 0:
		return fmt.Errorf("no SensorLink source matches %q. The device found:%s", query, sensorPairRowList(candidates, true))
	case len(matched) > 1:
		return fmt.Errorf("%q matches %d SensorLink sources, so none was paired. Name one by its source asset ID or principal URI:%s", query, len(matched), sensorPairRowList(matched, true))
	}

	row := matched[0]
	if row.pairing == nil && row.source != nil && !sensorSourcePairable(*row.source) {
		label := tui.StripControl(sensorSourceLabel(*row.source))
		if sensorSourcePKIUnsupported(*row.source) {
			return fmt.Errorf("%s is enrolled, but this version cannot pair it yet", label)
		}
		return withNextSteps(fmt.Errorf("%s needs enrollment before it can be paired", label),
			"Enroll it in the same organization as the target device, then run this command again.")
	}
	if refusal := sensorPairRefusal(row, true, scan.principalPairing); refusal != "" {
		err := errors.New(tui.StripControl(refusal))
		if row.pairing != nil {
			return withNextSteps(err, "To change its name or sensors, remove it with --forget and pair it again.")
		}
		return err
	}
	if row.source == nil {
		// Unreachable: a row without a source is a saved pairing, refused above.
		return fmt.Errorf("%q is not on the device's network", row.name)
	}
	pairing, err := scripted.add(ctx, *row.source)
	if err != nil {
		return err
	}
	if jsonOut {
		return writePairProtoJSON(out, &agentpbv2.AddSensorPairingResponse{Pairing: pairing})
	}
	_, err = fmt.Fprintf(out, "Paired %s. Its sensors will appear on this device.\n", tui.StripControl(pairing.Name))
	return err
}

// runSensorForget removes the one saved SensorLink pairing that query names,
// whether or not its source is online, so it needs no discovery.
func runSensorForget(ctx context.Context, h *sensorPairHandler, query string, jsonOut bool, out io.Writer) error {
	query = strings.TrimSpace(query)
	listCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	resp, err := h.client.ListSensorPairings(listCtx, &agentpbv2.ListSensorPairingsRequest{})
	cancel()
	if err != nil {
		return fmt.Errorf("could not load SensorLink pairings: %w", cleanRPCError(err))
	}
	rows := sensorPairRows(nil, resp.GetPairings())
	matched := matchingSensorPairRows(rows, query)
	switch {
	case len(rows) == 0:
		return fmt.Errorf("no saved SensorLink pairing matches %q: the device has none", query)
	case len(matched) == 0:
		return fmt.Errorf("no saved SensorLink pairing matches %q. Saved pairings:%s", query, sensorPairRowList(rows, false))
	case len(matched) > 1:
		return fmt.Errorf("%q matches %d saved SensorLink pairings, so none was forgotten. Name one by its source asset ID or principal URI:%s", query, len(matched), sensorPairRowList(matched, false))
	}

	row := matched[0]
	if err := h.remove(ctx, row.assetID, row.principal); err != nil {
		return err
	}
	if jsonOut {
		// The list was read before the removal; a removed pairing has no link.
		forgotten := proto.Clone(row.pairing).(*agentpbv2.SensorPairing)
		forgotten.Connected = false
		raw, err := pairJSON.Marshal(forgotten)
		if err != nil {
			return err
		}
		return writeJSON(out, map[string]json.RawMessage{"forgotten": raw})
	}
	_, err = fmt.Fprintf(out, "Forgot %s.\n", tui.StripControl(row.name))
	return err
}

// sensorPairCandidates is every SensorLink source a scripted pair can name:
// the SensorLink tab's rows (sources with an identity, and saved pairings
// including offline ones), then the sources the tab lists as needing
// enrollment, which have no identity yet. One without a display name is
// named by its address, as the tab labels it.
func sensorPairCandidates(devices []models.DiscoveredDevice, pairings []*agentpbv2.SensorPairing) []sensorPairRow {
	rows := sensorPairRows(devices, pairings)
	for i := range devices {
		if d := &devices[i]; d.Sensorlink && !sensorSourcePairable(*d) {
			name := d.DisplayName
			if name == "" {
				name = d.Address()
			}
			rows = append(rows, sensorPairRow{name: name, source: d})
		}
	}
	return rows
}

// matchingSensorPairRows returns the rows query names. An exact source asset
// ID or principal URI names its own row even when another row's name equals
// it, so the identity an ambiguity error lists always selects one. Otherwise
// query matches pairing or display names, ignoring case.
func matchingSensorPairRows(rows []sensorPairRow, query string) []sensorPairRow {
	if query == "" {
		return nil
	}
	var byIdentity, byName []sensorPairRow
	for _, row := range rows {
		switch {
		case (row.principal != "" && query == row.principal) ||
			(row.assetID > 0 && query == strconv.Itoa(int(row.assetID))):
			byIdentity = append(byIdentity, row)
		case strings.EqualFold(query, row.name) ||
			(row.source != nil && strings.EqualFold(query, row.source.DisplayName)):
			byName = append(byName, row)
		}
	}
	if len(byIdentity) > 0 {
		return byIdentity
	}
	return byName
}

// sensorPairRowList lists rows in an error, one per line, each with what a
// script can name it by and its state. discovered is whether the rows came
// from a scan, so a saved pairing without a source was not found.
func sensorPairRowList(rows []sensorPairRow, discovered bool) string {
	var list strings.Builder
	for _, row := range rows {
		var details []string
		switch {
		case row.principal != "":
			details = append(details, row.principal)
		case row.assetID > 0:
			details = append(details, "asset "+strconv.Itoa(int(row.assetID)))
		}
		if row.source != nil {
			if address := row.source.Address(); address != "" && address != row.name {
				details = append(details, address)
			}
		}
		switch {
		case row.pairing != nil && discovered && row.source == nil:
			details = append(details, "paired, not found on the network")
		case row.pairing != nil && discovered:
			details = append(details, "paired")
		case row.pairing != nil && row.pairing.Connected:
			details = append(details, "connected")
		case row.pairing != nil:
			details = append(details, "disconnected")
		case row.source != nil && !sensorSourcePairable(*row.source) && sensorSourcePKIUnsupported(*row.source):
			details = append(details, "enrolled, cannot be paired by this version")
		case row.source != nil && !sensorSourcePairable(*row.source):
			details = append(details, "needs enrollment")
		}
		line := strconv.Quote(row.name)
		if len(details) > 0 {
			line += " (" + strings.Join(details, ", ") + ")"
		}
		list.WriteString("\n  " + tui.StripControl(line))
	}
	return list.String()
}
