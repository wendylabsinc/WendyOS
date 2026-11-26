package mcp

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRobotFindUploadsCameraReceiptAndUsesBoundTransport(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/robot/find-object" {
			t.Error(r.URL.Path)
		}
		var body struct {
			Serial, Query, Model, CameraMode string
			Image                            []byte
			Received                         int64 `json:"gateway_received_unix_nanos"`
		}
		// Inspect exact serialized provenance field separately.
		var raw map[string]json.RawMessage
		json.NewDecoder(r.Body).Decode(&raw)
		data, _ := json.Marshal(raw)
		json.Unmarshal(data, &body)
		var mode string
		json.Unmarshal(raw["camera_mode"], &mode)
		if body.Serial != "robot" || body.Query != "blue chair" || string(body.Image) != "jpeg" || body.Received != 123 || mode != "go2-front-native-1280x720" {
			t.Fatalf("%+v mode%s", body, mode)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"seen":true,"navigable":false,"message":"visible only"}`))
	}))
	defer srv.Close()
	calls := 0
	dial := func(ctx context.Context) (net.Conn, error) {
		calls++
		return (&net.Dialer{}).DialContext(ctx, "tcp", srv.Listener.Addr().String())
	}
	result, err := requestRobotFind(context.Background(), "robot", "blue chair", "vision", cameraSnapshot{jpeg: []byte("jpeg"), timestampNS: 123}, dial)
	if err != nil || result["seen"] != true || calls != 1 {
		t.Fatalf("%+v %v calls%d", result, err, calls)
	}
	if _, err := requestRobotFind(context.Background(), "robot", "chair", "", cameraSnapshot{}, dial); err == nil || calls != 1 {
		t.Fatal("sent unknown receipt")
	}
}
