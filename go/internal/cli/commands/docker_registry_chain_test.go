package commands

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

// The registry trusts only the root. It must receive the operator's issuer
// from the client, as with Cloud v2 certificates; a leaf-only handshake fails.
func TestRegistryClientIntermediateChain(t *testing.T) {
	for _, mode := range []string{"http-proxy", "tcp-proxy", "tunnel", "buildkit"} {
		for _, embedded := range []bool{false, true} {
			name := mode + "/split"
			if embedded {
				name = mode + "/embedded"
			}
			t.Run(name, func(t *testing.T) {
				root := generateTestCA(t)
				issuer := generateTestIntermediate(t, root)
				client := generateTestLeaf(t, issuer, x509.ExtKeyUsageClientAuth)
				server := generateTestLeaf(t, root, x509.ExtKeyUsageServerAuth)
				serverPair, err := tls.X509KeyPair([]byte(server.pemStr), []byte(marshalKeyPEM(t, server.key)))
				if err != nil {
					t.Fatal(err)
				}
				target := startTestTLSServer(t, serverPair, root)
				leafPEM, chainPEM := client.pemStr, issuer.pemStr+root.pemStr
				if embedded {
					leafPEM += issuer.pemStr
					chainPEM = root.pemStr
				}
				keyPEM := marshalKeyPEM(t, client.key)
				t.Setenv("WENDY_CONFIG_DIR", t.TempDir())
				if err := config.Save(&config.Config{Auth: []config.AuthConfig{{Certificates: []config.CertificateInfo{{PemCertificate: leafPEM, PemCertificateChain: chainPEM, PemPrivateKey: keyPEM}}}}}); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				clientHTTP := &http.Client{Timeout: 4 * time.Second}
				url := ""
				switch mode {
				case "http-proxy":
					proxy, err := startMTLSRegistryHTTPProxy(target, leafPEM, keyPEM, chainPEM)
					if err != nil {
						t.Fatal(err)
					}
					defer proxy.Close()
					url = "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(proxy.Port()))
				case "tcp-proxy", "tunnel":
					var proxy *registryProxy
					if mode == "tcp-proxy" {
						proxy, err = startMTLSRegistryProxy(ctx, target)
					} else {
						dial, dialErr := tlsClientDialer(leafPEM, keyPEM, chainPEM, func(ctx context.Context) (net.Conn, error) { return (&net.Dialer{}).DialContext(ctx, "tcp", target) })
						if dialErr != nil {
							t.Fatal(dialErr)
						}
						proxy, err = startRegistryProxyWithDialer(ctx, "127.0.0.1:0", dial)
					}
					if err != nil {
						t.Fatal(err)
					}
					defer proxy.Close()
					url = "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(proxy.Port()))
				case "buildkit":
					// Stop before invoking real Docker; inspect the credential written for
					// BuildKit and use it in a real mutual-TLS exchange instead.
					t.Setenv("PATH", t.TempDir())
					dir := t.TempDir()
					if _, err := ensureMTLSBuilder(ctx, dir, target, "/certs", io.Discard); err == nil {
						t.Fatal("expected unavailable Docker")
					}
					cert, err := tls.LoadX509KeyPair(filepath.Join(dir, "certs", "client-cert.pem"), filepath.Join(dir, "certs", "client-key.pem"))
					if err != nil {
						t.Fatal(err)
					}
					roots := x509.NewCertPool()
					roots.AddCert(root.cert)
					transport := &http.Transport{TLSClientConfig: &tls.Config{Certificates: []tls.Certificate{cert}, RootCAs: roots}}
					defer transport.CloseIdleConnections()
					clientHTTP.Transport = transport
					url = "https://" + target
					// The exported credential must omit the proven self-signed root.
					if len(cert.Certificate) != 2 {
						t.Errorf("BuildKit chain has %d certificates, want leaf+issuer", len(cert.Certificate))
					}
				}
				response, err := clientHTTP.Get(url + "/v2/")
				if err != nil {
					t.Fatal(err)
				}
				defer response.Body.Close()
				_, _ = io.Copy(io.Discard, response.Body)
				if response.StatusCode != http.StatusOK {
					t.Fatalf("registry returned %d, want 200", response.StatusCode)
				}
			})
		}
	}
}
