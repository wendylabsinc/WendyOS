package services

import (
	"os"
	"strings"
	"testing"
)

// TestTelemetryDialHostPrefersOverride confirms the flusher dials the override
// when one is set and the enrolled cloud host otherwise. Without this the
// override would be accepted and silently ignored, which is the failure mode
// that leaves telemetry going to a collector that discards it.
func TestTelemetryDialHostPrefersOverride(t *testing.T) {
	const enrolled = "cloud.wendy.sh:443"
	cases := []struct {
		name     string
		override string
		want     string
	}{
		{"no override uses enrolled host", "", enrolled},
		{"bare host", "wendy-data-dev.example.run.app", "wendy-data-dev.example.run.app"},
		{"host with port", "wendy-data-dev.example.run.app:443", "wendy-data-dev.example.run.app:443"},
		{"https url is reduced to host", "https://wendy-data-dev.example.run.app", "wendy-data-dev.example.run.app"},
		{"url with path drops the path", "https://wendy-data-dev.example.run.app/ingest", "wendy-data-dev.example.run.app"},
		{"surrounding whitespace is trimmed", "  wendy-data-dev.example.run.app  ", "wendy-data-dev.example.run.app"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &CloudFlusher{}
			f.SetTelemetryHostOverride(tc.override)
			if got := f.telemetryDialHost(enrolled); got != tc.want {
				t.Fatalf("telemetryDialHost() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestDataPathSendsNoIdentityHeader is a static guard for the rule both cloud
// workers now follow: identity is the enrolled asset certificate presented in
// the TLS handshake, and no request header carries it. The x-wendy-client-cert
// header was a self-asserted identity the ingest service no longer reads; the
// tunnel broker client is a different service and is not covered here.
func TestDataPathSendsNoIdentityHeader(t *testing.T) {
	for _, f := range []string{"data_transfer_worker.go", "cloud_flusher.go"} {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{"x-wendy-client-cert", "urn:wendy:org:", "certIdentityDialOptions"} {
			if strings.Contains(string(src), forbidden) {
				t.Errorf("%s mentions %q; the data path must not carry an identity header", f, forbidden)
			}
		}
	}
}
