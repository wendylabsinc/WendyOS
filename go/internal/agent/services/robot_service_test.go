package services

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/wendylabsinc/wendy/go/internal/shared/robotcal"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

func startRobotServer(t *testing.T, root string) agentpbv2.WendyRobotServiceClient {
	t.Helper()
	lis := bufconn.Listen(bufSize)
	srv := grpc.NewServer()
	agentpbv2.RegisterWendyRobotServiceServer(srv, NewRobotService(zap.NewNop(), root))
	go func() { _ = srv.Serve(lis) }()
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() { conn.Close(); srv.Stop(); lis.Close() })
	return agentpbv2.NewWendyRobotServiceClient(conn)
}

// The store's writes are bounded to <root>/units/<unit>/calibration.json, and
// the unit-name rule is what keeps them there. This is the property that lets
// the service sit beside the other write-capable services rather than on the
// mTLS-only list, so it is checked at the RPC boundary and not only inside the
// store.
func TestRobotServiceRefusesAUnitNameThatCouldEscapeTheStore(t *testing.T) {
	root := t.TempDir()
	client := startRobotServer(t, root)
	ctx := t.Context()

	for _, unit := range []string{"../../../etc/wendy", "a/b", "..", "", "Leader", "with space"} {
		if _, err := client.LoadUnit(ctx, &agentpbv2.LoadUnitRequest{Unit: unit}); status.Code(err) != codes.InvalidArgument {
			t.Errorf("LoadUnit(%q) = %v, want InvalidArgument", unit, status.Code(err))
		}
		_, err := client.PutRecord(ctx, &agentpbv2.PutRecordRequest{
			Unit: unit, Record: &agentpbv2.CalibrationRecord{Id: "x"},
		})
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("PutRecord(%q) = %v, want InvalidArgument", unit, status.Code(err))
		}
		_, err = client.SetStableId(ctx, &agentpbv2.SetStableIdRequest{Unit: unit, StableId: "by-id:x"})
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("SetStableId(%q) = %v, want InvalidArgument", unit, status.Code(err))
		}
	}

	// Nothing was created anywhere: the only directory the store may make is
	// <root>/units/<valid unit>.
	if entries, err := filepath.Glob(filepath.Join(root, "*")); err == nil && len(entries) > 0 {
		t.Errorf("refused calls still touched the filesystem: %v", entries)
	}
}

func TestRobotServiceCodesSayWhichMistakeItWas(t *testing.T) {
	client := startRobotServer(t, t.TempDir())
	ctx := t.Context()

	if _, err := client.PutRecord(ctx, &agentpbv2.PutRecordRequest{Unit: "default"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a request with no record = %v, want InvalidArgument", status.Code(err))
	}
	_, err := client.PutRecord(ctx, &agentpbv2.PutRecordRequest{
		Unit: "default", Record: &agentpbv2.CalibrationRecord{},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("a record with no id = %v, want InvalidArgument", status.Code(err))
	}
	_, err = client.PutSession(ctx, &agentpbv2.PutSessionRequest{
		Unit: "default", Session: &agentpbv2.CalibrationSession{},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("a session with no procedure id = %v, want InvalidArgument", status.Code(err))
	}

	// Clearing a calibration that is not there is NOT_FOUND, never OK: a typo
	// reporting success would leave an operator believing a calibration was
	// invalidated after a repair.
	_, err = client.ClearRecord(ctx, &agentpbv2.ClearRecordRequest{Unit: "default", Id: "never-existed"})
	if status.Code(err) != codes.NotFound {
		t.Errorf("clearing an absent calibration = %v, want NotFound", status.Code(err))
	}
	if err != nil && !strings.Contains(err.Error(), "never-existed") {
		t.Errorf("the refusal must name what was not found, got: %v", err)
	}

	// Dropping a checkpoint that is not there is fine — a procedure that
	// finished without ever being interrupted has no session to clear, and the
	// wizard clears unconditionally on success.
	if _, err := client.ClearSession(ctx, &agentpbv2.ClearSessionRequest{
		Unit: "default", ProcedureId: "never-started",
	}); err != nil {
		t.Errorf("clearing an absent checkpoint should be a no-op, got: %v", err)
	}
}

// The service is a transport over robotcal.FileStore and must stay one: what it
// writes has to be the same file the on-device path reads, at the same place,
// or the two paths quietly stop being the same store.
func TestRobotServiceWritesTheSameFileTheDeviceReadsDirectly(t *testing.T) {
	root := t.TempDir()
	client := startRobotServer(t, root)
	ctx := t.Context()

	_, err := client.PutRecord(ctx, &agentpbv2.PutRecordRequest{
		Unit: "leader",
		Record: &agentpbv2.CalibrationRecord{
			Id: "joint-range", Qualified: true,
			Residual: &agentpbv2.Quantity{Value: 4, Unit: "tick"},
			Budget:   &agentpbv2.Quantity{Value: 10, Unit: "tick"},
			Payload:  []byte(`{"zeros":[1,2,3]}`),
		},
	})
	if err != nil {
		t.Fatalf("PutRecord: %v", err)
	}

	// Read it back the way an app on the device would, with no gRPC involved.
	direct := robotcal.NewFileStore(root)
	unit, err := direct.Load(ctx, "leader")
	if err != nil {
		t.Fatalf("reading the store directly: %v", err)
	}
	rec, ok := unit.Calibrations["joint-range"]
	if !ok {
		t.Fatalf("the record the service wrote is not in the file an on-device reader opens: %+v", unit)
	}
	// The same JSON value, not the same bytes: the record file is written with
	// MarshalIndent, which re-indents anything embedded in it. What matters is
	// that the service added no interpretation of its own.
	var got any
	if err := json.Unmarshal(rec.Payload, &got); err != nil {
		t.Fatalf("the payload did not come back as JSON: %v", err)
	}
	if !reflect.DeepEqual(got, map[string]any{"zeros": []any{1.0, 2.0, 3.0}}) {
		t.Errorf("payload = %s, want the value it was given", rec.Payload)
	}
	if rec.Verdict() != robotcal.VerdictQualified {
		t.Errorf("verdict = %q, want %q", rec.Verdict(), robotcal.VerdictQualified)
	}
	path := filepath.Join(root, "units", "leader", "calibration.json")
	if st, err := os.Stat(path); err != nil || st.IsDir() {
		t.Errorf("expected the record at %s: %v", path, err)
	}
}

func TestRobotServiceDescribesWhereTheStoreIs(t *testing.T) {
	root := t.TempDir()
	resp, err := startRobotServer(t, root).DescribeStore(t.Context(), &agentpbv2.DescribeStoreRequest{})
	if err != nil {
		t.Fatalf("DescribeStore: %v", err)
	}
	if resp.GetRoot() != root {
		t.Errorf("root = %q, want %q", resp.GetRoot(), root)
	}

	// An empty root means the device default, not an empty path.
	svc := NewRobotService(zap.NewNop(), "")
	got, err := svc.DescribeStore(t.Context(), &agentpbv2.DescribeStoreRequest{})
	if err != nil {
		t.Fatalf("DescribeStore: %v", err)
	}
	if got.GetRoot() != robotcal.DefaultRoot {
		t.Errorf("root = %q, want %q", got.GetRoot(), robotcal.DefaultRoot)
	}
}
