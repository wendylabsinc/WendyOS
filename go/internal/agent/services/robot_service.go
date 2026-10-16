package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/wendylabsinc/wendy/go/internal/shared/robotcal"
	"github.com/wendylabsinc/wendy/go/internal/shared/robotcal/robotcalpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// RobotService serves the device's robot calibration store to callers that are
// not on the device.
//
// It is a transport and nothing more. Every method delegates to the same
// robotcal.Store the on-device path opens directly — by default a FileStore
// rooted at /var/lib/wendy/robot — so there is one implementation of what a
// calibration record means, reached two ways. Any rule about the store (a unit
// name that could not be a directory, clearing a record that is not there,
// dropping a half-finished session with the record it belongs to) is enforced
// once, in the store, rather than twice and eventually differently.
//
// It writes to the device, so it is registered where every other write-capable
// v2 service is: inside registerAllServices, behind the mTLS org-equality
// interceptor on the mTLS listener. Its writes are bounded to
// <root>/units/<unit>/calibration.json, and robotcal.ValidUnit — which
// FileStore applies to every operation — is what keeps <unit> a plain lowercase
// token and therefore keeps those writes inside the store's own directory.
type RobotService struct {
	agentpbv2.UnimplementedWendyRobotServiceServer
	logger *zap.Logger
	store  robotcal.Store
}

// NewRobotService builds the service over a store rooted at root, or at
// robotcal.DefaultRoot when root is empty.
func NewRobotService(logger *zap.Logger, root string) *RobotService {
	return &RobotService{logger: logger, store: robotcal.NewFileStore(root)}
}

// newRobotServiceWithStore is the seam tests use to drive a store they own.
func newRobotServiceWithStore(logger *zap.Logger, store robotcal.Store) *RobotService {
	return &RobotService{logger: logger, store: store}
}

// storeError maps a store failure onto a gRPC code.
//
// The store's messages are written for an operator and are worth passing
// through verbatim; only the code is decided here. A unit name the store
// refuses is the caller's mistake (InvalidArgument), clearing a record that is
// not there is NotFound — a typo must not read as success — and anything else
// is the device's own filesystem failing.
func storeError(unit string, err error) error {
	if err == nil {
		return nil
	}
	if status.Code(err) != codes.Unknown {
		return err
	}
	if verr := robotcal.ValidUnit(unit); verr != nil {
		return status.Error(codes.InvalidArgument, verr.Error())
	}
	var noSuch *robotcal.NoSuchCalibrationError
	if errors.As(err, &noSuch) {
		return status.Error(codes.NotFound, err.Error())
	}
	var tooLarge *robotcalpb.ErrTooLarge
	if errors.As(err, &tooLarge) {
		return status.Error(codes.ResourceExhausted, err.Error())
	}
	return status.Error(codes.Internal, err.Error())
}

// checkUnit refuses a bad unit name up front, so the caller gets
// InvalidArgument and a name that could escape the store's directory never
// reaches the filesystem at all. FileStore applies the same rule itself; this
// is the explicit gate rather than a reliance on a backstop.
func checkUnit(unit string) error {
	if err := robotcal.ValidUnit(unit); err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	return nil
}

// sized refuses a response that would not survive the transport, naming what
// did not fit. Without it the caller sees gRPC's own limit error, which names
// neither the unit nor the store file it came from.
func sized[T proto.Message](what string, msg T, err error) (T, error) {
	if err != nil {
		var zero T
		return zero, err
	}
	if serr := robotcalpb.CheckSize(what, msg); serr != nil {
		var zero T
		return zero, status.Error(codes.ResourceExhausted, serr.Error())
	}
	return msg, nil
}

func (s *RobotService) LoadUnit(ctx context.Context, req *agentpbv2.LoadUnitRequest) (*agentpbv2.LoadUnitResponse, error) {
	unit := req.GetUnit()
	if err := checkUnit(unit); err != nil {
		return nil, err
	}
	rec, err := s.store.Load(ctx, unit)
	if err != nil {
		return nil, storeError(unit, err)
	}
	return sized(fmt.Sprintf("the calibration record for unit %q, read from %s", unit, s.store.Describe()),
		&agentpbv2.LoadUnitResponse{Unit: robotcalpb.UnitToProto(rec)}, nil)
}

