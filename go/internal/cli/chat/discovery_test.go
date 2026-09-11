package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func modelIDs(models []ModelOption) []string {
	ids := make([]string, len(models))
	for i, model := range models {
		ids[i] = model.ID
	}
	return ids
}

func TestListModelsCompatible(t *testing.T) {
	clearProviderEnv(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/models" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Error("missing API key")
		}
		fmt.Fprint(w, `{"data":[{"id":"gpt-4.1-2025-04-14"},{"id":"text-embedding-3-large"},{"id":"gpt-4o-audio-preview"},{"id":"gpt-image-1"},{"id":"gpt-4.1"},{"id":"gpt-4.1"},{"id":"gpt-3.5-turbo-instruct"},{"id":"gpt-5-codex"},{"id":"gpt-4o-search-preview"},{"id":"gpt-5-pro"},{"id":"o3-deep-research"},{"id":""},{"id":"\u001b[2J"}]}`)
	}))
	defer server.Close()
	models, err := ListModels(context.Background(), Config{Provider: "openai", BaseURL: server.URL + "/v1", APIKey: "test-key"})
	if err != nil {
		t.Fatal(err)
	}
	if ids := modelIDs(models); !slices.Equal(ids, []string{"gpt-4.1", "gpt-4.1-2025-04-14"}) {
		t.Fatalf("models = %v", ids)
	}
}

func TestListModelsLocalKeepsCustomInstructModels(t *testing.T) {
	clearProviderEnv(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"Qwen-Instruct-GGUF"},{"id":"my-custom-model"},{"id":"nomic-embed-text"}]}`)
	}))
	defer server.Close()
	models, err := ListModels(context.Background(), Config{Provider: "local", BaseURL: server.URL})
	if err != nil || len(models) != 2 {
		t.Fatalf("models = %v, error = %v", models, err)
	}
	for _, model := range models {
		if !strings.Contains(model.Description, "Tool support depends") {
			t.Fatalf("unknown capabilities described as supported: %#v", model)
		}
	}
}

func TestModelPickerPrefersGeneralChatAliases(t *testing.T) {
	var models []ModelOption
	for _, id := range []string{"o4-mini", "gpt-4.1", "gpt-5-mini", "gpt-5.9", "gpt-5.10", "gpt-5.10-2026-01-01"} {
		models = append(models, ModelOption{ID: id, Description: cloudModelDescription(id)})
	}
	sortModelOptions(models)
	want := []string{"gpt-5.10", "gpt-5.9", "gpt-4.1", "gpt-5-mini", "o4-mini", "gpt-5.10-2026-01-01"}
	if !slices.Equal(modelIDs(models), want) {
		t.Fatalf("picker order = %v", modelIDs(models))
	}
}

func TestListModelsAnthropicPagination(t *testing.T) {
	clearProviderEnv(t)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/v1/models" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
		}
		if r.Header.Get("x-api-key") != "test-key" || r.Header.Get("anthropic-version") != "2023-06-01" || r.Header.Get("Authorization") != "" {
			t.Error("incorrect Anthropic authentication headers")
		}
		if r.URL.Query().Get("after_id") == "" {
			fmt.Fprint(w, `{"data":[{"id":"claude-sonnet-4-20250514","display_name":"Claude Sonnet 4"}],"has_more":true,"last_id":"cursor +/?"}`)
		} else {
			if r.URL.Query().Get("after_id") != "cursor +/?" {
				t.Error("pagination cursor was not encoded")
			}
			fmt.Fprint(w, `{"data":[{"id":"claude-haiku","display_name":"Claude Haiku"}],"has_more":false}`)
		}
	}))
	defer server.Close()
	models, err := ListModels(context.Background(), Config{Provider: "anthropic", BaseURL: server.URL + "/v1", APIKey: "test-key"})
	if err != nil || len(models) != 2 || requests.Load() != 2 {
		t.Fatalf("models = %v, requests = %d, error = %v", models, requests.Load(), err)
	}
	if models[0].Name != "Claude Haiku" || models[1].Name != "Claude Sonnet 4" {
		t.Fatalf("display names missing: %#v", models)
	}
}

func TestListModelsAnthropicRejectsRepeatedCursor(t *testing.T) {
	clearProviderEnv(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"claude-example"}],"has_more":true,"last_id":"same"}`)
	}))
	defer server.Close()
	_, err := ListModels(context.Background(), Config{Provider: "anthropic", BaseURL: server.URL})
	if err == nil || !strings.Contains(err.Error(), "incomplete model list") {
		t.Fatalf("error = %v", err)
	}
}

