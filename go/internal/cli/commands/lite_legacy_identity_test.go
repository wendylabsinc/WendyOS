package commands

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/providers"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
)

func TestLegacyLiteDeviceInfoSelectsOrgUserAndVerifiesAsset(t *testing.T) {
	for _, tc := range []struct {
		name      string
		serverURN string
		wantError bool
	}{
		{name: "matching legacy asset", serverURN: certs.AssetURN(2, 606)},
		{name: "wrong asset in same organization", serverURN: certs.AssetURN(2, 607), wantError: true},
		{name: "same asset in wrong organization", serverURN: certs.AssetURN(3, 606), wantError: true},
		{name: "user identity cannot impersonate asset", serverURN: certs.UserURN(2, "606"), wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("WENDY_LITE_OPERATOR_CERT", "")
			t.Setenv("WENDY_LITE_OPERATOR_KEY", "")
			t.Setenv(renewEndpointEnv, "")
			t.Setenv("WENDY_SECRET_STORE", "file")
			auth, serverTLS := legacyLiteTestPKI(t, 2, tc.serverURN)
			tenantAuth, _ := localLiteTestPKI(t)
			wrongOrgAuth, _ := legacyLiteTestPKI(t, 3, certs.AssetURN(3, 606))
			// The matching legacy login is last, after two incompatible sessions.
			setTempConfig(t, &config.Config{Auth: []config.AuthConfig{*tenantAuth, *wrongOrgAuth, *auth}})
			clientLeaf, _ := pem.Decode([]byte(auth.Certificates[0].PemCertificate))
			serverTLS.VerifyConnection = func(state tls.ConnectionState) error {
				if state.Version != tls.VersionTLS12 {
					return fmt.Errorf("legacy connection negotiated TLS version %x", state.Version)
				}
				if len(state.PeerCertificates) == 0 || !bytes.Equal(state.PeerCertificates[0].Raw, clientLeaf.Bytes) {
					return fmt.Errorf("legacy connection did not use the matching organization user")
				}
				return nil
			}
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { listener.Close() })
			serverDone := make(chan error, 1)
			go func() {
				// Accept once: dialing an incompatible context first must fail the test.
				conn, err := listener.Accept()
				if err != nil {
					serverDone <- err
					return
				}
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
				serverDone <- serveLocalLiteDeviceInfo(tls.Server(conn, serverTLS))
			}()
			host, port, err := net.SplitHostPort(listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			device := models.ExternalDevice{ConnectionInfo: map[string]string{
				"type": "LAN", "ip": host, "port": port, "mtls": "true", "orgId": "2", "assetId": "606",
			}}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			info, err := (&providers.MicroWendyProvider{}).GetDeviceInfo(ctx, device)
			if tc.wantError {
				if err == nil {
					t.Fatal("connected to a server with a different authenticated identity")
				}
				if ctx.Err() != nil {
					t.Fatalf("connection timed out instead of rejecting the server identity: %v", err)
				}
				select {
				case serverErr := <-serverDone:
					if serverErr == nil {
						t.Fatal("untrusted device completed a WendyCom exchange")
					}
				case <-time.After(time.Second):
					t.Fatal("client did not attempt and reject the legacy TLS connection")
				}
				return
			}
			if err != nil {
				t.Fatalf("GetDeviceInfo: %v", err)
			}
			if info.OS != "wendy-lite" || info.OSVersion != "0.9.1" || info.DeviceType != "esp32c6" {
				t.Fatalf("GetDeviceInfo = %+v, want the legacy board's response", info)
			}
			if err := <-serverDone; err != nil {
				t.Fatalf("legacy device: %v", err)
			}
		})
	}
}

func legacyLiteTestPKI(t *testing.T, orgID int32, serverURN string) (*config.AuthConfig, *tls.Config) {
	t.Helper()
	issue := func(serial int64, principal string, parent *x509.Certificate, signer *ecdsa.PrivateKey) (*x509.Certificate, string, *ecdsa.PrivateKey) {
		t.Helper()
		keyPEM, key := newP256KeyPEM(t)
		template := &x509.Certificate{
			SerialNumber: big.NewInt(serial), NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
			BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature,
			ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		}
		if parent == nil {
			template.IsCA = true
			template.KeyUsage |= x509.KeyUsageCertSign
			parent, signer = template, key
		}
		if principal != "" {
			uri, err := url.Parse(principal)
			if err != nil {
				t.Fatal(err)
			}
			template.URIs = []*url.URL{uri}
		}
		der, err := x509.CreateCertificate(rand.Reader, template, parent, key.Public(), signer)
		if err != nil {
			t.Fatal(err)
		}
		certificate, err := x509.ParseCertificate(der)
		if err != nil {
			t.Fatal(err)
		}
		return certificate, keyPEM, key
	}
	root, _, rootKey := issue(1, "", nil, nil)
	operator, operatorKeyPEM, _ := issue(2, certs.UserURN(orgID, "test-user"), root, rootKey)
	device, _, deviceKey := issue(3, serverURN, root, rootKey)
	pool := x509.NewCertPool()
	pool.AddCert(root)
	return &config.AuthConfig{Name: fmt.Sprintf("legacy-org-%d", orgID), Certificates: []config.CertificateInfo{{
		OrganizationID: int(orgID), UserID: "test-user",
		PemCertificate:      string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: operator.Raw})),
		PemCertificateChain: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: root.Raw})),
		PemPrivateKey:       operatorKeyPEM,
	}}}, &tls.Config{
		MinVersion: tls.VersionTLS12, MaxVersion: tls.VersionTLS12,
		Certificates: []tls.Certificate{{Certificate: [][]byte{device.Raw}, PrivateKey: deviceKey}},
		ClientCAs:    pool, ClientAuth: tls.RequireAndVerifyClientCert,
	}
}
