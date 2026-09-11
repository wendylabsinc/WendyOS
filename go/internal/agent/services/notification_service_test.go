package services

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/known/structpb"

	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
	systempb "github.com/wendylabsinc/wendy/go/proto/gen/systempb"
)

type recordingNotificationSender struct {
	mu       sync.Mutex
	requests []*cloudpb.CreateNotificationV2Request
}

func (s *recordingNotificationSender) CreateNotificationV2(
	_ context.Context,
	request *cloudpb.CreateNotificationV2Request,
) (*cloudpb.CreateNotificationV2Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, request)
	return &cloudpb.CreateNotificationV2Response{
		NotificationId: request.GetNotificationId(),
	}, nil
}

type strictDuplicateNotificationSender struct {
	requests []*cloudpb.CreateNotificationV2Request
	seen     map[string]struct{}
}

func (s *strictDuplicateNotificationSender) CreateNotificationV2(
	_ context.Context,
	request *cloudpb.CreateNotificationV2Request,
) (*cloudpb.CreateNotificationV2Response, error) {
	s.requests = append(s.requests, proto.Clone(request).(*cloudpb.CreateNotificationV2Request))
	if s.seen == nil {
		s.seen = make(map[string]struct{})
	}
	if _, exists := s.seen[request.GetNotificationId()]; exists {
		return nil, status.Error(codes.AlreadyExists, "notification_id already exists")
	}
	s.seen[request.GetNotificationId()] = struct{}{}
	return &cloudpb.CreateNotificationV2Response{
		NotificationId: request.GetNotificationId(),
	}, nil
}

type deadlineNotificationSender struct {
	deadline time.Time
	has      bool
}

type mismatchedNotificationSender struct{}

func (mismatchedNotificationSender) CreateNotificationV2(
	context.Context,
	*cloudpb.CreateNotificationV2Request,
) (*cloudpb.CreateNotificationV2Response, error) {
	return &cloudpb.CreateNotificationV2Response{
		NotificationId: "c8a78877-4048-4829-8986-43528248a86e",
	}, nil
}

func (s *deadlineNotificationSender) CreateNotificationV2(
	ctx context.Context,
	request *cloudpb.CreateNotificationV2Request,
) (*cloudpb.CreateNotificationV2Response, error) {
	s.deadline, s.has = ctx.Deadline()
	return &cloudpb.CreateNotificationV2Response{NotificationId: request.GetNotificationId()}, nil
}

func validSystemNotificationRequest() *systempb.SendRequest {
	return &systempb.SendRequest{
		Audience: &systempb.NotificationAudience{
			Roles: []systempb.OrganizationRole{systempb.OrganizationRole_ORGANIZATION_ROLE_MEMBER},
		},
		Title:          "Fire detected",
		Body:           "Camera 2 detected smoke",
		Severity:       systempb.NotificationSeverity_NOTIFICATION_SEVERITY_CRITICAL,
		DeepLink:       "wendy://live?camera=camera-2",
		NotificationId: "123e4567-e89b-42d3-a456-426614174000",
	}
}

