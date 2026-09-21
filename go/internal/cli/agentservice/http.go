package agentservice

import (
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/cli/a2a"
	"github.com/wendylabsinc/wendy/go/internal/cli/chat"
)

// Handler exposes A2A 1.0 HTTP+JSON task operations plus Wendy's /events ingress.
// The card is public; every operation requires the configured bearer credential.
func (s *Service) Handler(publicURL, token string) (http.Handler, error) {
	if err := a2a.ValidateURL(publicURL); err != nil {
		return nil, err
	}
	if len(token) < 16 || strings.ContainsAny(token, "\r\n") {
		return nil, errors.New("agent token must contain at least 16 characters and no newlines")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/a2a+json")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("A2A-Version", a2a.Version)
		if r.Method == "GET" && r.URL.Path == "/.well-known/agent-card.json" {
			s.card(w, publicURL)
			return
		}
		supplied := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(supplied), []byte(token)) != 1 || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			w.Header().Set("WWW-Authenticate", `Bearer realm="wendy-agent"`)
			writeError(w, 401, 16, "UNAUTHENTICATED", "agent authentication required")
			return
		}
		if r.Header.Get("Origin") != "" {
			writeError(w, 403, 7, "PERMISSION_DENIED", "browser-origin requests are unsupported")
			return
		}
		if version := r.Header.Get("A2A-Version"); version != "" && version != a2a.Version {
			writeError(w, 400, 3, "VERSION_NOT_SUPPORTED", "this endpoint supports A2A 1.0")
			return
		}
		switch {
		case r.Method == "POST" && r.URL.Path == "/message:send":
			var req a2a.SendRequest
			if err := decodeBody(w, r, &req); err != nil {
				writeError(w, 400, 3, "INVALID_ARGUMENT", err.Error())
				return
			}
			if len(req.Configuration.Push) > 0 && string(req.Configuration.Push) != "null" {
				writeError(w, 400, 3, "PUSH_NOTIFICATION_NOT_SUPPORTED", "push notifications are unsupported")
				return
			}
			if req.Configuration.HistoryLength != nil && *req.Configuration.HistoryLength < 0 {
				writeError(w, 400, 3, "INVALID_ARGUMENT", "historyLength cannot be negative")
				return
			}
			modes := req.Configuration.AcceptedOutputModes
			if len(modes) > 0 {
				ok := false
				for _, m := range modes {
					if m == "text/plain" {
						ok = true
					}
				}
				if !ok {
					writeError(w, 400, 3, "CONTENT_TYPE_NOT_SUPPORTED", "only text/plain output is supported")
					return
				}
			}
			task, err := s.Submit(req)
			if err == nil && !req.Configuration.ReturnImmediately {
				task, err = s.Wait(r.Context(), task.ID)
			}
			if err != nil {
				serviceError(w, err)
				return
			}
			writeJSON(w, a2a.SendResponse{Task: &task})
		case r.Method == "POST" && r.URL.Path == "/events":
			var event SensorEvent
			if err := decodeBody(w, r, &event); err != nil {
				writeError(w, 400, 3, "INVALID_ARGUMENT", err.Error())
				return
			}
			result, err := s.Event(event)
			if err != nil {
				serviceError(w, err)
				return
			}
			writeJSON(w, result)
		case r.Method == "GET" && r.URL.Path == "/tasks":
			s.list(w, r)
		case strings.HasPrefix(r.URL.Path, "/tasks/"):
			id := strings.TrimPrefix(r.URL.Path, "/tasks/")
			var task a2a.Task
			var err error
			if r.Method == "POST" && strings.HasSuffix(id, ":cancel") {
				task, err = s.Cancel(strings.TrimSuffix(id, ":cancel"))
			} else if r.Method == "GET" {
				task, err = s.Get(id)
			} else {
				writeError(w, 405, 12, "UNSUPPORTED_OPERATION", "unsupported task operation")
				return
			}
			if err != nil {
				serviceError(w, err)
				return
			}
			writeJSON(w, task)
		default:
			writeError(w, 404, 12, "UNSUPPORTED_OPERATION", "operation is not supported")
		}
	}), nil
}
func decodeBody(w http.ResponseWriter, r *http.Request, out any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return errors.New("invalid or oversized JSON request")
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return errors.New("expected one JSON request")
	}
	return nil
}
func writeJSON(w http.ResponseWriter, v any) { _ = json.NewEncoder(w).Encode(v) }
func writeError(w http.ResponseWriter, status, code int, reason, message string) {
	w.WriteHeader(status)
	writeJSON(w, map[string]any{"code": code, "message": message, "details": []any{map[string]any{"@type": "type.googleapis.com/google.rpc.ErrorInfo", "reason": reason, "domain": "a2a-protocol.org"}}})
}
func serviceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrStorage):
		writeError(w, 500, 13, "INTERNAL", "agent persistence failed")
	case errors.Is(err, ErrNotFound):
		writeError(w, 404, 5, "TASK_NOT_FOUND", err.Error())
	case errors.Is(err, ErrTerminal):
		writeError(w, 409, 9, "TASK_NOT_CANCELABLE", err.Error())
	case errors.Is(err, ErrConflict):
		writeError(w, 409, 6, "ALREADY_EXISTS", err.Error())
	case errors.Is(err, ErrEventRate):
		w.Header().Set("Retry-After", "1")
		writeError(w, 429, 8, "RESOURCE_EXHAUSTED", err.Error())
	case errors.Is(err, ErrFull):
		writeError(w, 429, 8, "RESOURCE_EXHAUSTED", err.Error())
	default:
		writeError(w, 400, 3, "INVALID_ARGUMENT", err.Error())
	}
}
func (s *Service) card(w http.ResponseWriter, publicURL string) {
	profile, _ := chat.ResolveProfile(s.config.Profile)
	writeJSON(w, map[string]any{
		"name": s.config.Name, "description": profile.Description, "version": "1.0.0",
		"supportedInterfaces": []any{map[string]any{"url": strings.TrimRight(publicURL, "/"), "protocolBinding": "HTTP+JSON", "protocolVersion": a2a.Version}},
		"capabilities":        map[string]any{"streaming": false, "pushNotifications": false},
		"defaultInputModes":   []string{"text/plain"}, "defaultOutputModes": []string{"text/plain"},
		"securitySchemes":      map[string]any{"bearer": map[string]any{"httpAuthSecurityScheme": map[string]any{"scheme": "bearer"}}},
		"securityRequirements": []any{map[string]any{"schemes": map[string]any{"bearer": map[string]any{"list": []string{}}}}},
		"skills":               []any{map[string]any{"id": profile.Name, "name": profile.Name, "description": profile.Description, "tags": []string{"wendy", profile.Name}}},
	})
}
func (s *Service) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	size := 50
	var err error
	if q.Get("pageSize") != "" {
		size, err = strconv.Atoi(q.Get("pageSize"))
		if err != nil || size < 1 || size > 100 {
			writeError(w, 400, 3, "INVALID_ARGUMENT", "pageSize must be 1-100")
			return
		}
	}
	tasks := s.Tasks()
	filtered := make([]a2a.Task, 0, len(tasks))
	for _, t := range tasks {
		if q.Get("contextId") != "" && t.ContextID != q.Get("contextId") {
			continue
		}
		if q.Get("status") != "" && t.Status.State != q.Get("status") {
			continue
		}
		filtered = append(filtered, t)
	}
	start := 0
	if token := q.Get("pageToken"); token != "" {
		data, e := base64.RawURLEncoding.DecodeString(token)
		if e != nil {
			writeError(w, 400, 3, "INVALID_ARGUMENT", "invalid page token")
			return
		}
		found := false
		for i, t := range filtered {
			if t.ID == string(data) {
				start = i + 1
				found = true
				break
			}
		}
		if !found {
			writeError(w, 400, 3, "INVALID_ARGUMENT", "page token expired; restart listing")
			return
		}
	}
	end := start + size
	if end > len(filtered) {
		end = len(filtered)
	}
	// Leave room for the envelope and continuation token within the client cap.
	used := 0
	for i := start; i < end; i++ {
		encoded, err := json.Marshal(filtered[i])
		if err != nil || len(encoded) > a2a.MaxResponseBytes-4096 {
			writeError(w, 500, 13, "INTERNAL", "stored task exceeds response limit")
			return
		}
		if used+len(encoded)+1 > a2a.MaxResponseBytes-4096 {
			end = i
			break
		}
		used += len(encoded) + 1
	}
	result := a2a.TaskList{Tasks: filtered[start:end], PageSize: size, TotalSize: len(filtered)}
	if end < len(filtered) {
		result.NextPageToken = base64.RawURLEncoding.EncodeToString([]byte(filtered[end-1].ID))
	}
	writeJSON(w, result)
}
