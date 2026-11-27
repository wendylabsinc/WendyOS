package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/jpeg"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

func searchViewFixture(t *testing.T) (map[string]any, cameraSnapshot) {
	t.Helper()
	var data bytes.Buffer
	if err := jpeg.Encode(&data, image.NewGray(image.Rect(0, 0, 1280, 720)), nil); err != nil {
		t.Fatal(err)
	}
	return map[string]any{"state": "running", "object_search": map[string]any{"view_id": uuid.NewString()}}, cameraSnapshot{jpeg: data.Bytes(), width: 1280, height: 720, timestampNS: 101}
}

func TestSearchViewHandshakeBoundToReceiptAndOneFreshFrame(t *testing.T) {
	receipt, shot := searchViewFixture(t)
	captures, posts := 0, 0
	var captureCtx context.Context
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts++
		if r.URL.Path != "/v1/robot/find-view" || r.Method != http.MethodPost {
			t.Errorf("unexpected path: %s %s", r.Method, r.URL.Path)
		}
		var body struct {
			Serial    string `json:"serial"`
			RequestID string `json:"request_id"`
			ViewID    string `json:"view_id"`
			Image     []byte `json:"image"`
			Mode      string `json:"camera_mode"`
			Received  int64  `json:"gateway_received_unix_nanos"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.Serial != "robot-a" || body.RequestID != "search-once" || body.ViewID != receipt["object_search"].(map[string]any)["view_id"] || body.Mode != "go2-front-native-1280x720" || body.Received != 101 || !bytes.Equal(body.Image, shot.jpeg) {
			t.Error("frame target, identity, camera bytes or receipt provenance changed")
		}
		// Even if another view is demanded, this call must not loop/capture it.
		_ = json.NewEncoder(w).Encode(map[string]any{"state": "running", "object_search": map[string]any{"view_id": uuid.NewString()}})
	}))
	defer endpoint.Close()
	dial := func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", endpoint.Listener.Addr().String())
	}
	out, err := provideRobotSearchView(context.Background(), "robot-a", "search-once", receipt, func(ctx context.Context, serial string) (string, cameraSnapshot, int64, error) {
		captures++
		captureCtx = ctx
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 15*time.Second {
			t.Fatal("unbounded capture")
		}
		return serial, shot, 100, nil
	}, dial)
	if err != nil || out["state"] != "running" || captures != 1 || posts != 1 || captureCtx.Err() != context.Canceled {
		t.Fatalf("unbounded or failed handshake: %v", err)
	}
}

func TestSearchViewRejectsUnboundOrInvalidFrames(t *testing.T) {
	for _, kind := range []string{"target", "stale", "zero-cutoff", "overflow", "wrong-mode", "invalid-jpeg", "oversize", "capture-error", "canceled", "invalid-token"} {
		t.Run(kind, func(t *testing.T) {
			receipt, shot := searchViewFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if kind == "canceled" {
				cancel()
			}
			if kind == "invalid-token" {
				receipt["object_search"].(map[string]any)["view_id"] = "invented"
			}
			captures := 0
			_, err := provideRobotSearchView(ctx, "robot-a", "search-once", receipt, func(context.Context, string) (string, cameraSnapshot, int64, error) {
				captures++
				serial, cutoff := "robot-a", int64(100)
				switch kind {
				case "target":
					serial = "robot-b"
				case "stale":
					shot.timestampNS = 100
				case "zero-cutoff":
					cutoff = 0
				case "overflow":
					shot.timestampNS = math.MaxUint64
				case "wrong-mode":
					shot.width = 640
				case "invalid-jpeg":
					shot.jpeg = []byte("invalid")
				case "oversize":
					shot.jpeg = make([]byte, 2<<20+1)
				case "capture-error":
					return serial, shot, cutoff, errors.New("decoder failed")
				}
				return serial, shot, cutoff, nil
			}, func(context.Context) (net.Conn, error) {
				t.Error("invalid capture reached HTTP")
				return nil, errors.New("unexpected")
			})
			if err == nil {
				t.Fatal("invalid evidence accepted")
			}
			if (kind == "canceled" || kind == "invalid-token") && captures != 0 {
				t.Fatal("capture after cancellation/invalid demand")
			}
		})
	}
}

func TestSearchViewSkipsUnrequestedCameraAndRefusesRedirect(t *testing.T) {
	for _, receipt := range []map[string]any{{"state": "completed", "object_search": map[string]any{"view_id": uuid.NewString()}}, {"state": "running"}, {"state": "running", "object_search": map[string]any{}}} {
		_, err := provideRobotSearchView(context.Background(), "robot-a", "search-once", receipt, func(context.Context, string) (string, cameraSnapshot, int64, error) {
			t.Fatal("unsolicited capture")
			return "", cameraSnapshot{}, 0, nil
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
	}
	receipt, shot := searchViewFixture(t)
	calls := 0
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.Redirect(w, r, "https://example.invalid/leak", http.StatusTemporaryRedirect)
	}))
	defer endpoint.Close()
	_, err := provideRobotSearchView(context.Background(), "robot-a", "search-once", receipt, func(context.Context, string) (string, cameraSnapshot, int64, error) { return "robot-a", shot, 100, nil }, func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", endpoint.Listener.Addr().String())
	})
	if err == nil || calls != 1 {
		t.Fatal("followed frame redirect")
	}
}

func TestActiveSearchCaptureFailureDoesNotClaimNotStarted(t *testing.T) {
	receipt, _ := searchViewFixture(t)
	var sent map[string]any
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&sent)
		_ = json.NewEncoder(w).Encode(receipt)
	}))
	defer endpoint.Close()
	s := New(&config.Config{}, nil)
	s.SetConn(&grpcclient.AgentConnection{})
	s.SetRobotDialer(func(ctx context.Context, _ *grpcclient.AgentConnection) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", endpoint.Listener.Addr().String())
	})
	for _, lookup := range []bool{false, true} {
		result, err := s.handleRobotTool(context.Background(), callToolReq("robot_command", map[string]any{"action": "find_active", "serial": "robot-a", "request_id": "search-once", "query": "blue chair", "model": "vision", "receipt_only": lookup}))
		if err != nil || !result.IsError {
			t.Fatal("missing camera accepted", err)
		}
		if structuredMap(t, result)["command_not_started"] == true {
			t.Fatal("capture failure erased dispatched search")
		}
		if result.Meta.AdditionalFields["robot_serial"] != "robot-a" || sent["query"] != "blue chair" || sent["model"] != "vision" {
			t.Fatal("search identity/parameters lost")
		}
		want := "find_active"
		if lookup {
			want = "receipt"
		}
		if sent["action"] != want {
			t.Fatal("receipt retry reexecuted motion")
		}
	}
}
