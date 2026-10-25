package services

import (
	"context"
	"errors"

	"github.com/wendylabsinc/wendy/go/internal/agent/data/live"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

const (
	// maxStreamDataIDs bounds each list of one request's selection: ids,
	// prefixes and schemas. Apps declare at most 16 streams each, so this is
	// far above any real selection.
	maxStreamDataIDs = 256
	// maxStreamDataIDBytes fits the longest app stream id: "apps/", a
	// 253-byte app id, a 57-byte service name and a 24-byte stream name. It
	// bounds prefixes and schemas too.
	maxStreamDataIDBytes = 512
	// defaultStreamDataRateHz is the per-stream rate cap of a request that sets
	// none. It is a display rate: a viewer that wants more asks for it.
	defaultStreamDataRateHz = 30
	// minStreamDataRateHz and maxStreamDataRateHz bound max_rate_hz. Apps may
	// write at most 200 records per second in total, so the top of the range
	// is no cap at all.
	minStreamDataRateHz = 0.01
	maxStreamDataRateHz = 1000
)

// authorizeDataStream decides who may read live data through DataService.
// registerAllServices puts DataService on three listeners, which are not
// equally trusted:
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
// Every caller admitted here reads every stream. An app without admin reads
// through its System API socket instead, where the data-read entitlement and
// its allowlist decide; see systemDataService. That socket is a unix socket
// too, so it must never serve this handler: onLocalAdminSocket would take it
// for the admin socket.
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
	for _, list := range []struct {
		name   string
		values []string
	}{{"stream ids", req.GetStreamIds()}, {"stream prefixes", req.GetStreamPrefixes()}, {"schemas", req.GetSchemas()}} {
		if len(list.values) > maxStreamDataIDs {
			return live.Selection{}, status.Errorf(codes.InvalidArgument, "at most %d %s may be selected", maxStreamDataIDs, list.name)
		}
		for _, v := range list.values {
			if v == "" || len(v) > maxStreamDataIDBytes {
				return live.Selection{}, status.Errorf(codes.InvalidArgument, "%s must be 1..%d bytes", list.name, maxStreamDataIDBytes)
			}
		}
	}
	rate := req.GetMaxRateHz()
	if rate == 0 {
		rate = defaultStreamDataRateHz
	} else if !(rate >= minStreamDataRateHz && rate <= maxStreamDataRateHz) {
		return live.Selection{}, status.Errorf(codes.InvalidArgument, "max_rate_hz must be 0 for the default of %d, or %g to %d", defaultStreamDataRateHz, minStreamDataRateHz, maxStreamDataRateHz)
	}
	return live.Selection{
		StreamIDs:      req.GetStreamIds(),
		StreamPrefixes: req.GetStreamPrefixes(),
		Schemas:        req.GetSchemas(),
		MaxRateHz:      float64(rate),
		SendLatest:     req.GetSendLatest(),
	}, nil
}

// StreamData sends the device's live records: every app recording stream and
// the agent's own streams, as the agent accepts each record, and the catalog
// of those streams. It runs until the client goes away. A client that reads
// slowly receives the newest record of each stream and skips the rest; it
// never slows a publisher.
func (s *DataService) StreamData(req *agentpbv2.StreamDataRequest, out grpc.ServerStreamingServer[agentpbv2.DataRecord]) error {
	if err := authorizeDataStream(out.Context()); err != nil {
		return err
	}
	return streamLiveData(s.manager.Live(), req, nil, out)
}

// streamLiveData serves one StreamData call from hub. allow is the caller's
// read scope (see live.Permits); empty reads every stream. Naming a stream
// outside it is refused rather than left silent, so an app learns that its
// allowlist, not the publisher, is why nothing arrives.
func streamLiveData(hub *live.Hub, req *agentpbv2.StreamDataRequest, allow []string, out grpc.ServerStreamingServer[agentpbv2.DataRecord]) error {
	ctx := out.Context()
	selection, err := liveSelection(req)
	if err != nil {
		return err
	}
	for _, id := range selection.StreamIDs {
		if !live.Permits(allow, id) {
			return status.Errorf(codes.PermissionDenied, "stream %q is outside this app's data-read allowlist", id)
		}
	}
	selection.Allow = allow
	sub, err := hub.Subscribe(selection)
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
	for {
		d, err := sub.Next(ctx)
		if err == nil {
			// Next hands out a waiting record before it looks at ctx, so a
			// slow reader that always has one waiting would never see ctx
			// end, and a narrowed app stream would keep sending.
			err = ctx.Err()
		}
		if err != nil {
			return status.FromContextError(err).Err()
		}
		r := d.Record
		if err := out.Send(&agentpbv2.DataRecord{StreamId: r.StreamID, TimestampUnixNanos: r.TimestampUnixNanos, Schema: r.Schema, Payload: r.Payload, Skipped: d.Skipped, SourceId: r.SourceID, SampleId: r.SampleID}); err != nil {
			return err
		}
	}
}
