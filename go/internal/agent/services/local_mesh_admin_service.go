package services

import (
	"context"
	"errors"
	"path/filepath"
	"sync"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/meshsharing"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type LocalMeshRuntimeStatus struct {
	Available          bool
	State, Detail      string
	AuthenticatedPeers int
	GatewayAsset       int32
}

// LocalMeshAdminService serializes partial updates from every listener. The
// carrier document also contains manually configured TCP pairs, which are
// retained byte-for-byte in meaning when NAN or BLE is toggled.
type LocalMeshAdminService struct {
	pb.UnimplementedWendyLocalMeshAdminServiceServer
	mu          sync.Mutex
	sharingPath string
	carrierPath string
	identity    func() (org, asset int32)
	runtime     func() LocalMeshRuntimeStatus
	logger      *zap.Logger
}

func NewLocalMeshAdminService(logger *zap.Logger, configDir string, identity func() (org, asset int32)) *LocalMeshAdminService {
	return &LocalMeshAdminService{logger: logger, sharingPath: filepath.Join(configDir, "nan-mesh.json"),
		carrierPath: filepath.Join(configDir, "local-mesh.json"), identity: identity}
}

// SetRuntimeStatusSource should be called before service listeners start. A
// nil source reports saved intent as pending; it never claims active radios.
func (s *LocalMeshAdminService) SetRuntimeStatusSource(source func() LocalMeshRuntimeStatus) {
	s.mu.Lock()
	s.runtime = source
	s.mu.Unlock()
}

func (s *LocalMeshAdminService) authorize(ctx context.Context) (org, asset int32, err error) {
	actor, err := userIdentityFromContext(ctx, "administering local mesh")
	if err != nil {
		return 0, 0, err
	}
	if s.identity == nil {
		return 0, 0, status.Error(codes.FailedPrecondition, "device identity unavailable")
	}
	org, asset = s.identity()
	if org <= 0 || asset <= 0 {
		return 0, 0, status.Error(codes.FailedPrecondition, "device must be enrolled")
	}
	if actor.OrgID != org {
		return 0, 0, status.Error(codes.PermissionDenied, "local mesh administration requires a user in this device's organization")
	}
	return org, asset, nil
}

func (s *LocalMeshAdminService) read(asset int32) (meshsharing.Config, localmesh.TCPConfig, error) {
	sharing, err := meshsharing.LoadConfig(s.sharingPath)
	if err != nil {
		return meshsharing.Config{}, localmesh.TCPConfig{}, err
	}
	carriers, err := localmesh.LoadTCPConfig(s.carrierPath, asset)
	if err != nil {
		return meshsharing.Config{}, localmesh.TCPConfig{}, err
	}
	if carriers == nil {
		return sharing, localmesh.TCPConfig{}, nil
	}
	return sharing, *carriers, nil
}

func (s *LocalMeshAdminService) response(sharing meshsharing.Config, carriers localmesh.TCPConfig) *pb.LocalMeshStatus {
	state, detail := "disabled", "Local mesh is disabled."
	if sharing.Participate || carriers.NAN || carriers.BLE || carriers.Listen != "" {
		state, detail = "pending-runtime", "Configuration is saved; awaiting runtime state."
	}
	out := &pb.LocalMeshStatus{Configured: &pb.LocalMeshConfiguration{
		Participate: sharing.Participate, Roam: sharing.Roam, ShareUplink: sharing.ShareUplink,
		Nan: carriers.NAN, Ble: carriers.BLE, TcpListen: carriers.Listen, ConfiguredTcpPeers: uint32(len(carriers.Peers)),
	}, State: state, Detail: detail}
	if s.runtime != nil {
		runtime := s.runtime()
		out.RuntimeAvailable = runtime.Available
		if runtime.State != "" {
			out.State = runtime.State
		}
		if runtime.Detail != "" {
			out.Detail = runtime.Detail
		}
		if runtime.AuthenticatedPeers > 0 {
			out.AuthenticatedPeers = uint32(runtime.AuthenticatedPeers)
		}
		out.GatewayAssetId = runtime.GatewayAsset
	}
	return out
}

func (s *LocalMeshAdminService) GetLocalMeshStatus(ctx context.Context, _ *pb.GetLocalMeshStatusRequest) (*pb.LocalMeshStatus, error) {
	_, asset, err := s.authorize(ctx)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sharing, carriers, err := s.read(asset)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "reading local mesh configuration: %v", err)
	}
	return s.response(sharing, carriers), nil
}

func (s *LocalMeshAdminService) ConfigureLocalMesh(ctx context.Context, req *pb.ConfigureLocalMeshRequest) (*pb.LocalMeshStatus, error) {
	_, asset, err := s.authorize(ctx)
	if err != nil {
		return nil, err
	}
	if req == nil || (req.Participate == nil && req.Roam == nil && req.ShareUplink == nil && req.Nan == nil && req.Ble == nil) {
		return nil, status.Error(codes.InvalidArgument, "specify at least one local mesh setting")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sharing, carriers, err := s.read(asset)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "reading local mesh configuration: %v", err)
	}
	previous := sharing
	shareChanged := req.Participate != nil || req.Roam != nil || req.ShareUplink != nil
	carrierChanged := req.Nan != nil || req.Ble != nil
	if req.Participate != nil {
		sharing.Participate = *req.Participate
	}
	if req.Roam != nil {
		sharing.Roam = *req.Roam
	}
	if req.ShareUplink != nil {
		sharing.ShareUplink = *req.ShareUplink
	}
	if req.Nan != nil {
		carriers.NAN = *req.Nan
	}
	if req.Ble != nil {
		carriers.BLE = *req.Ble
	}
	if err = sharing.Validate(); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	// The two independent documents are each fsync-and-rename atomic. Save
	// sharing first so radio enablement cannot run before its policy exists.
	// Roll it back if writing the carrier document fails.
	if shareChanged {
		if err = meshsharing.SaveConfig(s.sharingPath, sharing); err != nil {
			return nil, status.Errorf(codes.Internal, "saving local mesh sharing policy: %v", err)
		}
	}
	if carrierChanged {
		if err = meshsharing.SaveCarrierConfig(s.carrierPath, asset, carriers); err != nil {
			if shareChanged {
				err = errors.Join(err, meshsharing.SaveConfig(s.sharingPath, previous))
			}
			return nil, status.Errorf(codes.Internal, "saving local mesh carriers: %v", err)
		}
	}
	if s.logger != nil {
		actor, _ := userIdentityFromContext(ctx, "local mesh configuration")
		s.logger.Info("local mesh configuration saved", zap.Int32("actorOrg", actor.OrgID), zap.String("actorUser", actor.EntityID),
			zap.Bool("participate", sharing.Participate), zap.Bool("roam", sharing.Roam), zap.Bool("shareUplink", sharing.ShareUplink),
			zap.Bool("nan", carriers.NAN), zap.Bool("ble", carriers.BLE))
	}
	return s.response(sharing, carriers), nil
}
