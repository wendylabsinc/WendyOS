package clouddefaults

import "testing"

func TestBrokerURL(t *testing.T) {
	tests := []struct {
		name        string
		cloudGRPC   string
		brokerURL   string
		defaultPort string
		want        string
	}{
		{
			name:        "explicit broker wins",
			cloudGRPC:   "wendy-cloud.example.com:443",
			brokerURL:   "broker.example.com:8443",
			defaultPort: "50052",
			want:        "broker.example.com:8443",
		},
		{
			name:        "wendy cloud keeps 443",
			cloudGRPC:   "wendy-cloud-services-114319063177.us-central1.run.app:443",
			defaultPort: "50052",
			want:        "wendy-cloud-services-114319063177.us-central1.run.app:443",
		},
		{
			name:        "local endpoint uses dedicated broker port",
			cloudGRPC:   "localhost:50051",
			defaultPort: "50052",
			want:        "localhost:50052",
		},
		{
			name:        "ipv6 endpoint uses dedicated broker port",
			cloudGRPC:   "[fe80::1%en0]:50051",
			defaultPort: "50052",
			want:        "[fe80::1%en0]:50052",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BrokerURL(tt.cloudGRPC, tt.brokerURL, tt.defaultPort)
			if got != tt.want {
				t.Fatalf("BrokerURL() = %q, want %q", got, tt.want)
			}
		})
	}
}