func TestNotificationProtoContract(t *testing.T) {
	legacyNotificationFields := []protoreflect.Name{
		"id", "user_id", "organization_id", "body", "severity", "related_entities", "created_at", "deleted_at",
	}
	cloudNotification := (&cloudpb.Notification{}).ProtoReflect().Descriptor()
	for index, wantName := range legacyNotificationFields {
		fieldNumber := protoreflect.FieldNumber(index + 1)
		field := cloudNotification.Fields().ByNumber(fieldNumber)
		if field == nil || field.Name() != wantName {
			t.Fatalf("Notification field %d = %v, want %q", fieldNumber, field, wantName)
		}
	}
	for fieldNumber, wantName := range map[protoreflect.FieldNumber]protoreflect.Name{
		11: "notification_id",
		14: "created_by_user_id",
		15: "created_by_asset_id",
		16: "created_by_app_id",
	} {
		if got := cloudNotification.Fields().ByNumber(fieldNumber); got == nil || got.Name() != wantName {
			t.Fatalf("Notification field %d = %v, want %q", fieldNumber, got, wantName)
		}
	}

	cloudRequest := (&cloudpb.CreateNotificationV2Request{}).ProtoReflect().Descriptor()
	legacyRequest := (&cloudpb.CreateNotificationRequest{}).ProtoReflect().Descriptor()
	legacyRequestOptions, ok := legacyRequest.Options().(*descriptorpb.MessageOptions)
	if !ok || !legacyRequestOptions.GetDeprecated() {
		t.Fatalf("CreateNotificationRequest deprecated option = %v, want true", legacyRequest.Options())
	}
	cloudService := cloudRequest.ParentFile().Services().ByName("NotificationService")
	legacyMethod := cloudService.Methods().ByName("CreateNotification")
	legacyMethodOptions, ok := legacyMethod.Options().(*descriptorpb.MethodOptions)
	if !ok || !legacyMethodOptions.GetDeprecated() {
		t.Fatalf("CreateNotification deprecated option = %v, want true", legacyMethod.Options())
	}
	cloudRequestFields := []protoreflect.Name{
		"organization_id", "audience", "title", "body", "severity", "deep_link", "notification_id", "metadata",
	}
	for index, wantName := range cloudRequestFields {
		fieldNumber := protoreflect.FieldNumber(index + 1)
		field := cloudRequest.Fields().ByNumber(fieldNumber)
		if field == nil || field.Name() != wantName {
			t.Fatalf("CreateNotificationV2Request field %d = %v, want %q", fieldNumber, field, wantName)
		}
	}
	if got := cloudRequest.Fields().ByNumber(9); got == nil || got.Name() != "app_id" || got.Kind() != protoreflect.StringKind {
		t.Fatalf("CreateNotificationV2Request field 9 = %v, want string app_id", got)
	}

	systemRequest := (&systempb.SendRequest{}).ProtoReflect().Descriptor()
	if got := systemRequest.Fields().ByNumber(6); got == nil || got.Name() != "notification_id" || got.Kind() != protoreflect.StringKind {
		t.Fatalf("SendRequest field 6 = %v, want string notification_id", got)
	}
	if got := systemRequest.Fields().ByName("app_id"); got != nil {
		t.Fatalf("app-facing SendRequest exposes trusted app identity: %v", got)
	}
	for name, descriptor := range map[string]protoreflect.MessageDescriptor{
		"CreateNotificationV2Response": (&cloudpb.CreateNotificationV2Response{}).ProtoReflect().Descriptor(),
		"SendResponse":                 (&systempb.SendResponse{}).ProtoReflect().Descriptor(),
	} {
		field1 := descriptor.Fields().ByNumber(1)
		if descriptor.Fields().Len() != 1 || field1 == nil || field1.Name() != "notification_id" {
			t.Fatalf("%s fields = %v, want {notification_id}", name, descriptor.Fields())
		}
		if field2 := descriptor.Fields().ByNumber(2); field2 != nil {
			t.Fatalf("%s field 2 = %v, want reserved recipient count", name, field2)
		}
	}
}

func TestSystemNotificationServiceBindsTrustedAppIdentityAndMapsTransport(t *testing.T) {
	sender := &recordingNotificationSender{}
	service := NewSystemNotificationService("com.example.firewatch", sender)
	input := validSystemNotificationRequest()
	input.Audience = &systempb.NotificationAudience{
		UserIds: []string{" user-1 ", "user-1", "user-2"},
		TeamIds: []int32{7, 7, 8},
		Roles: []systempb.OrganizationRole{
			systempb.OrganizationRole_ORGANIZATION_ROLE_MEMBER,
			systempb.OrganizationRole_ORGANIZATION_ROLE_MEMBER,
			systempb.OrganizationRole_ORGANIZATION_ROLE_ADMIN,
		},
	}

	response, err := service.Send(context.Background(), input)
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	if response.GetNotificationId() != input.GetNotificationId() {
		t.Fatalf("response = %+v, want notification ID %q", response, input.GetNotificationId())
	}
	sender.mu.Lock()
	defer sender.mu.Unlock()
	if len(sender.requests) != 1 {
		t.Fatalf("Cloud requests = %d, want 1", len(sender.requests))
	}
	request := sender.requests[0]
	if request.GetAppId() != "com.example.firewatch" {
		t.Fatalf("app_id = %q, want trusted app identity", request.GetAppId())
	}
	if request.GetNotificationId() != input.GetNotificationId() {
		t.Fatalf("notification_id = %q, want %q", request.GetNotificationId(), input.GetNotificationId())
	}
	if request.GetOrganizationId() != 0 || request.OrganizationId != nil {
		t.Fatal("System request must not supply organization identity")
	}
	if !slices.Equal(request.GetAudience().GetUserIds(), []string{"user-1", "user-2"}) {
		t.Fatalf("audience user_ids = %v", request.GetAudience().GetUserIds())
	}
	if !slices.Equal(request.GetAudience().GetTeamIds(), []int32{7, 8}) {
		t.Fatalf("audience team_ids = %v", request.GetAudience().GetTeamIds())
	}
	wantRoles := []cloudpb.OrganizationRole{
		cloudpb.OrganizationRole_ORGANIZATION_ROLE_MEMBER,
		cloudpb.OrganizationRole_ORGANIZATION_ROLE_ADMIN,
	}
	if !slices.Equal(request.GetAudience().GetRoles(), wantRoles) {
		t.Fatalf("audience roles = %v", request.GetAudience().GetRoles())
	}
	if request.GetSeverity() != cloudpb.NotificationSeverity_NOTIFICATION_SEVERITY_CRITICAL {
		t.Fatalf("severity = %v", request.GetSeverity())
	}
}

