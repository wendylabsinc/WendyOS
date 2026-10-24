package backend

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/chat"
)

func TestDeviceChatUsesSelectedTransportAndNoEnvironmentCredentials(t *testing.T) {
	t.Setenv("WENDY_CHAT_API_KEY", "must-not-be-sent")
	t.Setenv("WENDY_CHAT_BASE_URL", "https://unrelated.invalid/v1")
	b := webBackend(t)
	b.chatToolsFactory = testChatTools
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("leaked environment credential")
		}
		switch r.URL.Path {
		case "/api/version":
			w.WriteHeader(http.StatusNotFound)
		case "/v1/models":
			fmt.Fprint(w, `{"data":[{"id":"qwen-device"}]}`)
		case "/v1/chat/completions":
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hello from device\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()
	b.conn.RegistryDialer = func(ctx context.Context, port int) (net.Conn, error) {
		if port != 11434 {
			t.Errorf("wrong device port %d", port)
		}
		return (&net.Dialer{}).DialContext(ctx, "tcp", strings.TrimPrefix(server.URL, "http://"))
	}
	models, err := b.ChatModels(3, 11434)
	if err != nil || len(models) != 1 || models[0].ID != "qwen-device" {
		t.Fatalf("models: %v %v", models, err)
	}
	if err := b.StartChat(3, 12, 11434, models[0].ID, []chat.Message{{Role: "user", Content: "Hello"}}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		r, err := b.PollChat(3, 12)
		if err != nil {
			t.Fatal(err)
		}
		if r.Done {
			if r.Error != "" || r.Text != "Hello from device" {
				t.Fatalf("reply: %+v", r)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("chat did not finish")
}

func TestDeviceChatRefusesStaleSessionsAndInvalidPorts(t *testing.T) {
	b := webBackend(t)
	b.chatToolsFactory = testChatTools
	for _, session := range []uint64{0, 999} {
		if _, err := b.ChatModels(session, 11434); err == nil {
			t.Fatal("models accepted stale session")
		}
		if err := b.StartChat(session, 2, 11434, "qwen", []chat.Message{{Role: "user", Content: "hi"}}); err == nil {
			t.Fatal("chat accepted stale session")
		}
	}
	for _, port := range []uint32{0, 65536} {
		if _, err := deviceChatTransport(b.conn, port); err == nil {
			t.Fatal("accepted invalid port")
		}
	}
}

func TestDeviceChatCancellation(t *testing.T) {
	b := webBackend(t)
	b.chatToolsFactory = testChatTools
	started := make(chan struct{})
	cancelled := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/version" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
		close(cancelled)
	}))
	defer server.Close()
	b.conn.RegistryDialer = func(ctx context.Context, port int) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", strings.TrimPrefix(server.URL, "http://"))
	}
	if err := b.StartChat(3, 15, 11434, "qwen", []chat.Message{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("stream never started")
	}
	if err := b.StopChat(3, 15); err != nil {
		t.Fatal(err)
	}
	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("stop did not cancel device request")
	}
}

func TestDeviceModelPullAndInferenceReadiness(t *testing.T) {
	b := webBackend(t)
	b.chatToolsFactory = testChatTools
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("unexpected credentials")
		}
		requests = append(requests, r.URL.Path)
		switch r.URL.Path {
		case "/api/version":
			fmt.Fprint(w, `{"version":"test"}`)
		case "/api/pull":
			fmt.Fprintln(w, `{"status":"pulling weights","total":100,"completed":50}`)
			fmt.Fprintln(w, `{"status":"success"}`)
		case "/api/generate":
			fmt.Fprint(w, `{"done":true,"eval_count":1}`)
		default:
			t.Error("unexpected path", r.URL.Path)
		}
	}))
	defer server.Close()
	b.conn.RegistryDialer = func(ctx context.Context, port int) (net.Conn, error) {
		if port != 11435 {
			t.Errorf("wrong port: %d", port)
		}
		return (&net.Dialer{}).DialContext(ctx, "tcp", strings.TrimPrefix(server.URL, "http://"))
	}
	transport, err := deviceChatTransport(b.conn, 11435)
	if err != nil {
		t.Fatal(err)
	}
	defer transport.CloseIdleConnections()
	if err = chat.DeviceOllamaReady(context.Background(), transport); err != nil {
		t.Fatal(err)
	}
	var updates []string
	if err = chat.DevicePullModel(context.Background(), transport, "hf.co/Qwen/test:model.gguf", func(s string) { updates = append(updates, s) }); err != nil {
		t.Fatal(err)
	}
	if err = chat.DeviceWarmModel(context.Background(), transport, "hf.co/Qwen/test:model.gguf"); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 3 || len(updates) < 2 || updates[len(updates)-1] != "success" {
		t.Fatal(requests, updates)
	}
}

func TestModelJobRejectsStaleSessions(t *testing.T) {
	b := webBackend(t)
	b.chatToolsFactory = testChatTools
	for _, session := range []uint64{0, 999} {
		if err := b.StartModelRun(session, 1, 11434, 0, "Qwen/test", "model.gguf", "revision", true, false); err == nil {
			t.Fatal("accepted stale model job")
		}
		if _, err := b.ModelHardware(session); err == nil {
			t.Fatal("accepted stale hardware request")
		}
	}
}