func TestListModelsOllamaToolCapabilities(t *testing.T) {
	clearProviderEnv(t)
	var showed atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/proxy/api/tags":
			if r.Method != http.MethodGet {
				t.Error("tags must be read-only")
			}
			fmt.Fprint(w, `{"models":[{"name":"tools:4b","size":2500000000,"details":{"parameter_size":"4.0B"}},{"name":"embedding"},{"name":"unknown"},{"name":"failed"},{"name":"empty"},{"name":"tools:4b"}]}`)
		case "/proxy/api/show":
			showed.Add(1)
			if r.Method != http.MethodPost {
				t.Error("show requires POST")
			}
			var request map[string]string
			if json.NewDecoder(r.Body).Decode(&request) != nil || len(request) != 1 {
				t.Error("show should contain only model metadata request")
			}
			switch request["model"] {
			case "tools:4b":
				fmt.Fprint(w, `{"capabilities":["completion","tools"]}`)
			case "embedding":
				fmt.Fprint(w, `{"capabilities":["embedding"]}`)
			case "unknown":
				fmt.Fprint(w, `{}`)
			case "empty":
				fmt.Fprint(w, `{"capabilities":[]}`)
			case "failed":
				http.Error(w, "old server", http.StatusNotFound)
			default:
				t.Errorf("unexpected model %q", request["model"])
			}
		default:
			t.Errorf("unexpected request (discovery must not infer or load): %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	models, err := ListModels(context.Background(), Config{Provider: "ollama", BaseURL: server.URL + "/proxy/v1"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(modelIDs(models), []string{"tools:4b", "unknown", "failed"}) || showed.Load() != 5 {
		t.Fatalf("models = %v, showed = %d", models, showed.Load())
	}
	if !strings.Contains(models[0].Description, "Tool calling supported") || !strings.Contains(models[0].Description, "2.5 GB") || !strings.Contains(models[1].Description, "could not be checked") {
		t.Fatalf("missing capability descriptions: %v", models)
	}
}

