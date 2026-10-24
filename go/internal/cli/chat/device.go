package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// DeviceProvider uses an explicit transport to the selected device. It never
// inherits cloud endpoints, credentials, or model names from the environment.
func DeviceProvider(model string, transport http.RoundTripper) (Provider, error) {
	if !validModelID(model) {
		return nil, fmt.Errorf("choose a model running on the device")
	}
	c, err := resolveConnection(Config{Provider: "local", BaseURL: "http://wendy-device/v1", Model: model}, false)
	if err != nil {
		return nil, err
	}
	return &deviceProvider{fallback: &httpProvider{config: c, client: deviceHTTPClient(transport)}}, nil
}

func DeviceModels(ctx context.Context, transport http.RoundTripper) ([]ModelOption, error) {
	c, err := resolveConnection(Config{Provider: "local", BaseURL: "http://wendy-device/v1"}, false)
	if err != nil {
		return nil, err
	}
	return listCompatibleModels(ctx, &httpProvider{config: c, client: deviceHTTPClient(transport)})
}

func deviceHTTPClient(transport http.RoundTripper) *http.Client {
	return &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// DevicePullModel and DeviceWarmModel never resolve environment credentials or
// hosted endpoints. All traffic uses the selected Agent connection.
func DevicePullModel(ctx context.Context, transport http.RoundTripper, model string, progress func(string)) error {
	if !validModelID(model) {
		return fmt.Errorf("invalid model name")
	}
	p := &httpProvider{config: Config{Provider: "ollama", BaseURL: "http://wendy-device/v1"}, client: deviceHTTPClient(transport)}
	return pullOllamaModel(ctx, p, model, progress)
}
func DevicePullModelProgress(ctx context.Context, transport http.RoundTripper, model string, progress func(ModelPullProgress)) error {
	if !validModelID(model) {
		return fmt.Errorf("invalid model name")
	}
	p := &httpProvider{config: Config{Provider: "ollama", BaseURL: "http://wendy-device/v1"}, client: deviceHTTPClient(transport)}
	return pullOllamaModelProgress(ctx, p, model, progress)
}

func DeviceOllamaReady(ctx context.Context, transport http.RoundTripper) error {
	p := &httpProvider{config: Config{Provider: "ollama", BaseURL: "http://wendy-device/v1"}, client: deviceHTTPClient(transport)}
	r, err := discoveryRequest(ctx, p, http.MethodGet, "http://wendy-device/api/version", nil)
	if err != nil {
		return err
	}
	defer r.Body.Close()
	var v struct {
		Version string `json:"version"`
	}
	if err = json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&v); err != nil {
		return err
	}
	if v.Version == "" {
		return fmt.Errorf("this port does not serve Ollama")
	}
	return nil
}
func DeviceWarmModel(ctx context.Context, transport http.RoundTripper, model string) error {
	p := &httpProvider{config: Config{Provider: "ollama", BaseURL: "http://wendy-device/v1"}, client: deviceHTTPClient(transport)}
	// A real one-token generation checks model loading and inference, not just
	// server reachability or an entry in the downloaded model list.
	r, err := discoveryRequest(ctx, p, http.MethodPost, "http://wendy-device/api/generate", map[string]any{"model": model, "prompt": "Hello", "stream": false, "keep_alive": "10m", "options": map[string]any{"num_predict": 1, "num_ctx": 32768}})
	if err != nil {
		return err
	}
	defer r.Body.Close()
	var v struct {
		Done      bool   `json:"done"`
		Error     string `json:"error"`
		EvalCount int    `json:"eval_count"`
	}
	if err = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&v); err != nil {
		return err
	}
	if v.Error != "" {
		return fmt.Errorf("model could not load: %s", p.safeError(v.Error))
	}
	if !v.Done || v.EvalCount < 1 {
		return fmt.Errorf("model did not finish its readiness check")
	}
	return nil
}
