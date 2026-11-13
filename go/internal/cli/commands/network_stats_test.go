package commands

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/protobuf/proto"
)

func networkFixture(at uint64, rx, tx uint64) *pb.NetworkStats {
	return &pb.NetworkStats{BootId: "boot-1", SampledAtBoottimeNs: at,
		Interfaces: []*pb.NetworkInterfaceStats{{Name: "eth0", Index: 2, ReceivedBytes: rx, SentBytes: tx}},
		Agent:      &pb.AgentNetworkStats{ReceivedBytes: rx / 2, SentBytes: tx / 2, AccountingId: "service-1"}}
}

func TestNetworkTrafficRatesAndResets(t *testing.T) {
	old := networkFixture(1e9, 1000, 2000)
	cur := networkFixture(3e9, 3000, 6000)
	traffic := NetworkTrafficBetween(old, cur)
	if *traffic.Interfaces[0].ReceiveBytesPerSecond != 1000 || *traffic.Interfaces[0].SendBytesPerSecond != 2000 || *traffic.Agent.SendBytesPerSecond != 1000 {
		t.Fatalf("bad rates: %+v", traffic)
	}
	for _, tc := range []struct {
		name         string
		change       func(*pb.NetworkStats)
		iface, agent bool
	}{
		{"reboot", func(s *pb.NetworkStats) { s.BootId = "boot-2" }, true, true},
		{"missing boot", func(s *pb.NetworkStats) { s.BootId = "" }, true, true},
		{"same timestamp", func(s *pb.NetworkStats) { s.SampledAtBoottimeNs = 1e9 }, true, true},
		{"clock backwards", func(s *pb.NetworkStats) { s.SampledAtBoottimeNs = 1 }, true, true},
		{"new interface", func(s *pb.NetworkStats) { s.Interfaces[0].Index = 3 }, true, false},
		{"counter reset", func(s *pb.NetworkStats) { s.Interfaces[0].ReceivedBytes = 1 }, true, false},
		{"agent restart", func(s *pb.NetworkStats) { s.Agent.AccountingId = "service-2" }, false, true},
		{"agent reset", func(s *pb.NetworkStats) { s.Agent.SentBytes = 0 }, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sample := proto.Clone(cur).(*pb.NetworkStats)
			tc.change(sample)
			out := NetworkTrafficBetween(old, sample)
			if (out.Interfaces[0].ReceiveBytesPerSecond == nil) != tc.iface || (out.Agent.SendBytesPerSecond == nil) != tc.agent {
				t.Fatalf("invalid rates: %+v %+v", out.Interfaces[0], out.Agent)
			}
		})
	}
}

func TestNetworkTrafficUnknownAndVirtualInterfaces(t *testing.T) {
	if NetworkTrafficBetween(nil, nil) != nil {
		t.Fatal("old agent should have no network stats")
	}
	cur := networkFixture(1e9, 1000, 2000)
	cur.Interfaces = append(cur.Interfaces, &pb.NetworkInterfaceStats{Name: "veth0", Index: 3, Virtual: true, ReceivedBytes: 1000})
	cur.Agent = nil
	cur.AgentUnavailableReason = "unsupported"
	traffic := NetworkTrafficBetween(nil, cur)
	if traffic.Agent != nil || traffic.Interfaces[0].ReceiveBytesPerSecond != nil {
		t.Fatal("missing sample reported as zero")
	}
	data, err := json.Marshal(traffic)
	if err != nil || bytes.Contains(data, []byte("receiveBytesPerSecond")) {
		t.Fatalf("unknown JSON rates: %s %v", data, err)
	}
	if !bytes.Contains(data, []byte("veth0")) {
		t.Fatal("JSON must retain virtual interfaces")
	}
	text := strings.Join(networkSummaryLines(traffic), "\n")
	if strings.Contains(text, "veth0") || !strings.Contains(text, "unavailable") {
		t.Fatalf("unexpected summary %s", text)
	}
}

func TestTopNetworkPlainAndJSON(t *testing.T) {
	prev := topSample{host: &pb.HostStats{Network: networkFixture(1e9, 1000, 2000)}}
	cur := topSample{host: &pb.HostStats{Network: networkFixture(3e9, 3000, 6000)}}
	out := buildTopJSON(prev, cur, nil)
	if out.Host.Network == nil || *out.Host.Network.Agent.ReceiveBytesPerSecond != 500 {
		t.Fatal("top JSON lost network rates")
	}
	var buf bytes.Buffer
	if err := writeTopPlainSnapshot(&buf, prev, cur, nil); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"NET eth0", "Wendy agent service", "1000 B/s", "since interface reset"} {
		if !strings.Contains(buf.String(), want) {
			t.Fatalf("missing %q: %s", want, buf.String())
		}
	}
}
