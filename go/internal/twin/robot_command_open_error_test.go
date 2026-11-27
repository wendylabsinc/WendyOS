package twin

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/twinrobot"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Exercise the production concrete-to-interface opener, rather than a test
// opener returning a literal nil interface that cannot reproduce this panic.
type failedRobotOpenConn struct{ failure string }

func (c failedRobotOpenConn) Invoke(_ context.Context, _ string, _ any, out any, _ ...grpc.CallOption) error {
	switch response := out.(type) {
	case *pb.ListUnitreePairingsResponse:
		switch c.failure {
		case "gateway":
			return status.Error(codes.Unavailable, "pairing gateway unavailable")
		case "canceled":
			return context.Canceled
		case "unpaired":
			return nil
		}
		response.Robots = []*pb.UnitreeRobot{{Serial: "go2", Paired: true, Connected: true}}
		return nil
	case *pb.UnitreeCommandResponse:
		return nil
	default:
		return status.Error(codes.Unimplemented, "clock unavailable")
	}
}
func (c failedRobotOpenConn) NewStream(context.Context, *grpc.StreamDesc, string, ...grpc.CallOption) (grpc.ClientStream, error) {
	return nil, status.Error(codes.Unavailable, "paired Go2 is not connected")
}

func TestProductionRobotOpenerNormalizesFailedConcreteNil(t *testing.T) {
	for _, failure := range []string{"gateway", "canceled", "unpaired", "stream"} {
		t.Run(failure, func(t *testing.T) {
			for _, recovering := range []bool{false, true} {
				s := &Server{Context: context.Background(), Conn: failedRobotOpenConn{failure: failure}}
				var previous *robotSession
				if recovering {
					previous = &robotSession{robot: &setupTestRobot{state: twinrobot.SiteRobotSnapshot{Disconnected: true}}, serial: "go2", commandReceipts: map[string]robotCommandReceipt{"prior-motion": {result: twinrobot.CommandResult{State: "failed", Message: "Original motion failure"}}}}
					s.observers = map[string]*robotSession{"go2": previous}
					s.robots = map[string]*robotSession{"old-desktop": previous}
				}
				robot, err := s.openCommandRobot(context.Background(), "go2")
				if err == nil || robot != nil {
					t.Fatalf("failed concrete pointer escaped as non-nil interface: %T %v", robot, err)
				}
				// Repeated failed HTTP connects must return a normal error, retain any
				// old receipt and handle, and never publish a partial observation session.
				for i := 0; i < 2; i++ {
					w := setupCommandCall(s, context.Background(), s.openCommandRobot, robotCommandRequest{Action: "connect", Serial: "go2"})
					if w.Code == 200 || w.Body.Len() == 0 {
						t.Fatal("failed connect reported success", w.Body.String())
					}
					if s.observers["go2"] != previous {
						t.Fatal("failed opener published a session")
					}
					if recovering && (previous.commandRetired || len(previous.commandReceipts) != 1 || s.robots["old-desktop"] != previous) {
						t.Fatal("failed reconnect discarded previous ownership/history")
					}
				}
				if recovering {
					w := setupCommandCall(s, context.Background(), s.openCommandRobot, robotCommandRequest{Action: "receipt", Serial: "go2", RequestID: "prior-motion"})
					if !strings.Contains(w.Body.String(), "Original motion failure") {
						t.Fatal("old receipt lost", w.Body.String())
					}
				}
			}
		})
	}
}

func TestProductionConnectFailureReturnsHTTPErrorWithoutEOF(t *testing.T) {
	s := &Server{Context: context.Background(), Conn: failedRobotOpenConn{failure: "stream"}}
	server := httptest.NewServer(http.HandlerFunc(s.robotCommand))
	defer server.Close()
	for i := 0; i < 2; i++ {
		response, err := server.Client().Post(server.URL+"/v1/robot/command", "application/json", strings.NewReader(`{"action":"connect","serial":"go2"}`))
		if err != nil {
			t.Fatalf("failed opener dropped HTTP response: %v", err)
		}
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil || response.StatusCode != http.StatusBadRequest || strings.TrimSpace(string(body)) != "rpc error: code = Unavailable desc = paired Go2 is not connected" {
			t.Fatalf("lost original opening failure: status%d body%q read%v", response.StatusCode, body, err)
		}
		s.mu.Lock()
		published := len(s.observers)
		s.mu.Unlock()
		if published != 0 {
			t.Fatal("failed open published an observer")
		}
	}
}
