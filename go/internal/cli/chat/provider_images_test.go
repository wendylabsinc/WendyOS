package chat

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

func providerImageFixtures(t *testing.T) []Image {
	t.Helper()
	var pngData, jpegData bytes.Buffer
	picture := image.NewRGBA(image.Rect(0, 0, 2, 2))
	if err := png.Encode(&pngData, picture); err != nil {
		t.Fatal(err)
	}
	if err := jpeg.Encode(&jpegData, picture, nil); err != nil {
		t.Fatal(err)
	}
	return []Image{
		{MIMEType: "image/png", Data: base64.StdEncoding.EncodeToString(pngData.Bytes())},
		{MIMEType: "image/jpeg", Data: base64.StdEncoding.EncodeToString(jpegData.Bytes())},
	}
}

func imageTestProvider(t *testing.T, kind string, handler http.HandlerFunc) Provider {
	t.Helper()
	if kind == "responses" {
		return newResponsesTestProvider(t, handler)
	}
	if kind == "anthropic" {
		t.Setenv("WENDY_CHAT_API_KEY", "test-image-key")
	}
	return newTestProvider(t, kind, handler)
}

func writeImageTestReply(w http.ResponseWriter, kind string) {
	w.Header().Set("Content-Type", "text/event-stream")
	switch kind {
	case "responses":
		responseEvent(w, map[string]any{"type": "response.completed", "response": map[string]any{
			"status": "completed", "output": []any{responseTextItem("msg-image", "I see the camera image.")},
		}})
	case "anthropic":
		responseEvent(w, map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": "I see the camera image."}})
		responseEvent(w, map[string]any{"type": "content_block_stop", "index": 0})
		responseEvent(w, map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn"}})
		responseEvent(w, map[string]any{"type": "message_stop"})
	default:
		responseEvent(w, map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "I see the camera image."}, "finish_reason": "stop"}}})
		io.WriteString(w, "data: [DONE]\n\n")
	}
}

func assertImageWireJSON(t *testing.T, got any, want any) {
	t.Helper()
	// Normalize map/slice element types to compare the serialized contract.
	encoded, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var expected any
	if err := json.Unmarshal(encoded, &expected); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, expected) {
		actual, _ := json.Marshal(got)
		t.Errorf("provider image wire mismatch\n got: %s\nwant: %s", actual, encoded)
	}
}

