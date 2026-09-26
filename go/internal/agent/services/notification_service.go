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
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/cloudrelay"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	systempb "github.com/wendylabsinc/wendy/go/proto/gen/systempb"
)

const (
	maxNotificationMetadataBytes     = 8 * 1024
	maxNotificationAudienceSelectors = 100
	notificationForwardTimeout       = 15 * time.Second

	deviceProofURIHeader               = "x-wendy-device-uri"
	deviceProofCertificateSerialHeader = "x-wendy-device-certificate-serial"
	deviceProofTimestampHeader         = "x-wendy-device-timestamp"
	deviceProofSignatureHeader         = "x-wendy-device-signature"
	deviceProofDomain                  = "wendy-device-request-proof/v2"
	// The method this device actually invokes, and so the only one it signs.
	// Deliberately not cloudpb's generated FullMethodName constant: that one
	// carries a leading slash, and these bytes are pinned against Cloud.
	deviceProofFullMethod = "wendycloud.v1.NotificationService/CreateNotificationV2"
)

// NotificationSender forwards a trusted, app-attributed Notification to Wendy Cloud.
type NotificationSender interface {
	CreateNotificationV2(context.Context, *cloudpb.CreateNotificationV2Request, []string) (*cloudpb.CreateNotificationV2Response, error)
}

// SystemNotificationService serves one app-specific app-facing socket. The
// trusted app ID comes from the Agent's container metadata and socket mount; it
// is never accepted from workload-controlled input.
type SystemNotificationService struct {
	systempb.UnimplementedNotificationServiceServer
	appID   string
	sender  NotificationSender
	limiter notificationRateLimiter
}

func NewSystemNotificationService(appID string, sender NotificationSender) *SystemNotificationService {
	return &SystemNotificationService{
		appID:   appID,
		sender:  sender,
		limiter: newNotificationRateLimiter(10, time.Second),
	}
}

func (s *SystemNotificationService) Send(
	ctx context.Context,
	req *systempb.SendRequest,
) (*systempb.SendResponse, error) {
	if s.sender == nil {
		return nil, status.Error(codes.Unavailable, "notification delivery is unavailable")
	}
	cloudAudience, teamUUIDs, notificationID, err := validateNotificationSendRequest(req)
	if err != nil {
		return nil, err
	}
	if !s.limiter.allow(time.Now()) {
		return nil, status.Error(codes.ResourceExhausted, "notification rate exceeded; retry later with the same notification_id")
	}

	appID := s.appID
	cloudRequest := &cloudpb.CreateNotificationV2Request{
		Audience:       cloudAudience,
		Title:          req.GetTitle(),
		Body:           req.GetBody(),
		Severity:       cloudNotificationSeverity(req.GetSeverity()),
		DeepLink:       req.GetDeepLink(),
		NotificationId: notificationID,
		Metadata:       req.GetMetadata(),
		AppId:          &appID,
	}
	forwardCtx, cancel := notificationForwardContext(ctx)
	defer cancel()
	// One app-facing Send makes exactly one Cloud creation attempt. In particular,
	// ALREADY_EXISTS is terminal: retrying it here could duplicate downstream push work.
	response, err := s.sender.CreateNotificationV2(forwardCtx, cloudRequest, teamUUIDs)
	if err != nil {
		if cloudStatus, ok := status.FromError(err); ok {
			return nil, cloudStatus.Err()
		}
		return nil, status.Errorf(codes.Unavailable, "send notification: %v", err)
	}
	if response.GetNotificationId() != notificationID {
		return nil, status.Error(codes.DataLoss, "Cloud returned a mismatched notification_id")
	}
	return &systempb.SendResponse{NotificationId: response.GetNotificationId()}, nil
}

func notificationForwardContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= notificationForwardTimeout {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, notificationForwardTimeout)
}

type notificationRateLimiter struct {
	mu             sync.Mutex
	tokens         float64
	capacity       float64
	refillInterval time.Duration
	lastRefill     time.Time
}

func newNotificationRateLimiter(capacity int, refillInterval time.Duration) notificationRateLimiter {
	return notificationRateLimiter{
		tokens:         float64(capacity),
		capacity:       float64(capacity),
		refillInterval: refillInterval,
		lastRefill:     time.Now(),
	}
}

