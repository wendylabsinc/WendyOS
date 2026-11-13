package liteclient

import (
	"strings"
	"testing"
	"time"

	pb "github.com/wendylabsinc/wendy/go/proto/gen/litepb"
)

func TestResetEnrollmentRequiresFirmwareAcknowledgement(t *testing.T) {
	for _, result := range []pb.WendyComResult{pb.WendyComResult_WENDY_COM_RESULT_OK, pb.WendyComResult_WENDY_COM_RESULT_BAD_STATE} {
		t.Run(result.String(), func(t *testing.T) {
			client, peer := connectedOverPipe(t)
			server := newDirectLink(peer)
			done := make(chan error, 1)
			go func() {
				msg, err := server.recv(time.Second)
				if err != nil {
					done <- err
					return
				}
				cmd := msg.GetCommand()
				params := cmd.GetEnrollmentReset()
				if params.GetExpectedDeviceId() != "441bf6804824" || params.GetNonceHex() != strings.Repeat("12", 32) {
					t.Error("reset did not bind the selected hardware ID and challenge")
				}
				done <- server.send(&pb.WendyComMessage{Msg: &pb.WendyComMessage_Response{Response: &pb.WendyComResponse{RequestId: cmd.RequestId, Result: result}}})
			}()
			err := client.ResetEnrollment("441bf6804824", strings.Repeat("12", 32))
			if (err == nil) != (result == pb.WendyComResult_WENDY_COM_RESULT_OK) {
				t.Fatalf("reset result %s: %v", result, err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestResetEnrollmentRejectsInvalidBindingBeforeSending(t *testing.T) {
	for _, tc := range []struct{ id, nonce string }{
		{"", strings.Repeat("12", 32)},
		{"441bf6804824", ""},
		{"441bf6804824", strings.Repeat("gg", 32)},
		{"441bf6804824", strings.Repeat("12", 31)},
	} {
		if err := NewWendyLiteClient().ResetEnrollment(tc.id, tc.nonce); err == nil || strings.Contains(err.Error(), "connection") {
			t.Fatalf("invalid reset binding reached transport: %v", err)
		}
	}
}