func TestSystemNotificationServiceCanonicalizesUppercaseNotificationID(t *testing.T) {
	sender := &recordingNotificationSender{}
	request := validSystemNotificationRequest()
	request.NotificationId = strings.ToUpper(request.NotificationId)

	response, err := NewSystemNotificationService("com.example.firewatch", sender).Send(context.Background(), request)
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	const canonical = "123e4567-e89b-42d3-a456-426614174000"
	if got := sender.requests[0].GetNotificationId(); got != canonical {
		t.Fatalf("forwarded notification_id = %q, want %q", got, canonical)
	}
	if got := response.GetNotificationId(); got != canonical {
		t.Fatalf("response notification_id = %q, want %q", got, canonical)
	}
}

func TestSystemNotificationServiceRejectsMismatchedCloudNotificationID(t *testing.T) {
	_, err := NewSystemNotificationService("com.example.firewatch", mismatchedNotificationSender{}).Send(
		context.Background(),
		validSystemNotificationRequest(),
	)
	if status.Code(err) != codes.DataLoss {
		t.Fatalf("Send() code = %v, want DataLoss (err=%v)", status.Code(err), err)
	}
}

func TestSystemNotificationServicePassesThroughStrictDuplicateConflicts(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*systempb.SendRequest)
	}{
		{name: "same exact request", mutate: func(*systempb.SendRequest) {}},
		{name: "changed request", mutate: func(request *systempb.SendRequest) { request.Body = "Changed body" }},
		{name: "uppercase UUID", mutate: func(request *systempb.SendRequest) {
			request.NotificationId = strings.ToUpper(request.NotificationId)
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			sender := &strictDuplicateNotificationSender{}
			service := NewSystemNotificationService("com.example.firewatch", sender)
			first := validSystemNotificationRequest()
			if _, err := service.Send(context.Background(), first); err != nil {
				t.Fatalf("first Send() error = %v", err)
			}

			second := proto.Clone(first).(*systempb.SendRequest)
			test.mutate(second)
			_, err := service.Send(context.Background(), second)
			if status.Code(err) != codes.AlreadyExists {
				t.Fatalf("duplicate Send() code = %v, want AlreadyExists (err=%v)", status.Code(err), err)
			}
			if len(sender.requests) != 2 {
				t.Fatalf("Cloud creation attempts = %d, want exactly one per Send call", len(sender.requests))
			}
			for index, request := range sender.requests {
				if request.GetNotificationId() != first.GetNotificationId() {
					t.Fatalf("Cloud request %d notification_id = %q, want canonical %q", index, request.GetNotificationId(), first.GetNotificationId())
				}
			}
		})
	}
}

func TestSystemNotificationServiceRateLimitedUUIDCanBeRetried(t *testing.T) {
	sender := &recordingNotificationSender{}
	service := NewSystemNotificationService("com.example.firewatch", sender)
	service.limiter = newNotificationRateLimiter(1, time.Hour)
	service.limiter.tokens = 0
	service.limiter.lastRefill = time.Now()
	request := validSystemNotificationRequest()

	if _, err := service.Send(context.Background(), request); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("rate-limited Send() code = %v, want ResourceExhausted (err=%v)", status.Code(err), err)
	}
	if len(sender.requests) != 0 {
		t.Fatalf("Cloud creation attempts after local rejection = %d, want 0", len(sender.requests))
	}

	service.limiter.tokens = 1
	if _, err := service.Send(context.Background(), request); err != nil {
		t.Fatalf("retry with same notification_id error = %v", err)
	}
	if len(sender.requests) != 1 {
		t.Fatalf("Cloud creation attempts after accepted retry = %d, want 1", len(sender.requests))
	}
}

func proofCloudNotificationRequest() *cloudpb.CreateNotificationV2Request {
	appID := "com.example.firewatch"
	return &cloudpb.CreateNotificationV2Request{
		Audience:       &cloudpb.NotificationAudience{UserIds: []string{"user-1"}},
		Title:          "Fire",
		Body:           "Smoke detected",
		Severity:       cloudpb.NotificationSeverity_NOTIFICATION_SEVERITY_CRITICAL,
		DeepLink:       "wendy://live?camera=camera-2",
		NotificationId: "123e4567-e89b-42d3-a456-426614174000",
		AppId:          &appID,
	}
}