func (l *notificationRateLimiter) allow(now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.tokens += now.Sub(l.lastRefill).Seconds() / l.refillInterval.Seconds()
	if l.tokens > l.capacity {
		l.tokens = l.capacity
	}
	l.lastRefill = now
	if l.tokens < 1 {
		return false
	}
	l.tokens--
	return true
}

func validateNotificationSendRequest(request *systempb.SendRequest) (*cloudpb.NotificationAudience, []string, string, error) {
	if request == nil {
		return nil, nil, "", status.Error(codes.InvalidArgument, "request is required")
	}
	notificationID, valid := canonicalNotificationUUIDv4(request.GetNotificationId())
	if !valid {
		return nil, nil, "", status.Error(codes.InvalidArgument, "notification_id must be a UUID v4")
	}
	if !validNotificationText(request.GetTitle(), 120) {
		return nil, nil, "", status.Error(codes.InvalidArgument, "title must contain 1...120 printable UTF-8 bytes")
	}
	if !validNotificationText(request.GetBody(), 2000) {
		return nil, nil, "", status.Error(codes.InvalidArgument, "body must contain 1...2000 printable UTF-8 bytes")
	}
	switch request.GetSeverity() {
	case systempb.NotificationSeverity_NOTIFICATION_SEVERITY_INFO,
		systempb.NotificationSeverity_NOTIFICATION_SEVERITY_WARNING,
		systempb.NotificationSeverity_NOTIFICATION_SEVERITY_ERROR,
		systempb.NotificationSeverity_NOTIFICATION_SEVERITY_CRITICAL:
	default:
		return nil, nil, "", status.Error(codes.InvalidArgument, "severity is required")
	}
	cloudAudience, teamUUIDs, err := normalizeNotificationAudience(request.GetAudience())
	if err != nil {
		return nil, nil, "", err
	}
	if !validNotificationDeepLink(request.GetDeepLink()) {
		return nil, nil, "", status.Error(codes.InvalidArgument, "deep_link must be an absolute wendy:// URI with a host, no userinfo, and at most 2048 bytes")
	}
	if metadata := request.GetMetadata(); metadata != nil && proto.Size(metadata) > maxNotificationMetadataBytes {
		return nil, nil, "", status.Errorf(codes.InvalidArgument, "metadata must be at most %d encoded bytes", maxNotificationMetadataBytes)
	}
	return cloudAudience, teamUUIDs, notificationID, nil
}

