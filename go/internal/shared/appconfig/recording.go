package appconfig

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"mime"
	"regexp"
	"strings"
)

// RecordingStream describes payloads independently of transport guarantees.
// A durable stream sends recording.proto envelopes; a lightweight stream sends
// raw packets. Schema is a stable identifier, not a URL the agent fetches.
type RecordingStream struct {
	Mode       string               `json:"mode"`
	MediaType  string               `json:"mediaType"`
	Schema     string               `json:"schema,omitempty"`
	Event      string               `json:"event,omitempty"`
	Model      string               `json:"model,omitempty"`
	TimeSeries *RecordingTimeSeries `json:"timeSeries,omitempty"`
}
type RecordingTimeSeries struct {
	Clock          string             `json:"clock"`
	TimestampField string             `json:"timestampField,omitempty"`
	Channels       []RecordingChannel `json:"channels"`
}
type RecordingChannel struct {
	Name string `json:"name"`
	Type string `json:"type"`
	Unit string `json:"unit,omitempty"`
}

var recordingName = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,23}$`)

func ValidateRecordingStreams(streams map[string]RecordingStream) error {
	if len(streams) > 16 {
		return fmt.Errorf("episode-write supports at most 16 streams")
	}
	for name, s := range streams {
		if !recordingName.MatchString(name) || name == "data" {
			return fmt.Errorf("invalid recording stream name %q", name)
		}
		if s.Mode != "lightweight" && s.Mode != "durable" {
			return fmt.Errorf("stream %s: mode must be lightweight or durable", name)
		}
		if len(s.MediaType) > 256 {
			return fmt.Errorf("stream %s: mediaType too long", name)
		}
		mt, _, err := mime.ParseMediaType(s.MediaType)
		if err != nil || !strings.Contains(mt, "/") || strings.Contains(mt, "*") {
			return fmt.Errorf("stream %s: invalid mediaType", name)
		}
		if s.Schema == "wendy.agent.apps.v1.TimeSeriesBatch" && (mt != "application/protobuf" || s.TimeSeries == nil) {
			return fmt.Errorf("stream %s: TimeSeriesBatch needs application/protobuf and timeSeries", name)
		}
		if len(s.Schema) > 1024 || len(s.Event) > 128 || len(s.Model) > 128 {
			return fmt.Errorf("stream %s: descriptor too long", name)
		}
		if s.Event != "" && s.Model != "" {
			return fmt.Errorf("stream %s: choose event or model", name)
		}
		if ts := s.TimeSeries; ts != nil {
			if ts.Clock == "" || len(ts.Clock) > 128 || len(ts.TimestampField) > 128 || len(ts.Channels) == 0 || len(ts.Channels) > 128 {
				return fmt.Errorf("stream %s: timeSeries needs a clock and 1..128 channels", name)
			}
			if s.Mode == "lightweight" && ts.TimestampField == "" && s.Schema != "wendy.agent.apps.v1.TimeSeriesBatch" {
				return fmt.Errorf("stream %s: lightweight timeSeries needs timestampField", name)
			}
			seen := map[string]bool{}
			for _, c := range ts.Channels {
				if c.Name == "" || len(c.Name) > 128 || seen[c.Name] || len(c.Unit) > 64 {
					return fmt.Errorf("stream %s: invalid or duplicate channel", name)
				}
				seen[c.Name] = true
				switch c.Type {
				case "float32", "float64", "int32", "int64", "uint32", "uint64", "bool":
				default:
					return fmt.Errorf("stream %s: invalid channel type %q", name, c.Type)
				}
			}
		}
	}
	return nil
}

// RecordingStreams returns the already-validated episode-write declarations.
func RecordingStreams(entitlements []Entitlement) map[string]RecordingStream {
	out := map[string]RecordingStream{}
	for _, e := range entitlements {
		if e.Type == EntitlementEpisodeWrite {
			for k, v := range e.Streams {
				out[k] = v
			}
		}
	}
	return out
}

// Keep host socket paths below Linux's sockaddr_un limit, even for long service names.
func RecordingServiceDirectory(service string) string {
	h := sha256.Sum256([]byte(service))
	return "s" + hex.EncodeToString(h[:6])
}
