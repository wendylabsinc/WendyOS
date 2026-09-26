package commands

import (
	"testing"

	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

func linkMS(v uint32) *uint32 { return &v }

func TestFormatLinkTimeout(t *testing.T) {
	tests := []struct {
		name           string
		effective, req *uint32
		want           string
	}{
		{"not reported", nil, nil, ""},
		{"at the agent's value", linkMS(500), linkMS(3000), "0.5 s"},
		{"device's value in force", linkMS(3000), linkMS(3000), "3.0 s (device)"},
		{"override off", linkMS(3000), nil, "3.0 s"},
		{"floor above the target", linkMS(1010), linkMS(3000), "1.01 s"},
	}
	for _, tt := range tests {
		d := &agentpb.DiscoveredBluetoothPeripheral{SupervisionTimeoutMs: tt.effective, RequestedSupervisionTimeoutMs: tt.req}
		if got := formatLinkTimeout(d); got != tt.want {
			t.Errorf("%s: formatLinkTimeout = %q; want %q", tt.name, got, tt.want)
		}
	}
}
