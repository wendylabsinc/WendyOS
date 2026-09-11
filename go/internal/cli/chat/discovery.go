package chat

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

// ModelOption is a model advertised by the selected server, ready for a picker.
type ModelOption struct {
	ID          string
	Name        string
	Description string
}

// LocalServer describes a running local model server. Models may be empty when
// the server is reachable but needs a model to be downloaded or loaded.
type LocalServer struct {
	Name   string
	Config Config
	Models []ModelOption
}

// ErrNoModels means the server is reachable but has no usable models to list.
var ErrNoModels = errors.New("no chat models available")

// StarterLocalModel is a small, tool-capable Ollama model (about a 2.5 GB download).
// See https://ollama.com/library/qwen3:4b.
const StarterLocalModel = "qwen3:4b"

// ListModels discovers available models without generating tokens or loading a
// model into memory. An empty Config.Model is supported during first-run setup.
func ListModels(ctx context.Context, config Config) ([]ModelOption, error) {
	c, err := ResolveConnection(config)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return listModels(ctx, c)
}

func discoveryProvider(c Config) *httpProvider {
	return &httpProvider{config: c, client: &http.Client{
		// Never forward credentials to a redirect destination, even on localhost.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func listModels(ctx context.Context, c Config) ([]ModelOption, error) {
	p := discoveryProvider(c)
	if c.Provider == "ollama" {
		models, err := listOllamaModels(ctx, p)
		var status *discoveryHTTPError
		if !errors.As(err, &status) || (status.code != http.StatusNotFound && status.code != http.StatusMethodNotAllowed) {
			return models, err
		}
		// Some gateways expose only Ollama's OpenAI-compatible API.
	}
	return listCompatibleModels(ctx, p)
}

// DiscoverLocal probes common local servers concurrently for about one second.
// Discovery never forwards API keys or endpoint overrides from the environment.
func DiscoverLocal(ctx context.Context) []LocalServer {
	return discoverLocal(ctx, []LocalServer{
		{Name: "Ollama", Config: Config{Provider: "ollama", BaseURL: "http://localhost:11434/v1"}},
		{Name: "LM Studio", Config: Config{Provider: "local", BaseURL: "http://localhost:1234/v1"}},
		{Name: "llama.cpp", Config: Config{Provider: "local", BaseURL: "http://localhost:8080/v1"}},
	})
}

func discoverLocal(ctx context.Context, presets []LocalServer) []LocalServer {
	results := make([]*LocalServer, len(presets))
	var wg sync.WaitGroup
	for i, preset := range presets {
		wg.Go(func() {
			probeCtx, cancel := context.WithTimeout(ctx, time.Second)
			defer cancel()
			// Resolve without environment inheritance: discovery is not an opt-in
			// to sending even a Wendy-specific credential to every local server.
			preset.Config.APIKey = ""
			c, err := resolveConnection(preset.Config, false)
			if err != nil {
				return
			}
			c.environmentApplied = true
			models, err := listModels(probeCtx, c)
			if err != nil && !errors.Is(err, ErrNoModels) {
				return
			}
			preset.Config, preset.Models = c, models
			results[i] = &preset
		})
	}
	wg.Wait()
	var servers []LocalServer
	for _, result := range results {
		if result != nil {
			servers = append(servers, *result)
		}
	}
	return servers
}

func listCompatibleModels(ctx context.Context, p *httpProvider) ([]ModelOption, error) {
	var models []ModelOption
	seen := make(map[string]bool)
	cursors := make(map[string]bool)
	endpoint := p.config.BaseURL + "/models"
	for page := 0; page < 100; page++ {
		var result struct {
			Data []struct {
				ID          string `json:"id"`
				DisplayName string `json:"display_name"`
			} `json:"data"`
			HasMore bool   `json:"has_more"`
			LastID  string `json:"last_id"`
		}
		if err := discoveryJSON(ctx, p, http.MethodGet, endpoint, nil, &result); err != nil {
			return nil, err
		}
		if result.Data == nil {
			return nil, fmt.Errorf("the server did not return a model list; check the server address")
		}
		for _, model := range result.Data {
			if !validModelID(model.ID) || seen[model.ID] || !chatModelName(model.ID, p.config.Provider) {
				continue
			}
			seen[model.ID] = true
			description := "Available from this provider"
			if p.config.Provider == "local" || p.config.Provider == "ollama" {
				description = "Tool support depends on your model and server"
			} else if p.config.Provider == "openai" {
				description = cloudModelDescription(model.ID)
			}
			models = append(models, ModelOption{ID: model.ID, Name: p.safeError(firstValue(model.DisplayName, model.ID)), Description: description})
		}
		if p.config.Provider != "anthropic" || !result.HasMore {
			if len(models) == 0 {
				return nil, fmt.Errorf("%w; this server has no conversational models available yet", ErrNoModels)
			}
			sortModelOptions(models)
			return models, nil
		}
		if result.LastID == "" || cursors[result.LastID] {
			return nil, fmt.Errorf("the server returned an incomplete model list; please try again")
		}
		cursors[result.LastID] = true
		endpoint = p.config.BaseURL + "/models?" + url.Values{"after_id": {result.LastID}}.Encode()
	}
	return nil, fmt.Errorf("the server returned too many pages of models")
}

func listOllamaModels(ctx context.Context, p *httpProvider) ([]ModelOption, error) {
	var result struct {
		Models []struct {
			Name    string `json:"name"`
			Model   string `json:"model"`
			Size    int64  `json:"size"`
			Details struct {
				ParameterSize string `json:"parameter_size"`
			} `json:"details"`
		} `json:"models"`
	}
	if err := discoveryJSON(ctx, p, http.MethodGet, ollamaURL(p.config, "/api/tags"), nil, &result); err != nil {
		return nil, err
	}
	if result.Models == nil {
		return nil, fmt.Errorf("Ollama did not return a model list; check the server address")
	}
	var models []ModelOption
	seen := make(map[string]bool)
	for _, model := range result.Models {
		id := firstValue(model.Name, model.Model)
		if !validModelID(id) || seen[id] {
			continue
		}
		seen[id] = true
		details := ""
		if model.Details.ParameterSize != "" {
			details = p.safeError(model.Details.ParameterSize) + " parameters"
		}
		if model.Size > 0 {
			if details != "" {
				details += " · "
			}
			details += fmt.Sprintf("%.1f GB", float64(model.Size)/1e9)
		}
		models = append(models, ModelOption{ID: id, Name: p.safeError(id), Description: details})
	}
	// /api/show reads metadata; it does not load the model or run inference.
	// Bound both concurrency and latency so an old server cannot stall setup.
	capabilities := make([][]string, len(models))
	var wg sync.WaitGroup
	jobs := make(chan int)
	for range min(8, len(models)) {
		wg.Go(func() {
			for i := range jobs {
				if ctx.Err() != nil {
					continue
				}
				showCtx, cancel := context.WithTimeout(ctx, 400*time.Millisecond)
				var details struct {
					Capabilities []string `json:"capabilities"`
				}
				err := discoveryJSON(showCtx, p, http.MethodPost, ollamaURL(p.config, "/api/show"), map[string]string{"model": models[i].ID}, &details)
				cancel()
				if err == nil {
					capabilities[i] = details.Capabilities
				}
			}
		})
	}
	for i := range models {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	if errors.Is(ctx.Err(), context.Canceled) {
		return nil, ctx.Err()
	}
	var usable []ModelOption
	for i, model := range models {
		toolSupport := "Tool support could not be checked"
		if capabilities[i] != nil {
			found := false
			for _, capability := range capabilities[i] {
				if capability == "tools" {
					found = true
					break
				}
			}
			if !found {
				continue
			}
			toolSupport = "Tool calling supported"
		}
		if model.Description == "" {
			model.Description = toolSupport
		} else {
			model.Description = toolSupport + " · " + model.Description
		}
		usable = append(usable, model)
	}
	if len(usable) == 0 {
		return nil, fmt.Errorf("%w; Ollama is running, but needs a model with tool support", ErrNoModels)
	}
	sortModelOptions(usable)
	return usable, nil
}

func ollamaURL(c Config, suffix string) string {
	return strings.TrimSuffix(c.BaseURL, "/v1") + suffix
}

func validModelID(id string) bool {
	return strings.TrimSpace(id) != "" && len(id) <= 512 && strings.IndexFunc(id, unicode.IsControl) < 0
}

func chatModelName(id, provider string) bool {
	id = strings.ToLower(id)
	for _, part := range []string{"embedding", "embed-"} {
		if strings.Contains(id, part) {
			return false
		}
	}
	if provider == "local" || provider == "ollama" {
		// Compatible local servers accept arbitrary model names; a name such
		// as "image-assistant" may still be a conversational vision model.
		return true
	}
	for _, part := range []string{"moderation", "whisper", "tts-", "audio", "realtime", "transcribe", "dall-e", "image", "sora"} {
		if strings.Contains(id, part) {
			return false
		}
	}
	if provider == "openai" && (strings.HasPrefix(id, "gpt-") || strings.HasPrefix(id, "o1") || strings.HasPrefix(id, "o3") || strings.HasPrefix(id, "o4") || strings.HasPrefix(id, "codex") || strings.HasPrefix(id, "babbage") || strings.HasPrefix(id, "davinci")) {
		for _, part := range []string{"babbage", "davinci", "instruct", "deep-research", "computer-use", "search", "codex", "-pro"} {
			if strings.Contains(id, part) {
				return false
			}
		}
	}
	return true
}

var datedModelID = regexp.MustCompile(`-\d{4}-\d{2}-\d{2}$|-\d{8}$`)

func cloudModelDescription(id string) string {
	id = strings.ToLower(id)
	switch {
	case strings.HasPrefix(id, "o1"), strings.HasPrefix(id, "o3"), strings.HasPrefix(id, "o4"):
		return "Reasoning model"
	case strings.Contains(id, "mini"), strings.Contains(id, "nano"):
		return "Smaller model"
	case strings.HasPrefix(id, "gpt-"):
		return "General-purpose chat"
	default:
		return "Available from this provider"
	}
}

func modelOptionRank(model ModelOption) int {
	switch model.Description {
	case "General-purpose chat":
		return 0
	case "Smaller model":
		return 1
	case "Reasoning model":
		return 3
	default:
		return 2
	}
}

func sortModelOptions(models []ModelOption) {
	sort.SliceStable(models, func(i, j int) bool {
		// Verified local tool support precedes unknown support; rolling model
		// aliases precede dated snapshots. Never invent or silently select an ID.
		iUnknown := strings.HasPrefix(models[i].Description, "Tool support could not")
		jUnknown := strings.HasPrefix(models[j].Description, "Tool support could not")
		if iUnknown != jUnknown {
			return !iUnknown
		}
		iDated, jDated := datedModelID.MatchString(models[i].ID), datedModelID.MatchString(models[j].ID)
		if iDated != jDated {
			return !iDated
		}
		if iRank, jRank := modelOptionRank(models[i]), modelOptionRank(models[j]); iRank != jRank {
			return iRank < jRank
		}
		return compareModelIDs(models[i].ID, models[j].ID) > 0
	})
}

// compareModelIDs sorts numeric model versions naturally (10 follows 9), while
// remaining deterministic for arbitrary model names and quantization suffixes.
func compareModelIDs(a, b string) int {
	for len(a) > 0 && len(b) > 0 {
		if a[0] >= '0' && a[0] <= '9' && b[0] >= '0' && b[0] <= '9' {
			i, j := 0, 0
			for i < len(a) && a[i] >= '0' && a[i] <= '9' {
				i++
			}
			for j < len(b) && b[j] >= '0' && b[j] <= '9' {
				j++
			}
			n, m := strings.TrimLeft(a[:i], "0"), strings.TrimLeft(b[:j], "0")
			if len(n) != len(m) {
				return len(n) - len(m)
			}
			if result := strings.Compare(n, m); result != 0 {
				return result
			}
			a, b = a[i:], b[j:]
			continue
		}
		if a[0] != b[0] {
			return int(a[0]) - int(b[0])
		}
		a, b = a[1:], b[1:]
	}
	return strings.Compare(a, b)
}

type discoveryHTTPError struct {
	code    int
	message string
}

func (e *discoveryHTTPError) Error() string { return e.message }

func discoveryRequest(ctx context.Context, p *httpProvider, method, endpoint string, payload any) (*http.Response, error) {
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("could not prepare the model request")
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, fmt.Errorf("could not prepare the model request: %s", p.safeError(err.Error()))
	}
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if p.config.Provider == "anthropic" {
		req.Header.Set("anthropic-version", "2023-06-01")
		if p.config.APIKey != "" {
			req.Header.Set("x-api-key", p.config.APIKey)
		}
	} else if p.config.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.config.APIKey)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("could not reach the model server; check that it is running and the server address is correct")
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	detail := p.errorDetail(data)
	message := fmt.Sprintf("the model server returned HTTP %d", resp.StatusCode)
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		message = "the server did not accept this API key; check your key and account access"
	case http.StatusNotFound, http.StatusMethodNotAllowed:
		message = "this server does not support model discovery at this address"
	case http.StatusTooManyRequests:
		message = "the model server is busy or your account has reached its limit; try again shortly"
	}
	if detail != "" {
		message += ": " + detail
	}
	return nil, &discoveryHTTPError{code: resp.StatusCode, message: message}
}

