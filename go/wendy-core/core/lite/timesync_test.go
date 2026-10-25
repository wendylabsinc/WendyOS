package lite

import (
	"context"
	"errors"
	"strings"
	"testing"

	pb "github.com/wendylabsinc/WendyOS/go/wendy-core/core/lite/pb"
	"github.com/wendylabsinc/WendyOS/go/wendy-core/platform"
)

func TestRelayRoughtimeRequiresDeviceAcknowledgement(t *testing.T) {
	names := map[string]string{}
	for _, s := range testTimeServers {
		names[s.Address] = s.Name
	}
	for _, mode := range []string{"success", "no-quorum", "reject", "missing-result", "offline"} {
		t.Run(mode, func(t *testing.T) {
			seen := map[string]bool{}
			query := func(_ context.Context, s platform.LiteTimeServer, n []byte) ([]byte, error) {
				if len(n) != 32 || n[0] != 0x12 {
					t.Error("did not use device nonce")
				}
				if mode == "offline" {
					return nil, errors.New("offline")
				}
				return []byte(s.Name), nil
			}
			send := func(req *pb.WendyComCommand) (*pb.WendyComResponse, error) {
				p := req.GetSyncTime()
				if p == nil || names[p.Server] == "" || seen[p.Server] {
					t.Fatal("bad server mapping")
				}
				seen[p.Server] = true
				if string(p.Response) != names[p.Server] {
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
			when, err := relayRoughtime(context.Background(), strings.Repeat("12", 32), testTimeServers, query, send)
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

var testTimeServers = []platform.LiteTimeServer{{Name: "one", Address: "one:2002"}, {Name: "two", Address: "two:2002"}, {Name: "three", Address: "three:2002"}}
