package commands

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/legacycertproof"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type fallbackTransportAgent struct {
	agentpb.UnimplementedWendyAgentServiceServer
	agentpbv2.UnimplementedWendyBuildServiceServer
	agentpbv2.UnimplementedWendyTimeSyncServiceServer
	name     string
	id       int32
	versions atomic.Int32
	builds   chan *agentpbv2.BuildSpec
}

func (s *fallbackTransportAgent) GetAgentVersion(context.Context, *agentpb.GetAgentVersionRequest) (*agentpb.GetAgentVersionResponse, error) {
	s.versions.Add(1)
	return &agentpb.GetAgentVersionResponse{Version: s.name, Os: "linux", CpuArchitecture: "arm64"}, nil
}
func (s *fallbackTransportAgent) GetClock(context.Context, *agentpbv2.GetClockRequest) (*agentpbv2.GetClockResponse, error) {
	return &agentpbv2.GetClockResponse{UnixNanos: time.Now().UnixNano()}, nil
}
func (s *fallbackTransportAgent) GetBuildCapabilities(context.Context, *agentpbv2.GetBuildCapabilitiesRequest) (*agentpbv2.GetBuildCapabilitiesResponse, error) {
	return &agentpbv2.GetBuildCapabilitiesResponse{BuilderEnabled: true, BuildkitAvailable: true, NativePlatforms: []string{"linux/arm64"}}, nil
}
func (s *fallbackTransportAgent) BuildImage(stream grpc.BidiStreamingServer[agentpbv2.BuildImageRequest, agentpbv2.BuildImageProgress]) error {
	req, err := stream.Recv()
	if err != nil {
		return err
	}
	if req.GetSpec() == nil {
		return status.Error(codes.InvalidArgument, "missing build spec")
	}
	s.builds <- proto.Clone(req.Spec).(*agentpbv2.BuildSpec)
	if _, err := stream.Recv(); err != io.EOF {
		return status.Error(codes.InvalidArgument, "build request did not close its send side")
	}
	if err := stream.Send(&agentpbv2.BuildImageProgress{Event: &agentpbv2.BuildImageProgress_LogLine{LogLine: "built on " + s.name}}); err != nil {
		return err
	}
	return stream.Send(&agentpbv2.BuildImageProgress{Event: &agentpbv2.BuildImageProgress_Result{Result: &agentpbv2.BuildImageResult{ImageDigest: "sha256:" + strings.Repeat(strconv.Itoa(int(s.id)), 64)}}})
}

type fallbackTransportBroker struct {
	cloudpb.UnimplementedTunnelBrokerServiceServer
	targets map[int32]string
	mu      sync.Mutex
	opened  []int32
}

func (b *fallbackTransportBroker) ClientTunnel(stream grpc.BidiStreamingServer[cloudpb.ClientTunnelMessage, cloudpb.TunnelData]) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	open := first.GetOpen()
	if open == nil || open.Host != "localhost" || open.Port != 50052 {
		return status.Error(codes.InvalidArgument, "unexpected tunnel target")
	}
	md, _ := metadata.FromIncomingContext(stream.Context())
	if got := md.Get(legacycertproof.IdentityHeader); len(got) != 1 || got[0] != certs.UserURN(7, "fixture") {
		return status.Error(codes.Unauthenticated, "missing selected operator proof")
	}
	address, ok := b.targets[open.AssetId]
	if !ok {
		return status.Error(codes.NotFound, "unknown fixture asset")
	}
	remote, err := (&net.Dialer{}).DialContext(stream.Context(), "tcp", address)
	if err != nil {
		return err
	}
	defer remote.Close()
	b.mu.Lock()
	b.opened = append(b.opened, open.AssetId)
	b.mu.Unlock()
	done := make(chan error, 2)
	go func() {
		for {
			msg, err := stream.Recv()
			if err != nil {
				done <- err
				return
			}
			data := msg.GetData()
			if data == nil {
				done <- errors.New("missing tunnel payload")
				return
			}
			if _, err := remote.Write(data.Payload); err != nil {
				done <- err
				return
			}
			if data.HalfClose {
				done <- io.EOF
				return
			}
		}
	}()
	go func() {
		buf := make([]byte, 32768)
		for {
			n, err := remote.Read(buf)
			if n > 0 {
				if sendErr := stream.Send(&cloudpb.TunnelData{Payload: append([]byte(nil), buf[:n]...)}); sendErr != nil {
					done <- sendErr
					return
				}
			}
			if err != nil {
				done <- err
				return
			}
		}
	}()
	select {
	case <-stream.Context().Done():
		return stream.Context().Err()
	case err := <-done:
		if err == io.EOF {
			return nil
		}
		return err
	}
}

