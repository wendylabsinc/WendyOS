package liteclient

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/roughtime"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/litepb"
)

// SyncTime relays fresh signed evidence. The device enforces its own pinned
// keys, nonce deadline, two-server consensus and persisted anti-rollback floor.
func (c *WendyLiteClient) SyncTime(ctx context.Context) (time.Time, error) {
	challenge, err := c.EnrollmentChallenge(false)
	if err != nil {
		return time.Time{}, err
	}
	if !challenge.GetRoughtimeSupported() {
		return time.Time{}, fmt.Errorf("firmware does not support Roughtime; update the board first")
	}
	return c.SyncTimeChallenge(ctx, challenge.GetNonceHex())
}

func (c *WendyLiteClient) SyncTimeChallenge(ctx context.Context, nonceHex string) (time.Time, error) {
	return relayRoughtime(ctx, nonceHex, roughtime.QueryServerWithNonce, func(req *pb.WendyComCommand) (*pb.WendyComResponse, error) {
		req.RequestId = c.requestIdGen.Add(1)
		return c.sendCommand(req, 5*time.Second)
	})
}

func relayRoughtime(ctx context.Context, nonceHex string,
    query func(context.Context, roughtime.Server, []byte) (roughtime.Result, error),
    send func(*pb.WendyComCommand) (*pb.WendyComResponse, error)) (time.Time, error) {
	nonce, err := hex.DecodeString(nonceHex)
	if err != nil || len(nonce) != 32 {
		return time.Time{}, fmt.Errorf("invalid device Roughtime nonce")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	type answer struct {
		index  int
		result roughtime.Result
		err    error
	}
	replies := make(chan answer, len(roughtime.Servers))
	for i, server := range roughtime.Servers {
		go func(i int, server roughtime.Server) {
			result, err := query(ctx, server, nonce)
			replies <- answer{i, result, err}
		}(i, server)
	}
	var failures []string
	for range roughtime.Servers {
		select {
		case <-ctx.Done():
			return time.Time{}, fmt.Errorf("Roughtime sync: %w (%s)", ctx.Err(), strings.Join(failures, "; "))
		case a := <-replies:
			if a.err != nil {
				failures = append(failures, a.err.Error())
				continue
			}
			reply, err := send(&pb.WendyComCommand{
				Params:    &pb.WendyComCommand_SyncTime{SyncTime: &pb.WendyComSyncTimeParams{ServerIndex: uint32(a.index), Response: a.result.RawResponse}},
			})
			if err != nil {
				return time.Time{}, err
			}
			if err := resultToError(reply.Result); err != nil {
				return time.Time{}, fmt.Errorf("device rejected Roughtime proof: %w", err)
			}
			if reply.GetSyncTime() == nil {
				return time.Time{}, fmt.Errorf("device returned no time-sync result")
			}
			if reply.GetSyncTime().GetSynchronized() {
				return time.Unix(reply.GetSyncTime().GetUnixSeconds(), 0), nil
			}
		}
	}
	return time.Time{}, fmt.Errorf("device could not establish two-server Roughtime consensus (%s)", strings.Join(failures, "; "))
}
