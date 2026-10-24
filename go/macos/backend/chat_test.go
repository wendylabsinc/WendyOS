package backend

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/chat"
	"github.com/wendylabsinc/wendy/go/internal/shared/cloudrelay"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestCloudChatFallbackOnlyForUnsupportedService(t *testing.T) {
	for _, verdict := range []error{status.Error(codes.PermissionDenied, "access denied"), status.Error(codes.Unauthenticated, "sign in again"), context.DeadlineExceeded} {
		t.Run(verdict.Error(), func(t *testing.T) {
			b := webBackend(t)
			// Any attempt to fall back would access this nil service.
			b.conn.ContainerService = nil
			b.conn.RegistryDialer = func(context.Context, int) (net.Conn, error) { return nil, verdict }
			transport, _ := deviceChatTransport(b.conn, 11434)
			defer transport.CloseIdleConnections()
			req, _ := http.NewRequest(http.MethodGet, "http://wendy-device/api/tags", nil)
			_, err := transport.RoundTrip(req)
			if !errors.Is(err, verdict) {
				t.Fatalf("lost original refusal: %v", err)
			}
		})
	}
}

func TestLANChatNeverFallsBackToUnprotectedHTTP(t *testing.T) {
	for _, authenticated := range []bool{false, true} {
		t.Run(fmt.Sprint(authenticated), func(t *testing.T) {
			b := webBackend(t)
			server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				t.Error("model request reached the unauthenticated LAN HTTP listener")
			}))
			defer server.Close()
			host, port, _ := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
			var number uint32
			fmt.Sscan(port, &number)
			b.conn.Host, b.conn.IsMTLS = host, authenticated
			transport, err := deviceChatTransport(b.conn, number)
			if err != nil {
				t.Fatal(err)
			}
			defer transport.CloseIdleConnections()
			req, _ := http.NewRequest(http.MethodGet, "http://wendy-device/api/tags", nil)
			_, err = transport.RoundTrip(req)
			if err == nil {
				t.Fatal("unmanaged model port accepted")
			}
			want := "authenticated device connection"
			if authenticated {
				want = "no running Wendy-managed Ollama runtime"
			}
			if !strings.Contains(err.Error(), want) {
				t.Fatal(err)
			}
		})
	}
}

func TestCloudChatFallbackRequiresManagedRuntime(t *testing.T) {
	b := webBackend(t) // Running web app on 8080, but no managed Ollama runtime.
	b.conn.RegistryDialer = func(context.Context, int) (net.Conn, error) {
		return nil, &cloudrelay.UnsupportedServiceError{Service: "tcp-8080"}
	}
	transport, _ := deviceChatTransport(b.conn, 8080)
	defer transport.CloseIdleConnections()
	req, _ := http.NewRequest(http.MethodGet, "http://wendy-device/api/tags", nil)
	_, err := transport.RoundTrip(req)
	var unsupported *cloudrelay.UnsupportedServiceError
	if !errors.As(err, &unsupported) || !strings.Contains(err.Error(), "no running Wendy-managed Ollama runtime") {
		t.Fatalf("fallback failed to explain unmanaged port: %v", err)
	}
}

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

func TestDownloadedModelsOnAlternateRuntime(t *testing.T) {
	models, err := discoverDeviceChatModels(context.Background(), 11434, []uint32{11435, 11436, 11435}, func(_ context.Context, port uint32) ([]DeviceChatModel, error) {
		if port == 11434 {
			return nil, fmt.Errorf("default server unavailable")
		}
		if port == 11435 {
			return []DeviceChatModel{{ModelOption: chat.ModelOption{ID: "downloaded-qwen"}}}, nil
		}
		return []DeviceChatModel{{ModelOption: chat.ModelOption{ID: "downloaded-qwen"}}, {ModelOption: chat.ModelOption{ID: "downloaded-gemma"}}}, nil
	})
	if err != nil || len(models) != 2 || models[0].ID != "downloaded-qwen" || models[0].Port != 11435 || models[1].Port != 11436 {
		t.Fatalf("downloaded models: %+v, %v", models, err)
	}
	models, err = discoverDeviceChatModels(context.Background(), 11436, []uint32{11435}, func(_ context.Context, port uint32) ([]DeviceChatModel, error) {
		return []DeviceChatModel{{ModelOption: chat.ModelOption{ID: "same-model"}}}, nil
	})
	if err != nil || len(models) != 1 || models[0].Port != 11436 {
		t.Fatalf("preferred runtime lost: %+v, %v", models, err)
	}
}

func TestDownloadedModelPrefersReachableRuntime(t *testing.T) {
	models, err := discoverDeviceChatModels(context.Background(), 11434, []uint32{11437}, func(_ context.Context, port uint32) ([]DeviceChatModel, error) {
		model := DeviceChatModel{ModelOption: chat.ModelOption{ID: "qwen3:4b"}}
		if port == 11434 {
			model.UnavailableReason = "Connect through Nearby"
		}
		return []DeviceChatModel{model}, nil
	})
	if err != nil || len(models) != 1 || models[0].Port != 11437 || models[0].UnavailableReason != "" {
		t.Fatalf("model route: %+v, %v", models, err)
	}
}
