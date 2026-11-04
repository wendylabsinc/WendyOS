//go:build linux

package localmesh

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestNodeFailedServicePublicationPreservesAcceptedIntent(t *testing.T) {
	n := routePublicationNode(t)
	if err := n.Persist(n.routing.Checkpoint()); err != nil {
		t.Fatal(err)
	}
	accepted := []ManifestService{{Mesh: "default", AppID: "com.wendy.test", ServiceID: "temperature", Type: "_sensorlink._tcp", Instance: "Room", Port: 28080}}
	n.nextPublication = time.Now().Add(-time.Millisecond)
	if err := n.handle(nodeEvent{services: &accepted}); err != nil {
		t.Fatal(err)
	}

	// An immediate edit can fail while the node actor keeps serving. Catalog
	// treats that error as rejection and retains its previously accepted set.
	statePath := n.store.path
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, []byte("block writes"), 0600); err != nil {
		t.Fatal(err)
	}
	n.store.path = filepath.Join(blocker, "state.json")
	n.nextPublication = time.Now().Add(-time.Millisecond)
	rejected := cloneServices(accepted)
	rejected[0].Port = 28081
	if err := n.handle(nodeEvent{services: &rejected}); err == nil {
		t.Fatal("service update unexpectedly succeeded")
	}
	if got := n.directory.Snapshot(time.Now())[0].Services; !reflect.DeepEqual(got, accepted) {
		t.Fatalf("failed update changed signed directory: got %+v want %+v", got, accepted)
	}

	// A later ordinary renewal after storage recovers must not publish the
	// rejected edit, which the caller no longer considers active intent.
	n.store.path = statePath
	if err := n.publish(); err != nil {
		t.Fatal(err)
	}
	if got := n.directory.Snapshot(time.Now())[0].Services; !reflect.DeepEqual(got, accepted) {
		t.Fatalf("renewal published rejected service update: got %+v want %+v", got, accepted)
	}
}
