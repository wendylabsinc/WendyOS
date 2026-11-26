package chat

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func ollamaSceneFixture(t testing.TB, width, height int, transparent bool) Image {
	t.Helper()
	picture := image.NewRGBA(image.Rect(0, 0, width, height))
	if !transparent {
		for y := 0; y < height; y++ {
			for x := 0; x < width; x++ {
				pixel := color.RGBA{R: 240, A: 255}
				if x >= width/2 {
					pixel = color.RGBA{G: 240, A: 255}
				}
				picture.SetRGBA(x, y, pixel)
			}
		}
	}
	var encoded bytes.Buffer
	mime := "image/jpeg"
	var err error
	if transparent {
		mime = "image/png"
		err = png.Encode(&encoded, picture)
	} else {
		err = jpeg.Encode(&encoded, picture, &jpeg.Options{Quality: 90})
	}
	if err != nil {
		t.Fatal(err)
	}
	return Image{MIMEType: mime, Data: base64.StdEncoding.EncodeToString(encoded.Bytes())}
}

func TestOllamaSceneImageDimensionsAndOriginal(t *testing.T) {
	for _, tc := range []struct{ width, height, wantWidth, wantHeight int }{
		{1280, 720, 640, 360}, {720, 1280, 360, 640}, {1000, 1000, 640, 640}, {1920, 1080, 640, 360}, {800, 333, 640, 266}, {320, 180, 320, 180},
	} {
		t.Run(fmt.Sprintf("%dx%d", tc.width, tc.height), func(t *testing.T) {
			attachment := ollamaSceneFixture(t, tc.width, tc.height, false)
			original := attachment
			data, err := ollamaSceneImage(context.Background(), attachment)
			if err != nil {
				t.Fatal(err)
			}
			if attachment != original {
				t.Fatal("mutated original image")
			}
			encoded, err := base64.StdEncoding.DecodeString(data)
			if err != nil {
				t.Fatal(err)
			}
			picture, err := jpeg.Decode(bytes.NewReader(encoded))
			if err != nil {
				t.Fatal(err)
			}
			if picture.Bounds().Dx() != tc.wantWidth || picture.Bounds().Dy() != tc.wantHeight {
				t.Fatalf("wrong decoded bounds: %v", picture.Bounds())
			}
			r, g, b, _ := picture.At(tc.wantWidth/4, tc.wantHeight/2).RGBA()
			if r < 200*257 || g > 30*257 || b > 30*257 {
				t.Fatalf("left scene changed: RGB=%d,%d,%d", r/257, g/257, b/257)
			}
			if tc.width <= 640 && tc.height <= 640 && data != attachment.Data {
				t.Fatal("small image was needlessly re-encoded")
			}
		})
	}
}