// All agents and the legacy broker share a disposable CA. The broker carries
// the real ClientTunnel byte stream; the selected agent still performs its own
// nested mTLS handshake and verifies the requested org/asset identity.
func fallbackTransportPKI(t *testing.T) (config.CertificateInfo, func(int32) *tls.Config) {
	t.Helper()
	issue := func(serial int64, principal string, ca bool, parent *x509.Certificate, signer *ecdsa.PrivateKey) (*x509.Certificate, string, *ecdsa.PrivateKey) {
		keyPEM, key := newP256KeyPEM(t)
		template := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "fallback fixture"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), BasicConstraintsValid: true, IsCA: ca, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}}
		if ca {
			template.KeyUsage |= x509.KeyUsageCertSign
			parent = template
			signer = key
		} else {
			template.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
			u, err := url.Parse(principal)
			if err != nil {
				t.Fatal(err)
			}
			template.URIs = []*url.URL{u}
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
	root, _, rootKey := issue(1, "", true, nil, nil)
	encode := func(c *x509.Certificate) string {
		return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw}))
	}
	operator, key, _ := issue(2, certs.UserURN(7, "fixture"), false, root, rootKey)
	credential := config.CertificateInfo{OrganizationID: 7, UserID: "fixture", PemCertificate: encode(operator), PemPrivateKey: key, PemCertificateChain: encode(root)}
	pool := x509.NewCertPool()
	pool.AddCert(root)
	return credential, func(id int32) *tls.Config {
		server, _, key := issue(100+int64(id), certs.AssetURN(7, id), false, root, rootKey)
		return &tls.Config{MinVersion: tls.VersionTLS12, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool, Certificates: []tls.Certificate{{Certificate: [][]byte{server.Raw}, PrivateKey: key}}}
	}
}

type fallbackTransportFixture struct {
	agents          map[string]*fallbackTransportAgent
	broker          *fallbackTransportBroker
	directFailures  []string
	cloudSelections []string
}

