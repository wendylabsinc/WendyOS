package commands

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/proto/gen/wcomrelaypb"
	"google.golang.org/grpc"
)

func TestCloudLiteRelayUsesSelectedOperatorIdentity(t *testing.T) {
	for _, tc := range []struct {
		name        string
		otherName   string
		provisioned bool
	}{
		{name: "another operator in the same tenant", otherName: "other"},
		{name: "another key for the same principal", otherName: "test"},
		{name: "provisioned identity changed during discovery", otherName: "test", provisioned: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("WENDY_LITE_OPERATOR_CERT", "")
			t.Setenv("WENDY_LITE_OPERATOR_KEY", "")
			t.Setenv("WENDY_SECRET_STORE", "file")
			lis, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			relay := &liteRelayStub{opened: make(chan relayStubOpen, 1), endpoint: "http://" + lis.Addr().String()}
			auth := liteTestAuth(lis.Addr().String())
			auth.APIKey = "selected-session"
			auth.Certificates, relay.tlsConfig = liteRelayTestPKI(t)
			selectedIdentity := auth.Certificates[0]
			// The session's Lite leaf, never the primary, goes on the Lite lane.
			auth.LiteCertificate = selectedIdentity.PemCertificate + selectedIdentity.PemCertificateChain
			auth.LitePrivateKey = selectedIdentity.PemPrivateKey
			if tc.provisioned {
				var provisioned []config.CertificateInfo
				var provisionedTLS *tls.Config
				provisioned, provisionedTLS = liteRelayTestPKI(t)
				relay.tlsConfig.ClientCAs = provisionedTLS.ClientCAs
				selectedIdentity = provisioned[0]
				certPath, keyPath := provisionLiteRelayIdentity(t, selectedIdentity)
				// The signer has resolved the override before discovery. Replacing
				// its files now must not replace the certificate used for mTLS.
				relay.resolveHook = func() error {
					original := auth.Certificates[0]
					if err := os.WriteFile(certPath, []byte(original.PemCertificate+original.PemCertificateChain), 0600); err != nil {
						return err
					}
					return os.WriteFile(keyPath, []byte(original.PemPrivateKey), 0600)
				}
			}
			selectedLeaf, _ := pem.Decode([]byte(selectedIdentity.PemCertificate))
			relay.tlsConfig.VerifyConnection = func(state tls.ConnectionState) error {
				if len(state.PeerCertificates) == 0 || !bytes.Equal(state.PeerCertificates[0].Raw, selectedLeaf.Bytes) {
					return fmt.Errorf("mTLS did not present the exact selected operator certificate")
				}
				return nil
			}
			srv := grpc.NewServer()
			wcomrelaypb.RegisterWendyComRelayServiceServer(srv, relay)
			wcomrelaypb.RegisterWendyComRelayDiscoveryServiceServer(srv, relay)
			go srv.Serve(lis)
			t.Cleanup(srv.Stop)

			// This earlier login has the same tenant but belongs to another
			// Cloud endpoint. It must not supply the one-shot relay's TLS key.
			other := liteTestAuth("other-cloud.example:443")
			other.Certificates, _ = liteRelayTestPKIForPrincipal(t, "spiffe://wendy.sh/tenant/"+liteTestTenant+"/operator/"+tc.otherName)
			setTempConfig(t, &config.Config{Auth: []config.AuthConfig{*other, *auth}})
			selected, err := cloudLiteSelectedDevice(auth, liteCloudAsset(), "")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := selected.Provider.GetDeviceInfo(context.Background(), *selected.External); err != nil {
				t.Fatalf("selected identity could not use its relay: %v", err)
			}
			open := <-relay.opened
			if got := relayRequestCertificate(t, open.principalRequest); !bytes.Equal(got, selectedLeaf.Bytes) {
				t.Fatal("relay proof and inner mTLS did not use the same certificate")
			}
		})
	}
}

func TestLiteRelayOptionsPreserveProvisionedIdentity(t *testing.T) {
	auth := liteTestAuth("cloud.example:443")
	auth.Certificates, _ = liteRelayTestPKI(t)
	provisioned, _ := liteRelayTestPKI(t)
	identity := provisioned[0]
	_, keyPath := provisionLiteRelayIdentity(t, identity)
	options, err := liteRelayOptions(context.Background(), auth, false)
	if err != nil {
		t.Fatal(err)
	}
	// An override change after signing must not replace the TLS identity.
	if err := os.WriteFile(keyPath, []byte("replaced after resolving identity"), 0600); err != nil {
		t.Fatal(err)
	}
	if options.OperatorIdentity.PemCertificate != identity.PemCertificate || options.OperatorIdentity.PemCertificateChain != identity.PemCertificateChain || options.OperatorIdentity.PemPrivateKey != identity.PemPrivateKey {
		t.Fatal("relay options did not retain the provisioned certificate, chain and key")
	}
	if options.DeviceTrustChainPEM != auth.Certificates[0].PemCertificateChain {
		t.Fatal("relay options did not retain the selected Cloud session's device trust chain")
	}
	proof, err := options.SignRequest([]byte("request"))
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := pem.Decode([]byte(identity.PemCertificate))
	if got := relayRequestCertificate(t, proof); !bytes.Equal(got, leaf.Bytes) {
		t.Fatal("request signer did not retain the provisioned identity")
	}
}

func provisionLiteRelayIdentity(t *testing.T, identity config.CertificateInfo) (string, string) {
	t.Helper()
	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, "operator.pem"), filepath.Join(dir, "operator-key.pem")
	if err := os.WriteFile(certPath, []byte(identity.PemCertificate+identity.PemCertificateChain), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte(identity.PemPrivateKey), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WENDY_LITE_OPERATOR_CERT", certPath)
	t.Setenv("WENDY_LITE_OPERATOR_KEY", keyPath)
	return certPath, keyPath
}

func TestLiteRelayOptionsRejectMismatchedSelectedPrincipal(t *testing.T) {
	auth := liteTestAuth("cloud.example:443")
	auth.Certificates, _ = liteRelayTestPKI(t)
	auth.Certificates[0].PrincipalURI += "-other"
	provisionLiteRelayIdentity(t, auth.Certificates[0])
	if _, err := liteRelayOptions(context.Background(), auth, false); err == nil || !strings.Contains(err.Error(), "selected operator") {
		t.Fatalf("expected an exact-principal rejection, got %v", err)
	}
}

func relayRequestCertificate(t *testing.T, request []byte) []byte {
	t.Helper()
	header, _, found := strings.Cut(string(request), ".")
	if !found {
		t.Fatal("missing request signature header")
	}
	raw, err := base64.RawURLEncoding.DecodeString(header)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Certificates [][]byte `json:"x5c"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Certificates) != 1 {
		t.Fatalf("request carries %d certificates", len(decoded.Certificates))
	}
	return decoded.Certificates[0]
}