func TestOllamaSceneImageValidatesBeforeResize(t *testing.T) {
	valid := ollamaSceneFixture(t, 2, 2, false)
	oversizedPNG, _ := base64.StdEncoding.DecodeString(ollamaSceneFixture(t, 2, 2, true).Data)
	// Change only IHDR dimensions and checksum. The pixel cap must reject the
	// dimensions before any attempt to allocate/decode the claimed image.
	binary.BigEndian.PutUint32(oversizedPNG[16:20], 4096)
	binary.BigEndian.PutUint32(oversizedPNG[20:24], 4096)
	binary.BigEndian.PutUint32(oversizedPNG[29:33], crc32.ChecksumIEEE(oversizedPNG[12:29]))
	for _, bad := range []Image{
		{MIMEType: "image/jpeg", Data: "invalid-base64"},
		{MIMEType: "image/png", Data: valid.Data},
		{MIMEType: "image/jpeg", Data: base64.StdEncoding.EncodeToString([]byte("not an image"))},
		{MIMEType: "image/jpeg", Data: strings.Repeat("A", base64.StdEncoding.EncodedLen(maxImageBytes)+1)},
		{MIMEType: "image/png", Data: base64.StdEncoding.EncodeToString(oversizedPNG)},
	} {
		if _, err := ollamaSceneImage(context.Background(), bad); err == nil {
			t.Fatal("accepted malformed or oversized image")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ollamaSceneImage(ctx, valid); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	transparent := ollamaSceneFixture(t, 800, 800, true)
	data, err := ollamaSceneImage(context.Background(), transparent)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := base64.StdEncoding.DecodeString(data)
	decoded, err := jpeg.Decode(bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	r, g, b, _ := decoded.At(320, 320).RGBA()
	if r < 250*257 || g < 250*257 || b < 250*257 {
		t.Fatalf("transparent image did not flatten onto white: %d,%d,%d", r, g, b)
	}
}

func TestNativeOllamaResizesWireImageOnlyAndKeepsTools(t *testing.T) {
	attachment := ollamaSceneFixture(t, 1280, 720, false)
	history := []Message{
		{Role: "system", Content: "Inspect the robot"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "camera", Name: "robot_camera_snapshot", Arguments: json.RawMessage(`{"serial":"test"}`)}}},
		{Role: "tool", ToolCallID: "camera", Content: "Original snapshot metadata: 1280x720", Images: []Image{attachment}},
		{Role: "user", Content: "What does the camera see?"},
	}
	original, _ := json.Marshal(history)
	tools := []Tool{{Name: "robot_status", Description: "Read robot status", Parameters: json.RawMessage(`{"type":"object","properties":{}}`)}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/version":
			fmt.Fprint(w, `{"version":"test"}`)
		case "/api/show":
			fmt.Fprint(w, `{"capabilities":["completion","tools","vision"]}`)
		case "/api/chat":
			var payload struct {
				KeepAlive string         `json:"keep_alive"`
				Options   map[string]int `json:"options"`
				Tools     []struct {
					Function struct {
						Name, Description string
						Parameters        json.RawMessage
					}
				} `json:"tools"`
				Messages []struct {
					Role, Content string
					ToolName      string `json:"tool_name"`
					Images        []string
					ToolCalls     []struct {
						Function struct {
							Name      string
							Arguments json.RawMessage
						}
					} `json:"tool_calls"`
				} `json:"messages"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Error(err)
			}
			if payload.KeepAlive != "1h" || payload.Options["num_ctx"] != 32768 || len(payload.Messages) != 4 || len(payload.Tools) != 1 {
				t.Errorf("wrong native request settings: %+v", payload)
				return
			}
			if payload.Messages[0].Content != history[0].Content || payload.Messages[3].Content != history[3].Content || payload.Messages[2].Content != history[2].Content || payload.Messages[2].ToolName != "robot_camera_snapshot" || payload.Messages[1].ToolCalls[0].Function.Name != "robot_camera_snapshot" || string(payload.Messages[1].ToolCalls[0].Function.Arguments) != `{"serial":"test"}` || payload.Tools[0].Function.Name != tools[0].Name || payload.Tools[0].Function.Description != tools[0].Description {
				t.Error("non-image content or tool information changed")
			}
			data, err := base64.StdEncoding.DecodeString(payload.Messages[2].Images[0])
			if err != nil {
				t.Error(err)
			}
			picture, err := jpeg.Decode(bytes.NewReader(data))
			if err != nil {
				t.Error(err)
				return
			}
			if picture.Bounds().Dx() != 640 || picture.Bounds().Dy() != 360 {
				t.Errorf("wire image bounds %v", picture.Bounds())
			}
			fmt.Fprintln(w, `{"message":{"content":"I see the scene."},"done":true}`)
		default:
			t.Errorf("unexpected native request %s", r.URL.Path)
		}
	}))
	defer server.Close()
	provider, err := DeviceProvider("local-vlm", deviceTestTransport{base: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Stream(context.Background(), history, tools, nil); err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(history)
	if !bytes.Equal(original, after) {
		t.Fatal("provider resized or changed engine history")
	}
}

func TestCompatibleDeviceProviderKeepsOriginalImage(t *testing.T) {
	attachment := ollamaSceneFixture(t, 1280, 720, false)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/version" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected fallback request %s", r.URL.Path)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		encoded, _ := json.Marshal(body)
		if !bytes.Contains(encoded, []byte(attachment.Data)) {
			t.Error("compatible provider image changed")
		}
		writeImageTestReply(w, "local")
	}))
	defer server.Close()
	provider, err := DeviceProvider("local-vlm", deviceTestTransport{base: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Stream(context.Background(), []Message{{Role: "user", Content: "Describe image", Images: []Image{attachment}}}, nil, nil); err != nil {
		t.Fatal(err)
	}
}

func BenchmarkOllamaSceneImage720p(b *testing.B) {
	attachment := ollamaSceneFixture(b, 1280, 720, false)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := ollamaSceneImage(context.Background(), attachment); err != nil {
			b.Fatal(err)
		}
	}
}
