package hoststats

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

type NetworkInterface struct {
	Name                     string
	Index                    uint32
	ReceivedBytes, SentBytes uint64
	Virtual, Loopback        bool
}

type AgentNetwork struct {
	ReceivedBytes, SentBytes uint64
	AccountingID             string
}

type NetworkSample struct {
	BootID                      string
	BootTimeNS                  uint64
	Interfaces                  []NetworkInterface
	Agent                       *AgentNetwork
	InterfacesError, AgentError string
}

// ParseProcNetDev reads Linux interface counters without narrowing uint64s.
// A malformed row fails the sample rather than reporting partial zero totals.
func ParseProcNetDev(data []byte) ([]NetworkInterface, error) {
	var result []NetworkInterface
	for _, line := range strings.Split(string(data), "\n") {
		name, values, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		fields := strings.Fields(values)
		if strings.TrimSpace(name) == "" || len(fields) != 16 {
			return nil, fmt.Errorf("invalid network counter row")
		}
		rx, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid receive counter: %w", err)
		}
		tx, err := strconv.ParseUint(fields[8], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid send counter: %w", err)
		}
		result = append(result, NetworkInterface{Name: strings.TrimSpace(name), ReceivedBytes: rx, SentBytes: tx})
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("no network counters available")
	}
	return result, nil
}

func parseAgentNetwork(data []byte) (*AgentNetwork, error) {
	values := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok {
			values[key] = strings.TrimSpace(value)
		}
	}
	if values["IPAccounting"] != "yes" {
		return nil, fmt.Errorf("agent IP accounting is disabled")
	}
	if values["InvocationID"] == "" {
		return nil, fmt.Errorf("agent accounting identity is unavailable")
	}
	read := func(key string) (uint64, error) {
		value, err := strconv.ParseUint(values[key], 10, 64)
		if err != nil || value == math.MaxUint64 {
			return 0, fmt.Errorf("agent IP counters are unavailable on this kernel")
		}
		return value, nil
	}
	rx, err := read("IPIngressBytes")
	if err != nil {
		return nil, err
	}
	tx, err := read("IPEgressBytes")
	if err != nil {
		return nil, err
	}
	return &AgentNetwork{ReceivedBytes: rx, SentBytes: tx, AccountingID: values["InvocationID"]}, nil
}

// Only account the actual agent service. Never enable accounting for a shell,
// a user session, a container, or an enclosing slice when run outside systemd.
func agentAccountingUnit(cgroup []byte) (string, error) {
	for _, line := range strings.Split(string(cgroup), "\n") {
		for _, unit := range []string{"wendy-agent.service", "wendyos-agent.service"} {
			if line == "0::/system.slice/"+unit {
				return unit, nil
			}
		}
	}
	return "", fmt.Errorf("agent service IP accounting requires systemd and cgroup v2")
}
