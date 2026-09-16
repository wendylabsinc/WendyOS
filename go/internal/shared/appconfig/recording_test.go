package appconfig

import "testing"

func TestRecordingStreamsValidation(t *testing.T) {
	for _, media := range []string{"text/csv", "application/json", "application/protobuf", "application/cbor", "image/png", "image/jpeg", "application/octet-stream", "application/vnd.apache.arrow.stream", "application/vnd.example.sensor"} {
		if err := ValidateRecordingStreams(map[string]RecordingStream{"samples": {Mode: "lightweight", MediaType: media}}); err != nil {
			t.Fatal(media, err)
		}
	}
	for _, tc := range []struct {
		name   string
		stream RecordingStream
	}{
		{"../escape", RecordingStream{Mode: "durable", MediaType: "text/plain"}},
		{"data", RecordingStream{Mode: "durable", MediaType: "text/plain"}},
		{"samples", RecordingStream{Mode: "unknown", MediaType: "text/plain"}},
		{"samples", RecordingStream{Mode: "durable", MediaType: "text/*"}},
		{"samples", RecordingStream{Mode: "durable", MediaType: "garbage"}},
		{"samples", RecordingStream{Mode: "durable", MediaType: "text/plain", Event: "ready", Model: "detector"}},
		{"samples", RecordingStream{Mode: "lightweight", MediaType: "text/csv", TimeSeries: &RecordingTimeSeries{Clock: "UNIX", Channels: []RecordingChannel{{Name: "x", Type: "float64"}}}}},
	} {
		if err := ValidateRecordingStreams(map[string]RecordingStream{tc.name: tc.stream}); err == nil {
			t.Fatalf("accepted %+v", tc)
		}
	}
}