const deviceProofTestCertificateSerial = "01a2b3c4d5e6f708"

// The two identity spellings the canonical preimage has to cover. The int URN is
// what a device signs today; the UUID URN is the shape WDY-2808 moves every
// boundary to. Both are lowercase because Cloud's parser re-renders a parsed
// uuid and compares, so an upper-case variant is rejected, not accepted as a
// second spelling.
// deviceProofV2FullMethod is Cloud's 2.0 verifier method name. It is defined
// here rather than in the package because this agent does not invoke it: the
// generated client calls wendycloud.v1, so v1 is what a device signs. It exists
// so the fixture can pin the bytes Cloud will verify once wendyos migrates.
const deviceProofV2FullMethod = "wendycloud.v2.NotificationService/CreateNotificationV2"

const (
	deviceProofTestAssetURI     = "urn:wendy:org:17:asset:42"
	deviceProofTestAssetUUIDURI = "urn:wendy:org:13a72725-dfe3-4425-bd04-b253d2036089:asset:9f2c1d84-6b30-4e57-9a1e-2c7d5f08b413"
)

func deviceProofTestKeyPEM(t *testing.T) (*ecdsa.PrivateKey, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return key, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
}

func deviceProofTestCertificatePEM(t *testing.T, key *ecdsa.PrivateKey, serialHex string) string {
	t.Helper()
	serial, ok := new(big.Int).SetString(serialHex, 16)
	if !ok {
		t.Fatalf("invalid test certificate serial %q", serialHex)
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		NotBefore:    time.Unix(1_700_000_000, 0),
		NotAfter:     time.Unix(1_900_000_000, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create test certificate: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func TestProvisionedLeafCertificateSerialIsCanonicalLowercaseHex(t *testing.T) {
	key, _ := deviceProofTestKeyPEM(t)
	certificatePEM := deviceProofTestCertificatePEM(t, key, "0ABC")
	serial, err := provisionedLeafCertificateSerial(certificatePEM)
	if err != nil {
		t.Fatalf("provisionedLeafCertificateSerial() error = %v", err)
	}
	if serial != "0abc" {
		t.Fatalf("certificate serial = %q, want 0abc", serial)
	}
}

func TestNotificationDeviceProofRejectsMalformedProvisionedLeafCertificate(t *testing.T) {
	_, keyPEM := deviceProofTestKeyPEM(t)
	tests := []struct {
		name    string
		certPEM string
	}{
		{name: "empty", certPEM: ""},
		{name: "not PEM", certPEM: "not a certificate"},
		{name: "wrong PEM type", certPEM: string(keyPEM)},
		{name: "invalid certificate DER", certPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("invalid")}))},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx, err := notificationDeviceProofContext(
				context.Background(),
				proofCloudNotificationRequest(),
				deviceProofFullMethod,
				deviceProofTestAssetURI,
				1_753_267_200,
				test.certPEM,
				keyPEM,
				rand.Reader,
			)
			if err == nil || ctx != nil {
				t.Fatalf("notificationDeviceProofContext() = (%v, %v), want nil context and error", ctx, err)
			}
		})
	}
}

func TestCanonicalNotificationDeviceProofRejectsNoncanonicalCertificateSerial(t *testing.T) {
	for _, serial := range []string{"", "0", "00", "0001", "abc", "0ABC", "12-34", strings.Repeat("aa", 21)} {
		t.Run(serial, func(t *testing.T) {
			if _, _, _, err := canonicalNotificationDeviceProof(proofCloudNotificationRequest(), deviceProofFullMethod, deviceProofTestAssetURI, serial, 1_753_267_200); err == nil {
				t.Fatalf("certificate serial %q accepted", serial)
			}
		})
	}
}