func TestListModelsOllamaCompatibleFallback(t *testing.T) {
	clearProviderEnv(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			fmt.Fprint(w, `{"data":[{"id":"gateway-model"}]}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	models, err := ListModels(context.Background(), Config{Provider: "ollama", BaseURL: server.URL + "/v1"})
	if err != nil || !slices.Equal(modelIDs(models), []string{"gateway-model"}) {
		t.Fatalf("models = %v, error = %v", models, err)
	}
}

func TestListModelsFailures(t *testing.T) {
	for _, test := range []struct {
		name, body, want string
		status           int
		noModels         bool
	}{
		{"empty", `{"data":[]}`, "no chat models", http.StatusOK, true},
		{"wrong service", `{}`, "did not return a model list", http.StatusOK, false},
		{"malformed", `secret <html>`, "unexpected reply", http.StatusOK, false},
		{"unauthorized", `{"error":{"message":"invalid secret key\u001b[2J"}}`, "did not accept this API key", http.StatusUnauthorized, false},
		{"busy", `{"error":"secret"}`, "busy", http.StatusTooManyRequests, false},
		{"oversized", strings.Repeat("x", (4<<20)+1), "unusually large", http.StatusOK, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			clearProviderEnv(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(test.status)
				fmt.Fprint(w, test.body)
			}))
			defer server.Close()
			_, err := ListModels(context.Background(), Config{Provider: "local", BaseURL: server.URL, APIKey: "secret"})
			if err == nil || !strings.Contains(err.Error(), test.want) || errors.Is(err, ErrNoModels) != test.noModels {
				t.Fatalf("error = %v", err)
			}
			if strings.Contains(err.Error(), "secret") || strings.ContainsRune(err.Error(), '\x1b') {
				t.Fatalf("error contains secrets or terminal controls: %v", err)
			}
		})
	}
}

func TestDiscoveryDoesNotFollowRedirects(t *testing.T) {
	clearProviderEnv(t)
	var redirected atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected.Store(true)
		fmt.Fprint(w, `{"data":[]}`)
	}))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	_, err := ListModels(context.Background(), Config{Provider: "anthropic", BaseURL: server.URL, APIKey: "secret"})
	if err == nil || redirected.Load() {
		t.Fatalf("redirect followed = %v, error = %v", redirected.Load(), err)
	}
}

func TestDiscoverLocalIgnoresEnvironmentAndIncludesEmptyServers(t *testing.T) {
	clearProviderEnv(t)
	t.Setenv("WENDY_CHAT_PROVIDER", "anthropic")
	t.Setenv("WENDY_CHAT_API_KEY", "wendy-secret")
	t.Setenv("OPENAI_API_KEY", "openai-secret")
	t.Setenv("ANTHROPIC_API_KEY", "anthropic-secret")
	t.Setenv("WENDY_CHAT_BASE_URL", "http://invalid.example")
	t.Setenv("WENDY_CHAT_MAX_TOKENS", "invalid")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.Header.Get("x-api-key") != "" {
			t.Error("discovery forwarded a credential")
		}
		switch r.URL.Path {
		case "/api/tags":
			fmt.Fprint(w, `{"models":[]}`)
		case "/v1/models":
			fmt.Fprint(w, `{"data":[{"id":"my-model"}]}`)
		default:
			t.Errorf("unexpected discovery path %s", r.URL.Path)
		}
	}))
	defer server.Close()
	servers := discoverLocal(context.Background(), []LocalServer{
		{Name: "Ollama", Config: Config{Provider: "ollama", BaseURL: server.URL + "/v1"}},
		{Name: "Local", Config: Config{Provider: "local", BaseURL: server.URL + "/v1"}},
	})
	if len(servers) != 2 || len(servers[0].Models) != 0 || len(servers[1].Models) != 1 {
		t.Fatalf("servers = %v", servers)
	}
	for _, discovered := range servers {
		if discovered.Config.APIKey != "" || discovered.Config.MaxTokens != 4096 {
			t.Error("discovered settings inherited environment")
		}
		resolved, err := ResolveConnection(discovered.Config)
		if err != nil || resolved.APIKey != "" || resolved.BaseURL != discovered.Config.BaseURL {
			t.Errorf("reusing discovery settings reapplied the environment: %v", err)
		}
	}
}

func TestDiscoverLocalTimeoutIsConcurrent(t *testing.T) {
	clearProviderEnv(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer server.Close()
	started := time.Now()
	servers := discoverLocal(context.Background(), []LocalServer{
		{Name: "one", Config: Config{Provider: "local", BaseURL: server.URL}},
		{Name: "two", Config: Config{Provider: "local", BaseURL: server.URL}},
		{Name: "three", Config: Config{Provider: "local", BaseURL: server.URL}},
	})
	if len(servers) != 0 || time.Since(started) > 2500*time.Millisecond {
		t.Fatalf("discovery took %s, servers = %v", time.Since(started), servers)
	}
}

func TestListModelsCancellation(t *testing.T) {
	clearProviderEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := ListModels(ctx, Config{Provider: "local", BaseURL: "http://localhost:1/v1"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
}

func TestPullOllamaModelProgress(t *testing.T) {
	clearProviderEnv(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/proxy/api/pull" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
		}
		var body struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil || body.Model != StarterLocalModel || !body.Stream {
			t.Errorf("invalid pull payload: %#v", body)
		}
		for range 1000 {
			fmt.Fprintln(w, `{"status":"pulling secret\u001b[2J","total":100,"completed":50}`)
		}
		fmt.Fprintln(w, `{"status":"success"}`)
	}))
	defer server.Close()
	var progress []string
	err := PullOllamaModel(context.Background(), Config{Provider: "ollama", BaseURL: server.URL + "/proxy/v1", APIKey: "secret"}, StarterLocalModel, func(update string) { progress = append(progress, update) })
	if err != nil {
		t.Fatal(err)
	}
	if len(progress) != 2 || !strings.Contains(progress[0], "50%") || progress[1] != "success" {
		t.Fatalf("progress was not bounded: %v", progress)
	}
	if strings.Contains(progress[0], "secret") || strings.ContainsRune(progress[0], '\x1b') {
		t.Fatalf("progress leaked secret or terminal control: %v", progress)
	}
}

func TestPullOllamaModelFailures(t *testing.T) {
	for _, test := range []struct{ name, body, want string }{
		{"error", `{"error":"secret download failed"}`, "could not download"},
		{"truncated", `{"status":"pulling"}`, "stopped before"},
		{"malformed", `secret bad json`, "unexpected download update"},
		{"oversized update", strings.Repeat("x", 65<<10), "interrupted"},
	} {
		t.Run(test.name, func(t *testing.T) {
			clearProviderEnv(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, test.body) }))
			defer server.Close()
			err := PullOllamaModel(context.Background(), Config{Provider: "ollama", BaseURL: server.URL, APIKey: "secret"}, StarterLocalModel, nil)
			if err == nil || !strings.Contains(err.Error(), test.want) || strings.Contains(err.Error(), "secret") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestPullOllamaModelCancellation(t *testing.T) {
	clearProviderEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, `{"status":"pulling"}`)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	err := PullOllamaModel(ctx, Config{Provider: "ollama", BaseURL: server.URL}, StarterLocalModel, func(string) { cancel() })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
}
