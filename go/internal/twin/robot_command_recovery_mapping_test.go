package twin

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/twinrobot"
	"github.com/wendylabsinc/wendy/go/internal/twinrobot/sitenav"
)

type recoveryMappingRobot struct {
	*setupTestRobot
	mappingMu         sync.Mutex
	frame             string
	captured, release chan struct{}
	retired           bool
}

func (r *recoveryMappingRobot) MappingSample() (string, uint64, []sitenav.Vertex) {
	r.mappingMu.Lock()
	retired := r.retired
	r.mappingMu.Unlock()
	if retired {
		return "", 0, nil
	}
	close(r.captured)
	<-r.release
	return r.frame, 2, []sitenav.Vertex{{X: 10, Y: 1, Z: 20}}
}
func (r *recoveryMappingRobot) Retire() {
	r.mappingMu.Lock()
	r.retired = true
	r.mappingMu.Unlock()
	r.Close()
}
func TestRobotRecoveryDrainsCapturedMappingBeforePublishingReplacement(t *testing.T) {
	store, _ := NewStore(t.TempDir())
	room := mappingResetRoom(t, store, []string{"go2:go2"})
	frame := room.Site.ID + "/" + room.Site.ModelRevision
	old := &recoveryMappingRobot{setupTestRobot: &setupTestRobot{state: twinrobot.SiteRobotSnapshot{Disconnected: true}}, frame: frame, captured: make(chan struct{}), release: make(chan struct{})}
	previous := &robotSession{robot: old, serial: "go2"}
	server := &Server{Store: store, Context: context.Background(), observers: map[string]*robotSession{"go2": previous}, robots: map[string]*robotSession{"old-desktop": previous}}
	mapped := make(chan struct{})
	go func() { server.contributeMapping(previous, []Room{room}); close(mapped) }()
	<-old.captured
	opened := make(chan struct{})
	done := make(chan error, 1)
	replacement := &setupTestRobot{state: twinrobot.SiteRobotSnapshot{MeasurementsReady: true}}
	go func() {
		_, _, err := server.ensureCommandRobot(context.Background(), "go2", func(context.Context, string) (robotControl, error) { close(opened); return replacement, nil })
		done <- err
	}()
	waitSetupCondition(t, func() bool {
		previous.commandMu.Lock()
		defer previous.commandMu.Unlock()
		return previous.commandRetired
	})
	select {
	case <-opened:
		close(old.release)
		<-mapped
		t.Fatal("replacement opened before old captured mapping drained")
	case <-time.After(20 * time.Millisecond):
	}
	close(old.release)
	<-mapped
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	after, err := store.State(room.Site.ID)
	if err != nil || after.Sources["go2:go2"] != 2 {
		t.Fatalf("old sample did not finish before replacement: %+v %v", after.Sources, err)
	}
	if server.observers["go2"].robot != replacement || server.robots["old-desktop"] != nil {
		t.Fatal("replacement or desktop handle retirement failed")
	}
	// Even a mapping tick that captured the old session pointer before publication
	// must now observe retirement and contribute nothing.
	revision := after.Revision
	server.contributeMapping(previous, []Room{room})
	after, _ = store.State(room.Site.ID)
	if after.Revision != revision {
		t.Fatal("a retired observer contributed another old sample")
	}
}