func newFallbackTransportFixture(t *testing.T) *fallbackTransportFixture {
	t.Helper()
	restoreDeviceGlobals(t)
	stubNonInteractive(t)
	setPinCache(t)
	t.Setenv("WENDY_AGENT_SOCKET", "")
	t.Setenv(runNoCloudFallbackEnv, "")
	credential, serverTLS := fallbackTransportPKI(t)
	f := &fallbackTransportFixture{agents: make(map[string]*fallbackTransportAgent), broker: &fallbackTransportBroker{targets: make(map[int32]string)}}
	addresses := make(map[string]string)
	start := func(server *grpc.Server) string {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		go server.Serve(listener)
		t.Cleanup(server.Stop)
		return listener.Addr().String()
	}
	for index, name := range []string{"primary.test", "lan-member.test", "cloud-member.test", "build-host.test"} {
		agent := &fallbackTransportAgent{name: name, id: int32(index + 1), builds: make(chan *agentpbv2.BuildSpec, 4)}
		f.agents[name] = agent
		server := grpc.NewServer(grpc.Creds(credentials.NewTLS(serverTLS(agent.id))))
		agentpb.RegisterWendyAgentServiceServer(server, agent)
		agentpbv2.RegisterWendyBuildServiceServer(server, agent)
		agentpbv2.RegisterWendyTimeSyncServiceServer(server, agent)
		addresses[name] = start(server)
		f.broker.targets[agent.id] = addresses[name]
	}
	broker := grpc.NewServer(grpc.Creds(credentials.NewTLS(serverTLS(99))))
	cloudpb.RegisterTunnelBrokerServiceServer(broker, f.broker)
	brokerAddress := start(broker)
	cfg := &config.Config{CurrentContext: "fixture", DefaultDevice: "primary.test", Auth: []config.AuthConfig{{Name: "fixture", CloudGRPC: brokerAddress, Certificates: []config.CertificateInfo{credential}}}, DevicePins: make(map[string]config.DevicePin)}
	for name, agent := range f.agents {
		cfg.DevicePins[name] = config.DevicePin{OrgID: 7, AssetID: strconv.Itoa(int(agent.id)), CloudGRPC: brokerAddress, Source: config.PinSourceCloud}
	}
	setTempConfig(t, cfg)
	deviceFlag = "primary.test"
	refused, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedAddress := refused.Addr().String()
	refused.Close()
	oldLookup, oldBrowse, oldDiscover := osLookupHostFn, lanBrowseFn, discoverLANDevices
	oldUSB, oldFind, oldDial := usbDirectCandidatesFn, findDeviceByIDFn, dialAgentLadderFn
	oldCloud, oldFresh := connectCloudLookupFn, ensureFreshCertificateFn
	oldBroker, oldStart := connectSessionBrokerFn, startSessionBrokerFn
	t.Cleanup(func() {
		osLookupHostFn, lanBrowseFn, discoverLANDevices = oldLookup, oldBrowse, oldDiscover
		usbDirectCandidatesFn, findDeviceByIDFn, dialAgentLadderFn = oldUSB, oldFind, oldDial
		connectCloudLookupFn, ensureFreshCertificateFn = oldCloud, oldFresh
		connectSessionBrokerFn, startSessionBrokerFn = oldBroker, oldStart
	})
	osLookupHostFn = func(context.Context, string) ([]string, error) { return []string{"127.0.0.1"}, nil }
	lanBrowseFn = func(context.Context, time.Duration) ([]models.LANDevice, error) { return nil, nil }
	discoverLANDevices = lanBrowseFn
	usbDirectCandidatesFn = func() []discovery.USBDirectCandidate { return nil }
	findDeviceByIDFn = func(context.Context, string) *SelectedDevice { return nil }
	connectSessionBrokerFn = func(context.Context, string, certs.WendyIdentity) (*grpcclient.AgentConnection, error) {
		return nil, errors.New("no fixture session broker")
	}
	startSessionBrokerFn = func(string, *grpcclient.AgentConnection) error { return nil }
	ensureFreshCertificateFn = func(context.Context, *config.AuthConfig) error { return nil }
	dialAgentLadderFn = func(ctx context.Context, target dialTarget) (*grpcclient.AgentConnection, error, error) {
		name := target.PinKey
		agent := f.agents[name]
		if agent == nil {
			return nil, nil, fmt.Errorf("unexpected direct target %q", name)
		}
		if name == "cloud-member.test" || name == "build-host.test" {
			f.directFailures = append(f.directFailures, name)
			connection, err := (&net.Dialer{}).DialContext(ctx, "tcp", closedAddress)
			if connection != nil {
				connection.Close()
				return nil, nil, errors.New("fixture's refused TCP endpoint unexpectedly answered")
			}
			return nil, nil, err
		}
		connection, err := grpcclient.ConnectWithTLSExpecting(ctx, addresses[name], &credential, nil, target.Expected)
		if err != nil {
			return nil, nil, err
		}
		version, err := connection.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{})
		if err != nil {
			connection.Close()
			return nil, nil, err
		}
		connection.CacheAgentVersion(version)
		return connection, nil, nil
	}
	// Only directory lookup is replaced. Name/pin/session selection, Cloud
	// connection construction, broker RPCs, tunneled TLS and agent RPCs are real.
	connectCloudLookupFn = func(ctx context.Context, auth *config.AuthConfig, selector, _ string, expected *certs.WendyIdentity) (*grpcclient.AgentConnection, error) {
		id, err := strconv.Atoi(selector)
		if err != nil {
			return nil, err
		}
		var selected *fallbackTransportAgent
		for _, agent := range f.agents {
			if agent.id == int32(id) {
				selected = agent
				break
			}
		}
		if selected == nil || auth.CloudGRPC != brokerAddress || expected == nil || expected.OrgID != 7 || expected.EntityID != selector {
			return nil, errors.New("Cloud lookup lost the requested pin/session")
		}
		f.cloudSelections = append(f.cloudSelections, selected.name)
		return connectCloudAsset(ctx, auth, &cloudpb.Asset{Id: int32(id), Name: selected.name}, brokerAddress)
	}
	return f
}

