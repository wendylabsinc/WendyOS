package liteclient

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/roughtime"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/litepb"
)

func TestRelayRoughtimeRequiresDeviceAcknowledgement(t *testing.T) {
	for _, mode := range []string{"success", "no-quorum", "reject", "missing-result", "offline"} {
		t.Run(mode, func(t *testing.T) {
			seen := map[uint32]bool{}
			query := func(_ context.Context, s roughtime.Server, n []byte) (roughtime.Result, error) {
				if len(n) != 32 || n[0] != 0x12 {
					t.Error("did not use device nonce")
				}
				if mode == "offline" {
					return roughtime.Result{}, errors.New("offline")
				}
				return roughtime.Result{RawResponse: []byte(s.Name)}, nil
			}
			send := func(req *pb.WendyComCommand) (*pb.WendyComResponse, error) {
				p := req.GetSyncTime()
				if p == nil || int(p.ServerIndex) >= len(roughtime.Servers) || seen[p.ServerIndex] {
					t.Fatal("bad server mapping")
				}
				seen[p.ServerIndex] = true
				if string(p.Response) != roughtime.Servers[p.ServerIndex].Name {
					t.Fatal("proof changed")
				}
				if mode == "reject" {
					return &pb.WendyComResponse{Result: pb.WendyComResult_WENDY_COM_RESULT_BAD_STATE}, nil
				}
				if mode == "missing-result" {
					return &pb.WendyComResponse{}, nil
				}
				return &pb.WendyComResponse{Data: &pb.WendyComResponse_SyncTime{SyncTime: &pb.WendyComSyncTimeResult{
					Synchronized: mode == "success" && len(seen) >= 2, UnixSeconds: 1800000000,
				}}}, nil
			}
			when, err := relayRoughtime(context.Background(), strings.Repeat("12", 32), query, send)
			if mode == "success" {
				if err != nil || when.Unix() != 1800000000 || len(seen) != 2 {
					t.Fatalf("%v %v", when, err)
				}
			} else if err == nil {
				t.Fatal("false success")
			}
		})
	}
}
