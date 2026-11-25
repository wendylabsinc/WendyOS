package backend

import (
	"context"
	"encoding/json"
	"github.com/wendylabsinc/wendy/go/internal/cli/chat"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
	"strings"
	"testing"
)

func TestChatNativeCardsAreBoundedAndTyped(t *testing.T) {
	turn := &ChatTurn{}
	tools := &deviceCardTools{turn: turn}
	for i := 0; i < 3; i++ {
		_, err := tools.Execute(context.Background(), chat.ToolCall{Name: "show_device_card", Arguments: json.RawMessage(`{"kind":"resources"}`)})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(turn.Cards) != 1 || turn.Cards[0].Kind != "resources" {
		t.Fatal("duplicate native cards")
	}
	if _, err := tools.Execute(context.Background(), chat.ToolCall{Name: "show_device_card", Arguments: json.RawMessage(`{"kind":"html","url":"https://example.com"}`)}); err == nil {
		t.Fatal("arbitrary content accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := tools.Execute(ctx, chat.ToolCall{Name: "show_device_card", Arguments: json.RawMessage(`{"kind":"device_info"}`)}); err == nil {
		t.Fatal("cancelled device session rendered a card")
	}
	if len(turn.Cards) != 1 {
		t.Fatal("cancelled session changed cards")
	}
}

type chatCameraCatalog struct{ pb.WendyVideoServiceClient }

func (chatCameraCatalog) ListVideoDevices(context.Context, *pb.ListVideoDevicesRequest, ...grpc.CallOption) (*pb.ListVideoDevicesResponse, error) {
	return &pb.ListVideoDevicesResponse{Devices: []*pb.VideoDevice{{Id: 201, Name: "esp-camera-02:camera"}, {Id: 129, Name: "ROS 2 /camera/color/image_raw"}, {Id: 130, Name: "ROS 2 /camera/color/image_raw"}}}, nil
}
func TestChatNamedCameraDoesNotRenderEveryCamera(t *testing.T) {
	for _, test := range []struct {
		args   string
		count  int
		camera uint32
		fails  bool
	}{
		{`{"kind":"camera","camera_name":"ESP camera"}`, 1, 201, false},
		{`{"kind":"camera","camera_id":130}`, 1, 130, false},
		{`{"kind":"camera"}`, 0, 0, true},
		{`{"kind":"camera","camera_name":"ROS 2"}`, 0, 0, true},
		{`{"kind":"camera","camera_name":"missing"}`, 0, 0, true},
		{`{"kind":"camera","all_cameras":true}`, 3, 201, false},
		{`{"kind":"camera","all_cameras":true,"camera_id":201}`, 0, 0, true},
	} {
		turn := &ChatTurn{}
		tools := &deviceCardTools{conn: &grpcclient.AgentConnection{VideoService: chatCameraCatalog{}}, turn: turn}
		_, err := tools.Execute(context.Background(), chat.ToolCall{Name: "show_device_card", Arguments: json.RawMessage(test.args)})
		if (err != nil) != test.fails || len(turn.Cards) != test.count {
			t.Fatalf("%s: err=%v cards=%+v", test.args, err, turn.Cards)
		}
		if test.count > 0 && turn.Cards[0].Camera != test.camera {
			t.Fatal("wrong camera")
		}
	}
}

func TestChatNotificationCardOnlyOffersRegistration(t *testing.T) {
	state := &ChatTurn{}
	tools := &deviceCardTools{turn: state}
	result, err := tools.Execute(context.Background(), chat.ToolCall{Name: "show_device_card", Arguments: json.RawMessage(`{"kind":"notifications"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Cards) != 1 || state.Cards[0].Kind != "notifications" || !strings.Contains(result, "does not enable notifications") {
		t.Fatalf("unexpected card result: %s", result)
	}
}
