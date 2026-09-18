// Package robotcalclient reaches a device's robot calibration store over the
// agent's gRPC API.
//
// It is the second implementation of robotcal.Store, and the reason the
// calibration wizard can run from a laptop at all: the first, robotcal.FileStore,
// only works from inside an admin-entitled container on the robot itself. Both
// are driven by the same table of tests, because two implementations of one
// interface that are never compared is how a laptop and a robot come to
// disagree about whether a robot is calibrated.
package robotcalclient

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/shared/robotcal"
	"github.com/wendylabsinc/wendy/go/internal/shared/robotcal/robotcalpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// Store is a robotcal.Store served by the agent on the other end of conn.
type Store struct {
	client agentpbv2.WendyRobotServiceClient
	// root is what the device says its store is, fetched once when the store is
	// opened. Not re-derived from robotcal.DefaultRoot here: an error message
	// that names a path this process assumed rather than the path the device
	// actually used would send an operator to the wrong file.
	root string
	// device labels which machine this is, for the same error messages.
	device string
}

var _ robotcal.Store = (*Store)(nil)

// Open connects the store and confirms the device serves it.
//
// The DescribeStore round-trip is deliberate rather than lazy. It costs one
// cheap call and buys two things: the store can describe itself without a
// network call later (robotcal.Store.Describe takes no context and returns no
// error, so it must not dial), and an agent too old to serve the store says so
// here — by name, before the wizard has walked an operator through a procedure
// — instead of failing on the write at the end of it.
func Open(ctx context.Context, conn grpc.ClientConnInterface, device string) (*Store, error) {
	s := &Store{client: agentpbv2.NewWendyRobotServiceClient(conn), device: device}
	resp, err := s.client.DescribeStore(ctx, &agentpbv2.DescribeStoreRequest{})
	if err != nil {
		if status.Code(err) == codes.Unimplemented {
			return nil, fmt.Errorf("this device's agent does not serve the robot calibration store "+
				"(no RobotService in its v2 API), so there is nowhere on the robot to put a calibration "+
				"from here.\n\nUpdate the agent (`wendy device update`), or run the wizard from an "+
				"admin-entitled container on the device itself, where the store is the local file %s "+
				"(see Examples/ClaudeOnDevice)", robotcal.DefaultRoot)
		}
		return nil, fmt.Errorf("reaching the robot calibration store on this device: %w", err)
	}
	s.root = resp.GetRoot()
	if s.root == "" {
		s.root = robotcal.DefaultRoot
	}
	return s, nil
}

// Describe says where the store is, and on which machine — the remote case has
// a second half the on-device case does not.
func (s *Store) Describe() string {
	if s.device == "" {
		return s.root
	}
	return s.root + " on " + s.device
}

// wrap puts the store's location into a failure, because from a laptop "no such
// file" is otherwise about a file on the wrong computer.
func (s *Store) wrap(what string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s in the robot calibration store at %s: %w", what, s.Describe(), err)
}

func (s *Store) Load(ctx context.Context, unit string) (robotcal.UnitRecord, error) {
	if err := robotcal.ValidUnit(unit); err != nil {
		return robotcal.UnitRecord{}, err
	}
	resp, err := s.client.LoadUnit(ctx, &agentpbv2.LoadUnitRequest{Unit: unit})
	if err != nil {
		return robotcal.UnitRecord{}, s.wrap(fmt.Sprintf("reading unit %q", unit), err)
	}
	rec := robotcalpb.UnitFromProto(resp.GetUnit())
	// A unit nobody has calibrated comes back zero-valued; keep its name, the
	// same way FileStore does, so callers never see an anonymous record.
	rec.Unit = unit
	return rec, nil
}

func (s *Store) PutRecord(ctx context.Context, unit string, rec robotcal.Record) error {
	if err := robotcal.ValidUnit(unit); err != nil {
		return err
	}
	req := &agentpbv2.PutRecordRequest{Unit: unit, Record: robotcalpb.RecordToProto(rec)}
	// Checked here as well as on the device so the refusal happens before a
	// calibration is thrown at the network, and so it reads the same either way.
	if err := robotcalpb.CheckSize(fmt.Sprintf("the calibration record %q", rec.ID), req); err != nil {
		return err
	}
	if _, err := s.client.PutRecord(ctx, req); err != nil {
		return s.wrap(fmt.Sprintf("recording calibration %q for unit %q", rec.ID, unit), err)
	}
	return nil
}

func (s *Store) ClearRecord(ctx context.Context, unit, id string) error {
	if err := robotcal.ValidUnit(unit); err != nil {
		return err
	}
	if _, err := s.client.ClearRecord(ctx, &agentpbv2.ClearRecordRequest{Unit: unit, Id: id}); err != nil {
		return s.wrap(fmt.Sprintf("clearing calibration %q for unit %q", id, unit), err)
	}
	return nil
}

func (s *Store) PutSession(ctx context.Context, unit string, sess robotcal.Session) error {
	if err := robotcal.ValidUnit(unit); err != nil {
		return err
	}
	req := &agentpbv2.PutSessionRequest{Unit: unit, Session: robotcalpb.SessionToProto(sess)}
	if err := robotcalpb.CheckSize(fmt.Sprintf("the calibration session for procedure %q", sess.ProcedureID), req); err != nil {
		return err
	}
	if _, err := s.client.PutSession(ctx, req); err != nil {
		return s.wrap(fmt.Sprintf("checkpointing procedure %q for unit %q", sess.ProcedureID, unit), err)
	}
	return nil
}

func (s *Store) ClearSession(ctx context.Context, unit, procedureID string) error {
	if err := robotcal.ValidUnit(unit); err != nil {
		return err
	}
	if _, err := s.client.ClearSession(ctx, &agentpbv2.ClearSessionRequest{Unit: unit, ProcedureId: procedureID}); err != nil {
		return s.wrap(fmt.Sprintf("dropping the checkpoint for procedure %q on unit %q", procedureID, unit), err)
	}
	return nil
}

func (s *Store) SetProfileKind(ctx context.Context, unit, kind string) error {
	if err := robotcal.ValidUnit(unit); err != nil {
		return err
	}
	if _, err := s.client.SetProfileKind(ctx, &agentpbv2.SetProfileKindRequest{Unit: unit, ProfileKind: kind}); err != nil {
		return s.wrap(fmt.Sprintf("recording unit %q as a %q", unit, kind), err)
	}
	return nil
}

func (s *Store) SetStableID(ctx context.Context, unit, stableID string) error {
	if err := robotcal.ValidUnit(unit); err != nil {
		return err
	}
	if _, err := s.client.SetStableId(ctx, &agentpbv2.SetStableIdRequest{Unit: unit, StableId: stableID}); err != nil {
		return s.wrap(fmt.Sprintf("binding unit %q to %q", unit, stableID), err)
	}
	return nil
}

// TooLarge reports whether err is the transport refusing an oversize message,
// wherever in the chain it was raised.
func TooLarge(err error) bool {
	var e *robotcalpb.ErrTooLarge
	if errors.As(err, &e) {
		return true
	}
	return status.Code(err) == codes.ResourceExhausted
}