func assertFallbackPeer(t *testing.T, ctx context.Context, connection *grpcclient.AgentConnection, name string, id int32) {
	t.Helper()
	version, err := connection.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{})
	if err != nil {
		t.Fatal(err)
	}
	identity, verified := connection.ObservedServerIdentity()
	if version.Version != name || !verified || identity.OrgID != 7 || identity.EntityID != strconv.Itoa(int(id)) {
		t.Fatalf("request reached wrong authenticated peer: version=%q identity=%+v verified=%t", version.Version, identity, verified)
	}
}
func TestFleetCloudFallbackReachesDistinctLANAndCloudMembers(t *testing.T) {
	f := newFallbackTransportFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, name := range []string{"lan-member.test", "cloud-member.test"} {
		connection, err := connectFleetDevice(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		defer connection.Close()
		assertFallbackPeer(t, ctx, connection, name, f.agents[name].id)
	}
	if fmt.Sprint(f.directFailures) != "[cloud-member.test]" || fmt.Sprint(f.cloudSelections) != "[cloud-member.test]" {
		t.Fatalf("fleet did not preserve its individual routes: direct failures=%v Cloud=%v", f.directFailures, f.cloudSelections)
	}
	f.broker.mu.Lock()
	defer f.broker.mu.Unlock()
	if fmt.Sprint(f.broker.opened) != "[3]" || f.agents["primary.test"].versions.Load() != 0 {
		t.Fatalf("fleet tunneled to primary or LAN member: opens=%v primary requests=%d", f.broker.opened, f.agents["primary.test"].versions.Load())
	}
}
func TestBuildHostCloudFallbackCompletesBuildOnRequestedHost(t *testing.T) {
	f := newFallbackTransportFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	connection, err := connectBuildHost(ctx, "build-host.test")
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	assertFallbackPeer(t, ctx, connection, "build-host.test", 4)
	if _, err := prepareBuildHost(ctx, connection.BuildService, "build-host.test", "linux/arm64", nil); err != nil {
		t.Fatal(err)
	}
	spec := &agentpbv2.BuildSpec{AppId: "fallback-build", Platform: "linux/arm64", PushTarget: &agentpbv2.PushTarget{AssetUuid: "00000000-0000-4000-8000-000000000001", Repository: "fallback-build:latest", AgentPort: 50052}}
	var logs bytes.Buffer
	result, err := streamRemoteBuild(ctx, connection, spec, &logs)
	if err != nil || result.GetImageDigest() != "sha256:"+strings.Repeat("4", 64) || !strings.Contains(logs.String(), "built on build-host.test") {
		t.Fatalf("build did not finish on requested host: result=%v log=%q err=%v", result, logs.String(), err)
	}
	select {
	case got := <-f.agents["build-host.test"].builds:
		if !proto.Equal(got, spec) {
			t.Fatalf("build spec/target changed in transit: %v", got)
		}
	default:
		t.Fatal("requested build host never received the build")
	}
	if len(f.agents["primary.test"].builds) != 0 || fmt.Sprint(f.directFailures) != "[build-host.test]" || fmt.Sprint(f.cloudSelections) != "[build-host.test]" {
		t.Fatalf("build fell back to deployment target: direct=%v Cloud=%v", f.directFailures, f.cloudSelections)
	}
	f.broker.mu.Lock()
	defer f.broker.mu.Unlock()
	if fmt.Sprint(f.broker.opened) != "[4]" {
		t.Fatalf("build tunnel targeted wrong asset: %v", f.broker.opened)
	}
}
