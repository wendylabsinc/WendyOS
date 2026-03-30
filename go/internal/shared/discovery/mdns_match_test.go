package discovery

import "testing"

func TestMDNSEntryMatchesServiceType(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		entryName   string
		serviceType string
		want        bool
	}{
		{
			name:        "matches local instance name",
			entryName:   "wendy-device._wendyos._udp.local.",
			serviceType: "_wendyos._udp",
			want:        true,
		},
		{
			name:        "matches case insensitively",
			entryName:   "Wendy-Device._WENDYOS._UDP.LOCAL.",
			serviceType: "_wendyos._udp",
			want:        true,
		},
		{
			name:        "matches escaped dot in instance label",
			entryName:   "wendy\\.device._wendyos._udp.local.",
			serviceType: "_wendyos._udp",
			want:        true,
		},
		{
			name:        "rejects substring in different service type",
			entryName:   "iphone_wendyos_udp._remotepairing._tcp.local.",
			serviceType: "_wendyos._udp",
			want:        false,
		},
		{
			name:        "rejects service-like substring inside escaped instance label",
			entryName:   "iphone\\._wendyos._udp._remotepairing._tcp.local.",
			serviceType: "_wendyos._udp",
			want:        false,
		},
		{
			name:        "rejects incomplete service type match",
			entryName:   "wendy-device._wendyos._udp-alt.local.",
			serviceType: "_wendyos._udp",
			want:        false,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := mdnsEntryMatchesServiceType(tt.entryName, tt.serviceType); got != tt.want {
				t.Fatalf("mdnsEntryMatchesServiceType(%q, %q) = %v, want %v", tt.entryName, tt.serviceType, got, tt.want)
			}
		})
	}
}
