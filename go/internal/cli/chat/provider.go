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
	"sort"
	"strings"
	"time"
	"unicode"
)

type httpProvider struct {
	config Config
	client *http.Client
}

// NewProvider creates a streaming provider without making a network request.
func NewProvider(config Config) (Provider, error) {
	c, err := ResolveConfig(config)
	if err != nil {
		return nil, err
	}
	return &httpProvider{config: c, client: &http.Client{
		Timeout: 15 * time.Minute,
		// In particular, net/http does not treat x-api-key as a sensitive header
		// when following redirects. Keep credentials on the configured endpoint.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}}, nil
}

func (p *httpProvider) Stream(ctx context.Context, messages []Message, tools []Tool, emit func(string)) (Message, error) {
	if emit == nil {
		emit = func(string) {}
	}
	if p.config.Provider == "anthropic" {
		return p.streamAnthropic(ctx, messages, tools, emit)
	}
	if p.usesResponses() {
		return p.streamResponses(ctx, messages, tools, emit)
	}
	return p.streamOpenAI(ctx, messages, tools, emit)
}

func (p *httpProvider) usesResponses() bool {
	u, err := url.Parse(p.config.BaseURL)
	return err == nil && p.config.Provider == "openai" && strings.EqualFold(u.Hostname(), "api.openai.com")
}

func (p *httpProvider) post(ctx context.Context, suffix string, payload any) (*http.Response, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode chat request: %w", err)
	}
	endpoint := p.config.BaseURL
	if suffix == "/responses" && p.usesResponses() {
		endpoint = strings.TrimSuffix(endpoint, "/chat/completions")
	}
	if !strings.HasSuffix(endpoint, suffix) {
		endpoint += suffix
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create chat request: %s", p.safeError(err.Error()))
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
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
		return nil, fmt.Errorf("chat request failed: %s", p.safeError(err.Error()))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		detail := p.errorDetail(data)
		if detail == "" {
			detail = http.StatusText(resp.StatusCode)
		}
		return nil, fmt.Errorf("%s API returned HTTP %d: %s", p.config.Provider, resp.StatusCode, detail)
	}
	if strings.Contains(resp.Header.Get("Content-Type"), "application/json") {
		defer resp.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return nil, fmt.Errorf("chat endpoint returned JSON instead of an event stream; check --base-url and model streaming support: %s", p.errorDetail(data))
	}
	return resp, nil
}

func (p *httpProvider) safeError(s string) string {
	if p.config.APIKey != "" {
		for _, key := range []string{p.config.APIKey, url.QueryEscape(p.config.APIKey), url.PathEscape(p.config.APIKey)} {
			s = strings.ReplaceAll(s, key, "[redacted]")
		}
	}
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	if len(s) > 1024 {
		s = s[:1024] + "…"
	}
	return strings.TrimSpace(s)
}

func (p *httpProvider) errorDetail(data []byte) string {
	var envelope struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(data, &envelope) == nil && len(envelope.Error) != 0 {
		var object struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		}
		if json.Unmarshal(envelope.Error, &object) == nil {
			return p.safeError(firstValue(object.Message, object.Type, "provider error"))
		}
		var message string
		if json.Unmarshal(envelope.Error, &message) == nil {
			return p.safeError(message)
		}
	}
	return p.safeError(string(data))
}

var errStreamComplete = errors.New("chat stream complete")

// readEvents handles SSE comments, multiline data, and final unterminated
// events. Completion is determined by the provider, never by a clean EOF alone.
func readEvents(ctx context.Context, body io.Reader, handle func(string, []byte) error) error {
	scanner := bufio.NewScanner(io.LimitReader(body, 32<<20))
	scanner.Buffer(make([]byte, 4096), 2<<20)
	var event string
	var data []byte
	firstLine := true
	dispatch := func() error {
		if len(data) == 0 {
			return nil
		}
		err := handle(event, bytes.TrimSuffix(data, []byte{'\n'}))
		event, data = "", nil
		return err
	}
	for scanner.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		line := scanner.Text()
		if firstLine {
			line = strings.TrimPrefix(line, "\ufeff")
			firstLine = false
		}
		if line == "" {
			if err := dispatch(); err != nil {
				return err
			}
			event = ""
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			event = value
		case "data":
			data = append(data, value...)
			data = append(data, '\n')
			if len(data) > 4<<20 {
				return fmt.Errorf("chat stream event exceeded 4 MiB")
			}
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read chat stream: %w", err)
	}
	return dispatch()
}

func assembledCalls(calls map[int]*ToolCall) ([]ToolCall, error) {
	indices := make([]int, 0, len(calls))
	for index := range calls {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	result := make([]ToolCall, 0, len(indices))
	seen := make(map[string]bool)
	for _, index := range indices {
		call := *calls[index]
		if call.Name == "" {
			return nil, fmt.Errorf("model returned a tool call without a name")
		}
		// Some local compatible servers omit IDs. Assign one here so the
		// assistant call and its later result have a consistent wire identity.
		if call.ID == "" || seen[call.ID] {
			call.ID = fmt.Sprintf("wendy_call_%d", index)
			for seen[call.ID] {
				call.ID += "_"
			}
		}
		if len(call.Arguments) == 0 {
			call.Arguments = json.RawMessage(`{}`)
		}
		if !json.Valid(call.Arguments) || !bytes.HasPrefix(bytes.TrimSpace(call.Arguments), []byte{'{'}) {
			return nil, fmt.Errorf("model returned invalid JSON arguments for a tool call")
		}
		seen[call.ID] = true
		result = append(result, call)
	}
	return result, nil
}