func TestCanonicalNotificationDeviceProofIsDeterministic(t *testing.T) {
	const timestamp = int64(1_753_267_200)
	request := proofCloudNotificationRequest()
	first, uri, timestampText, err := canonicalNotificationDeviceProof(request, deviceProofFullMethod, deviceProofTestAssetURI, deviceProofTestCertificateSerial, timestamp)
	if err != nil {
		t.Fatalf("canonicalNotificationDeviceProof() error = %v", err)
	}
	second, _, _, err := canonicalNotificationDeviceProof(proto.Clone(request).(*cloudpb.CreateNotificationV2Request), deviceProofFullMethod, deviceProofTestAssetURI, deviceProofTestCertificateSerial, timestamp)
	if err != nil {
		t.Fatalf("second canonicalNotificationDeviceProof() error = %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("canonical proof bytes changed for the same semantic protobuf request")
	}
	requestBytes, err := (proto.MarshalOptions{Deterministic: true}).Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	prefix := deviceProofDomain + "\x00" + deviceProofFullMethod + "\x00" + uri + "\x00" + deviceProofTestCertificateSerial + "\x00" + timestampText + "\x00"
	if !bytes.Equal(first, append([]byte(prefix), requestBytes...)) {
		t.Fatal("canonical proof does not match the versioned NUL-framed contract")
	}
}

// deviceProofFixtures is the cross-repo pin. Cloud's DeviceNotificationProofTests
// pins these same digests, copied from here, over the same request, serial and
// timestamp. The preimage is signature input, so a framing or spelling change
// made on one side only breaks every device proof in the fleet; pinning it in
// both repos turns that into two red suites instead.
var deviceProofFixtures = []struct {
	name   string
	method string
	uri    string
	digest string
}{
	// What a device signs today, and the check that no refactor moved the bytes.
	{
		name:   "v1 method and integer identity",
		method: deviceProofFullMethod,
		uri:    deviceProofTestAssetURI,
		digest: "438186e9443c54083a91beb6815bad1a7376c1c1f3e3ca9ec9c1cbe4fd85ec94",
	},
	// What Cloud's 2.0 verifier expects. No device produces these bytes yet:
	// this agent calls wendycloud.v1 and has no uuid identity to sign.
	{
		name:   "v2 method and uuid identity",
		method: deviceProofV2FullMethod,
		uri:    deviceProofTestAssetUUIDURI,
		digest: "4dc7c616beb4cf9eea9467ada6fd464cfce45ef4f87473724a44ef8859ef779c",
	},
}

func TestCanonicalNotificationDeviceProofMatchesCrossRepoFixture(t *testing.T) {
	for _, fixture := range deviceProofFixtures {
		t.Run(fixture.name, func(t *testing.T) {
			canonical := mustCanonicalNotificationProof(
				t, proofCloudNotificationRequest(), fixture.method, fixture.uri,
				deviceProofTestCertificateSerial, 1_753_267_200)
			if got := hex.EncodeToString(sliceDigest(canonical)); got != fixture.digest {
				t.Fatalf("canonical proof SHA-256 for %s = %s, want fixture %s", fixture.name, got, fixture.digest)
			}
		})
	}
}

func sliceDigest(data []byte) []byte {
	digest := sha256.Sum256(data)
	return digest[:]
}

func TestCanonicalNotificationDeviceProofRejectsNoncanonicalIdentityURN(t *testing.T) {
	for _, uri := range []string{
		"",
		"urn:wendy:org:17:asset:",
		"spiffe://wendy.sh/tenant/x/service/asset-42",
		"urn:wendy:org:13A72725-DFE3-4425-BD04-B253D2036089:asset:42",
		"urn:wendy:org:17:asset:42\x00urn:wendy:org:1:asset:1",
		"urn:wendy:org:17:asset: 42",
		"urn:wendy:org:" + strings.Repeat("a", 256),
	} {
		t.Run(uri, func(t *testing.T) {
			if _, _, _, err := canonicalNotificationDeviceProof(
				proofCloudNotificationRequest(), deviceProofFullMethod, uri, deviceProofTestCertificateSerial, 1_753_267_200); err == nil {
				t.Fatalf("non-canonical identity URN %q accepted", uri)
			}
		})
	}
}

func TestNotificationDeviceProofSignatureAndTamperResistance(t *testing.T) {
	const timestamp = int64(1_753_267_200)
	key, keyPEM := deviceProofTestKeyPEM(t)
	certificatePEM := deviceProofTestCertificatePEM(t, key, deviceProofTestCertificateSerial)
	request := proofCloudNotificationRequest()
	baseCtx := metadata.NewOutgoingContext(context.Background(), metadata.Pairs(
		deviceProofURIHeader, "forged",
		deviceProofCertificateSerialHeader, "forged-one",
		deviceProofCertificateSerialHeader, "forged-two",
		deviceProofTimestampHeader, "0",
		deviceProofSignatureHeader, "forged",
		"x-unrelated", "preserved",
	))
	ctx, err := notificationDeviceProofContext(baseCtx, request, deviceProofFullMethod, deviceProofTestAssetURI, timestamp, certificatePEM, keyPEM, rand.Reader)
	if err != nil {
		t.Fatalf("notificationDeviceProofContext() error = %v", err)
	}
	md, ok := metadata.FromOutgoingContext(ctx)
	if !ok {
		t.Fatal("proof metadata missing")
	}
	if got := md.Get(deviceProofURIHeader); len(got) != 1 || got[0] != "urn:wendy:org:17:asset:42" {
		t.Fatalf("device URI metadata = %v", got)
	}
	if got := md.Get(deviceProofCertificateSerialHeader); len(got) != 1 || got[0] != deviceProofTestCertificateSerial {
		t.Fatalf("certificate serial metadata = %v", got)
	}
	if got := md.Get("x-unrelated"); len(got) != 1 || got[0] != "preserved" {
		t.Fatalf("unrelated outgoing metadata = %v", got)
	}
	if got := md.Get(deviceProofTimestampHeader); len(got) != 1 || got[0] != "1753267200" {
		t.Fatalf("timestamp metadata = %v", got)
	}
	signatureValues := md.Get(deviceProofSignatureHeader)
	if len(signatureValues) != 1 || strings.Contains(signatureValues[0], "=") {
		t.Fatalf("signature metadata is not one unpadded base64url value: %v", signatureValues)
	}
	signature, err := base64.RawURLEncoding.DecodeString(signatureValues[0])
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	canonical, _, _, err := canonicalNotificationDeviceProof(request, deviceProofFullMethod, deviceProofTestAssetURI, deviceProofTestCertificateSerial, timestamp)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(canonical)
	if !ecdsa.VerifyASN1(&key.PublicKey, digest[:], signature) {
		t.Fatal("valid device proof signature did not verify")
	}

	tamperedRequest := proto.Clone(request).(*cloudpb.CreateNotificationV2Request)
	tamperedRequest.Body = "different body"
	for name, tamperedCanonical := range map[string][]byte{
		"request":            mustCanonicalNotificationProof(t, tamperedRequest, deviceProofFullMethod, deviceProofTestAssetURI, deviceProofTestCertificateSerial, timestamp),
		"identity":           mustCanonicalNotificationProof(t, request, deviceProofFullMethod, "urn:wendy:org:17:asset:43", deviceProofTestCertificateSerial, timestamp),
		"certificate serial": mustCanonicalNotificationProof(t, request, deviceProofFullMethod, deviceProofTestAssetURI, "b1b2c3d4e5f60718", timestamp),
		"timestamp":          mustCanonicalNotificationProof(t, request, deviceProofFullMethod, deviceProofTestAssetURI, deviceProofTestCertificateSerial, timestamp+1),
	} {
		t.Run(name, func(t *testing.T) {
			tamperedDigest := sha256.Sum256(tamperedCanonical)
			if ecdsa.VerifyASN1(&key.PublicKey, tamperedDigest[:], signature) {
				t.Fatal("signature verified after proof tampering")
			}
		})
	}
}

func mustCanonicalNotificationProof(
	t *testing.T,
	request *cloudpb.CreateNotificationV2Request,
	fullMethod string,
	uri string,
	certificateSerial string,
	timestamp int64,
) []byte {
	t.Helper()
	canonical, _, _, err := canonicalNotificationDeviceProof(request, fullMethod, uri, certificateSerial, timestamp)
	if err != nil {
		t.Fatal(err)
	}
	return canonical
}

func TestSystemNotificationServiceBoundsCloudForwardingDeadline(t *testing.T) {
	t.Run("adds default deadline", func(t *testing.T) {
		sender := &deadlineNotificationSender{}
		started := time.Now()
		if _, err := NewSystemNotificationService("com.example.app", sender).Send(context.Background(), validSystemNotificationRequest()); err != nil {
			t.Fatalf("Send() error = %v", err)
		}
		if !sender.has {
			t.Fatal("Cloud forwarding context has no deadline")
		}
		minimum := started.Add(notificationForwardTimeout - time.Second)
		maximum := time.Now().Add(notificationForwardTimeout)
		if sender.deadline.Before(minimum) || sender.deadline.After(maximum) {
			t.Fatalf("forwarding deadline = %v, want approximately %v", sender.deadline, started.Add(notificationForwardTimeout))
		}
	})

	t.Run("preserves shorter caller deadline", func(t *testing.T) {
		sender := &deadlineNotificationSender{}
		callerDeadline := time.Now().Add(time.Second)
		ctx, cancel := context.WithDeadline(context.Background(), callerDeadline)
		defer cancel()
		if _, err := NewSystemNotificationService("com.example.app", sender).Send(ctx, validSystemNotificationRequest()); err != nil {
			t.Fatalf("Send() error = %v", err)
		}
		if !sender.has || !sender.deadline.Equal(callerDeadline) {
			t.Fatalf("forwarding deadline = %v, want caller deadline %v", sender.deadline, callerDeadline)
		}
	})
}

func TestSystemNotificationServiceValidation(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*systempb.SendRequest)
	}{
		{name: "missing audience", mutate: func(r *systempb.SendRequest) { r.Audience = nil }},
		{name: "empty audience", mutate: func(r *systempb.SendRequest) {
			r.Audience = &systempb.NotificationAudience{}
		}},
		{name: "invalid user", mutate: func(r *systempb.SendRequest) {
			r.Audience = &systempb.NotificationAudience{UserIds: []string{"unsafe user"}}
		}},
		{name: "invalid team", mutate: func(r *systempb.SendRequest) {
			r.Audience = &systempb.NotificationAudience{TeamIds: []int32{0}}
		}},
		{name: "invalid role", mutate: func(r *systempb.SendRequest) {
			r.Audience = &systempb.NotificationAudience{Roles: []systempb.OrganizationRole{systempb.OrganizationRole_ORGANIZATION_ROLE_UNSPECIFIED}}
		}},
		{name: "blank title", mutate: func(r *systempb.SendRequest) { r.Title = "" }},
		{name: "control body", mutate: func(r *systempb.SendRequest) { r.Body = "bad\nbody" }},
		{name: "unspecified severity", mutate: func(r *systempb.SendRequest) {
			r.Severity = systempb.NotificationSeverity_NOTIFICATION_SEVERITY_UNSPECIFIED
		}},
		{name: "relative deep link", mutate: func(r *systempb.SendRequest) { r.DeepLink = "/live" }},
		{name: "wrong deep link scheme", mutate: func(r *systempb.SendRequest) { r.DeepLink = "https://example.com/live" }},
		{name: "deep link userinfo", mutate: func(r *systempb.SendRequest) { r.DeepLink = "wendy://user@live" }},
		{name: "deep link without host", mutate: func(r *systempb.SendRequest) { r.DeepLink = "wendy:///live" }},
		{name: "malformed notification ID", mutate: func(r *systempb.SendRequest) { r.NotificationId = "not-a-uuid" }},
		{name: "non-v4 notification ID", mutate: func(r *systempb.SendRequest) { r.NotificationId = "6ba7b810-9dad-11d1-80b4-00c04fd430c8" }},
		{name: "zero notification ID", mutate: func(r *systempb.SendRequest) { r.NotificationId = "00000000-0000-0000-0000-000000000000" }},
		{name: "oversized metadata", mutate: func(r *systempb.SendRequest) {
			r.Metadata, _ = structpb.NewStruct(map[string]any{"value": strings.Repeat("x", maxNotificationMetadataBytes+1)})
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := validSystemNotificationRequest()
			test.mutate(request)
			_, err := NewSystemNotificationService("com.example.app", &recordingNotificationSender{}).Send(context.Background(), request)
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("Send() code = %v, want InvalidArgument (err=%v)", status.Code(err), err)
			}
		})
	}
}

