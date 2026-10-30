package commands

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"net"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

type transitioningEnrollmentAgent struct {
	acmeProvisioningServer
	transition func(string) error
}

func (s *transitioningEnrollmentAgent) StartACMEProvisioning(ctx context.Context, req *pb.StartACMEProvisioningRequest) (*pb.StartACMEProvisioningResponse, error) {
	response, err := s.acmeProvisioningServer.StartACMEProvisioning(ctx, req)
	if err == nil {
		err = s.transition(response.PrincipalUri)
	}
	return response, err
}

// The real agent opens TLS on plaintextPort+1 and closes plaintext as its
// enrollment RPC returns. Verify through that transition, including follow-up
// RPCs on the returned connection, without registering the device twice.
func TestDesktopEnrollmentTransitionsToAuthenticatedConnection(t *testing.T) {
	for _, mode := range []string{"success", "delayed listener", "wrong device", "wrong organization status", "unreachable"} {
		t.Run(mode, func(t *testing.T) {
			auth := oidcEnrollmentAuth(t)
			auth.Name = "chosen"
			issue, roots := desktopEnrollmentTestCA(t, auth)
			originalRefresh := ensureFreshCertificateFn
			ensureFreshCertificateFn = func(context.Context, *config.AuthConfig) error { return nil }
			t.Cleanup(func() { ensureFreshCertificateFn = originalRefresh })
			plain, secure := desktopEnrollmentTestPorts(t)
			plainServer := grpc.NewServer()
			t.Cleanup(plainServer.Stop)
			cloud := &oidcEnrollmentServer{response: &cloudpb.EnrollDeviceResponse{CredentialKind: "eab", EabKeyId: "eab-id", EabHmacKey: strings.Repeat("ab", 32)}}
			cloudListener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			cloudServer := grpc.NewServer()
			cloudpb.RegisterDeviceEnrollmentServiceServer(cloudServer, cloud)
			cloudpb.RegisterOperatorSessionServiceServer(cloudServer, cloud)
			go cloudServer.Serve(cloudListener)
			t.Cleanup(cloudServer.Stop)
			auth.CloudGRPC = cloudListener.Addr().String()
			load := seedConfig(t, &config.Config{CurrentContext: "other", Auth: []config.AuthConfig{{Name: "other"}, *auth}})
			var secureServer *grpc.Server
			defer func() {
				if secureServer != nil {
					secureServer.Stop()
				}
			}()
			agent := &transitioningEnrollmentAgent{}
			agent.transition = func(principal string) error {
				// A real TLS certificate is required, with the exact freshly
				// minted identity, even if a different device shares its CA.
				serverPrincipal := principal
				if mode == "wrong device" {
					serverPrincipal = certs.DeviceSPIFFEURI(testOperatorTenant, "11111111-1111-4111-8111-111111111111")
				}
				pair, err := issue(serverPrincipal)
				if err != nil {
					return err
				}
				secureServer = grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{
					Certificates: []tls.Certificate{pair}, ClientCAs: roots, ClientAuth: tls.RequireAndVerifyClientCert,
				})))
				reported := principal
				if mode == "wrong organization status" {
					reported = certs.DeviceSPIFFEURI("11111111-1111-4111-8111-111111111111", "11111111-1111-4111-8111-111111111111")
				}
				pb.RegisterWendyProvisioningServiceServer(secureServer, infoProvisioningV2Server{response: &pb.IsProvisionedResponse{ResponseType: &pb.IsProvisionedResponse_Provisioned{Provisioned: &pb.ProvisionedResponse{PrincipalUri: reported}}}})
				// Close the listening socket before replying, leaving the
				// in-flight RPC free to finish on its existing connection.
				plain.Close()
				go plainServer.GracefulStop()
				if mode == "unreachable" {
					secure.Close()
					return nil
				}
				go func() {
					if mode == "delayed listener" {
						time.Sleep(300 * time.Millisecond)
					}
					secureServer.Serve(secure)
				}()
				return nil
			}
			pb.RegisterWendyProvisioningServiceServer(plainServer, agent)
			pb.RegisterWendyDeviceInfoServiceServer(plainServer, agent)
			go plainServer.Serve(plain)
			conn, err := grpcclient.Connect(context.Background(), plain.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancel()
			next, err := DesktopEnrollDevice(ctx, conn, "chosen", testOperatorTenant, "test-device")
			if mode == "success" || mode == "delayed listener" {
				if err != nil {
					t.Fatal(err)
				}
				defer next.Close()
				if next == conn || !next.IsMTLS || next.Addr != secure.Addr().String() {
					t.Fatalf("did not replace plaintext connection: %+v", next)
				}
				state, err := DesktopDeviceEnrollment(ctx, next)
				if err != nil || !state.Enrolled || state.OrganizationID != testOperatorTenant {
					t.Fatalf("follow-up enrollment read = %+v, %v", state, err)
				}
			} else if err == nil || next != nil {
				t.Fatalf("accepted unverified enrollment: %v", err)
			}
			if cloud.registered != 1 || cloud.req == nil || agent.req == nil {
				t.Fatal("enrollment was missing or repeated")
			}
			if load().CurrentContext != "other" {
				t.Fatal("changed the global account")
			}
		})
	}
}

func desktopEnrollmentTestPorts(t *testing.T) (net.Listener, net.Listener) {
	t.Helper()
	for i := 0; i < 100; i++ {
		plain, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := plain.Addr().(*net.TCPAddr).Port
		secure, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port+1)))
		if err == nil {
			t.Cleanup(func() { plain.Close(); secure.Close() })
			return plain, secure
		}
		plain.Close()
	}
	t.Fatal("could not reserve adjacent enrollment ports")
	return nil, nil
}

func desktopEnrollmentTestCA(t *testing.T, auth *config.AuthConfig) (func(string) (tls.Certificate, error), *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	root := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, root, root, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	root, err = x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(root)
	issue := func(principal string, public any) ([]byte, error) {
		uri, err := url.Parse(principal)
		if err != nil {
			return nil, err
		}
		template := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), URIs: []*url.URL{uri}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}}
		return x509.CreateCertificate(rand.Reader, template, root, public, key)
	}
	operator := &auth.Certificates[0]
	operatorKey, err := certs.ParseSigningPrivateKeyPEM([]byte(operator.PemPrivateKey))
	if err != nil {
		t.Fatal(err)
	}
	der, err = issue(operator.PrincipalURI, operatorKey.Public())
	if err != nil {
		t.Fatal(err)
	}
	operator.PemCertificate = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	operator.PemCertificateChain = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: root.Raw}))
	return func(principal string) (tls.Certificate, error) {
		leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return tls.Certificate{}, err
		}
		der, err := issue(principal, leafKey.Public())
		return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: leafKey}, err
	}, roots
}
