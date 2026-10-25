package services

import (
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/agent/inference"
	"github.com/wendylabsinc/wendy/go/proto/gen/datapb"
	recordingpb "github.com/wendylabsinc/wendy/go/proto/gen/recordingpb"
	"google.golang.org/protobuf/proto"
)

// markersDescriptor describes an inference markers stream.
var markersDescriptor = &recordingpb.StreamDescriptor{MediaType: "application/protobuf", Schema: string(proto.MessageName(&datapb.VideoMarkers{}))}

// campaignMarkersStream is the record stream, below "agent/", that carries a
// campaign's detections on one camera source.
func campaignMarkersStream(campaign, sourceID string) string {
	return "campaign/" + campaign + "/markers/" + sourceID
}

// advertiseMarkers lists the source's markers stream in the live catalog and
// returns the function that marks it gone. The job's stream goroutine holds
// it for as long as it serves the source, reconnects included.
func (j *campaignInferenceJob) advertiseMarkers(sourceID string) func() {
	stream := campaignMarkersStream(j.campaign.Name, sourceID)
	j.owner.service.manager.AdvertiseAgentStream(stream, markersDescriptor)
	return func() { j.owner.service.manager.UnadvertiseAgentStream(stream) }
}

// publishDetections puts one scored frame's boxes on the record stream
// "agent/campaign/<campaign>/markers/<source>", as wendy.data.v1.VideoMarkers.
//
// It runs for every scored frame, before anything decides what to record, so
// a viewer sees the boxes whether or not an episode records them. A frame
// with no detections publishes no markers, which clears the camera's boxes.
// The record names the camera sample the frame came in as its input, and that
// sample's receipt stamps as its time: VideoFrame.timestamp_ns live, and the
// camera index's boot time in an episode. So the boxes line up with the video.
func (j *campaignInferenceJob) publishDetections(result inference.Result, detections []inference.Detection) {
	if result.Width <= 0 || result.Height <= 0 {
		return // boxes in pixels of a picture of unknown size cannot be placed
	}
	markers := &datapb.VideoMarkers{
		Camera:        markersCamera(result.SourceID),
		ValidForNanos: int64(markersValidFor(j.campaign.Inference.Rate)),
		Markers:       detectionMarkers(detections, result.Width, result.Height),
	}
	payload, err := proto.Marshal(markers)
	if err != nil {
		j.owner.service.manager.Warnf("campaign %q markers: %v", j.campaign.Name, err)
		return
	}
	record := &recordingpb.Record{Payload: payload}
	if s := result.Sample; s != nil {
		record.Inputs = []*recordingpb.SampleRef{{SourceId: result.SourceID, SampleId: s.ID}}
		if s.UnixNanos > 0 {
			record.Timing = &recordingpb.SampleTiming{Clock: "CLOCK_REALTIME", Count: 1, TimestampsNanos: []int64{s.UnixNanos}}
		}
		if s.BootNanos > 0 {
			record.ClientBoottimeNanos, record.BootId = proto.Int64(s.BootNanos), data.BootID()
		}
	}
	if err := j.owner.service.manager.Publish(campaignMarkersStream(j.campaign.Name, result.SourceID), markersDescriptor, record); err != nil {
		j.owner.service.manager.Warnf("campaign %q markers: %v", j.campaign.Name, err)
	}
}

// markersValidFor is how long a viewer keeps showing a frame's boxes without a
// newer frame: three scoring intervals and at least two seconds, so the boxes
// outlast a late frame but not a stopped model.
func markersValidFor(rate float64) time.Duration {
	return max(3*time.Duration(float64(time.Second)/rate), 2*time.Second)
}

// detectionMarkers turns boxes in pixels of a width by height picture into
// normalised box markers, clamped to the picture. A box with no area inside
// the picture is dropped. The value is the score in percent.
func detectionMarkers(detections []inference.Detection, width, height int) []*datapb.Marker {
	w, h := float64(width), float64(height)
	clamp := func(v, hi float64) float64 { return min(max(v, 0), hi) }
	out := make([]*datapb.Marker, 0, len(detections))
	for _, d := range detections {
		x1, y1 := clamp(d.Box[0], w), clamp(d.Box[1], h)
		x2, y2 := clamp(d.Box[2], w), clamp(d.Box[3], h)
		if x2 <= x1 || y2 <= y1 {
			continue
		}
		out = append(out, &datapb.Marker{
			Kind:   datapb.Marker_KIND_BOX,
			X:      float32((x1 + x2) / 2 / w),
			Y:      float32((y1 + y2) / 2 / h),
			Width:  float32((x2 - x1) / w),
			Height: float32((y2 - y1) / h),
			Label:  d.Label,
			Value:  proto.Float64(d.Score * 100),
			Unit:   "%",
		})
	}
	return out
}

// markersCamera names a camera source the way the video service lists the
// camera to viewers: a V4L2 camera by its device path, a network camera by its
// device id.
func markersCamera(sourceID string) string {
	if path, ok := strings.CutPrefix(sourceID, "v4l2:"); ok {
		return path
	}
	if id, ok := strings.CutPrefix(sourceID, "ipcamera:"); ok {
		return id
	}
	return ""
}

// inferenceSample is the camera sample identity the model worker carries back
// with the prediction for a picture.
func inferenceSample(s SensorSample) *inference.Sample {
	return &inference.Sample{ID: s.SampleID, UnixNanos: s.UnixNanos, BootNanos: s.BootNanos}
}
