package unitree

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

func lidarFrame(topic string, payload []byte) []byte {
	header, _ := json.Marshal(map[string]any{"type": "msg", "topic": topic, "data": map[string]any{"resolution": 0.05}})
	buf := make([]byte, 12)
	binary.LittleEndian.PutUint16(buf, 2)
	binary.LittleEndian.PutUint32(buf[4:], uint32(len(header)))
	return append(append(buf, header...), payload...)
}

func TestBinaryTopic(t *testing.T) {
	if topic, ok := binaryTopic(lidarFrame("rt/utlidar/voxel_map_compressed", []byte{1, 2, 3})); !ok || topic != "rt/utlidar/voxel_map_compressed" {
		t.Fatalf("lidar framing: %q %v", topic, ok)
	}
	header := []byte(`{"topic":"rt/other"}`)
	normal := make([]byte, 4)
	binary.LittleEndian.PutUint16(normal, uint16(len(header)))
	if topic, ok := binaryTopic(append(append(normal, header...), 9, 9)); !ok || topic != "rt/other" {
		t.Fatalf("normal framing: %q %v", topic, ok)
	}
	for _, bad := range [][]byte{nil, {1}, {2, 0, 0, 0, 255, 255, 255, 255, 0, 0, 0, 0}, {200, 0, 0, 0, '{'}} {
		if _, ok := binaryTopic(bad); ok {
			t.Fatalf("accepted %v", bad)
		}
	}
}

// robotFixture answers like a Go2: validation, topic messages after
// "subscribe", a LiDAR frame after "rt/utlidar/switch" on, and H.264 video
// after "vid" on. It records every control message it receives.
type robotFixture struct {
	mu       sync.Mutex
	received []string
}

func (f *robotFixture) log() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.received...)
}

func TestTopicsLidarAndVideoWithFixturePeer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	robot, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer robot.Close()
	camera, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000}, "video", "go2")
	if err != nil {
		t.Fatal(err)
	}
	frame := []byte{0, 0, 0, 1, 0x65, 0x88, 0x84, 0x21, 0x10}
	fixture := &robotFixture{}
	videoOn := make(chan struct{})
	var videoOnce sync.Once
	robot.OnDataChannel(func(ch *webrtc.DataChannel) {
		ch.OnOpen(func() { _ = ch.SendText(`{"type":"validation","data":"challenge"}`) })
		ch.OnMessage(func(m webrtc.DataChannelMessage) {
			var msg struct {
				Type  string          `json:"type"`
				Topic string          `json:"topic"`
				Data  json.RawMessage `json:"data"`
			}
			if json.Unmarshal(m.Data, &msg) != nil {
				return
			}
			switch msg.Type {
			case "validation":
				_ = ch.SendText(`{"type":"validation","data":"Validation Ok."}`)
				return
			case "heartbeat":
				return
			}
			fixture.mu.Lock()
			fixture.received = append(fixture.received, msg.Type+" "+msg.Topic+" "+string(msg.Data))
			fixture.mu.Unlock()
			switch {
			case msg.Type == "subscribe" && msg.Topic == "rt/lf/lowstate":
				_ = ch.SendText(`{"type":"msg","topic":"rt/lf/lowstate","data":{"bms_state":{"soc":87}}}`)
				_ = ch.SendText(`{"type":"msg","topic":"rt/not_subscribed","data":{}}`)
			case msg.Type == "msg" && msg.Topic == "rt/utlidar/switch" && string(msg.Data) == `"on"`:
				_ = ch.Send(lidarFrame("rt/utlidar/voxel_map_compressed", []byte{7, 7, 7}))
			case msg.Type == "vid" && string(msg.Data) == `"on"`:
				videoOnce.Do(func() { close(videoOn) })
			}
		})
	})
	go func() {
		select {
		case <-videoOn:
		case <-ctx.Done():
			return
		}
		ticker := time.NewTicker(33 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_ = camera.WriteSample(media.Sample{Data: frame, Duration: 33 * time.Millisecond})
			}
		}
	}()
	c, err := connectWithSignaling(ctx, func(ctx context.Context, offer []byte) ([]byte, error) {
		var payload struct {
			SDP string `json:"sdp"`
		}
		if err := json.Unmarshal(offer, &payload); err != nil {
			return nil, err
		}
		if err := robot.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: payload.SDP}); err != nil {
			return nil, err
		}
		if _, err := robot.AddTrack(camera); err != nil {
			return nil, err
		}
		answer, err := robot.CreateAnswer(nil)
		if err != nil {
			return nil, err
		}
		gathered := webrtc.GatheringCompletePromise(robot)
		if err := robot.SetLocalDescription(answer); err != nil {
			return nil, err
		}
		<-gathered
		return json.Marshal(robot.LocalDescription())
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	messages, video, stop, err := c.Subscribe([]string{"rt/lf/lowstate", "rt/utlidar/voxel_map_compressed"}, true)
	if err != nil {
		t.Fatal(err)
	}
	gotText, gotLidar, gotVideo := false, false, false
	for !(gotText && gotLidar && gotVideo) {
		select {
		case <-ctx.Done():
			t.Fatalf("text=%v lidar=%v video=%v; robot saw %v", gotText, gotLidar, gotVideo, fixture.log())
		case m := <-messages:
			switch {
			case m.Topic == "rt/lf/lowstate" && !m.Binary && bytes.Contains(m.Data, []byte(`"soc":87`)):
				gotText = true
			case m.Topic == "rt/utlidar/voxel_map_compressed" && m.Binary && bytes.HasSuffix(m.Data, []byte{7, 7, 7}):
				gotLidar = true
			default:
				t.Fatalf("unexpected message %s %q", m.Topic, m.Data)
			}
		case f := <-video:
			if !bytes.Equal(f.AnnexB, frame) {
				t.Fatalf("video frame %x", f.AnnexB)
			}
			gotVideo = true
		}
	}
	stop()

	want := []string{
		`subscribe rt/lf/lowstate `,
		`subscribe rt/utlidar/voxel_map_compressed `,
		`rtc_inner_req  {"instruction":"on","req_type":"disable_traffic_saving"}`,
		`msg rt/utlidar/switch "on"`,
		`vid  "on"`,
		`unsubscribe rt/lf/lowstate `,
		`unsubscribe rt/utlidar/voxel_map_compressed `,
		`rtc_inner_req  {"instruction":"off","req_type":"disable_traffic_saving"}`,
		`msg rt/utlidar/switch "off"`,
		`vid  "off"`,
	}
	deadline := time.Now().Add(3 * time.Second)
	for len(fixture.log()) < len(want) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := fixture.log(); !sameSet(got, want) {
		t.Fatalf("robot control messages:\n got %q\nwant %q", got, want)
	}
}

// Map iteration order makes subscribe/unsubscribe order vary.
func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]int{}
	for _, s := range a {
		seen[s]++
	}
	for _, s := range b {
		seen[s]--
	}
	for _, n := range seen {
		if n != 0 {
			return false
		}
	}
	return true
}

func TestSubscribeRejectsBadInput(t *testing.T) {
	c := &Connection{done: make(chan struct{})}
	c.connected.Store(true)
	for _, topics := range [][]string{nil, {"lowstate"}, {"rt/../x"}, {"rt/a b"}} {
		if _, _, _, err := c.Subscribe(topics, false); err == nil {
			t.Fatalf("accepted %q", topics)
		}
	}
}
