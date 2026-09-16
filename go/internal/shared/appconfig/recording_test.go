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

func TestRecordingStorageValidation(t *testing.T) {
	for _, tc := range []struct {
		quota, retention int64
		mode             string
		valid            bool
	}{
		{1 << 30, 0, "durable", true}, {1 << 20, 3600, "durable", true}, {0, 86400, "durable", true},
		{1 << 20, 0, "lightweight", false}, {-1, 0, "durable", false}, {1, 0, "durable", false},
		{1 << 41, 0, "durable", false}, {1 << 20, -1, "durable", false}, {1 << 20, 31536001, "durable", false},
	} {
		cfg := RecordingStream{Mode: tc.mode, MediaType: "application/octet-stream", Storage: &RecordingStorage{MaxBytes: tc.quota, RetentionSeconds: &tc.retention}}
		err := ValidateRecordingStreams(map[string]RecordingStream{"samples": cfg})
		if (err == nil) != tc.valid {
			t.Fatalf("%+v: %v", tc, err)
		}
	}
}
