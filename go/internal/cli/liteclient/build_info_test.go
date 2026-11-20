package liteclient

import (
	"errors"
	"fmt"
	"testing"
	"time"

	pb "github.com/wendylabsinc/wendy/go/proto/gen/litepb"
)

// answerBuildInfo answers one get_build_info command with result and info.
func answerBuildInfo(t *testing.T, wire *directLink, result pb.WendyComResult, info *pb.WendyComBuildInfo) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		message, err := wire.recv(time.Second)
		if err != nil {
			done <- err
			return
		}
		command := message.GetCommand()
		if command.GetGetBuildInfo() == nil {
			done <- fmt.Errorf("expected get_build_info, got %v", message)
			return
		}
		response := &pb.WendyComResponse{RequestId: command.RequestId, Result: result}
		if info != nil {
			response.Data = &pb.WendyComResponse_BuildInfo{BuildInfo: info}
		}
		done <- wire.send(&pb.WendyComMessage{Msg: &pb.WendyComMessage_Response{Response: response}})
	}()
	return done
}

func TestGetBuildInfoReturnsTheDeviceValues(t *testing.T) {
	client, peer := connectedOverPipe(t)
	done := answerBuildInfo(t, newDirectLink(peer), pb.WendyComResult_WENDY_COM_RESULT_OK, &pb.WendyComBuildInfo{
		OsVersion:    "1d93ece2f307d9722325a6f2eebe52fdcd475740-dirty",
		BuildProfile: `{"board":"m5_stamp_s3","app_type":"native"}`,
		BuildTool:    "idf-v6.0.3",
	})
	info, err := client.GetBuildInfo(time.Second)
	if err != nil {
		t.Fatal(err)
	}
	// The client does not interpret the values: "-dirty" stays.
	want := BuildInfo{
		OSVersion:    "1d93ece2f307d9722325a6f2eebe52fdcd475740-dirty",
		BuildProfile: `{"board":"m5_stamp_s3","app_type":"native"}`,
		BuildTool:    "idf-v6.0.3",
	}
	if *info != want {
		t.Errorf("GetBuildInfo = %+v, want %+v", *info, want)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestGetBuildInfoOnOlderFirmware(t *testing.T) {
	for _, tc := range []struct {
		name        string
		result      pb.WendyComResult
		unsupported bool
	}{
		// The default case of the device's command dispatch.
		{"unknown command", pb.WendyComResult_WENDY_COM_RESULT_UNKNOWN_ERROR, true},
		// A link without the management permission.
		{"bad state", pb.WendyComResult_WENDY_COM_RESULT_BAD_STATE, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, peer := connectedOverPipe(t)
			done := answerBuildInfo(t, newDirectLink(peer), tc.result, nil)
			_, err := client.GetBuildInfo(time.Second)
			if err == nil {
				t.Fatal("GetBuildInfo succeeded, want an error")
			}
			if got := errors.Is(err, ErrBuildInfoUnsupported); got != tc.unsupported {
				t.Errorf("GetBuildInfo error %v: ErrBuildInfoUnsupported = %v, want %v", err, got, tc.unsupported)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}
