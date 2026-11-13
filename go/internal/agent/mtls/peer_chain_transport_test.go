package mtls

import (
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
)

// Use an ECDSA endpoint key so crypto/tls can prove possession while the
// production verifiers validate its actual ML-DSA issuer signature. Changing
// parsed certificate fields would not exercise the bytes carried by TLS.
func TestDirectMLDSAPolicyOverTLS(t *testing.T) {
	for _, peer := range []string{"client", "server"} {
		for _, policy := range []string{"valid", "leaf-critical", "issuer-critical", "leaf-unknown-eku"} {
			t.Run(peer+"/"+policy, func(t *testing.T) {
				root, rootKey := buildMLDSACACert(t, pkix.Name{CommonName: "transport root"}, true)
				critical := func(tbs *tbsCertificate) {
					tbs.Extensions = append(tbs.Extensions, pkix.Extension{Id: asn1.ObjectIdentifier{1, 2, 3, 4}, Critical: true, Value: []byte{5, 0}})
				}
				var rootEdits []func(*tbsCertificate)
				if policy == "issuer-critical" {
					rootEdits = append(rootEdits, critical)
				}
				root = signPeerTestCertificate(t, root, root, rootKey, rootEdits...)
				rootPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: root.Raw}))
				pair := func(invalid bool) tls.Certificate {
					leafPEM, keyPEM := testLeafCertificate(t, "endpoint")
					block, _ := pem.Decode([]byte(leafPEM))
					leaf, err := x509.ParseCertificate(block.Bytes)
					if err != nil {
						t.Fatal(err)
					}
					edits := []func(*tbsCertificate){func(tbs *tbsCertificate) { tbs.Signature = algID{Algorithm: oidMLDSA65} }}
					if invalid && policy == "leaf-critical" {
						edits = append(edits, critical)
					}
					if invalid && policy == "leaf-unknown-eku" {
						edits = append(edits, func(tbs *tbsCertificate) {
							eku, err := buildEKUExt([]asn1.ObjectIdentifier{{1, 2, 3, 99}})
							if err != nil {
								t.Fatal(err)
							}
							for i := range tbs.Extensions {
								if tbs.Extensions[i].Id.Equal(eku.Id) {
									tbs.Extensions[i] = eku
								}
							}
						})
					}
					leaf = signPeerTestCertificate(t, leaf, root, rootKey, edits...)
					keyBlock, _ := pem.Decode([]byte(keyPEM))
					key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
					if err != nil {
						t.Fatal(err)
					}
					return tls.Certificate{Certificate: [][]byte{leaf.Raw}, PrivateKey: key, Leaf: leaf}
				}
				serverPair, clientPair := pair(peer == "server"), pair(peer == "client")
				// Only the peer under test uses the malformed issuer policy. The
				// opposite verifier still checks the original unmodified root.
				validRoot := signPeerTestCertificate(t, root, root, rootKey, func(tbs *tbsCertificate) {
					filtered := tbs.Extensions[:0]
					for _, extension := range tbs.Extensions {
						if !extension.Id.Equal(asn1.ObjectIdentifier{1, 2, 3, 4}) {
							filtered = append(filtered, extension)
						}
					}
					tbs.Extensions = filtered
				})
				serverRoot := validRoot
				if peer == "client" {
					serverRoot = root
				} else {
					rootPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: root.Raw}))
				}
				if peer == "client" {
					rootPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: validRoot.Raw}))
				}
				verifyClient := buildVerifyPeerCertificate(x509.NewCertPool(), []*x509.Certificate{serverRoot}, nil, time.Time{})
				verifyServer, err := certs.BuildServerVerifyConnection(certs.ServerVerifyOpts{ChainPEM: rootPEM, ExpectedIdentity: &certs.WendyIdentity{OrgID: 7, EntityType: certs.EntityAsset, EntityID: "42"}})
				if err != nil {
					t.Fatal(err)
				}
				var checked, delivered atomic.Int32
				var policyErr error // Read only after both handshake goroutines have joined.
				serverConfig := &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{serverPair}, ClientAuth: tls.RequireAnyClientCert,
					VerifyPeerCertificate: func(raw [][]byte, chains [][]*x509.Certificate) error {
						err := verifyClient(raw, chains)
						if peer == "client" {
							checked.Add(1)
							policyErr = err
						}
						return err
					}}
				ownPair := pair(false)
				ownKey, err := x509.MarshalPKCS8PrivateKey(ownPair.PrivateKey)
				if err != nil {
					t.Fatal(err)
				}
				clientConfig, err := NewClientTLSConfig(
					string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ownPair.Certificate[0]})),
					string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: validRoot.Raw})),
					string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: ownKey})), nil)
				if err != nil {
					t.Fatal(err)
				}
				clientConfig.MinVersion = tls.VersionTLS13
				clientConfig.Certificates = []tls.Certificate{clientPair}
				chainVerify := clientConfig.VerifyPeerCertificate
				clientConfig.VerifyPeerCertificate = func(raw [][]byte, chains [][]*x509.Certificate) error {
					err := chainVerify(raw, chains)
					if peer == "server" {
						checked.Add(1)
						policyErr = err
					}
					return err
				}
				clientConfig.VerifyConnection = func(state tls.ConnectionState) error {
					err := verifyServer(state)
					if peer == "server" {
						policyErr = err
					}
					return err
				}
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				defer listener.Close()
				done := make(chan error, 1)
				go func() {
					connection, err := listener.Accept()
					if err != nil {
						done <- err
						return
					}
					defer connection.Close()
					_ = connection.SetDeadline(time.Now().Add(3 * time.Second))
					secured := tls.Server(connection, serverConfig)
					if err = secured.Handshake(); err == nil {
						var request [1]byte
						_, err = io.ReadFull(secured, request[:])
						if err == nil && request[0] == 42 {
							delivered.Add(1)
							_, err = secured.Write([]byte{43})
						} else if err == nil {
							err = fmt.Errorf("unexpected request %v", request)
						}
					}
					done <- err
				}()
				connection, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
				if err != nil {
					_ = listener.Close()
					<-done
					t.Fatal(err)
				}
				_ = connection.SetDeadline(time.Now().Add(3 * time.Second))
				secured := tls.Client(connection, clientConfig)
				clientErr := secured.Handshake()
				if clientErr == nil {
					_, clientErr = secured.Write([]byte{42})
					if clientErr == nil {
						var response [1]byte
						_, clientErr = io.ReadFull(secured, response[:])
						if clientErr == nil && response[0] != 43 {
							clientErr = fmt.Errorf("unexpected response %v", response)
						}
					}
				}
				_ = connection.Close()
				serverErr := <-done
				if checked.Load() != 1 {
					t.Fatalf("production peer verifier not reached exactly once: %d; client=%v server=%v", checked.Load(), clientErr, serverErr)
				}
				if policy == "valid" {
					if clientErr != nil || serverErr != nil || delivered.Load() != 1 {
						t.Fatalf("valid signed chain did not carry application bytes: client=%v server=%v delivered=%d", clientErr, serverErr, delivered.Load())
					}
				} else {
					if clientErr == nil || serverErr == nil || delivered.Load() != 0 || policyErr == nil {
						t.Fatalf("MLDSA_TRANSPORT_POLICY malformed chain admitted: client=%v server=%v delivered=%d policy=%v", clientErr, serverErr, delivered.Load(), policyErr)
					}
					t.Logf("MLDSA_POLICY_REJECTED %s", policyErr)
				}
			})
		}
	}
}
