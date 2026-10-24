package unitree

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/pion/webrtc/v4"
	"math"
	"testing"
	"time"
)

func TestTeleopVelocityLimits(t *testing.T) {
	for _, value := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), .801, -.801} {
		if ValidMotion(value, 0, 0) {
			t.Errorf("accepted forward %v", value)
		}
	}
	for _, value := range []float64{math.NaN(), math.Inf(1), 1.001, -1.001} {
		if ValidMotion(0, 0, value) {
			t.Errorf("accepted yaw %v", value)
		}
	}
	if ValidMotion(0, .501, 0) || !ValidMotion(-.8, .5, 1) {
		t.Fatal("rejected bounded velocity")
	}
}
func TestTeleopAcknowledgementMatchesIDAPIAndStatus(t *testing.T) {
	response := make(chan int, 1)
	c := &Connection{pending: map[uint64]pendingSportRequest{12: {api: 1008, response: response}}}
	for _, raw := range []string{
		`{"type":"res","topic":"rt/api/sport/request","data":{"header":{"identity":{"id":13,"api_id":1008},"status":{"code":0}}}}`,
		`{"type":"res","topic":"rt/api/sport/request","data":{"header":{"identity":{"id":12,"api_id":1003},"status":{"code":0}}}}`,
		`{"type":"res","topic":"rt/api/sport/request","data":{"header":{"identity":{"id":12,"api_id":1008}}}}`,
	} {
		c.motionResponse([]byte(raw))
		select {
		case <-response:
			t.Fatal("accepted unrelated/missing acknowledgement")
		default:
		}
	}
	// Reply topic is not assumed to echo the request topic; identity/API are.
	c.motionResponse([]byte(`{"type":"res","topic":"rt/api/sport/response","data":{"header":{"identity":{"id":12,"api_id":1008},"status":{"code":0}}}}`))
	select {
	case <-response:
	default:
		t.Fatal("lost matching acknowledgement")
	}
}
func TestTeleopWireCommandsWithFixturePeer(t *testing.T) {
	robot, err := loopbackTeleopAPI().NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer robot.Close()
	commands := make(chan uint64, 10)
	robot.OnDataChannel(func(ch *webrtc.DataChannel) {
		ch.OnOpen(func() { _ = ch.SendText(`{"type":"validation","data":"fixture"}`) })
		ch.OnMessage(func(m webrtc.DataChannelMessage) {
			var msg struct {
				Type  string          `json:"type"`
				Topic string          `json:"topic"`
				Data  json.RawMessage `json:"data"`
			}
			if json.Unmarshal(m.Data, &msg) != nil {
				return
			}
			if msg.Type == "validation" {
				_ = ch.SendText(`{"type":"validation","data":"Validation Ok."}`)
				return
			}
			if msg.Type != "req" {
				return
			}
			var d struct {
				Header struct {
					Identity struct {
						ID  uint64 `json:"id"`
						API uint64 `json:"api_id"`
					} `json:"identity"`
					Policy struct {
						NoReply bool `json:"noreply"`
					} `json:"policy"`
				} `json:"header"`
				Parameter string `json:"parameter"`
			}
			if json.Unmarshal(msg.Data, &d) != nil {
				return
			}
			if msg.Topic != sportTopic {
				t.Error("wrong topic")
			}
			if d.Header.Identity.API == 1008 && d.Parameter != `{"x":0.1,"y":-0.05,"z":0.2}` {
				t.Errorf("unexpected parameter %s", d.Parameter)
			}
			commands <- d.Header.Identity.API
			if d.Header.Identity.API == 1008 {
				if !d.Header.Policy.NoReply {
					t.Error("velocity update must request no reply")
				}
				return // Match firmware that never acknowledges velocity updates.
			}
			if d.Header.Policy.NoReply {
				t.Error("Stop must still request acknowledgement")
			}
			_ = ch.SendText(fmt.Sprintf(`{"type":"res","topic":%q,"data":{"header":{"identity":{"id":%d,"api_id":%d},"status":{"code":0}}}}`, sportTopic, d.Header.Identity.ID, d.Header.Identity.API))
		})
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := loopbackTeleopAPI().NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	c, err := connectWithPeer(ctx, client, func(ctx context.Context, offer []byte) ([]byte, error) { return answerOffer(t, ctx, robot, offer) })
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// Validation can finish before SCTP acknowledges the validation response.
	// Wait for those bytes to drain before exercising Move's backlog guard.
	drained := make(chan struct{}, 1)
	c.channel.OnBufferedAmountLow(func() {
		select {
		case drained <- struct{}{}:
		default:
		}
	})
	for c.channel.BufferedAmount() != 0 {
		select {
		case <-drained:
		case <-ctx.Done():
			t.Fatal("validation response did not drain from command channel")
		}
	}
	if err = c.Move(ctx, .1, -.05, .2); err != nil {
		t.Fatal(err)
	}
	c.responseMu.Lock()
	pending := len(c.pending)
	c.responseMu.Unlock()
	if pending != 0 {
		t.Fatal("velocity update left an acknowledgement waiter")
	}
	if err = c.Move(ctx, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	for _, want := range []uint64{1008, 1003} {
		select {
		case got := <-commands:
			if got != want {
				t.Fatalf("api=%d want %d", got, want)
			}
		case <-ctx.Done():
			t.Fatal("no wire command")
		}
	}
	if err = c.Move(ctx, math.NaN(), 0, 0); err == nil {
		t.Fatal("accepted nonfinite Move")
	}
	select {
	case <-commands:
		t.Fatal("invalid Move sent command")
	default:
	}
}

func TestSlowPostureAcknowledgementDoesNotHoldUpStop(t *testing.T) {
	helloReceived := make(chan struct{}, 1)
	robot, err := loopbackTeleopAPI().NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer robot.Close()
	robot.OnDataChannel(func(ch *webrtc.DataChannel) {
		ch.OnOpen(func() { _ = ch.SendText(`{"type":"validation","data":"fixture"}`) })
		ch.OnMessage(func(m webrtc.DataChannelMessage) {
			var msg struct {
				Type string          `json:"type"`
				Data json.RawMessage `json:"data"`
			}
			if json.Unmarshal(m.Data, &msg) != nil {
				return
			}
			if msg.Type == "validation" {
				_ = ch.SendText(`{"type":"validation","data":"Validation Ok."}`)
				return
			}
			var d struct {
				Header struct {
					Identity struct {
						ID  uint64 `json:"id"`
						API uint64 `json:"api_id"`
					} `json:"identity"`
				} `json:"header"`
			}
			if msg.Type != "req" || json.Unmarshal(msg.Data, &d) != nil {
				return
			}
			id, api := d.Header.Identity.ID, d.Header.Identity.API
			reply := func() {
				_ = ch.SendText(fmt.Sprintf(`{"type":"res","topic":%q,"data":{"header":{"identity":{"id":%d,"api_id":%d},"status":{"code":0}}}}`, sportTopic, id, api))
			}
			if api == 1016 {
				// Like the real Go2, acknowledge Hello only after waving for a while.
				helloReceived <- struct{}{}
				time.AfterFunc(600*time.Millisecond, reply)
				return
			}
			reply()
		})
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := loopbackTeleopAPI().NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	c, err := connectWithPeer(ctx, client, func(ctx context.Context, offer []byte) ([]byte, error) { return answerOffer(t, ctx, robot, offer) })
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	hello := make(chan error, 1)
	go func() { hello <- c.Hello(ctx) }()
	select {
	case <-helloReceived:
	case <-ctx.Done():
		t.Fatal("fixture did not receive Hello")
	}
	start := time.Now()
	if err := c.Stop(ctx); err != nil {
		t.Fatalf("Stop during Hello: %v", err)
	}
	if waited := time.Since(start); waited > 300*time.Millisecond {
		t.Fatalf("Stop waited %v behind Hello", waited)
	}
	if err := <-hello; err != nil {
		t.Fatalf("Hello acknowledged after 600 ms failed: %v", err)
	}
}

// Keep fixture traffic on loopback, independent of LAN permissions and VPNs.
func loopbackTeleopAPI() *webrtc.API {
	var settings webrtc.SettingEngine
	settings.SetIncludeLoopbackCandidate(true)
	settings.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	settings.SetInterfaceFilter(func(name string) bool { return name == "lo0" || name == "lo" })
	return webrtc.NewAPI(webrtc.WithSettingEngine(settings))
}