func TestProviderImagesParallelToolsAndNextUserTurn(t *testing.T) {
	for _, kind := range []string{"local", "ollama", "responses", "anthropic"} {
		t.Run(kind, func(t *testing.T) {
			clearProviderEnv(t)
			images := providerImageFixtures(t)
			cameraCall := map[string]any{"type": "function_call", "id": "fc_camera", "call_id": "camera", "name": "camera_snapshot", "arguments": `{}`, "status": "completed"}
			statusCall := map[string]any{"type": "function_call", "id": "fc_status", "call_id": "status", "name": "wendy_status", "arguments": `{}`, "status": "completed"}
			reasoning := json.RawMessage(`{"type":"reasoning","id":"rs_camera","summary":[],"encrypted_content":"opaque-camera-reasoning"}`)
			assistant := Message{Role: "assistant", ToolCalls: []ToolCall{
				{ID: "camera", Name: "camera_snapshot", Arguments: json.RawMessage(`{}`)},
				{ID: "status", Name: "wendy_status", Arguments: json.RawMessage(`{}`)},
			}}
			// Exercise normalized tool IDs alongside untouched opaque reasoning.
			savedCamera, _ := json.Marshal(cameraCall)
			savedCamera = bytes.ReplaceAll(savedCamera, []byte(`"call_id":"camera"`), []byte(`"call_id":"old_camera"`))
			savedStatus, _ := json.Marshal(statusCall)
			assistant.ResponseItems = []json.RawMessage{reasoning, savedCamera, savedStatus}
			history := []Message{
				{Role: "system", Content: "Develop hardware"},
				{Role: "user", Content: "What can the camera see?"},
				assistant,
				{Role: "tool", ToolCallID: "camera", Content: "Two camera snapshots", Images: images},
				{Role: "tool", ToolCallID: "status", Content: "Device connected"},
			}
			requests := make(chan map[string]any, 2)
			p := imageTestProvider(t, kind, func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				requests <- body
				writeImageTestReply(w, kind)
			})

			var expected []any
			key := "messages"
			switch kind {
			case "local", "ollama":
				parts := []any{map[string]any{"type": "text", "text": `Images returned by tool call "camera" (tool output):`}}
				for _, image := range images {
					parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:" + image.MIMEType + ";base64," + image.Data}})
				}
				expected = []any{
					map[string]any{"role": "system", "content": "Develop hardware"},
					map[string]any{"role": "user", "content": "What can the camera see?"},
					map[string]any{"role": "assistant", "content": "", "tool_calls": []any{
						map[string]any{"id": "camera", "type": "function", "function": map[string]any{"name": "camera_snapshot", "arguments": `{}`}},
						map[string]any{"id": "status", "type": "function", "function": map[string]any{"name": "wendy_status", "arguments": `{}`}},
					}},
					map[string]any{"role": "tool", "tool_call_id": "camera", "content": "Two camera snapshots"},
					map[string]any{"role": "tool", "tool_call_id": "status", "content": "Device connected"},
					map[string]any{"role": "user", "content": parts},
				}
			case "responses":
				key = "input"
				parts := []any{map[string]any{"type": "input_text", "text": "Two camera snapshots"}}
				for _, image := range images {
					parts = append(parts, map[string]any{"type": "input_image", "image_url": "data:" + image.MIMEType + ";base64," + image.Data, "detail": "auto"})
				}
				expected = []any{
					map[string]any{"role": "system", "content": "Develop hardware"},
					map[string]any{"role": "user", "content": "What can the camera see?"},
					reasoning, cameraCall, statusCall,
					map[string]any{"type": "function_call_output", "call_id": "camera", "output": parts},
					map[string]any{"type": "function_call_output", "call_id": "status", "output": "Device connected"},
				}
			case "anthropic":
				parts := []any{map[string]any{"type": "text", "text": "Two camera snapshots"}}
				for _, image := range images {
					parts = append(parts, map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": image.MIMEType, "data": image.Data}})
				}
				expected = []any{
					map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "What can the camera see?"}}},
					map[string]any{"role": "assistant", "content": []any{
						map[string]any{"type": "tool_use", "id": "camera", "name": "camera_snapshot", "input": map[string]any{}},
						map[string]any{"type": "tool_use", "id": "status", "name": "wendy_status", "input": map[string]any{}},
					}},
					map[string]any{"role": "user", "content": []any{
						map[string]any{"type": "tool_result", "tool_use_id": "camera", "content": parts},
						map[string]any{"type": "tool_result", "tool_use_id": "status", "content": "Device connected"},
					}},
				}
			}

			for turn := 0; turn < 2; turn++ {
				reply, err := p.Stream(context.Background(), history, nil, nil)
				if err != nil {
					t.Fatal(err)
				}
				body := <-requests
				assertImageWireJSON(t, body[key], expected)
				if kind == "anthropic" && body["system"] != "Develop hardware" {
					t.Error("system instructions lost")
				}
				if reply.Content != "I see the camera image." {
					t.Fatalf("unexpected assistant reply: %+v", reply)
				}
				history = append(history, reply, Message{Role: "user", Content: "Describe it in more detail"})
				switch kind {
				case "responses":
					expected = append(expected, responseTextItem("msg-image", reply.Content), map[string]any{"role": "user", "content": "Describe it in more detail"})
				case "anthropic":
					expected = append(expected,
						map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": reply.Content}}},
						map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "Describe it in more detail"}}})
				default:
					expected = append(expected, map[string]any{"role": "assistant", "content": reply.Content}, map[string]any{"role": "user", "content": "Describe it in more detail"})
				}
			}
		})
	}
}