func (s *RobotService) PutRecord(ctx context.Context, req *agentpbv2.PutRecordRequest) (*agentpbv2.PutRecordResponse, error) {
	unit := req.GetUnit()
	if err := checkUnit(unit); err != nil {
		return nil, err
	}
	if req.GetRecord() == nil {
		return nil, status.Error(codes.InvalidArgument, "no calibration record in the request")
	}
	if req.GetRecord().GetId() == "" {
		return nil, status.Error(codes.InvalidArgument,
			"a calibration record needs an id: it is the key the profile's requires_calibration names it by")
	}
	// The payload is opaque — the platform never looks at what a method
	// measured — but the store is a JSON file, so it has to be storable as
	// JSON. Checked here so the caller is told that plainly; without it the
	// store fails at marshal time and the caller gets an Internal error about
	// a character offset.
	if p := req.GetRecord().GetPayload(); len(p) > 0 && !json.Valid(p) {
		return nil, status.Errorf(codes.InvalidArgument,
			"the payload of calibration %q is not valid JSON. Its contents are opaque to WendyOS — a rigid "+
				"transform, per-joint zeros, whatever the method produced — but the record it goes into is a "+
				"JSON file on the device, so it has to be a JSON value. Encode it (a base64 string is fine for "+
				"binary) rather than sending raw bytes",
			req.GetRecord().GetId())
	}
	// A client that skipped its own check, or a client that is not ours.
	if err := robotcalpb.CheckSize(fmt.Sprintf("the calibration record %q", req.GetRecord().GetId()), req); err != nil {
		return nil, status.Error(codes.ResourceExhausted, err.Error())
	}
	if err := s.store.PutRecord(ctx, unit, robotcalpb.RecordFromProto(req.GetRecord())); err != nil {
		return nil, storeError(unit, err)
	}
	s.logger.Info("recorded a robot calibration",
		zap.String("unit", unit),
		zap.String("calibration_id", req.GetRecord().GetId()),
		zap.Bool("qualified", req.GetRecord().GetQualified()))
	return &agentpbv2.PutRecordResponse{}, nil
}

func (s *RobotService) ClearRecord(ctx context.Context, req *agentpbv2.ClearRecordRequest) (*agentpbv2.ClearRecordResponse, error) {
	unit := req.GetUnit()
	if err := checkUnit(unit); err != nil {
		return nil, err
	}
	if req.GetId() == "" {
		return nil, status.Error(codes.InvalidArgument, "name the calibration to clear")
	}
	if err := s.store.ClearRecord(ctx, unit, req.GetId()); err != nil {
		return nil, storeError(unit, err)
	}
	// Worth a line in the journal: after a repair the old calibration is worse
	// than none, and "when was this invalidated" is the question asked later.
	s.logger.Info("cleared a robot calibration",
		zap.String("unit", unit), zap.String("calibration_id", req.GetId()))
	return &agentpbv2.ClearRecordResponse{}, nil
}

func (s *RobotService) PutSession(ctx context.Context, req *agentpbv2.PutSessionRequest) (*agentpbv2.PutSessionResponse, error) {
	unit := req.GetUnit()
	if err := checkUnit(unit); err != nil {
		return nil, err
	}
	if req.GetSession() == nil {
		return nil, status.Error(codes.InvalidArgument, "no session in the request")
	}
	if req.GetSession().GetProcedureId() == "" {
		return nil, status.Error(codes.InvalidArgument,
			"a calibration session needs a procedure id: it is what a resume looks the checkpoint up by")
	}
	// Step results are opaque for the same reason payloads are, and storable
	// under the same constraint. See PutRecord.
	for step, result := range req.GetSession().GetSteps() {
		if len(result) > 0 && !json.Valid(result) {
			return nil, status.Errorf(codes.InvalidArgument,
				"the result recorded for step %q of procedure %q is not valid JSON. What a step measured is "+
					"opaque to WendyOS, but the checkpoint it goes into is a JSON file on the device, so it has "+
					"to be a JSON value",
				step, req.GetSession().GetProcedureId())
		}
	}
	if err := robotcalpb.CheckSize(fmt.Sprintf("the calibration session for procedure %q", req.GetSession().GetProcedureId()), req); err != nil {
		return nil, status.Error(codes.ResourceExhausted, err.Error())
	}
	if err := s.store.PutSession(ctx, unit, robotcalpb.SessionFromProto(req.GetSession())); err != nil {
		return nil, storeError(unit, err)
	}
	return &agentpbv2.PutSessionResponse{}, nil
}

func (s *RobotService) ClearSession(ctx context.Context, req *agentpbv2.ClearSessionRequest) (*agentpbv2.ClearSessionResponse, error) {
	unit := req.GetUnit()
	if err := checkUnit(unit); err != nil {
		return nil, err
	}
	if err := s.store.ClearSession(ctx, unit, req.GetProcedureId()); err != nil {
		return nil, storeError(unit, err)
	}
	return &agentpbv2.ClearSessionResponse{}, nil
}

func (s *RobotService) SetProfileKind(ctx context.Context, req *agentpbv2.SetProfileKindRequest) (*agentpbv2.SetProfileKindResponse, error) {
	unit := req.GetUnit()
	if err := checkUnit(unit); err != nil {
		return nil, err
	}
	if err := s.store.SetProfileKind(ctx, unit, req.GetProfileKind()); err != nil {
		return nil, storeError(unit, err)
	}
	return &agentpbv2.SetProfileKindResponse{}, nil
}

func (s *RobotService) SetStableId(ctx context.Context, req *agentpbv2.SetStableIdRequest) (*agentpbv2.SetStableIdResponse, error) {
	unit := req.GetUnit()
	if err := checkUnit(unit); err != nil {
		return nil, err
	}
	if err := s.store.SetStableID(ctx, unit, req.GetStableId()); err != nil {
		return nil, storeError(unit, err)
	}
	return &agentpbv2.SetStableIdResponse{}, nil
}

func (s *RobotService) DescribeStore(_ context.Context, _ *agentpbv2.DescribeStoreRequest) (*agentpbv2.DescribeStoreResponse, error) {
	return &agentpbv2.DescribeStoreResponse{Root: s.store.Describe()}, nil
}
