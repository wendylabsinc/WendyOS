package hoststats

import (
	"math"
	"strings"
	"testing"
)

func TestParseNetworkCounters(t *testing.T) {
	input := "Inter-| Receive | Transmit\n eth0: 18446744073709551614 1 0 0 0 0 0 0 4200 2 0 0 0 0 0 0\n lo: 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0\n"
	rows, err := ParseProcNetDev([]byte(input))
	if err != nil || len(rows) != 2 || rows[0].ReceivedBytes != math.MaxUint64-1 || rows[0].SentBytes != 4200 {
		t.Fatalf("%+v, %v", rows, err)
	}
	for _, invalid := range []string{"", " eth0: 0", strings.Replace(input, "4200", "bad", 1), strings.Replace(input, "4200", "-1", 1)} {
		if _, err := ParseProcNetDev([]byte(invalid)); err == nil {
			t.Fatalf("accepted %q", invalid)
		}
	}
}

func TestAgentAccountingUnavailableIsNotZero(t *testing.T) {
	valid := "IPAccounting=yes\nInvocationID=service-1\nIPIngressBytes=0\nIPEgressBytes=2048\n"
	sample, err := parseAgentNetwork([]byte(valid))
	if err != nil || sample.ReceivedBytes != 0 || sample.SentBytes != 2048 || sample.AccountingID != "service-1" {
		t.Fatalf("%+v, %v", sample, err)
	}
	for _, invalid := range []string{
		strings.Replace(valid, "yes", "no", 1),
		strings.Replace(valid, "IPIngressBytes=0", "IPIngressBytes=18446744073709551615", 1),
		strings.Replace(valid, "IPEgressBytes=2048", "IPEgressBytes=[no data]", 1),
		strings.Replace(valid, "InvocationID=service-1", "InvocationID=", 1),
	} {
		if sample, err := parseAgentNetwork([]byte(invalid)); err == nil || sample != nil {
			t.Fatalf("unavailable became %+v: %q", sample, invalid)
		}
	}
}

func TestAccountingOnlyTargetsAgentService(t *testing.T) {
	for _, unit := range []string{"wendyos-agent.service", "wendy-agent.service"} {
		got, err := agentAccountingUnit([]byte("0::/system.slice/" + unit + "\n"))
		if err != nil || got != unit {
			t.Fatalf("%q %v", got, err)
		}
	}
	for _, cgroup := range []string{"0::/user.slice/user-501.slice/session.scope", "0::/system.slice/containerd.service", "0::/system.slice/wendyos-agent.service/child", "1:cpu:/system.slice/wendyos-agent.service", "0::/"} {
		if _, err := agentAccountingUnit([]byte(cgroup)); err == nil {
			t.Fatalf("accepted unrelated cgroup %s", cgroup)
		}
	}
}
