// Package a2a implements the text task subset of the A2A 1.0 HTTP+JSON binding.
// Streaming, push notifications, and extended cards are not advertised.
package a2a

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const Version = "1.0"
const (
	Submitted = "TASK_STATE_SUBMITTED"
	Working   = "TASK_STATE_WORKING"
	Completed = "TASK_STATE_COMPLETED"
	Failed    = "TASK_STATE_FAILED"
	Canceled  = "TASK_STATE_CANCELED"
	Rejected  = "TASK_STATE_REJECTED"
)

func Terminal(state string) bool {
	return state == Completed || state == Failed || state == Canceled || state == Rejected
}

type Part struct {
	Text     string         `json:"text"`
	Metadata map[string]any `json:"metadata,omitempty"`
}
type Message struct {
	Metadata         map[string]any `json:"metadata,omitempty"`
	Extensions       []string       `json:"extensions,omitempty"`
	ReferenceTaskIDs []string       `json:"referenceTaskIds,omitempty"`
	MessageID        string         `json:"messageId"`
	Role             string         `json:"role"`
	Parts            []Part         `json:"parts"`
	ContextID        string         `json:"contextId,omitempty"`
	TaskID           string         `json:"taskId,omitempty"`
}
type Status struct {
	State     string    `json:"state"`
	Timestamp time.Time `json:"timestamp"`
	Message   *Message  `json:"message,omitempty"`
}
type Artifact struct {
	ID    string `json:"artifactId"`
	Name  string `json:"name,omitempty"`
	Parts []Part `json:"parts"`
}
type Task struct {
	ID        string     `json:"id"`
	ContextID string     `json:"contextId"`
	Status    Status     `json:"status"`
	Artifacts []Artifact `json:"artifacts,omitempty"`
}
type SendRequest struct {
	Metadata      map[string]any `json:"metadata,omitempty"`
	Message       Message        `json:"message"`
	Configuration struct {
		ReturnImmediately   bool            `json:"returnImmediately"`
		AcceptedOutputModes []string        `json:"acceptedOutputModes,omitempty"`
		HistoryLength       *int            `json:"historyLength,omitempty"`
		Push                json.RawMessage `json:"taskPushNotificationConfig,omitempty"`
	} `json:"configuration,omitempty"`
}
type SendResponse struct {
	Task    *Task    `json:"task,omitempty"`
	Message *Message `json:"message,omitempty"`
}

// MaxResponseBytes bounds client responses and server task-list pages.
const MaxResponseBytes = 1 << 20

type TaskList struct {
	Tasks         []Task `json:"tasks"`
	NextPageToken string `json:"nextPageToken,omitempty"`
	PageSize      int    `json:"pageSize"`
	TotalSize     int    `json:"totalSize"`
}

// ValidateURL requires encrypted transport beyond loopback. An SSH or Wendy
// tunnel may expose a remote agent at a loopback HTTP endpoint.
func ValidateURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
		return errors.New("agent URL must be an absolute URL without credentials, query, or fragment")
	}
	if u.Path != "" && u.Path != "/" {
		return errors.New("agent URL must be an origin without a path prefix")
	}
	if u.Scheme == "https" {
		return nil
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme == "http" && (strings.EqualFold(u.Hostname(), "localhost") || (ip != nil && ip.IsLoopback())) {
		return nil
	}
	return errors.New("remote agents require HTTPS; HTTP is allowed only through a loopback endpoint or tunnel")
}

type Client struct {
	URL, Token string
	HTTP       *http.Client
}

func NewClient(endpoint, token string) (*Client, error) {
	if err := ValidateURL(endpoint); err != nil {
		return nil, err
	}
	if err := ValidateToken(token); err != nil {
		return nil, err
	}
	u, _ := url.Parse(endpoint)
	if u.Scheme == "http" && strings.EqualFold(u.Hostname(), "localhost") {
		port := u.Port()
		u.Host = "127.0.0.1"
		if port != "" {
			u.Host = net.JoinHostPort("127.0.0.1", port)
		}
		endpoint = u.String()
	}
	return &Client{URL: strings.TrimRight(endpoint, "/"), Token: token, HTTP: &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}
func (c *Client) Do(ctx context.Context, method, path string, input, output any) error {
	var body io.Reader
	if input != nil {
		data, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.URL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/a2a+json")
	req.Header.Set("A2A-Version", Version)
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("agent request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("agent returned HTTP %d", resp.StatusCode)
	}
	if output == nil {
		return nil
	}
	return json.NewDecoder(io.LimitReader(resp.Body, MaxResponseBytes)).Decode(output)
}
func (c *Client) Send(ctx context.Context, req SendRequest) (SendResponse, error) {
	var result SendResponse
	err := c.Do(ctx, "POST", "/message:send", req, &result)
	return result, err
}
func (c *Client) Get(ctx context.Context, id string) (Task, error) {
	var task Task
	err := c.Do(ctx, "GET", "/tasks/"+url.PathEscape(id), nil, &task)
	return task, err
}
func (c *Client) Cancel(ctx context.Context, id string) (Task, error) {
	var task Task
	err := c.Do(ctx, "POST", "/tasks/"+url.PathEscape(id)+":cancel", struct{}{}, &task)
	return task, err
}

// Wendy peers propagate depth in task metadata to bound accidental delegation
// cycles. This is a Wendy limit, not a general A2A authorization mechanism.
const DelegationDepthKey = "wendy.delegation_depth"
const MaxDelegationDepth = 4

type depthKey struct{}

func WithDelegationDepth(ctx context.Context, depth int) context.Context {
	return context.WithValue(ctx, depthKey{}, depth)
}
func DelegationDepth(ctx context.Context) int { depth, _ := ctx.Value(depthKey{}).(int); return depth }
func RequestDepth(metadata map[string]any) (int, error) {
	value, ok := metadata[DelegationDepthKey]
	if !ok {
		return 0, nil
	}
	var depth int
	switch v := value.(type) {
	case int:
		depth = v
	case float64:
		depth = int(v)
		if float64(depth) != v {
			return 0, errors.New("invalid delegation depth")
		}
	default:
		return 0, errors.New("invalid delegation depth")
	}
	if depth < 0 || depth > MaxDelegationDepth {
		return 0, errors.New("remote delegation depth exceeds four hops")
	}
	return depth, nil
}

// ValidateToken applies the same credential minimum to callers and listeners.
func ValidateToken(token string) error {
	if len(token) < 16 || strings.ContainsAny(token, "\r\n") {
		return errors.New("agent token must contain at least 16 characters and no newlines")
	}
	return nil
}