func normalizeNotificationAudience(audience *systempb.NotificationAudience) (*cloudpb.NotificationAudience, []string, error) {
	if audience == nil {
		return nil, nil, status.Error(codes.InvalidArgument, "audience is required")
	}
	if len(audience.GetTeamIds()) != 0 && len(audience.GetTeamUuids()) != 0 {
		return nil, nil, status.Error(codes.InvalidArgument, "audience cannot contain both team_ids and team_uuids")
	}
	rawSelectorCount := len(audience.GetUserIds()) + len(audience.GetTeamIds()) + len(audience.GetTeamUuids()) + len(audience.GetRoles())
	if rawSelectorCount == 0 {
		return nil, nil, status.Error(codes.InvalidArgument, "audience must contain at least one selector")
	}
	if rawSelectorCount > maxNotificationAudienceSelectors {
		return nil, nil, status.Errorf(codes.InvalidArgument, "audience must contain at most %d selectors", maxNotificationAudienceSelectors)
	}

	mapped := &cloudpb.NotificationAudience{}
	seenUsers := make(map[string]struct{}, len(audience.GetUserIds()))
	for _, rawUserID := range audience.GetUserIds() {
		userID := strings.TrimSpace(rawUserID)
		if !validNotificationIdentifier(userID, 128) {
			return nil, nil, status.Error(codes.InvalidArgument, "audience user_ids must contain 1...128 safe ASCII bytes")
		}
		if _, exists := seenUsers[userID]; exists {
			continue
		}
		seenUsers[userID] = struct{}{}
		mapped.UserIds = append(mapped.UserIds, userID)
	}

	seenTeams := make(map[int32]struct{}, len(audience.GetTeamIds()))
	for _, teamID := range audience.GetTeamIds() {
		if teamID <= 0 {
			return nil, nil, status.Error(codes.InvalidArgument, "audience team_ids must be positive")
		}
		if _, exists := seenTeams[teamID]; exists {
			continue
		}
		seenTeams[teamID] = struct{}{}
		mapped.TeamIds = append(mapped.TeamIds, teamID)
	}

	teamUUIDs := make([]string, 0, len(audience.GetTeamUuids()))
	seenTeamUUIDs := make(map[string]struct{}, len(audience.GetTeamUuids()))
	for _, rawTeamUUID := range audience.GetTeamUuids() {
		teamUUID := strings.TrimSpace(rawTeamUUID)
		parsed, err := uuid.Parse(teamUUID)
		if err != nil || parsed.String() != teamUUID {
			return nil, nil, status.Error(codes.InvalidArgument, "audience team_uuids must contain canonical UUIDs")
		}
		if _, exists := seenTeamUUIDs[teamUUID]; exists {
			continue
		}
		seenTeamUUIDs[teamUUID] = struct{}{}
		teamUUIDs = append(teamUUIDs, teamUUID)
	}

	seenRoles := make(map[systempb.OrganizationRole]struct{}, len(audience.GetRoles()))
	for _, role := range audience.GetRoles() {
		switch role {
		case systempb.OrganizationRole_ORGANIZATION_ROLE_OWNER,
			systempb.OrganizationRole_ORGANIZATION_ROLE_ADMIN,
			systempb.OrganizationRole_ORGANIZATION_ROLE_BILLING_MANAGER,
			systempb.OrganizationRole_ORGANIZATION_ROLE_MEMBER,
			systempb.OrganizationRole_ORGANIZATION_ROLE_VIEWER:
		default:
			return nil, nil, status.Error(codes.InvalidArgument, "audience roles must be specified")
		}
		if _, exists := seenRoles[role]; exists {
			continue
		}
		seenRoles[role] = struct{}{}
		mapped.Roles = append(mapped.Roles, cloudOrganizationRole(role))
	}

	return mapped, teamUUIDs, nil
}

func cloudOrganizationRole(role systempb.OrganizationRole) cloudpb.OrganizationRole {
	switch role {
	case systempb.OrganizationRole_ORGANIZATION_ROLE_OWNER:
		return cloudpb.OrganizationRole_ORGANIZATION_ROLE_OWNER
	case systempb.OrganizationRole_ORGANIZATION_ROLE_ADMIN:
		return cloudpb.OrganizationRole_ORGANIZATION_ROLE_ADMIN
	case systempb.OrganizationRole_ORGANIZATION_ROLE_BILLING_MANAGER:
		return cloudpb.OrganizationRole_ORGANIZATION_ROLE_BILLING_MANAGER
	case systempb.OrganizationRole_ORGANIZATION_ROLE_MEMBER:
		return cloudpb.OrganizationRole_ORGANIZATION_ROLE_MEMBER
	case systempb.OrganizationRole_ORGANIZATION_ROLE_VIEWER:
		return cloudpb.OrganizationRole_ORGANIZATION_ROLE_VIEWER
	default:
		return cloudpb.OrganizationRole_ORGANIZATION_ROLE_UNSPECIFIED
	}
}

func cloudNotificationSeverity(severity systempb.NotificationSeverity) cloudpb.NotificationSeverity {
	switch severity {
	case systempb.NotificationSeverity_NOTIFICATION_SEVERITY_INFO:
		return cloudpb.NotificationSeverity_NOTIFICATION_SEVERITY_INFO
	case systempb.NotificationSeverity_NOTIFICATION_SEVERITY_WARNING:
		return cloudpb.NotificationSeverity_NOTIFICATION_SEVERITY_WARNING
	case systempb.NotificationSeverity_NOTIFICATION_SEVERITY_ERROR:
		return cloudpb.NotificationSeverity_NOTIFICATION_SEVERITY_ERROR
	case systempb.NotificationSeverity_NOTIFICATION_SEVERITY_CRITICAL:
		return cloudpb.NotificationSeverity_NOTIFICATION_SEVERITY_CRITICAL
	default:
		return cloudpb.NotificationSeverity_NOTIFICATION_SEVERITY_UNSPECIFIED
	}
}

