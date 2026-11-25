package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDeviceOllamaPreservesToolsAndResults(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/version":
			fmt.Fprint(w, `{"version":"test"}`)
		case "/api/show":
			fmt.Fprint(w, `{"capabilities":["completion","tools","thinking"]}`)
		case "/api/chat":
			requests++
			var body struct {
				Think    bool           `json:"think"`
				Options  map[string]int `json:"options"`
				Tools    []any          `json:"tools"`
				Messages []struct {
					Role     string `json:"role"`
					ToolName string `json:"tool_name"`
					Content  string `json:"content"`
				} `json:"messages"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if body.Think || body.Options["num_ctx"] != 32768 || len(body.Tools) != 1 {
				t.Errorf("wrong request: %+v", body)
			}
			if requests == 1 {
				fmt.Fprintln(w, `{"message":{"tool_calls":[{"function":{"name":"device_info","arguments":{}}}]},"done":true}`)
			} else {
				last := body.Messages[len(body.Messages)-1]
				if last.Role != "tool" || last.ToolName != "device_info" || last.Content != "Thor" {
					t.Errorf("lost tool result: %+v", last)
				}
				fmt.Fprintln(w, `{"message":{"content":"Private reasoning</think>Thor"},"done":false}`)
				fmt.Fprintln(w, `{"done":true}`)
			}
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()
	transport := deviceTestTransport{base: server.URL}
	p, err := DeviceProvider("qwen3", transport)
	if err != nil {
		t.Fatal(err)
	}
	tools := []Tool{{Name: "device_info", Parameters: json.RawMessage(`{"type":"object","properties":{}}`)}}
	first, err := p.Stream(context.Background(), []Message{{Role: "user", Content: "Inspect device"}}, tools, nil)
	if err != nil || len(first.ToolCalls) != 1 {
		t.Fatalf("tool call: %+v, %v", first, err)
	}
	first.ToolCalls[0].ID = "call-1"
	var emitted string
	final, err := p.Stream(context.Background(), []Message{first, {Role: "tool", ToolCallID: "call-1", Content: "Thor"}}, tools, func(s string) { emitted += s })
	if err != nil || final.Content != "Thor" || emitted != "Thor" {
		t.Fatalf("response: %+v, %v", final, err)
	}
}

type deviceTestTransport struct{ base string }

func (t deviceTestTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	copy := r.Clone(r.Context())
	u := *r.URL
	copy.URL = &u
	copy.URL.Host = strings.TrimPrefix(t.base, "http://")
	return http.DefaultTransport.RoundTrip(copy)
}

func TestDeviceOllamaRejectsNonToolModel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/version":
			fmt.Fprint(w, `{"version":"test"}`)
		case "/api/show":
			fmt.Fprint(w, `{"capabilities":["completion"]}`)
		default:
			t.Errorf("generation should not start: %s", r.URL.Path)
		}
	}))
	defer server.Close()
	p, _ := DeviceProvider("plain", deviceTestTransport{base: server.URL})
	_, err := p.Stream(context.Background(), nil, []Tool{{Name: "device_info"}}, nil)
	if err == nil || !strings.Contains(err.Error(), "does not support function tools") {
		t.Fatalf("error: %v", err)
	}
}

func TestDeviceModelPickerExcludesCalibrationFiles(t *testing.T) {
	for _, name := range []string{"imatrix.gguf", "mmproj.gguf", "mtp-model.gguf", "dflash-model.gguf"} {
		if chatModelName("hf.co/example/model:"+name, "local") {
			t.Errorf("offered %s", name)
		}
	}
	if !chatModelName("hf.co/Qwen/Qwen3-4B-GGUF:Qwen3-4B-Q4_K_M.gguf", "local") {
		t.Fatal("filtered real model")
	}
}