func TestSystemNotificationServiceBoundsRawAudienceSelectors(t *testing.T) {
	request := validSystemNotificationRequest()
	request.Audience = &systempb.NotificationAudience{
		UserIds: make([]string, maxNotificationAudienceSelectors-5),
		TeamIds: []int32{7, 8},
		Roles: []systempb.OrganizationRole{
			systempb.OrganizationRole_ORGANIZATION_ROLE_OWNER,
			systempb.OrganizationRole_ORGANIZATION_ROLE_ADMIN,
			systempb.OrganizationRole_ORGANIZATION_ROLE_VIEWER,
		},
	}
	for index := range request.Audience.UserIds {
		request.Audience.UserIds[index] = fmt.Sprintf("user-%d", index)
	}
	if _, err := NewSystemNotificationService("com.example.app", &recordingNotificationSender{}).Send(context.Background(), request); err != nil {
		t.Fatalf("100 selectors across categories rejected: %v", err)
	}

	request.Audience.TeamIds = append(request.Audience.TeamIds, 9)
	if _, err := NewSystemNotificationService("com.example.app", &recordingNotificationSender{}).Send(context.Background(), request); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("101-selector code = %v, want InvalidArgument (err=%v)", status.Code(err), err)
	}

	request.Audience.UserIds = make([]string, maxNotificationAudienceSelectors+1)
	for index := range request.Audience.UserIds {
		request.Audience.UserIds[index] = "same-user"
	}
	request.Audience.TeamIds = nil
	request.Audience.Roles = nil
	if _, err := NewSystemNotificationService("com.example.app", &recordingNotificationSender{}).Send(context.Background(), request); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("101 duplicate selectors code = %v, want InvalidArgument (err=%v)", status.Code(err), err)
	}
}

