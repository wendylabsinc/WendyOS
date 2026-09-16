package containerd

import (
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"testing"
)

type streamAwareDataProvider struct {
	recordingDataProvider
	app, service string
	streams      map[string]appconfig.RecordingStream
}

func (p *streamAwareDataProvider) EnsureStreams(app, service string, streams map[string]appconfig.RecordingStream) (string, error) {
	p.app = app
	p.service = service
	p.streams = streams
	return "/streams", nil
}
func TestEnsureDataSocketsCarriesServiceDeclarations(t *testing.T) {
	e := []appconfig.Entitlement{{Type: appconfig.EntitlementEpisodeWrite, Streams: map[string]appconfig.RecordingStream{"samples": {Mode: "durable", MediaType: "text/csv"}}}}
	p := new(streamAwareDataProvider)
	if _, err := ensureDataSockets(p, "test.app", "worker", e); err != nil {
		t.Fatal(err)
	}
	if p.app != "test.app" || p.service != "worker" || p.streams["samples"].Mode != "durable" {
		t.Fatal("stream configuration lost")
	}
	if _, err := ensureDataSockets(new(recordingDataProvider), "test.app", "worker", e); err == nil {
		t.Fatal("unsupported stream grant silently downgraded")
	}
}
