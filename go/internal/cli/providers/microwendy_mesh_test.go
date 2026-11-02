package providers

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/liteclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/sessionbroker"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

func TestMeshLiteUsesSelectedPrimaryContextAndLiteCredential(t *testing.T) {
	primary := localLiteTestCertificate(t, localLiteOperator, true)
	lane := localLiteTestCertificate(t, localLiteOperator, false)
	other := localLiteTestCertificate(t, localLiteOperator+"-other", false)
	localLiteTestSessions(t, config.AuthConfig{Name: "other", Certificates: []config.CertificateInfo{other}}, config.AuthConfig{Name: "selected", Certificates: []config.CertificateInfo{primary}})
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.CurrentContext = "selected"
	if err = config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	calls := 0
	LiteIdentityResolver = func(_ context.Context, auth *config.AuthConfig) (config.CertificateInfo, string, error) {
		calls++
		if auth.Name != "selected" || auth.Certificates[0].PemCertificate != primary.PemCertificate {
			t.Fatal("wrong primary context", auth.Name)
		}
		return lane, "device trust", nil
	}
	connected := false
	err = connectWithScopedCLIPrincipal(context.Background(), localLiteTenant, 0, localLiteOperator, func(cert tls.Certificate, chain string, _ liteclient.LiteDeviceCAChain) error {
		leaf, err := x509.ParseCertificate(cert.Certificate[0])
		if err != nil {
			return err
		}
		identity, ok, err := certs.IdentityFromCert(leaf)
		if err != nil || !ok || identity.Principal != localLiteOperator || chain != "device trust" {
			t.Fatal("resolved Lite credential", identity, chain, err)
		}
		connected = true
		return nil
	})
	if err != nil || !connected || calls != 1 {
		t.Fatal("selected primary-to-Lite credential", connected, calls, err)
	}
}

func TestMeshLiteDescriptorPreservesNativeCapabilities(t *testing.T) {
	const device = "33333333-3333-4333-8333-333333333333"
	selector := "mesh://" + localLiteTenant + "/" + device
	info := sessionbroker.OperatorTargetInfo{Selector: selector, Principal: "spiffe://wendy.sh/tenant/" + localLiteTenant + "/device/" + device, Tenant: localLiteTenant, Device: device, Platform: "Lite", Transport: "mtls-v1", Caps: []string{"sensors", "future-optional"}, Name: "C6"}
	external, err := MeshLiteDevice(info)
	if err != nil {
		t.Fatal(err)
	}
	if external.ID != selector || external.LiteDedupID() != device || external.DisplayName != "C6" || external.ConnectionInfo["caps"] != "sensors,future-optional" || external.ConnectionType() != "Mesh" {
		t.Fatal(external)
	}
}