func TestProviderImagesRejectionsAreActionableWithoutRetry(t *testing.T) {
	for _, kind := range []string{"local", "responses", "anthropic"} {
		for _, streamError := range []bool{false, true} {
			name := kind + "/http"
			if streamError {
				name = kind + "/stream"
			}
			t.Run(name, func(t *testing.T) {
				clearProviderEnv(t)
				var requests atomic.Int32
				p := imageTestProvider(t, kind, func(w http.ResponseWriter, r *http.Request) {
					requests.Add(1)
					io.Copy(io.Discard, r.Body)
					message := "This model does not support image input"
					if streamError {
						w.Header().Set("Content-Type", "text/event-stream")
						responseEvent(w, map[string]any{"type": "error", "message": message, "error": map[string]any{"message": message}})
					} else {
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusBadRequest)
						json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": message}})
					}
				})
				_, err := p.Stream(context.Background(), []Message{{Role: "user", Content: "Look", Images: providerImageFixtures(t)}}, nil, nil)
				if err == nil || !strings.Contains(err.Error(), "vision-capable") || !strings.Contains(err.Error(), "/setup") || requests.Load() != 1 {
					t.Fatalf("image rejection = %v, requests = %d", err, requests.Load())
				}
			})
		}
	}
}

func TestProviderImagesErrorPayloadRedaction(t *testing.T) {
	for _, kind := range []string{"local", "responses", "anthropic"} {
		for _, bare := range []bool{false, true} {
			name := kind + "/data-url"
			if bare {
				name = kind + "/bare-base64"
			}
			t.Run(name, func(t *testing.T) {
				clearProviderEnv(t)
				images := providerImageFixtures(t)
				p := imageTestProvider(t, kind, func(w http.ResponseWriter, r *http.Request) {
					io.Copy(io.Discard, r.Body)
					payload := images[1].Data
					if !bare {
						payload = "data:image/jpeg;base64," + payload
					}
					// Trigger existing error truncation partway through echoed
					// media, so redaction cannot rely on the complete input.
					payload += strings.Repeat("a", 2048)
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusBadRequest)
					json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "Unsupported image input: " + payload}})
				})
				_, err := p.Stream(context.Background(), []Message{{Role: "user", Content: "Look", Images: images}}, nil, nil)
				if err == nil || !strings.Contains(err.Error(), "[image omitted]") || !strings.Contains(err.Error(), "/setup") || strings.Contains(err.Error(), images[1].Data[:32]) || strings.Contains(err.Error(), "data:image/") || len(err.Error()) > 500 {
					t.Fatalf("image payload leaked into provider error: %v", err)
				}
			})
		}
	}
}

func TestProviderImagesCancellationAndUnsupportedRoles(t *testing.T) {
	clearProviderEnv(t)
	images := providerImageFixtures(t)
	for _, err := range []error{context.Canceled, context.DeadlineExceeded, errors.New("local API returned HTTP 401: Invalid key")} {
		if got := imageRequestError([]Message{{Role: "user", Images: images}}, err); got != err {
			t.Errorf("unrelated failure was converted into an image error: %v", got)
		}
	}
	for _, kind := range []string{"local", "responses", "anthropic"} {
		t.Run(kind, func(t *testing.T) {
			p := imageTestProvider(t, kind, func(w http.ResponseWriter, r *http.Request) {
				t.Error("unsupported image role reached server")
			})
			_, err := p.Stream(context.Background(), []Message{{Role: "system", Content: "test", Images: images}}, nil, nil)
			if err == nil || !strings.Contains(err.Error(), "image attachments are supported") {
				t.Fatalf("unsupported role attachments were discarded: %v", err)
			}
		})
	}
}