func canonicalNotificationUUIDv4(value string) (string, bool) {
	if len(value) != 36 {
		return "", false
	}
	identifier, err := uuid.Parse(value)
	if err != nil || identifier.Version() != 4 || identifier.Variant() != uuid.RFC4122 {
		return "", false
	}
	canonical := identifier.String()
	if !strings.EqualFold(value, canonical) {
		return "", false
	}
	return canonical, true
}

func validNotificationIdentifier(value string, maximumBytes int) bool {
	if value == "" || len(value) > maximumBytes {
		return false
	}
	for _, char := range []byte(value) {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || char == '-' || char == '.' || char == '_' || char == ':' {
			continue
		}
		return false
	}
	return true
}

func validNotificationText(value string, maximumBytes int) bool {
	if value == "" || len(value) > maximumBytes || strings.TrimSpace(value) != value {
		return false
	}
	for _, char := range value {
		if unicode.IsControl(char) {
			return false
		}
	}
	return true
}

func validNotificationDeepLink(value string) bool {
	if value == "" || len(value) > 2048 || strings.TrimSpace(value) != value {
		return false
	}
	parsed, err := url.ParseRequestURI(value)
	return err == nil && parsed.IsAbs() && strings.EqualFold(parsed.Scheme, "wendy") && parsed.User == nil && parsed.Host != ""
}

type notificationDeviceProof struct {
	uri               string
	certificateSerial string
	timestamp         string
	signature         string
}

// canonicalNotificationDeviceProof builds the NUL-framed byte string the device
// signs. uri is the device's canonical Wendy identity URN and is signature
// input, not a comparison: Cloud rebuilds these exact bytes from the URI the
// device puts in the x-wendy-device-uri header, so the two spellings must agree
// byte for byte or every proof fails verification.
func canonicalNotificationDeviceProof(
	request *cloudpb.CreateNotificationV2Request,
	fullMethod string,
	uri string,
	certificateSerial string,
	timestamp int64,
) ([]byte, string, string, error) {
	if request == nil {
		return nil, "", "", fmt.Errorf("notification request is required")
	}
	if !validDeviceProofMethod(fullMethod) {
		return nil, "", "", fmt.Errorf("device proof method must be a printable gRPC method name")
	}
	if !validCanonicalIdentityURI(uri) {
		return nil, "", "", fmt.Errorf("device proof identity must be a canonical lowercase urn:wendy URN")
	}
	if !validCanonicalCertificateSerial(certificateSerial) {
		return nil, "", "", fmt.Errorf("device proof certificate serial must be canonical lowercase octet hexadecimal")
	}
	if timestamp < 0 {
		return nil, "", "", fmt.Errorf("device proof timestamp must be non-negative")
	}
	requestBytes, err := (proto.MarshalOptions{Deterministic: true}).Marshal(request)
	if err != nil {
		return nil, "", "", fmt.Errorf("deterministically serialize notification request: %w", err)
	}
	timestampText := strconv.FormatInt(timestamp, 10)
	canonical := make([]byte, 0, len(deviceProofDomain)+len(fullMethod)+len(uri)+len(certificateSerial)+len(timestampText)+len(requestBytes)+5)
	canonical = append(canonical, deviceProofDomain...)
	canonical = append(canonical, 0)
	canonical = append(canonical, fullMethod...)
	canonical = append(canonical, 0)
	canonical = append(canonical, uri...)
	canonical = append(canonical, 0)
	canonical = append(canonical, certificateSerial...)
	canonical = append(canonical, 0)
	canonical = append(canonical, timestampText...)
	canonical = append(canonical, 0)
	canonical = append(canonical, requestBytes...)
	return canonical, uri, timestampText, nil
}

// validDeviceProofMethod bounds the gRPC method name the same way the identity
// is bounded: it sits in the same NUL-framed preimage, so a NUL or a space in it
// would let one field bleed into the next.
func validDeviceProofMethod(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, character := range value {
		if character < '!' || character > '~' {
			return false
		}
	}
	return true
}

