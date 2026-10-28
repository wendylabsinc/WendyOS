package liteclient

import (
	"fmt"
	"strings"
	"testing"
	"time"

	pb "github.com/wendylabsinc/wendy/go/proto/gen/litepb"
	"google.golang.org/protobuf/proto"
)

func TestWendyComNetGetSetPreservesTypedTwoBLESettings(t *testing.T) {
	client, peer := connectedOverPipe(t)
	wire := newDirectLink(peer)
	done := make(chan error, 1)
	go func() {
		desired := &pb.WendyComNetConfig{MaxLinks: 1, BleLinks: 1, WifiLinks: 1, NanLinks: 1}
		for operation := 0; operation < 2; operation++ {
			message, err := wire.recv(time.Second)
			if err != nil {
				done <- err
				return
			}
			command := message.GetCommand()
			if command == nil {
				done <- fmt.Errorf("missing typed command")
				return
			}
			if operation == 0 {
				if command.GetNetGet() == nil {
					done <- fmt.Errorf("expected read-only NET_GET")
					return
				}
			} else {
				request := command.GetNetSet()
				if request == nil || request.Config == nil {
					done <- fmt.Errorf("expected typed NET_SET")
					return
				}
				desired = request.Config
			}
			response := &pb.WendyComMessage{Msg: &pb.WendyComMessage_Response{Response: &pb.WendyComResponse{RequestId: command.RequestId, Result: pb.WendyComResult_WENDY_COM_RESULT_OK, Data: &pb.WendyComResponse_NetStatus{NetStatus: &pb.WendyComNetStatus{Desired: desired, BleSupported: true}}}}}
			if err := wire.send(response); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	initial, err := client.GetNetStatus(time.Second)
	if err != nil || initial.Desired.Enabled || initial.WifiSupported || !initial.BleSupported {
		t.Fatalf("status %v %v", initial, err)
	}
	desired := proto.Clone(initial.Desired).(*pb.WendyComNetConfig)
	desired.Enabled = true
	desired.Ble = true
	desired.MaxLinks = 2
	desired.BleLinks = 2
	actual, err := client.SetNetConfig(desired, time.Second)
	if err != nil || !proto.Equal(actual.Desired, desired) || actual.WifiSupported || actual.NanSupported {
		t.Fatalf("NET_SET roundtrip %v %v", actual, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestWendyComCarrierChangeReportsAppliedRestarting(t *testing.T) {
	client, peer := connectedOverPipe(t)
	wire := newDirectLink(peer)
	done := make(chan error, 1)
	go func() {
		request, err := wire.recv(time.Second)
		if err != nil {
			done <- err
			return
		}
		command := request.GetCommand()
		response := &pb.WendyComResponse{RequestId: command.RequestId, Result: pb.WendyComResult_WENDY_COM_RESULT_APPLIED_RESTARTING,
			Data: &pb.WendyComResponse_NetStatus{NetStatus: &pb.WendyComNetStatus{Desired: command.GetNetSet().Config}}}
		done <- wire.send(&pb.WendyComMessage{Msg: &pb.WendyComMessage_Response{Response: response}})
	}()
	desired := &pb.WendyComNetConfig{Enabled: true, Ble: true, MaxLinks: 1}
	actual, err := client.SetNetConfig(desired, time.Second)
	if err != nil || !actual.GetRestarting() || !proto.Equal(actual.GetDesired(), desired) {
		t.Fatalf("applied restart = %v, %v", actual, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestWendyComCarrierCapabilityReasons(t *testing.T) {
	for _, test := range []struct {
		result pb.WendyComResult
		reason string
	}{
		{pb.WendyComResult_WENDY_COM_RESULT_NET_NO_WIFI, "WiFi is not supported by this device"},
		{pb.WendyComResult_WENDY_COM_RESULT_NET_NO_BLE, "BLE is not supported by this device"},
		{pb.WendyComResult_WENDY_COM_RESULT_NET_NO_NAN, "NAN is not supported by this device"},
		{pb.WendyComResult_WENDY_COM_RESULT_NET_ONE_CARRIER_ONLY, "only one carrier is supported at a time"},
	} {
		if err := resultToError(test.result); err == nil || err.Error() != test.reason {
			t.Errorf("%v = %v", test.result, err)
		}
	}
}

func TestWendyComNetResponseValidation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		set    bool
		result pb.WendyComResult
		status *pb.WendyComNetStatus
		want   string
	}{
		{name: "old firmware rejects command", result: pb.WendyComResult_WENDY_COM_RESULT_UNKNOWN_ERROR, want: "unavailable"},
		{name: "missing status", result: pb.WendyComResult_WENDY_COM_RESULT_OK, want: "no Wendy Net status"},
		{name: "restart only applies to writes", result: pb.WendyComResult_WENDY_COM_RESULT_APPLIED_RESTARTING, status: &pb.WendyComNetStatus{}, want: "invalid restarting response"},
		{name: "restart requires status", set: true, result: pb.WendyComResult_WENDY_COM_RESULT_APPLIED_RESTARTING, want: "invalid restarting response"},
		{name: "capability error includes device diagnostic", set: true, result: pb.WendyComResult_WENDY_COM_RESULT_NET_NO_NAN, status: &pb.WendyComNetStatus{ErrorCode: 42}, want: "NAN is not supported by this device (device code 42)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, peer := connectedOverPipe(t)
			wire := newDirectLink(peer)
			done := make(chan error, 1)
			go func() {
				message, err := wire.recv(time.Second)
				if err != nil {
					done <- err
					return
				}
				response := &pb.WendyComResponse{RequestId: message.GetCommand().RequestId, Result: tc.result}
				if tc.status != nil {
					response.Data = &pb.WendyComResponse_NetStatus{NetStatus: tc.status}
				}
				done <- wire.send(&pb.WendyComMessage{Msg: &pb.WendyComMessage_Response{Response: response}})
			}()
			var err error
			if tc.set {
				_, err = client.SetNetConfig(&pb.WendyComNetConfig{}, time.Second)
			} else {
				_, err = client.GetNetStatus(time.Second)
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}
