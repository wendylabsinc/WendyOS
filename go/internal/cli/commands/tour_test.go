package commands

import "testing"

func TestParseNetshSSID(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "typical connected interface",
			in: `There is 1 interface on the system:

    Name                   : Wi-Fi
    Description            : Intel(R) Wi-Fi 6 AX201 160MHz
    GUID                   : abcdef01-2345-6789-abcd-ef0123456789
    Physical address       : 00:11:22:33:44:55
    State                  : connected
    SSID                   : MyHomeNetwork
    BSSID                  : aa:bb:cc:dd:ee:ff
    Network type           : Infrastructure
`,
			want: "MyHomeNetwork",
		},
		{
			name: "BSSID line alone does not match",
			in: `    Name                   : Wi-Fi
    BSSID                  : aa:bb:cc:dd:ee:ff
`,
			want: "",
		},
		{
			name: "SSID with spaces and punctuation",
			in: `    SSID                   : My Coffee Shop - 5G
    BSSID                  : aa:bb:cc:dd:ee:ff
`,
			want: "My Coffee Shop - 5G",
		},
		{
			name: "disconnected interface (empty SSID)",
			in: `    State                  : disconnected
    SSID                   :
`,
			want: "",
		},
		{
			name: "no SSID line",
			in: `    State                  : disconnected
`,
			want: "",
		},
		{
			name: "empty output",
			in:   ``,
			want: "",
		},
		{
			name: "SSID label not followed by colon is skipped",
			in: `    SSIDLike  not a real label
    SSID                   : RealNetwork
`,
			want: "RealNetwork",
		},
		{
			name: "first SSID wins when multiple present",
			in: `    SSID                   : First
    SSID                   : Second
`,
			want: "First",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseNetshSSID(c.in)
			if got != c.want {
				t.Fatalf("parseNetshSSID(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}
