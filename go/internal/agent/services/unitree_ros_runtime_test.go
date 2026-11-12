//go:build linux

package services

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/wendylabsinc/wendy/go/internal/shared/unitree"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
)

// The dedicated runner provides ROS Humble in an isolated, unprivileged
// container. This is a protocol robot, not a physical posture/firmware test.
func TestGo2BridgeROSDeadlines(t *testing.T) {
	if os.Getenv("WENDY_GO2_BRIDGE_ROS") != "1" {
		t.Skip("requires the dedicated ROS image")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 80*time.Second)
	defer cancel()
	robot, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer robot.Close()
	var mu sync.Mutex
	events := []map[string]any{}
	epoch := time.Now()
	channelReady := make(chan *webrtc.DataChannel, 1)
	helloDelay, stopDelay := 1200*time.Millisecond, 0*time.Millisecond
	var pending sync.WaitGroup
	closing := false
	robot.OnDataChannel(func(ch *webrtc.DataChannel) {
		ch.OnOpen(func() {
			_ = ch.SendText(`{"type":"validation","data":"fixture"}`)
			channelReady <- ch
		})
		ch.OnMessage(func(m webrtc.DataChannelMessage) {
			var wire struct {
				Type  string `json:"type"`
				Topic string `json:"topic"`
				Data  struct {
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
				} `json:"data"`
			}
			// Validation data is a string, unlike request data.
			var envelope struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(m.Data, &envelope) != nil {
				return
			}
			if envelope.Type == "validation" {
				_ = ch.SendText(`{"type":"validation","data":"Validation Ok."}`)
				return
			}
			if json.Unmarshal(m.Data, &wire) != nil || (wire.Type != "req" && wire.Type != "msg") || wire.Topic != "rt/api/sport/request" {
				return
			}
			id, api := wire.Data.Header.Identity.ID, wire.Data.Header.Identity.API
			mu.Lock()
			if closing {
				mu.Unlock()
				return
			}
			if api != 1008 {
				pending.Add(1)
			}
			delay := stopDelay
			if api == 1016 {
				delay = helloDelay
			}
			event := map[string]any{"api": api, "id": id, "at": time.Since(epoch).Seconds(), "parameter": wire.Data.Parameter, "noreply": wire.Data.Header.Policy.NoReply, "type": wire.Type}
			events = append(events, event)
			mu.Unlock()
			if api == 1008 {
				return
			} // Firmware does not acknowledge no-reply velocity.
			go func() {
				defer pending.Done()
				select {
				case <-ctx.Done():
					return
				case <-time.After(delay):
				}
				if err := ch.SendText(fmt.Sprintf(`{"type":"res","topic":"rt/api/sport/response","data":{"header":{"identity":{"id":%d,"api_id":%d},"status":{"code":0}}}}`, id, api)); err == nil {
					mu.Lock()
					event["ack_at"] = time.Since(epoch).Seconds()
					mu.Unlock()
				}
			}()
		})
	})
	// Production Connect uses the real legacy local signaling endpoint. The
	// dedicated container owns this port; no LAN/device address is accepted.
	listener, err := net.Listen("tcp", "127.0.0.1:8081")
	if err != nil {
		t.Fatal(err)
	}
	signalServer := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/offer" || r.Method != "POST" {
			http.NotFound(w, r)
			return
		}
		var offer struct {
			SDP   string `json:"sdp"`
			Type  string `json:"type"`
			Token string `json:"token"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&offer); err != nil || offer.Type != "offer" || offer.Token != "" {
			http.Error(w, "invalid offer", 400)
			return
		}
		if err := robot.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offer.SDP}); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		answer, err := robot.CreateAnswer(nil)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		gathered := webrtc.GatheringCompletePromise(robot)
		if err = robot.SetLocalDescription(answer); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-gathered:
		}
		_ = json.NewEncoder(w).Encode(robot.LocalDescription())
	})}
	go func() { _ = signalServer.Serve(listener) }()
	defer signalServer.Close()
	connection, err := unitree.Connect(ctx, "127.0.0.1", "", "")
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	dataDone := make(chan struct{})
	go func() {
		defer close(dataDone)
		var channel *webrtc.DataChannel
		select {
		case channel = <-channelReady:
		case <-ctx.Done():
			return
		}
		ticker := time.NewTicker(40 * time.Millisecond)
		defer ticker.Stop()
		sequence := 0
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				sequence++
				_ = channel.SendText(fmt.Sprintf(`{"type":"msg","topic":"rt/lf/sportmodestate","data":{"imu_state":{"quaternion":[1,0,0,0]},"position":[%f,2,0.3],"velocity":[0.1,0,0],"yaw_speed":0}}`, float64(sequence)/100))
			}
		}
	}()
	defer func() { cancel(); <-dataDone }()
	pairing := testUnitreeService(t)
	pairing.sessions["GO2TEST1"] = &unitreeSession{link: connection, cancel: func() {}, done: make(chan struct{})}
	// Close the fixture session's completion channel before pairing.Close waits.
	close(pairing.sessions["GO2TEST1"].done)
	service := NewUnitreeRobotService(pairing) // Real 200ms command/500ms lease limits.
	defer func() {
		service.mu.Lock()
		for _, timer := range service.timers {
			timer.Stop()
		}
		service.mu.Unlock()
	}()
	socket := filepath.Join(t.TempDir(), "agent.sock")
	unix, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	pb.RegisterWendyUnitreeRobotServiceServer(server, service)
	go func() { _ = server.Serve(unix) }()
	defer server.Stop()
	defer unix.Close()
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/events":
			_ = json.NewEncoder(w).Encode(events)
		case "/policy":
			var p struct {
				HelloMS int `json:"hello_ms"`
				StopMS  int `json:"stop_ms"`
			}
			if r.Method != "POST" || json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&p) != nil || p.HelloMS < 0 || p.HelloMS > 4000 || p.StopMS < 0 || p.StopMS > 4000 {
				http.Error(w, "bad fixture policy", 400)
				return
			}
			helloDelay = time.Duration(p.HelloMS) * time.Millisecond
			stopDelay = time.Duration(p.StopMS) * time.Millisecond
			w.WriteHeader(204)
		default:
			http.NotFound(w, r)
		}
	}))
	defer control.Close()
	check := exec.CommandContext(ctx, "python3", os.Getenv("WENDY_GO2_BRIDGE_TEST"))
	check.Env = append(os.Environ(), "WENDY_AGENT_SOCKET="+socket, "GO2_SERIAL=GO2TEST1", "GO2_FIXTURE_CONTROL="+control.URL)
	check.Stdout = os.Stdout
	check.Stderr = os.Stderr
	if err := check.Run(); err != nil {
		t.Errorf("ROS transport journey failed: %v", err)
	}
	// Close the real channel before joining delayed acknowledgements. No worker
	// can enqueue another acknowledgement once the robot has closed.
	mu.Lock()
	closing = true
	mu.Unlock()
	_ = robot.Close()
	cancel()
	pending.Wait()
}