func TestSystemNotificationServiceRateLimit(t *testing.T) {
	service := NewSystemNotificationService("com.example.app", &recordingNotificationSender{})
	for i := 0; i < 10; i++ {
		if _, err := service.Send(context.Background(), validSystemNotificationRequest()); err != nil {
			t.Fatalf("Send() %d error = %v", i, err)
		}
	}
	if _, err := service.Send(context.Background(), validSystemNotificationRequest()); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("eleventh Send() code = %v, want ResourceExhausted", status.Code(err))
	}
}

func TestAppSystemAPISocketManagerSharesPerAppAndIsolatesIdentity(t *testing.T) {
	oldRoot := AppSystemAPISocketRootPath
	root, err := os.MkdirTemp("/tmp", "wendy-system-api-")
	if err != nil {
		t.Fatal(err)
	}
	AppSystemAPISocketRootPath = root
	t.Cleanup(func() {
		AppSystemAPISocketRootPath = oldRoot
		_ = os.RemoveAll(root)
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sender := &recordingNotificationSender{}
	manager := NewAppSystemAPISocketManager(ctx, zap.NewNop(), sender)

	appADir, err := manager.Ensure("com.example.a", "api", []string{SystemAPICapabilityNotifications})
	if err != nil {
		t.Fatalf("Ensure(app A/api) error = %v", err)
	}
	appAWorkerDir, err := manager.Ensure("com.example.a", "worker", []string{SystemAPICapabilityNotifications})
	if err != nil {
		t.Fatalf("Ensure(app A/worker) error = %v", err)
	}
	if appADir != appAWorkerDir {
		t.Fatalf("multi-service app directories differ: %q != %q", appADir, appAWorkerDir)
	}
	appBDir, err := manager.Ensure("com.example.b", "", []string{SystemAPICapabilityNotifications})
	if err != nil {
		t.Fatalf("Ensure(app B) error = %v", err)
	}
	if appADir == appBDir {
		t.Fatal("different apps shared a System API directory")
	}

	for _, test := range []struct {
		directory string
		wantAppID string
	}{
		{directory: appADir, wantAppID: "com.example.a"},
		{directory: appBDir, wantAppID: "com.example.b"},
	} {
		conn, err := grpc.NewClient(
			"unix://"+filepath.Join(test.directory, SystemAPISocketFilename),
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		)
		if err != nil {
			t.Fatalf("dial %s: %v", test.wantAppID, err)
		}
		client := systempb.NewNotificationServiceClient(conn)
		callCtx, callCancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, err = client.Send(callCtx, validSystemNotificationRequest())
		callCancel()
		_ = conn.Close()
		if err != nil {
			t.Fatalf("Send(%s) error = %v", test.wantAppID, err)
		}
	}

	sender.mu.Lock()
	if len(sender.requests) != 2 || sender.requests[0].GetAppId() != "com.example.a" || sender.requests[1].GetAppId() != "com.example.b" {
		t.Fatalf("trusted app identities = %+v", sender.requests)
	}
	sender.mu.Unlock()

	manager.Release("com.example.a", "api")
	if _, err := os.Stat(appADir); err != nil {
		t.Fatalf("directory removed while worker still owns it: %v", err)
	}
	manager.Release("com.example.a", "worker")
	if _, err := os.Stat(appADir); !os.IsNotExist(err) {
		t.Fatalf("directory remains after final release: %v", err)
	}
}

func TestAppSystemAPISocketManagerRecreatesSocketInStableDirectory(t *testing.T) {
	oldRoot := AppSystemAPISocketRootPath
	root, err := os.MkdirTemp("/tmp", "wendy-system-restart-")
	if err != nil {
		t.Fatal(err)
	}
	AppSystemAPISocketRootPath = root
	t.Cleanup(func() {
		AppSystemAPISocketRootPath = oldRoot
		_ = os.RemoveAll(root)
	})

	ctx1, cancel1 := context.WithCancel(context.Background())
	manager1 := NewAppSystemAPISocketManager(ctx1, zap.NewNop(), &recordingNotificationSender{})
	directory, err := manager1.Ensure("com.example.restart", "", []string{SystemAPICapabilityNotifications})
	if err != nil {
		t.Fatalf("first Ensure() error = %v", err)
	}
	before, err := os.Stat(directory)
	if err != nil {
		t.Fatal(err)
	}
	manager1.stopAll()
	cancel1()

	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	manager2 := NewAppSystemAPISocketManager(ctx2, zap.NewNop(), &recordingNotificationSender{})
	restored, err := manager2.Ensure("com.example.restart", "", []string{SystemAPICapabilityNotifications})
	if err != nil {
		t.Fatalf("restored Ensure() error = %v", err)
	}
	after, err := os.Stat(restored)
	if err != nil {
		t.Fatal(err)
	}
	if restored != directory || !os.SameFile(before, after) {
		t.Fatal("Agent restart replaced the mounted System API directory inode")
	}
}
