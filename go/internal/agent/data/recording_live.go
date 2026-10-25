package data

import (
	"errors"
	"fmt"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data/live"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	recordingpb "github.com/wendylabsinc/wendy/go/proto/gen/recordingpb"
	"google.golang.org/protobuf/proto"
)

// Live returns the hub that serves accepted records to live subscribers.
func (m *Manager) Live() *live.Hub { return m.live }

// maxAgentStream bounds an agent producer's stream name, so that
// "agent/<stream>" stays within the 512 bytes a StreamData request may name.
const maxAgentStream = 480

// Publish puts a record of one of the agent's own producers, campaign
// inference for example, on the record stream "agent/<stream>". It takes the
// path an application's recording stream takes, without the socket and the
// journal: live viewers get the record first, then the stream pre-roll and
// every open episode that captures applications record it, as records.wdr.
// What a viewer saw live and what an episode holds are the same record.
//
// stream names the producer and what it publishes, for example
// "campaign/<campaign>/markers/<source>". descriptor names the payload's
// schema and media type. Both are cloned, so the caller may reuse them. The
// record's timing, client boot time and inputs say which sample it describes,
// as on an app's durable record; a record without them is stamped with its
// receipt.
//
// A producer lists its streams in the live catalog with AdvertiseAgentStream
// when it starts and UnadvertiseAgentStream when it stops. Until it does, its
// records are live but neither in the catalog nor kept for a viewer that asks
// for the newest record.
func (m *Manager) Publish(stream string, descriptor *recordingpb.StreamDescriptor, r *recordingpb.Record) error {
	if stream == "" || len(stream) > maxAgentStream {
		return fmt.Errorf("agent stream name must be 1..%d bytes", maxAgentStream)
	}
	if descriptor == nil || r == nil {
		return errors.New("agent record needs a descriptor and a record")
	}
	if proto.Size(r) > MaxRecordingPacket {
		return errors.New("record exceeds 1 MiB")
	}
	if err := ValidateSampleTiming(r.Timing); err != nil {
		return err
	}
	stored, err := stampStreamRecord("", "", stream, proto.Clone(descriptor).(*recordingpb.StreamDescriptor), r)
	if err != nil {
		return err
	}
	m.publish(stored)
	return nil
}

// agentPublisher is the publisher the live catalog names for the agent's own
// streams, where an application stream names its app id.
const agentPublisher = "agent"

// AdvertiseAgentStream lists the agent stream "agent/<stream>" in the live
// catalog, idle until its first record, and makes the hub keep its newest
// record. descriptor gives the schema and media type the catalog shows. A
// producer calls it when it starts publishing the stream, and advertising
// again updates the description.
func (m *Manager) AdvertiseAgentStream(stream string, descriptor *recordingpb.StreamDescriptor) {
	m.live.Advertise(live.StreamInfo{ID: agentStreamID(stream), Schema: descriptor.GetSchema(), MediaType: descriptor.GetMediaType(), Publisher: agentPublisher})
}

// UnadvertiseAgentStream marks the agent stream gone in the live catalog,
// when its producer stops.
func (m *Manager) UnadvertiseAgentStream(stream string) {
	m.live.Unadvertise(agentStreamID(stream))
}

// streamID is the live stream id of a stored record. An application's stream
// is "apps/<app-id>/<service>/<stream>". The agent's own producers have no
// app id, and their streams are "agent/<stream>": RecordStream refuses an
// empty app id, so no app can publish there.
func streamID(s *recordingpb.StoredRecord) string {
	if s.AppId == "" {
		return agentStreamID(s.Stream)
	}
	return appStreamID(s.AppId, s.Service, s.Stream)
}

// agentStreamID is the live stream id of the agent's own stream.
func agentStreamID(stream string) string { return "agent/" + stream }

// appStreamID is the live stream id of an application recording stream. An
// app without a service name has no service segment. App ids, service names
// and stream names cannot contain "/", so the two forms never collide.
func appStreamID(app, service, stream string) string {
	if service == "" {
		return "apps/" + app + "/" + stream
	}
	return "apps/" + app + "/" + service + "/" + stream
}

// AdvertiseAppStream lists an application recording stream in the live
// catalog and makes the hub keep its newest record. The app's recording
// socket calls it when it opens the stream.
func (m *Manager) AdvertiseAppStream(app, service, stream string, cfg appconfig.RecordingStream) {
	m.live.Advertise(live.StreamInfo{ID: appStreamID(app, service, stream), Schema: cfg.Schema, MediaType: cfg.MediaType, Publisher: app})
}

// UnadvertiseAppStream marks the stream gone in the live catalog, when its
// app stops or is removed.
func (m *Manager) UnadvertiseAppStream(app, service, stream string) {
	m.live.Unadvertise(appStreamID(app, service, stream))
}

// liveRecord is the live view of a record publish accepted. The payload
// aliases s, which was cloned from the caller's buffer and never changes
// afterwards.
func liveRecord(s *recordingpb.StoredRecord) *live.Record {
	return &live.Record{
		StreamID:           streamID(s),
		Schema:             s.StreamDescriptor.GetSchema(),
		TimestampUnixNanos: liveTimestamp(s),
		Payload:            s.Record.GetPayload(),
	}
}

// liveTimestamp is the time the record's data describes, on CLOCK_REALTIME,
// the clock of VideoFrame.timestamp_ns. In order of preference:
//
//  1. Sample timing on a UNIX or CLOCK_REALTIME clock: the first sample time.
//  2. Sample timing on CLOCK_BOOTTIME from this boot, or an accepted
//     client_boottime_nanos, converted at the current realtime-boottime
//     offset. That matches video within the microseconds between two clock
//     reads, plus any step the realtime clock took since the sample.
//  3. The agent's receipt time. Lightweight records have nothing else.
//
// A named device clock is not converted: the agent has no mapping for it.
func liveTimestamp(s *recordingpb.StoredRecord) int64 {
	r := s.GetRecord()
	if t := r.GetTiming(); t != nil {
		first, ok := firstSampleNanos(t)
		switch {
		case !ok:
		case t.Clock == "UNIX" || t.Clock == "CLOCK_REALTIME":
			return first
		case t.Clock == "CLOCK_BOOTTIME" && t.BootId == s.ReceiptBootId && s.ReceiptBootId != "unavailable":
			if unix, ok := bootToUnixNanos(first); ok {
				return unix
			}
		}
	}
	if s.ClientTimestampAccepted {
		if unix, ok := bootToUnixNanos(r.GetClientBoottimeNanos()); ok {
			return unix
		}
	}
	return s.ReceiptUnixNanos
}

func firstSampleNanos(t *recordingpb.SampleTiming) (int64, bool) {
	if len(t.TimestampsNanos) > 0 {
		return t.TimestampsNanos[0], true
	}
	if t.StartNanos != nil {
		return *t.StartNanos, true
	}
	return 0, false
}

// bootToUnixNanos converts a CLOCK_BOOTTIME reading of this boot to
// CLOCK_REALTIME. The two clocks are read back to back, so the offset is
// current to within microseconds.
func bootToUnixNanos(boot int64) (int64, bool) {
	now, err := readBootTime()
	if err != nil {
		return 0, false
	}
	return boot + (time.Now().UnixNano() - now), true
}