func discoveryJSON(ctx context.Context, p *httpProvider, method, endpoint string, payload, output any) error {
	resp, err := discoveryRequest(ctx, p, method, endpoint, payload)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, (4<<20)+1))
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("could not read the model server's reply; try again")
	}
	if len(data) > 4<<20 {
		return fmt.Errorf("the model server returned an unusually large reply")
	}
	if json.Unmarshal(data, output) != nil {
		return fmt.Errorf("the model server returned an unexpected reply; check the server address")
	}
	return nil
}

// PullOllamaModel downloads a model only when explicitly requested by the user.
// Progress reports are bounded and contain no credentials or terminal controls.
func PullOllamaModel(ctx context.Context, config Config, model string, progress func(string)) error {
	c, err := ResolveConnection(config)
	if err != nil {
		return err
	}
	if c.Provider != "ollama" {
		return fmt.Errorf("model downloads are available for Ollama")
	}
	if !validModelID(model) {
		return fmt.Errorf("choose a valid Ollama model to download")
	}
	ctx, cancel := context.WithTimeout(ctx, time.Hour)
	defer cancel()
	p := discoveryProvider(c)
	resp, err := discoveryRequest(ctx, p, http.MethodPost, ollamaURL(c, "/api/pull"), map[string]any{"model": model, "stream": true})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	scanner := bufio.NewScanner(io.LimitReader(resp.Body, 32<<20))
	scanner.Buffer(make([]byte, 4096), 64<<10)
	var lastUpdate time.Time
	var previous string
	updates := 0
	for scanner.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if len(bytes.TrimSpace(scanner.Bytes())) == 0 {
			continue
		}
		var update struct {
			Status    string `json:"status"`
			Error     string `json:"error"`
			Total     int64  `json:"total"`
			Completed int64  `json:"completed"`
		}
		if json.Unmarshal(scanner.Bytes(), &update) != nil {
			return fmt.Errorf("Ollama returned an unexpected download update; please retry")
		}
		if update.Error != "" {
			return fmt.Errorf("Ollama could not download the model: %s", p.safeError(update.Error))
		}
		status := p.safeError(update.Status)
		if update.Total > 0 {
			percent := min(100, max(0, float64(update.Completed)/float64(update.Total)*100))
			status += fmt.Sprintf(" %.0f%%", percent)
		}
		if progress != nil && status != "" && ((status != previous && updates < 4096 && (lastUpdate.IsZero() || time.Since(lastUpdate) >= 250*time.Millisecond)) || update.Status == "success") {
			progress(status)
			lastUpdate, previous = time.Now(), status
			updates++
		}
		if update.Status == "success" {
			return nil
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if scanner.Err() != nil {
		return fmt.Errorf("the model download was interrupted; retry to resume it")
	}
	return fmt.Errorf("Ollama stopped before the download finished; retry to resume it")
}
