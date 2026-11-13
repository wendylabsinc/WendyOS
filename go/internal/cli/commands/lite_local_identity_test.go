package commands

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/providers"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	wendypb "github.com/wendylabsinc/wendy/go/proto/gen/litepb"
	"google.golang.org/protobuf/proto"
)

// Exercise the production resolver hook through a LAN provider connection. The
// tenant CA signs the ML-DSA primary and device. A cached P-256 relay identity
// belongs to an unrelated authority and must not replace the LAN credential.
func TestLocalLiteDeviceInfoUsesTenantPrimaryIdentity(t *testing.T) {
	for _, tc := range []struct {
		name     string
		cached   bool
		override bool
		wrongCA  bool
	}{
		{name: "ML-DSA tenant primary"},
		{name: "ignore cached P-256 relay identity", cached: true},
		{name: "explicit identity override preserves device trust", override: true},
		{name: "reject unrelated device authority", cached: true, wrongCA: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("WENDY_LITE_OPERATOR_CERT", "")
			t.Setenv("WENDY_LITE_OPERATOR_KEY", "")
			t.Setenv(renewEndpointEnv, "")
			t.Setenv("WENDY_SECRET_STORE", "file")

			auth, serverTLS := localLiteTestPKI(t)
			liteIdentity, liteTLS := liteRelayTestPKI(t)
			if tc.cached {
				auth.LiteCertificate = liteIdentity[0].PemCertificate + liteIdentity[0].PemCertificateChain
				auth.LitePrivateKey = liteIdentity[0].PemPrivateKey
			}
			presented := auth.Certificates[0]
			if tc.override {
				presented = liteIdentity[0]
				serverTLS.ClientCAs = liteTLS.ClientCAs
				dir := t.TempDir()
				certPath, keyPath := filepath.Join(dir, "operator.pem"), filepath.Join(dir, "operator-key.pem")
				if err := os.WriteFile(certPath, []byte(presented.PemCertificate+presented.PemCertificateChain), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(keyPath, []byte(presented.PemPrivateKey), 0600); err != nil {
					t.Fatal(err)
				}
				t.Setenv("WENDY_LITE_OPERATOR_CERT", certPath)
				t.Setenv("WENDY_LITE_OPERATOR_KEY", keyPath)
			}
			if tc.wrongCA {
				auth.Certificates[0].PemCertificateChain = liteIdentity[0].PemCertificateChain
			}
			setTempConfig(t, &config.Config{Auth: []config.AuthConfig{*auth}})

			clientLeaf, _ := pem.Decode([]byte(presented.PemCertificate))
			clientCA, _ := pem.Decode([]byte(presented.PemCertificateChain))
			serverTLS.VerifyConnection = func(state tls.ConnectionState) error {
				// Self-signed roots stay in the verifier's trust pool, not on the wire.
				if len(state.PeerCertificates) != 1 || !bytes.Equal(state.PeerCertificates[0].Raw, clientLeaf.Bytes) {
					return fmt.Errorf("LAN mTLS did not present only the selected tenant operator leaf")
				}
				if len(state.VerifiedChains) == 0 || len(state.VerifiedChains[0]) != 2 || !bytes.Equal(state.VerifiedChains[0][1].Raw, clientCA.Bytes) {
					return fmt.Errorf("LAN mTLS did not verify the selected operator authority")
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
				"type": "LAN", "ip": host, "port": port, "mtls": "true",
				"tenantId": liteTestTenant, "deviceId": "00aa11bb", "enrolledDeviceId": liteTestAssetID,
			}}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			info, err := (&providers.MicroWendyProvider{}).GetDeviceInfo(ctx, device)
			if tc.wrongCA {
				if err == nil {
					t.Fatalf("expected device trust verification failure, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("GetDeviceInfo: %v", err)
			}
			if info.OS != "wendy-lite" || info.OSVersion != "0.9.1" || info.DeviceType != "esp32c6" {
				t.Fatalf("GetDeviceInfo = %+v, want the LAN board's response", info)
			}
			if err := <-serverDone; err != nil {
				t.Fatalf("LAN device: %v", err)
			}
		})
	}
}

func TestLocalLiteIdentityRejectsExpiredPrimary(t *testing.T) {
	t.Setenv(renewEndpointEnv, "")
	auth := localLiteTestSession(t, "unused device CA", time.Now().Add(-time.Minute))
	keyPEM, key := newP256KeyPEM(t)
	auth.LiteCertificate = liteLeafPEM(t, key, auth.Certificates[0].PrincipalURI, time.Now().Add(time.Hour), true)
	auth.LitePrivateKey = keyPEM
	_, _, err := localLiteIdentity(context.Background(), auth)
	if !errors.Is(err, errCertExpired) {
		t.Fatalf("localLiteIdentity = %v, want expired primary rejection despite valid cached Lite leaf", err)
	}
}

func TestLocalLiteIdentityHonorsCancellation(t *testing.T) {
	t.Setenv(renewEndpointEnv, "")
	auth := localLiteTestSession(t, "unused device CA", time.Now().Add(time.Hour))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := localLiteIdentity(ctx, auth)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("localLiteIdentity = %v, want context.Canceled before resolving the operator certificate", err)
	}
}

func localLiteTestPKI(t *testing.T) (*config.AuthConfig, *tls.Config) {
	t.Helper()
	_, caKey := newP256KeyPEM(t)
	ca := &x509.Certificate{
		SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, caKey.Public(), caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, err = x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	auth := localLiteTestSession(t, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})), ca.NotAfter)
	primary, err := certs.ParseCertsFromPEM([]byte(auth.Certificates[0].PemCertificate))
	if err != nil {
		t.Fatal(err)
	}
	// Reissue the ML-DSA operator leaf under the same tenant CA as the device.
	primary[0].SignatureAlgorithm = x509.ECDSAWithSHA256
	primaryDER, err := x509.CreateCertificate(rand.Reader, primary[0], ca, primary[0].PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	auth.Certificates[0].PemCertificate = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: primaryDER}))
	_, deviceKey := newP256KeyPEM(t)
	deviceURI, err := url.Parse("spiffe://wendy.sh/tenant/" + liteTestTenant + "/device/" + liteTestAssetID)
	if err != nil {
		t.Fatal(err)
	}
	device := &x509.Certificate{
		SerialNumber: big.NewInt(3), NotBefore: ca.NotBefore, NotAfter: ca.NotAfter,
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		URIs: []*url.URL{deviceURI},
	}
	deviceDER, err := x509.CreateCertificate(rand.Reader, device, ca, deviceKey.Public(), caKey)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	return auth, &tls.Config{
		MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{{Certificate: [][]byte{deviceDER}, PrivateKey: deviceKey}},
		ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert,
	}
}