// validCanonicalIdentityURI accepts only the canonical lowercase spelling of a
// Wendy identity URN. Cloud's parser re-renders what it parses and compares, so
// an upper-case variant of the same identity is rejected there rather than
// accepted as a second spelling; refusing to sign one here means the device
// never produces a proof Cloud will throw away. The character bound also keeps
// a NUL or a space out of the URN, which would otherwise let one field of the
// NUL-framed preimage bleed into the next.
func validCanonicalIdentityURI(value string) bool {
	if value == "" || len(value) > 256 {
		return false
	}
	for _, character := range value {
		if character < '!' || character > '~' || (character >= 'A' && character <= 'Z') {
			return false
		}
	}
	// "urn:wendy:org:<org>:(asset|user):<id>". Six non-empty fields holds for
	// both the int-era spelling and the uuid one, since a uuid carries no colon.
	fields := strings.Split(value, ":")
	if len(fields) != 6 || fields[0] != "urn" || fields[1] != "wendy" || fields[2] != "org" {
		return false
	}
	if fields[4] != "asset" && fields[4] != "user" {
		return false
	}
	return fields[3] != "" && fields[5] != ""
}

func validCanonicalCertificateSerial(value string) bool {
	// Match Cloud's Swift X509 representation: two lowercase hexadecimal
	// characters per unsigned, big-endian serial byte, with no redundant 00 byte.
	if value == "" || len(value) > 40 || len(value)%2 != 0 || value == "00" || strings.HasPrefix(value, "00") {
		return false
	}
	for _, character := range value {
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f')) {
			return false
		}
	}
	return true
}

func provisionedLeafCertificateSerial(certPEM string) (string, error) {
	block, _ := pem.Decode([]byte(certPEM))
	if block == nil || block.Type != "CERTIFICATE" {
		return "", fmt.Errorf("decode provisioned device leaf certificate PEM")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("parse provisioned device leaf certificate: %w", err)
	}
	if certificate.SerialNumber == nil || certificate.SerialNumber.Sign() <= 0 {
		return "", fmt.Errorf("provisioned device leaf certificate serial must be positive")
	}
	serial := hex.EncodeToString(certificate.SerialNumber.Bytes())
	if !validCanonicalCertificateSerial(serial) {
		return "", fmt.Errorf("provisioned device leaf certificate serial is not canonical")
	}
	return serial, nil
}

func parseDeviceProofPrivateKey(keyData []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(keyData)
	if block == nil {
		return nil, fmt.Errorf("decode provisioned device private key PEM")
	}
	var key *ecdsa.PrivateKey
	switch block.Type {
	case "EC PRIVATE KEY":
		parsed, err := x509.ParseECPrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse provisioned EC private key: %w", err)
		}
		key = parsed
	case "PRIVATE KEY":
		parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse provisioned PKCS#8 private key: %w", err)
		}
		var ok bool
		key, ok = parsed.(*ecdsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("provisioned private key is not ECDSA")
		}
	default:
		return nil, fmt.Errorf("unsupported provisioned private key PEM type %q", block.Type)
	}
	if key.Curve != elliptic.P256() {
		return nil, fmt.Errorf("provisioned device proof key must use ECDSA P-256")
	}
	return key, nil
}

func signNotificationDeviceProof(
	request *cloudpb.CreateNotificationV2Request,
	fullMethod string,
	uri string,
	certificateSerial string,
	timestamp int64,
	keyData []byte,
	random io.Reader,
) (notificationDeviceProof, error) {
	canonical, uri, timestampText, err := canonicalNotificationDeviceProof(request, fullMethod, uri, certificateSerial, timestamp)
	if err != nil {
		return notificationDeviceProof{}, err
	}
	key, err := parseDeviceProofPrivateKey(keyData)
	if err != nil {
		return notificationDeviceProof{}, err
	}
	digest := sha256.Sum256(canonical)
	signature, err := ecdsa.SignASN1(random, key, digest[:])
	if err != nil {
		return notificationDeviceProof{}, fmt.Errorf("sign device request proof: %w", err)
	}
	return notificationDeviceProof{
		uri:               uri,
		certificateSerial: certificateSerial,
		timestamp:         timestampText,
		signature:         base64.RawURLEncoding.EncodeToString(signature),
	}, nil
}

