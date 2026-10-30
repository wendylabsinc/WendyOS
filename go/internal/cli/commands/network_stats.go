package commands

import (
	"fmt"
	"math"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// NetworkTraffic is shared by top and the native clients. Nil rates mean that
// there is not yet a comparable pair of samples, rather than no traffic.
type NetworkTraffic struct {
	Interfaces                  []NetworkTrafficRow `json:"interfaces"`
	Agent                       *NetworkTrafficRow  `json:"agent,omitempty"`
	InterfacesUnavailableReason string              `json:"interfacesUnavailableReason,omitempty"`
	AgentUnavailableReason      string              `json:"agentUnavailableReason,omitempty"`
}

type NetworkTrafficRow struct {
	Name                  string   `json:"name"`
	Index                 uint32   `json:"index,omitempty"`
	Virtual               bool     `json:"virtual,omitempty"`
	Loopback              bool     `json:"loopback,omitempty"`
	ReceivedBytes         uint64   `json:"receivedBytes"`
	SentBytes             uint64   `json:"sentBytes"`
	ReceiveBytesPerSecond *float64 `json:"receiveBytesPerSecond,omitempty"`
	SendBytesPerSecond    *float64 `json:"sendBytesPerSecond,omitempty"`
}

// NetworkTrafficBetween uses the device's monotonic clock, not RPC arrival
// times. Boot, interface and service identities prevent spikes on reconnects.
func NetworkTrafficBetween(prev, cur *pb.NetworkStats) *NetworkTraffic {
	if cur == nil {
		return nil
	}
	result := &NetworkTraffic{Interfaces: []NetworkTrafficRow{}, InterfacesUnavailableReason: cur.GetInterfacesUnavailableReason(), AgentUnavailableReason: cur.GetAgentUnavailableReason()}
	elapsed := 0.0
	if prev != nil && cur.GetBootId() != "" && cur.GetBootId() == prev.GetBootId() && cur.GetSampledAtBoottimeNs() > prev.GetSampledAtBoottimeNs() {
		elapsed = float64(cur.GetSampledAtBoottimeNs()-prev.GetSampledAtBoottimeNs()) / 1e9
	}
	rates := func(row *NetworkTrafficRow, rx, tx uint64) {
		if elapsed <= 0 || row.ReceivedBytes < rx || row.SentBytes < tx {
			return
		}
		r, s := float64(row.ReceivedBytes-rx)/elapsed, float64(row.SentBytes-tx)/elapsed
		row.ReceiveBytesPerSecond, row.SendBytesPerSecond = &r, &s
	}
	for _, iface := range cur.GetInterfaces() {
		row := NetworkTrafficRow{Name: iface.GetName(), Index: iface.GetIndex(), Virtual: iface.GetVirtual(), Loopback: iface.GetLoopback(), ReceivedBytes: iface.GetReceivedBytes(), SentBytes: iface.GetSentBytes()}
		for _, old := range prev.GetInterfaces() {
			if row.Index != 0 && old.GetIndex() == row.Index && old.GetName() == row.Name {
				rates(&row, old.GetReceivedBytes(), old.GetSentBytes())
				break
			}
		}
		result.Interfaces = append(result.Interfaces, row)
	}
	if agent := cur.GetAgent(); agent != nil {
		row := &NetworkTrafficRow{Name: "Wendy agent service", ReceivedBytes: agent.GetReceivedBytes(), SentBytes: agent.GetSentBytes()}
		if old := prev.GetAgent(); old != nil && agent.GetAccountingId() != "" && agent.GetAccountingId() == old.GetAccountingId() {
			rates(row, old.GetReceivedBytes(), old.GetSentBytes())
		}
		result.Agent = row
	}
	return result
}

func networkByteSize(bytes float64) string {
	units := []string{"B", "KiB", "MiB", "GiB", "TiB", "PiB", "EiB"}
	i := 0
	for bytes >= 1024 && i < len(units)-1 {
		bytes /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%.0f %s", bytes, units[i])
	}
	return fmt.Sprintf("%.1f %s", bytes, units[i])
}

func networkRate(rate *float64) string {
	if rate == nil || math.IsNaN(*rate) || math.IsInf(*rate, 0) {
		return "—"
	}
	return networkByteSize(*rate) + "/s"
}

func networkSummaryLines(traffic *NetworkTraffic) []string {
	if traffic == nil {
		return []string{"Network: unavailable on this agent"}
	}
	var lines []string
	rowText := func(row NetworkTrafficRow) string {
		return fmt.Sprintf("NET %s: ↓ %s  ↑ %s  total ↓ %s ↑ %s", tui.StripControl(row.Name), networkRate(row.ReceiveBytesPerSecond), networkRate(row.SendBytesPerSecond), networkByteSize(float64(row.ReceivedBytes)), networkByteSize(float64(row.SentBytes)))
	}
	for _, row := range traffic.Interfaces {
		if !row.Virtual && !row.Loopback {
			lines = append(lines, rowText(row))
		}
	}
	if traffic.InterfacesUnavailableReason != "" {
		lines = append(lines, "Network: "+tui.StripControl(traffic.InterfacesUnavailableReason))
	}
	if traffic.Agent != nil {
		lines = append(lines, rowText(*traffic.Agent))
	} else {
		lines = append(lines, "NET Wendy agent service: unavailable")
	}
	lines = append(lines, " Network totals since interface reset; agent IP totals since accounting began.")
	return lines
}

func networkSummary(prev, cur topSample) string {
	return strings.Join(networkSummaryLines(NetworkTrafficBetween(prev.host.GetNetwork(), cur.host.GetNetwork())), "\n")
}
