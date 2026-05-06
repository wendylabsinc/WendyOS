package main

import "testing"

func TestBrokerURLForCloudHost(t *testing.T) {
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
			if got := brokerURLForCloudHost(tt.cloudHost); got != tt.want {
				t.Fatalf("brokerURLForCloudHost(%q) = %q, want %q", tt.cloudHost, got, tt.want)
			}
		})
	}
}
