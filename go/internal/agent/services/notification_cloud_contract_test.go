package services

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	systempb "github.com/wendylabsinc/wendy/go/proto/gen/systempb"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type notificationContractCloud struct {
	cloudpb.UnimplementedNotificationServiceServer
	mu       sync.Mutex
	requests map[string][]proto.Message
	result   func(string, int) error
}

func (c *notificationContractCloud) record(id string, request proto.Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requests[id] = append(c.requests[id], proto.Clone(request))
	return c.result(id, len(c.requests[id]))
}

func (c *notificationContractCloud) CreateNotificationV2(_ context.Context, request *cloudpb.CreateNotificationV2Request) (*cloudpb.CreateNotificationV2Response, error) {
	return &cloudpb.CreateNotificationV2Response{NotificationId: request.GetNotificationId()}, c.record(request.GetNotificationId(), request)
}

type notificationContractCloudV2 struct {
	cloudpbv2.UnimplementedNotificationServiceServer
	cloud *notificationContractCloud
}

func (c *notificationContractCloudV2) CreateNotificationV2(_ context.Context, request *cloudpbv2.CreateNotificationV2Request) (*cloudpbv2.CreateNotificationV2Response, error) {
	return &cloudpbv2.CreateNotificationV2Response{NotificationId: request.GetNotificationId()}, c.cloud.record(request.GetNotificationId(), request)
}

// Unlike the older header-presence fixture, this ingress requires a verified
// client certificate and independently verifies the proof over received bytes.
// ACME enrollment is identified by its mTLS URI and uses the actual v2 RPC.
func newNotificationContractCloud(t *testing.T, acme bool, result func(string, int) error) (*CloudNotificationSender, *notificationContractCloud, *observer.ObservedLogs) {
	t.Helper()
	t.Setenv("WENDY_DEVICE_CLOUD_URL", "")
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, BasicConstraintsValid: true, IsCA: true,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}))
	deviceURI := "urn:wendy:org:7:asset:9"
	if acme {
		deviceURI = "spiffe://wendy.sh/tenant/2558fd76-afc7-466e-9613-6b715296a526/device/dev-1"
	}
	uri, err := url.Parse(deviceURI)
	if err != nil {
		t.Fatal(err)
	}
	deviceKey, keyPEM := deviceProofTestKeyPEM(t)
	device := &x509.Certificate{SerialNumber: big.NewInt(17), NotBefore: ca.NotBefore, NotAfter: ca.NotAfter,
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, URIs: []*url.URL{uri}}
	deviceDER, err := x509.CreateCertificate(rand.Reader, device, ca, &deviceKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM([]byte(caPEM))
	cloud := &notificationContractCloud{requests: make(map[string][]proto.Message), result: result}
	verify := func(ctx context.Context, request any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		p, ok := peer.FromContext(ctx)
		if !ok {
			return nil, status.Error(codes.Unauthenticated, "missing TLS peer")
		}
		auth, ok := p.AuthInfo.(credentials.TLSInfo)
		if !ok || len(auth.State.VerifiedChains) == 0 || len(auth.State.PeerCertificates) == 0 {
			return nil, status.Error(codes.Unauthenticated, "unverified device certificate")
		}
		leaf := auth.State.PeerCertificates[0]
		if len(leaf.URIs) != 1 || leaf.URIs[0].String() != deviceURI {
			return nil, status.Error(codes.Unauthenticated, "wrong device identity")
		}
		md, _ := metadata.FromIncomingContext(ctx)
		if acme {
			if info.FullMethod != cloudpbv2.NotificationService_CreateNotificationV2_FullMethodName {
				return nil, status.Error(codes.Unimplemented, "ACME device used the legacy notification RPC")
			}
			for _, name := range []string{"uri", "certificate-serial", "timestamp", "signature"} {
				if len(md.Get("x-wendy-device-"+name)) != 0 {
					return nil, status.Error(codes.InvalidArgument, "ACME device sent a legacy proof")
				}
			}
		} else {
			get := func(name string) string {
				values := md.Get("x-wendy-device-" + name)
				if len(values) != 1 {
					return ""
				}
				return values[0]
			}
			timestamp, parseErr := strconv.ParseInt(get("timestamp"), 10, 64)
			if parseErr != nil || time.Since(time.Unix(timestamp, 0)).Abs() > time.Minute || get("uri") != deviceURI || get("certificate-serial") != "11" {
				return nil, status.Error(codes.Unauthenticated, "proof does not identify the authenticated device")
			}
			payload, err := (proto.MarshalOptions{Deterministic: true}).Marshal(request.(proto.Message))
			if err != nil {
				return nil, err
			}
			// These literal framing bytes are the Cloud contract, not the
			// production canonicalNotificationDeviceProof helper under test.
			prefix := "wendy-device-request-proof/v2\x00" + strings.TrimPrefix(info.FullMethod, "/") + "\x00" + deviceURI + "\x0011\x00" + get("timestamp") + "\x00"
			digest := sha256.Sum256(append([]byte(prefix), payload...))
			signature, err := base64.RawURLEncoding.DecodeString(get("signature"))
			public, ok := leaf.PublicKey.(*ecdsa.PublicKey)
			if err != nil || !ok || !ecdsa.VerifyASN1(public, digest[:], signature) {
				return nil, status.Error(codes.Unauthenticated, "received notification proof failed verification")
			}
		}
		return handler(ctx, request)
	}
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS13,
		Certificates: []tls.Certificate{{Certificate: [][]byte{caDER}, PrivateKey: caKey}}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: roots})), grpc.UnaryInterceptor(verify))
	cloudpb.RegisterNotificationServiceServer(server, cloud)
	cloudpbv2.RegisterNotificationServiceServer(server, &notificationContractCloudV2{cloud: cloud})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go server.Serve(listener) //nolint:errcheck
	t.Cleanup(server.Stop)
	provisioning := &ProvisioningService{enrolled: true, cloudHost: listener.Addr().String(), orgID: 7, assetID: 9,
		certPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: deviceDER})), chainPEM: caPEM, keyPEM: keyPEM}
	if acme {
		provisioning.principalURI = deviceURI
	}
	core, logs := observer.New(zap.WarnLevel)
	sender := NewCloudNotificationSender(zap.New(core), provisioning)
	t.Cleanup(func() {
		sender.mu.Lock()
		defer sender.mu.Unlock()
		if sender.connection != nil {
			_ = sender.connection.Close()
		}
	})
	return sender, cloud, logs
}

