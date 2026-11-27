package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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
				fmt.Fprintln(w, `{"message":{"thinking":"Private reasoning"},"done":false}`)
				fmt.Fprintln(w, `{"message":{"content":"Thor"},"done":false}`)
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
	if err == nil {
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

func TestNativeQwenAnswerStreamsBeforeCompletionWithoutThinking(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/version":
			fmt.Fprint(w, `{"version":"test"}`)
		case "/api/show":
			fmt.Fprint(w, `{"capabilities":["completion","vision","tools","thinking"]}`)
		case "/api/chat":
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || payload["think"] != false {
				t.Errorf("thinking was not explicitly disabled: %v", err)
			}
			fmt.Fprintln(w, `{"message":{"thinking":"private reasoning"},"done":false}`)
			fmt.Fprintln(w, `{"message":{"content":"The Go2 sees a chair."},"done":false}`)
			w.(http.Flusher).Flush()
			select {
			case <-release:
				fmt.Fprintln(w, `{"done":true,"done_reason":"stop"}`)
			case <-r.Context().Done():
			}
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	provider, err := DeviceProvider("qwen3-vl:4b", deviceTestTransport{base: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	emitted := make(chan string, 2)
	done := make(chan error, 1)
	go func() {
		message, err := provider.Stream(ctx, []Message{{Role: "user", Content: "What does Go2 see?"}}, nil, func(s string) { emitted <- s })
		if message.Content != "The Go2 sees a chair." {
			t.Errorf("incorrect visible history: %q", message.Content)
		}
		done <- err
	}()
	select {
	case text := <-emitted:
		if text != "The Go2 sees a chair." {
			t.Fatalf("thinking channel leaked: %q", text)
		}
	case <-time.After(time.Second):
		cancel()
		<-done
		t.Fatal("ordinary native content was buffered until completion")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestNativeOllamaRejectsEmptyAndExhaustedCompletions(t *testing.T) {
	for _, tc := range []struct {
		name, stream string
		wantError    bool
	}{
		{"empty", `{"done":true,"done_reason":"stop"}`, true},
		{"thinking-only", `{"message":{"thinking":"private reasoning"},"done":true,"done_reason":"stop"}`, true},
		{"thinking-length", `{"message":{"thinking":"private reasoning"},"done":true,"done_reason":"length"}`, true},
		{"answer-length", `{"message":{"content":"Partial answer"},"done":true,"done_reason":"length"}`, true},
		{"tool-length", `{"message":{"tool_calls":[{"function":{"name":"robot_status","arguments":{}}}]},"done":true,"done_reason":"length"}`, true},
		{"complete-tool", `{"message":{"tool_calls":[{"function":{"name":"robot_status","arguments":{}}}]},"done":true,"done_reason":"stop"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/version":
					fmt.Fprint(w, `{"version":"test"}`)
				case "/api/show":
					fmt.Fprint(w, `{"capabilities":["completion","tools","thinking"]}`)
				case "/api/chat":
					fmt.Fprintln(w, tc.stream)
				}
			}))
			defer server.Close()
			provider, err := DeviceProviderWithLimit("qwen3-vl:4b", deviceTestTransport{base: server.URL}, 512)
			if err != nil {
				t.Fatal(err)
			}
			var visible strings.Builder
			message, err := provider.Stream(context.Background(), nil, nil, func(s string) { visible.WriteString(s) })
			if tc.wantError {
				if err == nil {
					t.Fatalf("accepted incomplete result: %+v", message)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(visible.String()+message.Content, "private reasoning") {
				t.Fatal("thinking output leaked into answer or history")
			}
		})
	}
}
