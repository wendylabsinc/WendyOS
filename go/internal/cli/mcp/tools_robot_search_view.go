package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image/jpeg"
	"io"
	"math"
	"net"
	"net/http"
	"time"

	"github.com/google/uuid"
)

// The backend owns search motion. MCP supplies at most one freshly captured
// frame for the opaque view token in this receipt, never coordinates or moves.
func provideRobotSearchView(ctx context.Context, serial, requestID string, receipt map[string]any,
	capture func(context.Context, string) (string, cameraSnapshot, int64, error),
	dial func(context.Context) (net.Conn, error),
) (map[string]any, error) {
	return provideRobotCameraView(ctx, serial, requestID, receipt, "object_search", "/v1/robot/find-view", capture, dial)
}

func provideRobotCameraView(ctx context.Context, serial, requestID string, receipt map[string]any, field, path string,
	capture func(context.Context, string) (string, cameraSnapshot, int64, error),
	dial func(context.Context) (net.Conn, error),
) (map[string]any, error) {
	search, ok := receipt[field].(map[string]any)
	if receipt["state"] != "running" || !ok {
		return receipt, nil
	}
	viewID, _ := search["view_id"].(string)
	if viewID == "" {
		return receipt, nil
	}
	if _, err := uuid.Parse(viewID); err != nil || len(requestID) < 8 || len(requestID) > 80 || serial == "" {
		return nil, errors.New("invalid robot camera frame demand identity")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	captureCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	selected, shot, cutoff, err := capture(captureCtx, serial)
	cancel()
	if err != nil {
		return nil, fmt.Errorf("fresh Go2 capture: %w", err)
	}
	if selected != serial {
		return nil, errors.New("camera target changed during robot search")
	}
	if cutoff <= 0 || shot.timestampNS > math.MaxInt64 || shot.timestampNS <= uint64(cutoff) {
		return nil, errors.New("Go2 camera frame is not newer than the gateway clock cutoff")
	}
	if len(shot.jpeg) == 0 || len(shot.jpeg) > 2<<20 {
		return nil, errors.New("Go2 camera frame exceeds bounded JPEG size")
	}
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(shot.jpeg))
	if err != nil || shot.width != 1280 || shot.height != 720 || cfg.Width != 1280 || cfg.Height != 720 {
		return nil, errors.New("robot search requires native 1280x720 Go2 JPEG")
	}
	data, err := json.Marshal(map[string]any{
		"serial": serial, "request_id": requestID, "view_id": viewID,
		"image": shot.jpeg, "gateway_received_unix_nanos": int64(shot.timestampNS),
		"camera_mode": "go2-front-native-1280x720",
	})
	if err != nil {
		return nil, err
	}
	postCtx, done := context.WithTimeout(ctx, 5*time.Second)
	defer done()
	transport := &http.Transport{DisableKeepAlives: true, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return dial(ctx) }}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error {
		return errors.New("robot camera frame redirect refused")
	}}
	request, err := http.NewRequestWithContext(postCtx, http.MethodPost, "http://digital-twin"+path, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return nil, fmt.Errorf("robot camera frame rejected (%d): %s", response.StatusCode, bytes.TrimSpace(message))
	}
	return decodeRobotReceipt(response.Body)
}
