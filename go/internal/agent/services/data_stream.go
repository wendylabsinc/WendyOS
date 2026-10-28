package services

import (
	"context"
	"errors"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data/live"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

const (
	// maxStreamDataIDs bounds one request's selection. Apps declare at most 16
	// streams each, so this is far above any real selection.
	maxStreamDataIDs = 256
	// maxStreamDataIDBytes fits the longest app stream id: "apps/", a
	// 253-byte app id, a 57-byte service name and a 24-byte stream name.
	maxStreamDataIDBytes = 512
)

// authorizeDataStream decides who may read live data. registerAllServices puts
// DataService on three listeners, which are not equally trusted:
//
//   - The mTLS TCP server: a user certificate is admitted, on the LAN and
//     through the cloud tunnel alike, and so is a pki-core service account,
//     which reads as a user. A device certificate is refused: no device reads
//     another device's data in this version.
//   - The local admin socket: admitted. Reaching it is already full agent
//     authority; see onLocalAdminSocket.
//   - The pre-provisioning plaintext TCP server: refused. Anyone on the LAN can
//     reach it, with no credential at all.
//
// The interceptors on the mTLS server already enforce the device's tenant.
func authorizeDataStream(ctx context.Context) error {
	if p, ok := peer.FromContext(ctx); ok && onLocalAdminSocket(p) {
		return nil
	}
	_, err := userIdentityFromContext(ctx, "streaming live data")
	return err
}

func liveSelection(req *agentpbv2.StreamDataRequest) (live.Selection, error) {
	// SECURITY: an empty selection gives an admitted operator every app's live
	// stream. That is the device's existing trust boundary, not a widening of it:
	// the same certificate already reads every app's recorded data (Download,
	// ExportRecording), watches every camera (StreamVideo) and opens a root host
	// shell. The agent has no per-app operator authorization, so filtering here
	// alone would protect nothing. Apps are gated separately by data-read.
	ids := req.GetStreamIds()
	if len(ids) > maxStreamDataIDs {
		return live.Selection{}, status.Errorf(codes.InvalidArgument, "at most %d stream ids may be selected", maxStreamDataIDs)
	}
	for _, id := range ids {
		if id == "" || len(id) > maxStreamDataIDBytes {
			return live.Selection{}, status.Errorf(codes.InvalidArgument, "stream ids must be 1..%d bytes", maxStreamDataIDBytes)
		}
	}
	return live.Selection{StreamIDs: ids}, nil
}

// StreamData sends the device's live records: every app recording stream, as
// the agent accepts each record. It runs until the client goes away. A client
// that reads slowly receives the newest record of each stream and skips the
// rest; it never slows a publisher.
func (s *DataService) StreamData(req *agentpbv2.StreamDataRequest, out grpc.ServerStreamingServer[agentpbv2.DataRecord]) error {
	ctx := out.Context()
	if err := authorizeDataStream(ctx); err != nil {
		return err
	}
	selection, err := liveSelection(req)
	if err != nil {
		return err
	}
	sub, err := s.manager.Live().Subscribe(selection)
	if errors.Is(err, live.ErrTooManySubscribers) {
		return status.Error(codes.ResourceExhausted, err.Error())
	}
	if err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	defer sub.Close()
	// Headers tell the client the subscription is in place: every record
	// accepted from now on is offered to it.
	if err := out.SendHeader(metadata.MD{}); err != nil {
		return err
	}
	out = stallGuardedStream{out, streamDataStallTimeout}
	for {
		d, err := sub.Next(ctx)
		if err != nil {
			return status.FromContextError(err).Err()
		}
		r := d.Record
		if err := out.Send(&agentpbv2.DataRecord{StreamId: r.StreamID, TimestampUnixNanos: r.TimestampUnixNanos, Schema: r.Schema, Payload: r.Payload}); err != nil {
			return err
		}
	}
}

// streamDataStallTimeout ends a StreamData whose client takes no record for
// this long. A client that stops reading fills its flow-control window and
// Send blocks, while its connection still answers keepalive pings, so the
// viewer would otherwise hold one of the hub's device-wide slots for as long
// as the connection lives. Send waits only for the previous record to drain,
// so a slow link still gets a minute per record. A variable so tests can
// shorten it.
var streamDataStallTimeout = time.Minute

// stallGuardedStream fails a Send that makes no progress for timeout. Send has
// no deadline of its own, so it runs on its own goroutine; once the handler
// returns, gRPC cancels the stream, which unblocks it. Sends stay sequential:
// the handler never sends again after one fails.
type stallGuardedStream struct {
	grpc.ServerStreamingServer[agentpbv2.DataRecord]
	timeout time.Duration
}

func (s stallGuardedStream) Send(r *agentpbv2.DataRecord) error {
	sent := make(chan error, 1)
	go func() { sent <- s.ServerStreamingServer.Send(r) }()
	timer := time.NewTimer(s.timeout)
	defer timer.Stop()
	select {
	case err := <-sent:
		return err
	case <-timer.C:
		return status.Errorf(codes.DeadlineExceeded, "the client read no live data for %v, so the agent ended the stream", s.timeout)
	}
}