func localLiteTestSession(t *testing.T, deviceTrust string, primaryExpiry time.Time) *config.AuthConfig {
	t.Helper()
	keyPEM, err := certs.GenerateMLDSAKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	key, err := certs.ParseSigningPrivateKeyPEM([]byte(keyPEM))
	if err != nil {
		t.Fatal(err)
	}
	principal := "spiffe://wendy.sh/tenant/" + liteTestTenant + "/operator/test"
	return &config.AuthConfig{Name: "local-lite-test", Certificates: []config.CertificateInfo{{
		PrincipalURI: principal, PemPrivateKey: keyPEM,
		PemCertificate: liteLeafPEM(t, key, principal, primaryExpiry, false), PemCertificateChain: deviceTrust,
	}}}
}

// Serve only the WendyCom handshake and device-info command, preserving the
// client's framing so the test covers the normal provider and protocol path.
func serveLocalLiteDeviceInfo(conn *tls.Conn) error {
	if err := conn.Handshake(); err != nil {
		return err
	}
	for {
		header := make([]byte, 8)
		if _, err := io.ReadFull(conn, header); err != nil {
			return err
		}
		body := make([]byte, binary.BigEndian.Uint16(header[6:8]))
		if _, err := io.ReadFull(conn, body); err != nil {
			return err
		}
		request := &wendypb.WendyComMessage{}
		if err := proto.Unmarshal(body, request); err != nil {
			return err
		}
		done := false
		var reply *wendypb.WendyComMessage
		switch {
		case request.GetHandshake() != nil:
			reply = request
		case request.GetCommand().GetGetDeviceInfo() != nil:
			done = true
			reply = &wendypb.WendyComMessage{Msg: &wendypb.WendyComMessage_Response{Response: &wendypb.WendyComResponse{
				RequestId: request.GetCommand().GetRequestId(),
				Data: &wendypb.WendyComResponse_DeviceInfo{DeviceInfo: &wendypb.WendyComDeviceInfo{
					Os: "wendy-lite", OsVersion: "0.9.1", CpuArchitecture: "riscv32", Target: "esp32c6",
				}},
			}}}
		default:
			return fmt.Errorf("unexpected WendyCom request")
		}
		body, err := proto.Marshal(reply)
		if err != nil {
			return err
		}
		binary.BigEndian.PutUint16(header[6:8], uint16(len(body)))
		if _, err := conn.Write(append(header, body...)); err != nil {
			return err
		}
		if done {
			return nil
		}
	}
}
