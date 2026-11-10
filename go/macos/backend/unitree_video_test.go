package backend

import (
	"github.com/wendylabsinc/wendy/go/internal/shared/unitree"
	"testing"
	"time"
)

type fakeUnitreeCamera struct {
	*fakeUnitreeControl
	frames                  chan unitree.VideoFrame
	subscriptions, canceled int
}

func (f *fakeUnitreeCamera) Subscribe(_ []string, video bool) (<-chan unitree.TopicMessage, <-chan unitree.VideoFrame, func(), error) {
	f.subscriptions++
	return nil, f.frames, func() { f.canceled++ }, nil
}
func TestUnitreeCameraLifecycle(t *testing.T) {
	_, motion := newUnitreeControlTest()
	camera := &fakeUnitreeCamera{fakeUnitreeControl: motion, frames: make(chan unitree.VideoFrame, 1)}
	c := &UnitreeController{handle: 1, link: camera}
	if c.StartVideo(2) == nil {
		t.Fatal("accepted stale handle")
	}
	if err := c.StartVideo(1); err != nil {
		t.Fatal(err)
	}
	if err := c.StartVideo(1); err != nil {
		t.Fatal(err)
	}
	if camera.subscriptions != 1 {
		t.Fatal("duplicate subscription")
	}
	camera.frames <- unitree.VideoFrame{AnnexB: []byte{0, 0, 1, 0x65}}
	data, err := c.NextVideo(1)
	if err != nil || len(data) != 4 {
		t.Fatalf("frame=%v error=%v", data, err)
	}
	c.StopVideo(2)
	if camera.canceled != 0 {
		t.Fatal("stale view stopped camera")
	}
	c.Close(1)
	if camera.canceled != 1 {
		t.Fatal("closing did not unsubscribe")
	}
	if _, err := c.NextVideo(1); err == nil {
		t.Fatal("read after disconnect")
	}
}
func TestUnitreeCameraWaitDoesNotBlockStop(t *testing.T) {
	_, motion := newUnitreeControlTest()
	camera := &fakeUnitreeCamera{fakeUnitreeControl: motion, frames: make(chan unitree.VideoFrame)}
	c := &UnitreeController{handle: 1, link: camera}
	if err := c.StartVideo(1); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { c.NextVideo(1); close(done) }()
	stopped := make(chan struct{})
	go func() { c.Move(1, 0, 0, 0); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("camera blocked Stop")
	}
	<-done
	c.Close(1)
}
