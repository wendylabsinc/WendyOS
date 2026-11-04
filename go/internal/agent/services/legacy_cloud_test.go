package services

import "testing"

func TestLegacyBrokerURL(t *testing.T) {
	t.Setenv("WENDY_BROKER_URL", "")
	tests := []struct {
		name      string
		cloudHost string
		want      string
	}{
		{
			name:      "host without port uses broker port",
			cloudHost: "cloud.wendy.io",
			want:      "cloud.wendy.io:50052",
		},
		{
			name:      "cloud run endpoint keeps tls port",
			cloudHost: "wendy-cloud-services-114319063177.us-central1.run.app:443",
			want:      "wendy-cloud-services-114319063177.us-central1.run.app:443",
		},
		{
			name:      "local certificate port maps to local broker port",
			cloudHost: "localhost:50051",
			want:      "localhost:50052",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := LegacyBrokerURL(tt.cloudHost); got != tt.want {
				t.Fatalf("LegacyBrokerURL(%q) = %q, want %q", tt.cloudHost, got, tt.want)
			}
		})
	}
}

func TestLegacyBrokerOverride(t *testing.T) {
	t.Setenv("WENDY_BROKER_URL", "broker.example:8443")
	for _, host := range []string{"api.example:443", "api.example:50051", "api.example"} {
		if got := LegacyBrokerURL(host); got != "broker.example:8443" {
			t.Fatalf("override lost for %s: %s", host, got)
		}
	}
}
