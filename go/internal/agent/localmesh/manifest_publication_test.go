package localmesh

import (
	"testing"
	"time"
)

func TestCombinedManifestImmediateAndCoalescedPublication(t *testing.T) {
	n := routePublicationNode(t)
	if err := n.Persist(n.routing.Checkpoint()); err != nil {
		t.Fatal(err)
	}
	first := n.advertisement
	quiet := n.publicationDeadline()
	if remaining := time.Until(quiet); remaining < 4*time.Minute+50*time.Second || remaining > 5*time.Minute {
		t.Fatal("quiet refresh is not five minutes after publication", remaining)
	}
	service := ManifestService{Mesh: "default", AppID: "com.wendy.test", ServiceID: "temperature", Type: "_sensorlink._tcp", Instance: "Room", Port: 28080}
	// The earlier router publication consumes this second's one publication.
	for _, port := range []uint16{28080, 28081, 28082} {
		service.Port = port
		services := []ManifestService{service}
		if err := n.handle(nodeEvent{services: &services}); err != nil {
			t.Fatal(err)
		}
	}
	if n.state.Revision != first.Revision || !n.publicationDirty {
		t.Fatal("burst signed more than once or lost dirty intent")
	}
	if len(n.directory.Snapshot(time.Now())[0].Services) != 0 {
		t.Fatal("unsigned draft leaked into roster")
	}
	// Route edits in the same burst share the same durable publication gate.
	checkpoint := *n.state.Babel
	checkpoint.Seqno++
	if err := n.Persist(checkpoint); err != nil {
		t.Fatal(err)
	}
	if n.state.Revision != first.Revision || n.state.Babel.Seqno != checkpoint.Seqno || n.advertisement.Route.Seqno != first.Route.Seqno {
		t.Fatal("coalesced route change published early or lost its durable checkpoint")
	}
	// Advance the monotonic gate without sleeping or altering signed wall time.
	n.nextPublication = time.Now().Add(-time.Millisecond)
	if err := n.publish(); err != nil {
		t.Fatal(err)
	}
	latest := n.directory.Snapshot(time.Now())[0]
	if !n.publicationDeadline().After(quiet) {
		t.Fatal("an edit did not push back the quiet refresh")
	}
	if latest.Revision != first.Revision+1 || latest.Route.Seqno != checkpoint.Seqno || len(latest.Services) != 1 || latest.Services[0].Port != 28082 || n.publicationDirty {
		t.Fatalf("coalesced publication = %+v", latest)
	}
	if latest.Expires < first.Expires || latest.Expires-latest.Issued != MaxLease.Milliseconds() {
		t.Fatal("whole manifest lease was not renewed")
	}
	// After an idle interval the next edit is immediately signed; no debounce.
	n.nextPublication = time.Now().Add(-time.Millisecond)
	services := []ManifestService{}
	if err := n.handle(nodeEvent{services: &services}); err != nil {
		t.Fatal(err)
	}
	removed := n.directory.Snapshot(time.Now())[0]
	if removed.Revision != latest.Revision+1 || len(removed.Services) != 0 {
		t.Fatal("idle removal delayed or omitted service remained")
	}
}
