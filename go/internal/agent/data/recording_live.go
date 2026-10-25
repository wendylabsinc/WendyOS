package data

import (
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data/live"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	recordingpb "github.com/wendylabsinc/wendy/go/proto/gen/recordingpb"
)

// Live returns the hub that serves accepted records to live subscribers.
func (m *Manager) Live() *live.Hub { return m.live }

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
// catalog and makes the hub keep its newest record. The app's data socket
// manager calls it as the app's task starts.
func (m *Manager) AdvertiseAppStream(app, service, stream string, cfg appconfig.RecordingStream) {
	m.live.Advertise(live.StreamInfo{ID: appStreamID(app, service, stream), Schema: cfg.Schema, MediaType: cfg.MediaType, Publisher: app})
}

// UnadvertiseAppStream marks the stream gone in the live catalog, when its
// app stops or is removed.
func (m *Manager) UnadvertiseAppStream(app, service, stream string) {
	m.live.Unadvertise(appStreamID(app, service, stream))
}

// liveRecord is the live view of a record RecordStream accepted. The payload
// aliases s, which RecordStream cloned from the caller's buffer and never
// changes afterwards.
func liveRecord(s *recordingpb.StoredRecord) *live.Record {
	return &live.Record{
		StreamID:           appStreamID(s.AppId, s.Service, s.Stream),
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