func notificationDeviceProofContext(
	ctx context.Context,
	request *cloudpb.CreateNotificationV2Request,
	fullMethod string,
	uri string,
	timestamp int64,
	certPEM string,
	keyData []byte,
	random io.Reader,
) (context.Context, error) {
	certificateSerial, err := provisionedLeafCertificateSerial(certPEM)
	if err != nil {
		return nil, err
	}
	proof, err := signNotificationDeviceProof(request, fullMethod, uri, certificateSerial, timestamp, keyData, random)
	if err != nil {
		return nil, err
	}
	md, _ := metadata.FromOutgoingContext(ctx)
	md = md.Copy()
	md.Set(deviceProofURIHeader, proof.uri)
	md.Set(deviceProofCertificateSerialHeader, proof.certificateSerial)
	md.Set(deviceProofTimestampHeader, proof.timestamp)
	md.Set(deviceProofSignatureHeader, proof.signature)
	return metadata.NewOutgoingContext(ctx, md), nil
}

// CloudNotificationSender authenticates its request with a short-lived proof
// signed by the provisioned device key. This preserves device attribution when
// the production Cloud ingress cannot forward the TLS client certificate.
type CloudNotificationSender struct {
	logger          *zap.Logger
	provisioningSvc *ProvisioningService
	mu              sync.Mutex
	connectionKey   [32]byte
	connection      *grpc.ClientConn
}

func NewCloudNotificationSender(logger *zap.Logger, provisioningSvc *ProvisioningService) *CloudNotificationSender {
	return &CloudNotificationSender{logger: logger, provisioningSvc: provisioningSvc}
}

func (s *CloudNotificationSender) CreateNotificationV2(
	ctx context.Context,
	request *cloudpb.CreateNotificationV2Request,
	teamUUIDs []string,
) (*cloudpb.CreateNotificationV2Response, error) {
	cloudHost, orgID, assetID, enrolled := s.provisioningSvc.ProvisioningInfo()
	if !enrolled {
		return nil, status.Error(codes.FailedPrecondition, "device must be enrolled before sending notifications")
	}
	certPEM, chainPEM, keyData := s.provisioningSvc.ProvisioningCerts()
	defer func() {
		for i := range keyData {
			keyData[i] = 0
		}
	}()

	if s.provisioningSvc.ProvisioningPrincipal() != "" {
		return s.createNotificationV2WithACME(ctx, cloudHost, certPEM, chainPEM, keyData, request, teamUUIDs)
	}
	if len(teamUUIDs) != 0 {
		return nil, status.Error(codes.FailedPrecondition, "Cloud v2 team audiences require ACME enrollment")
	}
	if orgID <= 0 || assetID <= 0 {
		return nil, status.Error(codes.FailedPrecondition, "device proof requires positive organization and asset IDs")
	}
	proofCtx, err := notificationDeviceProofContext(
		ctx,
		request,
		deviceProofFullMethod,
		certs.AssetURN(orgID, assetID),
		time.Now().Unix(),
		certPEM,
		keyData,
		rand.Reader,
	)
	if err != nil {
		return nil, fmt.Errorf("create notification device request proof: %w", err)
	}
	connection, err := s.connectionFor(cloudHost, certPEM, chainPEM, keyData)
	if err != nil {
		return nil, err
	}
	response, err := cloudpb.NewNotificationServiceClient(connection).CreateNotificationV2(proofCtx, request)
	if err != nil {
		s.logger.Warn("app-originated notification delivery failed",
			zap.String("app_id", request.GetAppId()),
			zap.String("notification_id", request.GetNotificationId()),
			zap.Error(err))
		return nil, err
	}
	return response, nil
}

func (s *CloudNotificationSender) createNotificationV2WithACME(
	ctx context.Context,
	cloudHost, certPEM, chainPEM string,
	keyData []byte,
	request *cloudpb.CreateNotificationV2Request,
	teamUUIDs []string,
) (*cloudpb.CreateNotificationV2Response, error) {
	v2Request, err := cloudNotificationRequestV2(request, teamUUIDs)
	if err != nil {
		return nil, err
	}
	endpoint, err := cloudrelay.DeviceEndpoint(cloudHost, os.Getenv("WENDY_DEVICE_CLOUD_URL"))
	if err != nil {
		return nil, fmt.Errorf("resolve Wendy Cloud device endpoint: %w", err)
	}
	connection, err := s.connectionForACME(endpoint, certPEM, chainPEM, keyData)
	if err != nil {
		return nil, err
	}
	response, err := cloudpbv2.NewNotificationServiceClient(connection).CreateNotificationV2(ctx, v2Request)
	if err != nil {
		s.logger.Warn("app-originated Cloud v2 notification delivery failed",
			zap.String("app_id", request.GetAppId()),
			zap.String("notification_id", request.GetNotificationId()),
			zap.Error(err))
		return nil, err
	}
	return &cloudpb.CreateNotificationV2Response{NotificationId: response.GetNotificationId()}, nil
}

