package services

import (
	"context"
	"errors"
	"path/filepath"
	"sync"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/meshsharing"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

type LocalMeshRuntimeStatus struct {
	Available          bool
	State, Detail      string
	AuthenticatedPeers int
	GatewayPrincipal   string
}

// LocalMeshAdminService serializes partial updates from every listener. The
// carrier document also contains manually configured TCP pairs, which are
// retained byte-for-byte in meaning when NAN or BLE is toggled.
type LocalMeshAdminService struct {
	pb.UnimplementedWendyLocalMeshAdminServiceServer
	mu          sync.Mutex
	sharingPath string
	carrierPath string
	identity    func() (meshid.Tenant, meshid.Device)
	runtime     func() LocalMeshRuntimeStatus
	topology    func() localmesh.NodeSnapshot
	logger      *zap.Logger
}

func NewLocalMeshAdminService(logger *zap.Logger, configDir string, identity func() (meshid.Tenant, meshid.Device)) *LocalMeshAdminService {
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

// authorize admits only a user certificate in this device's tenant. It gates
// ConfigureLocalMesh, whose audit log names that user.
func (s *LocalMeshAdminService) authorize(ctx context.Context) (meshid.Device, error) {
	actor, err := userIdentityFromContext(ctx, "administering wendy net")
	if err != nil {
		return meshid.Device{}, err
	}
	tenant, device, err := s.enrolledIdentity()
	if err != nil {
		return meshid.Device{}, err
	}
	if actor.TenantUUID == "" || actor.TenantUUID != tenant.UUID().String() {
		return meshid.Device{}, status.Error(codes.PermissionDenied, "wendy net administration requires a user in this device's tenant")
	}
	return device, nil
}

// authorizeRead gates the read-only RPCs. Beyond what authorize admits, it
// admits the local admin socket: that caller has no certificate, but reaching
// the socket is already full-agent authority (see onLocalAdminSocket), so
// withholding this device's own Wendy Net view from it protects nothing. The
// plaintext provisioning listener has no certificate either, but it is TCP, so
// it falls through to authorize and is refused. Writes stay user-only, as
// SetBuildHostEnabled does: they change which radios run and what this device
// offers its tenant, and the audit log should name the person who did that.
func (s *LocalMeshAdminService) authorizeRead(ctx context.Context) (meshid.Device, error) {
	if p, ok := peer.FromContext(ctx); ok && onLocalAdminSocket(p) {
		_, device, err := s.enrolledIdentity()
		return device, err
	}
	return s.authorize(ctx)
}

// enrolledIdentity re-reads the device identity on every call, because
// enrollment can replace it while the agent runs.
func (s *LocalMeshAdminService) enrolledIdentity() (meshid.Tenant, meshid.Device, error) {
	if s.identity == nil {
		return meshid.Tenant{}, meshid.Device{}, status.Error(codes.FailedPrecondition, "device identity unavailable")
	}
	tenant, device := s.identity()
	if tenant == (meshid.Tenant{}) || device == (meshid.Device{}) {
		return meshid.Tenant{}, meshid.Device{}, status.Error(codes.FailedPrecondition, "device must be enrolled")
	}
	return tenant, device, nil
}

func (s *LocalMeshAdminService) read(device meshid.Device) (meshsharing.Config, localmesh.TCPConfig, error) {
	sharing, err := meshsharing.LoadConfig(s.sharingPath)
	if err != nil {
		return meshsharing.Config{}, localmesh.TCPConfig{}, err
	}
	carriers, err := localmesh.LoadTCPConfig(s.carrierPath, device)
	if err != nil {
		return meshsharing.Config{}, localmesh.TCPConfig{}, err
	}
	if carriers == nil {
		return sharing, localmesh.TCPConfig{}, nil
	}
	return sharing, *carriers, nil
}

func (s *LocalMeshAdminService) response(sharing meshsharing.Config, carriers localmesh.TCPConfig) *pb.LocalMeshStatus {
	state, detail := "disabled", "Wendy net is disabled."
	if sharing.Participate {
		state, detail = "pending-runtime", "Configuration is saved; awaiting runtime state."
	}
	out := &pb.LocalMeshStatus{Configured: &pb.LocalMeshConfiguration{
		Participate: sharing.Participate, Roam: sharing.Roam, ShareUplink: sharing.ShareUplink,
		Nan: carriers.NAN, Ble: carriers.BLE, Ethernet: carriers.Ethernet, InfrastructureWifi: carriers.InfrastructureWiFi,
		TcpListen: carriers.Listen, ConfiguredTcpPeers: uint32(len(carriers.Peers)),
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
		out.GatewayPrincipal = runtime.GatewayPrincipal
	}
	return out
}

func (s *LocalMeshAdminService) GetLocalMeshStatus(ctx context.Context, _ *pb.GetLocalMeshStatusRequest) (*pb.LocalMeshStatus, error) {
	device, err := s.authorizeRead(ctx)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sharing, carriers, err := s.read(device)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "reading wendy net configuration: %v", err)
	}
	return s.response(sharing, carriers), nil
}

func (s *LocalMeshAdminService) ConfigureLocalMesh(ctx context.Context, req *pb.ConfigureLocalMeshRequest) (*pb.LocalMeshStatus, error) {
	device, err := s.authorize(ctx)
	if err != nil {
		return nil, err
	}
	if req == nil || (req.Participate == nil && req.Roam == nil && req.ShareUplink == nil && req.Nan == nil && req.Ble == nil && req.Ethernet == nil && req.InfrastructureWifi == nil) {
		return nil, status.Error(codes.InvalidArgument, "specify at least one wendy net setting")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sharing, carriers, err := s.read(device)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "reading wendy net configuration: %v", err)
	}
	previous := sharing
	shareChanged := req.Participate != nil || req.Roam != nil || req.ShareUplink != nil
	carrierChanged := req.Nan != nil || req.Ble != nil || req.Ethernet != nil || req.InfrastructureWifi != nil
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
	if req.Ethernet != nil {
		carriers.Ethernet = *req.Ethernet
	}
	if req.InfrastructureWifi != nil {
		carriers.InfrastructureWiFi = *req.InfrastructureWifi
	}
	if err = sharing.Validate(); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	// The two independent documents are each fsync-and-rename atomic. Save
	// sharing first so radio enablement cannot run before its policy exists.
	// Roll it back if writing the carrier document fails.
	if shareChanged {
		if err = meshsharing.SaveConfig(s.sharingPath, sharing); err != nil {
			return nil, status.Errorf(codes.Internal, "saving wendy net sharing policy: %v", err)
		}
	}
	if carrierChanged {
		if err = meshsharing.SaveCarrierConfig(s.carrierPath, device, carriers); err != nil {
			if shareChanged {
				err = errors.Join(err, meshsharing.SaveConfig(s.sharingPath, previous))
			}
			return nil, status.Errorf(codes.Internal, "saving wendy net carriers: %v", err)
		}
	}
	if s.logger != nil {
		actor, _ := userIdentityFromContext(ctx, "wendy net configuration")
		s.logger.Info("wendy net configuration saved", zap.String("actorTenant", actor.TenantUUID), zap.String("actorUser", actor.EntityID),
			zap.Bool("participate", sharing.Participate), zap.Bool("roam", sharing.Roam), zap.Bool("shareUplink", sharing.ShareUplink),
			zap.Bool("nan", carriers.NAN), zap.Bool("ble", carriers.BLE),
			zap.Bool("ethernet", carriers.Ethernet), zap.Bool("infrastructureWifi", carriers.InfrastructureWiFi))
	}
	return s.response(sharing, carriers), nil
}
