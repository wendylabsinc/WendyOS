package chat

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

type sessionDeviceTransport func(*http.Request) (*http.Response, error)

func (f sessionDeviceTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestDeviceSessionFactoryInheritedByDelegates(t *testing.T) {
	t.Setenv("WENDY_CHAT_PROVIDER", "openai")
	t.Setenv("WENDY_CHAT_MODEL", "hosted-model")
	t.Setenv("WENDY_CHAT_API_KEY", "hosted-secret")
	profile, _ := ResolveProfile("general")
	local := Config{Provider: "local", Model: "local-tools", BaseURL: "http://wendy-device/v1"}
	var called atomic.Int32
	factory := func(got Config) (Provider, error) {
		called.Add(1)
		if got != local {
			return nil, errors.New("alternate inference configuration rejected")
		}
		return engineTestProvider(func(context.Context, []Message, []Tool, func(string)) (Message, error) {
			return Message{Role: "assistant", Content: "local delegate answered"}, nil
		}), nil
	}
	supervisor := &agentSupervisor{options: SessionOptions{Profile: profile, Config: local, ProviderFactory: factory, Device: "robot"}, gate: make(chan struct{}, 1)}
	supervisor.factory = func(ctx context.Context, opts SessionOptions, _ *Profile) (*Engine, func(), error) {
		if opts.ProviderFactory == nil {
			t.Error("delegate dropped pinned provider factory")
		}
		provider, err := sessionProvider(opts)
		if err != nil {
			return nil, nil, err
		}
		return NewEngine(provider, &engineTestExecutor{}, "test"), func() {}, nil
	}
	ctx := context.WithValue(context.Background(), turnRuntimeKey{}, &turnRuntime{emit: func(Event) {}})
	response, err := supervisor.delegate(ctx, ToolCall{Arguments: json.RawMessage(`{"tasks":[{"profile":"device-sensors","prompt":"inspect camera"},{"profile":"device-control","prompt":"inspect movement","device":"different-robot"}]}`)})
	if err != nil {
		t.Fatal(err)
	}
	var results []agentResult
	if err := json.Unmarshal([]byte(response), &results); err != nil {
		t.Fatal(err)
	}
	if called.Load() != 2 || len(results) != 2 {
		t.Fatalf("missing pinned delegate calls: %d; %s", called.Load(), response)
	}
	for _, result := range results {
		if result.State != "completed" || result.Text != "local delegate answered" || result.Model != "local-tools" || result.Provider != "local" {
			t.Fatalf("delegate escaped selected model: %+v", result)
		}
	}
}

func TestDeviceSessionProviderFailureCannotFallBackToHostedConfig(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "hosted-test-secret")
	stop := errors.New("selected device unavailable")
	opts := SessionOptions{Profile: Profile{Name: "general"}, Config: Config{Provider: "openai", Model: "hosted-model"}, ProviderFactory: func(Config) (Provider, error) { return nil, stop }}
	if _, err := NewSession(context.Background(), opts); !errors.Is(err, stop) {
		t.Fatalf("parent fell back or launched tools: %v", err)
	}
	if _, _, err := (&agentSupervisor{}).child(context.Background(), opts, &opts.Profile); !errors.Is(err, stop) {
		t.Fatalf("child fell back or launched tools: %v", err)
	}
}

func TestDeviceSessionDoesNotFollowRuntimeRedirects(t *testing.T) {
	requests := 0
	transport := sessionDeviceTransport(func(req *http.Request) (*http.Response, error) {
		requests++
		if req.URL.Host != "wendy-device" || req.Header.Get("Authorization") != "" {
			t.Errorf("request escaped local inference: %s", req.URL)
		}
		return &http.Response{StatusCode: http.StatusTemporaryRedirect, Status: "307 Temporary Redirect", Header: http.Header{"Location": {"https://hosted.invalid/v1/models"}}, Body: io.NopCloser(strings.NewReader("redirect")), Request: req}, nil
	})
	if _, err := DeviceModels(context.Background(), transport); err == nil || requests != 1 {
		t.Fatalf("model discovery followed redirect: requests=%d, err=%v", requests, err)
	}
	requests = 0
	provider, err := DeviceProviderWithLimit("local-tools", transport, 512)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Stream(context.Background(), []Message{{Role: "user", Content: "private prompt"}}, nil, nil); err == nil || requests != 2 {
		t.Fatalf("inference followed redirect: requests=%d, err=%v", requests, err)
	}
}
