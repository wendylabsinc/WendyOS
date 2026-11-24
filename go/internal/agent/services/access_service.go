package services

import (
	"context"
	"errors"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/interceptor"
	"github.com/wendylabsinc/wendy/go/internal/agent/trustedkeys"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var errEnrolled = status.Error(codes.FailedPrecondition,
	"this device is enrolled, so Wendy Cloud manages who may use it; unenroll it to manage its local keys")

// AccessService reports and changes the access shape of a device that is not
// enrolled, and manages a Closed device's trusted keys. Who may call what on
// a Closed device is the role table's decision (interceptor.ClosedAuthorizer);
// these handlers only check the shape.
type AccessService struct {
	agentpbv2.UnimplementedWendyAccessServiceServer
	logger   *zap.Logger
	keys     *trustedkeys.Store
	enrolled func() bool
	// restart applies a change of shape or of the keyless switch: the
	// listeners and the registry differ per shape, so the agent restarts into
	// the new one after replying.
	restart func()
	now     func() time.Time
}

func NewAccessService(logger *zap.Logger, keys *trustedkeys.Store, enrolled func() bool, restart func()) *AccessService {
	return &AccessService{logger: logger, keys: keys, enrolled: enrolled, restart: restart, now: time.Now}
}

func (s *AccessService) GetAccess(ctx context.Context, _ *agentpbv2.GetAccessRequest) (*agentpbv2.GetAccessResponse, error) {
	resp := &agentpbv2.GetAccessResponse{Shape: agentpbv2.AccessShape_ACCESS_SHAPE_OPEN}
	switch {
	case s.enrolled():
		resp.Shape = agentpbv2.AccessShape_ACCESS_SHAPE_ENROLLED
	case s.keys.Closed():
		resp.Shape = agentpbv2.AccessShape_ACCESS_SHAPE_CLOSED
		resp.KeylessApps = s.keys.KeylessApps()
		if c, ok := interceptor.CallerFromContext(ctx); ok {
			resp.CallerRole = roleToProto(c.Role)
		}
	}
	return resp, nil
}

func (s *AccessService) CloseDevice(ctx context.Context, req *agentpbv2.CloseDeviceRequest) (*agentpbv2.CloseDeviceResponse, error) {
	if s.enrolled() {
		return nil, errEnrolled
	}
	spki, err := trustedkeys.ParsePublicKey(req.GetPublicKey())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	owner, err := s.keys.Close(spki, req.GetLabel(), s.now())
	if err != nil {
		return nil, keyError(err)
	}
	s.audit("closed", "", owner.Fingerprint())
	s.restart()
	return &agentpbv2.CloseDeviceResponse{Owner: keyToProto(owner)}, nil
}

func (s *AccessService) OpenDevice(ctx context.Context, _ *agentpbv2.OpenDeviceRequest) (*agentpbv2.OpenDeviceResponse, error) {
	if s.enrolled() {
		return nil, errEnrolled
	}
	actor := actorOf(ctx)
	if err := s.keys.Reopen(actor); err != nil {
		return nil, keyError(err)
	}
	s.audit("opened", actor, "")
	s.restart()
	return &agentpbv2.OpenDeviceResponse{}, nil
}

func (s *AccessService) SetKeylessApps(ctx context.Context, req *agentpbv2.SetKeylessAppsRequest) (*agentpbv2.SetKeylessAppsResponse, error) {
	if s.enrolled() {
		return nil, errEnrolled
	}
	actor := actorOf(ctx)
	if err := s.keys.SetKeylessApps(req.GetEnabled(), actor); err != nil {
		return nil, keyError(err)
	}
	action := "keyless apps off"
	if req.GetEnabled() {
		action = "keyless apps on"
	}
	s.audit(action, actor, "")
	s.restart()
	return &agentpbv2.SetKeylessAppsResponse{}, nil
}

func (s *AccessService) AddTrustedKey(ctx context.Context, req *agentpbv2.AddTrustedKeyRequest) (*agentpbv2.AddTrustedKeyResponse, error) {
	if s.enrolled() {
		return nil, errEnrolled
	}
	spki, err := trustedkeys.ParsePublicKey(req.GetPublicKey())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	role := roleFromProto(req.GetRole())
	if role == trustedkeys.RoleNone {
		return nil, status.Error(codes.InvalidArgument, "a role is required: owner, operator or viewer")
	}
	actor := actorOf(ctx)
	added, err := s.keys.Add(spki, role, req.GetLabel(), actor, s.now())
	if err != nil {
		return nil, keyError(err)
	}
	s.audit("key added as "+role.String(), actor, added.Fingerprint())
	return &agentpbv2.AddTrustedKeyResponse{Key: keyToProto(added)}, nil
}

func (s *AccessService) ListTrustedKeys(context.Context, *agentpbv2.ListTrustedKeysRequest) (*agentpbv2.ListTrustedKeysResponse, error) {
	if s.enrolled() {
		return nil, errEnrolled
	}
	if !s.keys.Closed() {
		return nil, keyError(trustedkeys.ErrNotClosed)
	}
	resp := &agentpbv2.ListTrustedKeysResponse{}
	for _, e := range s.keys.List() {
		resp.Keys = append(resp.Keys, keyToProto(e))
	}
	return resp, nil
}

func (s *AccessService) RemoveTrustedKey(ctx context.Context, req *agentpbv2.RemoveTrustedKeyRequest) (*agentpbv2.RemoveTrustedKeyResponse, error) {
	if s.enrolled() {
		return nil, errEnrolled
	}
	actor := actorOf(ctx)
	removed, err := s.keys.Remove(req.GetFingerprint(), actor)
	if err != nil {
		return nil, keyError(err)
	}
	s.audit("key removed", actor, removed.Fingerprint())
	// The role table runs when a call or stream opens; the restart ends the
	// removed key's open shells, execs, attaches and log streams.
	s.restart()
	return &agentpbv2.RemoveTrustedKeyResponse{}, nil
}

func (s *AccessService) audit(action, actor, key string) {
	s.logger.Info("audit: device access changed",
		zap.String("audit", "access"), zap.String("action", action),
		zap.String("actor_fingerprint", actor), zap.String("key_fingerprint", key))
}

// actorOf is the calling key's fingerprint; the store re-checks under its
// lock that it is still an owner.
func actorOf(ctx context.Context) string {
	c, _ := interceptor.CallerFromContext(ctx)
	return c.Fingerprint
}

func keyError(err error) error {
	switch {
	case errors.Is(err, trustedkeys.ErrNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, trustedkeys.ErrAlreadyExists):
		return status.Error(codes.AlreadyExists, err.Error())
	case errors.Is(err, trustedkeys.ErrActorNotOwner):
		return status.Error(codes.PermissionDenied, err.Error())
	case errors.Is(err, trustedkeys.ErrAlreadyClosed), errors.Is(err, trustedkeys.ErrNotClosed),
		errors.Is(err, trustedkeys.ErrLastOwner):
		return status.Error(codes.FailedPrecondition, err.Error())
	default:
		return status.Error(codes.Internal, err.Error())
	}
}

func roleToProto(r trustedkeys.Role) agentpbv2.TrustedKeyRole {
	switch r {
	case trustedkeys.RoleOwner:
		return agentpbv2.TrustedKeyRole_TRUSTED_KEY_ROLE_OWNER
	case trustedkeys.RoleOperator:
		return agentpbv2.TrustedKeyRole_TRUSTED_KEY_ROLE_OPERATOR
	case trustedkeys.RoleViewer:
		return agentpbv2.TrustedKeyRole_TRUSTED_KEY_ROLE_VIEWER
	}
	return agentpbv2.TrustedKeyRole_TRUSTED_KEY_ROLE_UNSPECIFIED
}

func roleFromProto(r agentpbv2.TrustedKeyRole) trustedkeys.Role {
	switch r {
	case agentpbv2.TrustedKeyRole_TRUSTED_KEY_ROLE_OWNER:
		return trustedkeys.RoleOwner
	case agentpbv2.TrustedKeyRole_TRUSTED_KEY_ROLE_OPERATOR:
		return trustedkeys.RoleOperator
	case agentpbv2.TrustedKeyRole_TRUSTED_KEY_ROLE_VIEWER:
		return trustedkeys.RoleViewer
	}
	return trustedkeys.RoleNone
}

func keyToProto(e trustedkeys.Entry) *agentpbv2.TrustedKey {
	return &agentpbv2.TrustedKey{
		Fingerprint: e.Fingerprint(),
		PublicKey:   e.SPKI(),
		Role:        roleToProto(e.KeyRole()),
		Label:       e.Label,
		AddedAt:     e.AddedAt.Unix(),
		AddedBy:     e.AddedBy,
	}
}