func cloudNotificationRequestV2(request *cloudpb.CreateNotificationV2Request, teamUUIDs []string) (*cloudpbv2.CreateNotificationV2Request, error) {
	if len(request.GetAudience().GetTeamIds()) != 0 {
		return nil, status.Error(codes.FailedPrecondition, "Cloud v2 team audiences require UUID team IDs")
	}
	return &cloudpbv2.CreateNotificationV2Request{
		Audience: &cloudpbv2.NotificationAudience{
			UserIds: request.GetAudience().GetUserIds(),
			TeamIds: teamUUIDs,
			Roles:   notificationRolesV2(request.GetAudience().GetRoles()),
		},
		Title:          request.GetTitle(),
		Body:           request.GetBody(),
		Severity:       cloudpbv2.NotificationSeverity(request.GetSeverity()),
		DeepLink:       request.GetDeepLink(),
		NotificationId: request.GetNotificationId(),
		Metadata:       request.GetMetadata(),
		AppId:          request.AppId,
	}, nil
}

func notificationRolesV2(roles []cloudpb.OrganizationRole) []cloudpbv2.OrganizationRole {
	mapped := make([]cloudpbv2.OrganizationRole, len(roles))
	for index, role := range roles {
		mapped[index] = cloudpbv2.OrganizationRole(role)
	}
	return mapped
}

func (s *CloudNotificationSender) connectionForACME(
	endpoint, certPEM, chainPEM string,
	keyData []byte,
) (*grpc.ClientConn, error) {
	key := sha256.Sum256([]byte("v2\x00" + endpoint + "\x00" + certPEM))
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.connection != nil && s.connectionKey == key {
		return s.connection, nil
	}
	connection, err := cloudrelay.DialCloud(endpoint, certPEM, chainPEM, keyData)
	if err != nil {
		return nil, fmt.Errorf("connect to Wendy Cloud device endpoint: %w", err)
	}
	if s.connection != nil {
		_ = s.connection.Close()
	}
	s.connectionKey = key
	s.connection = connection
	return connection, nil
}

func (s *CloudNotificationSender) connectionFor(
	cloudHost, certPEM, chainPEM string,
	keyData []byte,
) (*grpc.ClientConn, error) {
	key := sha256.Sum256([]byte("v1\x00" + cloudHost + "\x00" + certPEM))
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.connection != nil && s.connectionKey == key {
		return s.connection, nil
	}

	certificate, err := certs.TLSKeyPair(certPEM, chainPEM, string(keyData))
	if err != nil {
		return nil, fmt.Errorf("parse device notification client certificate: %w", err)
	}
	roots, err := x509.SystemCertPool()
	if err != nil {
		return nil, fmt.Errorf("load system roots for Wendy Cloud: %w", err)
	}
	// SECURITY: The enrollment chain is controlled by Wendy Cloud and is also
	// used by the existing telemetry flusher. Adding it supports direct local
	// broker deployments whose server certificate uses the Wendy development CA.
	if chainPEM != "" && certs.AppendChainToPool(roots, chainPEM) == 0 {
		return nil, fmt.Errorf("parse Wendy enrollment CA chain")
	}
	connection, err := grpc.NewClient(
		normalizeCloudHost(cloudHost),
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
			MinVersion:   tls.VersionTLS13,
			Certificates: []tls.Certificate{certificate},
			RootCAs:      roots,
		})),
	)
	if err != nil {
		return nil, fmt.Errorf("connect to Wendy Cloud: %w", err)
	}
	if s.connection != nil {
		_ = s.connection.Close()
	}
	s.connection = connection
	s.connectionKey = key
	return connection, nil
}
