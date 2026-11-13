package clouddefaults

import (
	"bytes"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

// TestCloudTransportCredentials pins WDY-3756: the cloud dial is TLS for every
// address, including non-:443 ones, and plaintext only on an explicit
// WENDY_CLOUD_INSECURE=true, which always warns.
func TestCloudTransportCredentials(t *testing.T) {
	certInfo, _ := testCertInfo(t)
	const addr = "cloud.example.com:50051"

	tests := []struct {
		name     string
		env      string
		withCert bool
		want     string
	}{
		{name: "non-443 without opt-in dials TLS", want: "tls"},
		{name: "non-443 with client cert dials TLS", withCert: true, want: "tls"},
		{name: "opt-in false keeps TLS", env: "false", want: "tls"},
		{name: "unparseable opt-in keeps TLS", env: "yes-please", want: "tls"},
		{name: "explicit opt-in dials plaintext", env: "true", want: "insecure"},
		{name: "explicit opt-in ignores client cert", env: "1", withCert: true, want: "insecure"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(CloudInsecureEnv, tt.env)
			var warn bytes.Buffer
			prev := insecureWarningOut
			insecureWarningOut = &warn
			t.Cleanup(func() { insecureWarningOut = prev })

			var cert *config.CertificateInfo
			if tt.withCert {
				cert = &certInfo
			}
			creds, err := CloudTransportCredentials(addr, cert)
			if err != nil {
				t.Fatalf("CloudTransportCredentials() error = %v", err)
			}
			if got := creds.Info().SecurityProtocol; got != tt.want {
				t.Errorf("SecurityProtocol = %q, want %q", got, tt.want)
			}
			warned := strings.Contains(warn.String(), CloudInsecureEnv) && strings.Contains(warn.String(), addr)
			if warned != (tt.want == "insecure") {
				t.Errorf("warning = %q, want a warning naming %s and %s only for plaintext", warn.String(), CloudInsecureEnv, addr)
			}
		})
	}
}