func TestAppNotificationsCrossUnixSocketAndVerifiedCloudMTLS(t *testing.T) {
	for _, acme := range []bool{false, true} {
		t.Run(fmt.Sprintf("acme=%t", acme), func(t *testing.T) {
			const privateError = "private audience fixture@example.invalid and secret body"
			sender, cloud, logs := newNotificationContractCloud(t, acme, func(_ string, attempt int) error {
				switch attempt {
				case 1:
					return status.Error(codes.Unavailable, privateError)
				case 2:
					return nil
				default:
					return status.Error(codes.AlreadyExists, privateError)
				}
			})
			root, err := os.MkdirTemp("/tmp", "wn-notify-")
			if err != nil {
				t.Fatal(err)
			}
			previousRoot := AppSystemAPISocketRootPath
			AppSystemAPISocketRootPath = root
			defer func() { AppSystemAPISocketRootPath = previousRoot; _ = os.RemoveAll(root) }()
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			manager := NewAppSystemAPISocketManager(ctx, zap.NewNop(), sender)
			defer manager.stopAll()
			directory, err := manager.Ensure("dev.example.notifier", "main", []string{SystemAPICapabilityNotifications})
			if err != nil {
				t.Fatal(err)
			}
			connection, err := grpc.NewClient("unix://"+filepath.Join(directory, SystemAPISocketFilename), grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil {
				t.Fatal(err)
			}
			defer connection.Close()
			client := systempb.NewNotificationServiceClient(connection)
			request := validSystemNotificationRequest()
			for index, want := range []codes.Code{codes.Unavailable, codes.OK, codes.AlreadyExists} {
				response, err := client.Send(ctx, request)
				if status.Code(err) != want || (err == nil && response.GetNotificationId() != request.GetNotificationId()) {
					t.Fatalf("verified Cloud contract call %d: response=%v err=%v, want %v", index+1, response, err, want)
				}
				cloud.mu.Lock()
				count := len(cloud.requests[request.GetNotificationId()])
				cloud.mu.Unlock()
				if count != index+1 {
					t.Fatalf("one app Send made %d Cloud attempts, want %d total", count, index+1)
				}
			}
			cloud.mu.Lock()
			defer cloud.mu.Unlock()
			requests := cloud.requests[request.GetNotificationId()]
			for _, actual := range requests {
				if !proto.Equal(actual, requests[0]) {
					t.Fatal("explicit retry changed the notification identity or content")
				}
				fields := actual.ProtoReflect()
				if fields.Get(fields.Descriptor().Fields().ByName("app_id")).String() != "dev.example.notifier" || fields.Has(fields.Descriptor().Fields().ByName("organization_id")) {
					t.Fatal("Cloud request did not preserve trusted app/device attribution")
				}
			}
			if logs.Len() != 2 || strings.Contains(fmt.Sprint(logs.All()), privateError) {
				t.Fatalf("Cloud sender logs exposed private response content: %v", logs.All())
			}
		})
	}
}

func TestCampaignNotificationsRetryThroughVerifiedCloudMTLS(t *testing.T) {
	const retryID = "00000000-0000-4000-8000-000000000001"
	const duplicateID = "00000000-0000-4000-8000-000000000002"
	sender, cloud, _ := newNotificationContractCloud(t, true, func(id string, attempt int) error {
		if id == duplicateID {
			return status.Error(codes.AlreadyExists, "already delivered")
		}
		if attempt == 1 {
			return status.Error(codes.Unavailable, "transient outage")
		}
		return nil
	})
	manager, err := data.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	campaign, err := data.ParseCampaign(inferenceTestYAML(t))
	if err != nil {
		t.Fatal(err)
	}
	campaign.Notify.Webhook = ""
	job := &campaignInferenceJob{owner: &campaignInferenceManager{service: NewDataService(manager), sender: &CampaignCloudSender{Cloud: sender}}, campaign: campaign}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	queue := make(chan DetectionNotification, 1)
	done := make(chan struct{})
	go func() { defer close(done); job.notifications(ctx, queue) }()
	defer func() { cancel(); <-done }()
	for _, id := range []string{retryID, duplicateID} {
		job.mu.Lock()
		job.status.NotificationError = "pending"
		job.mu.Unlock()
		queue <- DetectionNotification{ID: id, Campaign: campaign.Name, Event: "person_detected", SourceID: "fixture-camera", Model: "fixture-model", Revision: "revision-1", Count: 2}
		for {
			job.mu.Lock()
			reported := job.status.NotificationError
			job.mu.Unlock()
			if reported != "pending" {
				if (id == retryID && reported != "") || (id == duplicateID && !strings.Contains(reported, "AlreadyExists")) {
					t.Fatalf("campaign Cloud retry outcome: %q", reported)
				}
				break
			}
			select {
			case <-ctx.Done():
				t.Fatal("campaign Cloud delivery did not settle")
			case <-time.After(time.Millisecond):
			}
		}
	}
	cloud.mu.Lock()
	defer cloud.mu.Unlock()
	if len(cloud.requests[retryID]) != 2 || len(cloud.requests[duplicateID]) != 1 {
		t.Fatalf("Cloud attempt counts: transient=%d duplicate=%d", len(cloud.requests[retryID]), len(cloud.requests[duplicateID]))
	}
	first := cloud.requests[retryID][0].(*cloudpbv2.CreateNotificationV2Request)
	if !proto.Equal(first, cloud.requests[retryID][1]) || first.GetAppId() != data.CampaignAppID(campaign.Name) || first.OrganizationId != nil || first.GetMetadata().AsMap()["source_id"] != "fixture-camera" || first.GetMetadata().AsMap()["count"] != float64(2) {
		t.Fatal("campaign retry changed identity, trusted attribution or detection metadata")
	}
}

func TestCampaignCloudFailureRedactsStatusAndWarnings(t *testing.T) {
	for _, code := range []codes.Code{codes.PermissionDenied, codes.Unavailable} {
		t.Run(code.String(), func(t *testing.T) {
			const privateError = "private audience fixture@example.invalid token=fixture-secret"
			const failedID = "00000000-0000-4000-8000-000000000003"
			const recoveredID = "00000000-0000-4000-8000-000000000004"
			sender, cloud, logs := newNotificationContractCloud(t, true, func(id string, _ int) error {
				if id == failedID {
					return status.Error(code, privateError)
				}
				return nil
			})
			manager, err := data.NewManager(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			warnings := make(chan string, 1)
			manager.SetWarnLogger(func(message string) { warnings <- message })
			campaign, err := data.ParseCampaign(inferenceTestYAML(t))
			if err != nil {
				t.Fatal(err)
			}
			campaign.Notify.Webhook = ""
			job := &campaignInferenceJob{owner: &campaignInferenceManager{service: NewDataService(manager), sender: &CampaignCloudSender{Cloud: sender}}, campaign: campaign}
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			queue := make(chan DetectionNotification, 1)
			done := make(chan struct{})
			go func() { defer close(done); job.notifications(ctx, queue) }()
			defer func() { cancel(); <-done }()
			queue <- DetectionNotification{ID: failedID, Campaign: campaign.Name, Event: "person_detected"}
			var warning string
			select {
			case warning = <-warnings:
			case <-ctx.Done():
				t.Fatal("failed Cloud delivery did not report its outcome")
			}
			job.mu.Lock()
			reported := job.status.NotificationError
			job.mu.Unlock()
			for _, text := range []string{reported, warning} {
				if strings.Contains(text, privateError) || strings.Contains(text, "fixture-secret") || !strings.Contains(text, code.String()) {
					t.Errorf("campaign error exposed private Cloud response or lost status code: %q", text)
				}
			}
			if strings.Contains(fmt.Sprint(logs.All()), privateError) {
				t.Error("sender warning exposed private Cloud response")
			}
			cloud.mu.Lock()
			attempts := len(cloud.requests[failedID])
			cloud.mu.Unlock()
			wantAttempts := 1
			if code == codes.Unavailable {
				wantAttempts = 3
			}
			if attempts != wantAttempts {
				t.Fatalf("error redaction changed retry classification: got %d attempts, want %d", attempts, wantAttempts)
			}
			queue <- DetectionNotification{ID: recoveredID, Campaign: campaign.Name, Event: "person_detected"}
			for {
				job.mu.Lock()
				reported = job.status.NotificationError
				job.mu.Unlock()
				if reported == "" {
					break
				}
				select {
				case <-ctx.Done():
					t.Fatal("successful Cloud delivery did not clear the error")
				case <-time.After(time.Millisecond):
				}
			}
			select {
			case warning := <-warnings:
				t.Fatalf("successful delivery emitted a warning: %q", warning)
			default:
			}
		})
	}
}
