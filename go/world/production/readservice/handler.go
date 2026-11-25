package readservice

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/wendylabsinc/wendy/go/world/production/jws"
	"github.com/wendylabsinc/wendy/go/world/production/request"
)

type Handler struct {
	cfg        Config
	active     chan struct{}
	mu         sync.Mutex
	principals map[string]int
}

func New(cfg Config) (*Handler, error) {
	if cfg.MaxConcurrent == 0 {
		cfg.MaxConcurrent = 8
	}
	if cfg.MaxPerPrincipal == 0 {
		cfg.MaxPerPrincipal = min(2, cfg.MaxConcurrent)
	}
	if cfg.MaxResponseBytes == 0 {
		cfg.MaxResponseBytes = 1 << 20
	}
	if cfg.RequestTimeout == 0 {
		cfg.RequestTimeout = 5 * time.Second
	}
	if !bounded(cfg.Tenant, 256) || !bounded(cfg.Site, 256) || !bounded(cfg.Audience, 512) || validateDataset(cfg.Dataset) != nil || cfg.Authorizer == nil || cfg.Reader == nil || cfg.MaxConcurrent < 1 || cfg.MaxConcurrent > 128 || cfg.MaxPerPrincipal < 1 || cfg.MaxPerPrincipal > cfg.MaxConcurrent || cfg.MaxResponseBytes < 1024 || cfg.MaxResponseBytes > 4<<20 || cfg.RequestTimeout < time.Millisecond || cfg.RequestTimeout > 30*time.Second {
		return nil, errors.New("explicit bounded read-service configuration required")
	}
	return &Handler{cfg: cfg, active: make(chan struct{}, cfg.MaxConcurrent), principals: map[string]int{}}, nil
}

func fail(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, `{"error":"`+code+`"}`+"\n")
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// No ServeMux canonicalization or redirect can turn another signed target
	// into this route. Even a trailing '?' or percent-encoded path is refused.
	if r.URL == nil || r.URL.Path != request.Path || r.URL.RawPath != "" || r.URL.RawQuery != "" || r.URL.ForceQuery || r.URL.Opaque != "" || r.RequestURI != request.Path {
		fail(w, 404, "unsupported_route")
		return
	}
	if r.Method != request.Method {
		w.Header().Set("Allow", request.Method)
		fail(w, 405, "unsupported_method")
		return
	}
	if r.TLS == nil || r.TLS.Version < tls.VersionTLS13 {
		fail(w, 426, "tls_1_3_required")
		return
	}
	media, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" || len(params) > 1 || (len(params) == 1 && params["charset"] != "utf-8") || r.Header.Get("Content-Encoding") != "" {
		fail(w, 415, "json_envelope_required")
		return
	}
	if r.ContentLength > MaxEnvelopeBytes {
		fail(w, 413, "envelope_too_large")
		return
	}
	select {
	case h.active <- struct{}{}:
		defer func() { <-h.active }()
	default:
		fail(w, 429, "service_busy")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), h.cfg.RequestTimeout)
	defer cancel()
	// These are transport deadlines, never authenticated authority time.
	deadline, _ := ctx.Deadline()
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(deadline)
	_ = controller.SetWriteDeadline(deadline)
	defer func() { _ = controller.SetReadDeadline(time.Time{}); _ = controller.SetWriteDeadline(time.Time{}) }()
	reader := http.MaxBytesReader(w, r.Body, MaxEnvelopeBytes)
	defer reader.Close()
	outer, err := io.ReadAll(reader)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			fail(w, 413, "envelope_too_large")
		} else {
			fail(w, 400, "invalid_envelope")
		}
		return
	}
	envelope, body, err := decodeEnvelope(outer)
	if err != nil {
		fail(w, 400, "invalid_envelope")
		return
	}
	var query Query
	if jws.DecodeStrict(body, &query) != nil || query.Version != 1 || query.DatasetID != h.cfg.Dataset.ID || query.Limit < 1 || query.Limit > MaxComponents {
		fail(w, 400, "invalid_query")
		return
	}
	if ctx.Err() != nil {
		fail(w, 504, "request_deadline")
		return
	}
	decision, err := h.cfg.Authorizer.Authorize(envelope.Proof, r.Method, r.URL.Path, body)
	if err != nil {
		fail(w, 403, "read_authority_denied")
		return
	}
	if decision.Tenant != h.cfg.Tenant || decision.Site != h.cfg.Site || decision.Audience != h.cfg.Audience || decision.Operation != request.OperationRead {
		fail(w, 403, "read_authority_denied")
		return
	}
	if ctx.Err() != nil {
		fail(w, 504, "request_deadline")
		return
	}
	if !h.enterPrincipal(decision.Principal) {
		fail(w, 429, "reader_busy")
		return
	}
	defer h.leavePrincipal(decision.Principal)
	snapshot, err := h.cfg.Reader.Query(ctx, query)
	if ctx.Err() != nil {
		fail(w, 504, "request_deadline")
		return
	}
	if err != nil || validateSnapshot(snapshot, h.cfg.Dataset, query.Limit) != nil {
		fail(w, 503, "dataset_unavailable")
		return
	}
	response := Response{Schema: Schema, Authority: AuthorityScope{Domain: "physical", Tenant: h.cfg.Tenant, Site: h.cfg.Site, Audience: h.cfg.Audience}, Dataset: snapshot.Dataset, Cursor: snapshot.Cursor, Components: snapshot.Components}
	encoded := &limitedBuffer{limit: h.cfg.MaxResponseBytes}
	if err = json.NewEncoder(encoded).Encode(response); err != nil {
		fail(w, 503, "response_budget_exceeded")
		return
	}
	if ctx.Err() != nil {
		fail(w, 504, "request_deadline")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(encoded.Bytes())
}

func (h *Handler) enterPrincipal(principal string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.principals[principal] >= h.cfg.MaxPerPrincipal {
		return false
	}
	h.principals[principal]++
	return true
}
func (h *Handler) leavePrincipal(principal string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.principals[principal]--
	if h.principals[principal] == 0 {
		delete(h.principals, principal)
	}
}

type limitedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.Len() {
		return 0, errors.New("response exceeds budget")
	}
	return b.Buffer.Write(p)
}

// The outer body is deliberately flat. Its limit is larger than a JWS payload
// because it contains both a bounded proof and a base64-encoded raw query body.
func decodeEnvelope(data []byte) (Envelope, []byte, error) {
	var envelope Envelope
	if len(data) == 0 || len(data) > MaxEnvelopeBytes || !utf8.Valid(data) {
		return envelope, nil, errors.New("invalid envelope")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	start, err := d.Token()
	if err != nil || start != json.Delim('{') {
		return envelope, nil, errors.New("envelope object required")
	}
	seen := map[string]bool{}
	for d.More() {
		key, err := d.Token()
		if err != nil {
			return envelope, nil, err
		}
		name, ok := key.(string)
		if !ok || seen[name] || (name != "proof" && name != "body_base64") {
			return envelope, nil, errors.New("duplicate or unknown envelope field")
		}
		seen[name] = true
		var value string
		if err = d.Decode(&value); err != nil {
			return envelope, nil, err
		}
		if name == "proof" {
			envelope.Proof = value
		} else {
			envelope.BodyBase64 = value
		}
	}
	end, err := d.Token()
	if err != nil || end != json.Delim('}') || !seen["proof"] || !seen["body_base64"] {
		return envelope, nil, errors.New("incomplete envelope")
	}
	if _, err = d.Token(); !errors.Is(err, io.EOF) {
		return envelope, nil, errors.New("trailing envelope data")
	}
	if len(envelope.Proof) == 0 || len(envelope.Proof) > jws.MaxProofBytes || len(envelope.BodyBase64) > base64.StdEncoding.EncodedLen(request.MaxBodyBytes) {
		return envelope, nil, errors.New("proof or body exceeds budget")
	}
	body, err := base64.StdEncoding.Strict().DecodeString(envelope.BodyBase64)
	if err != nil || len(body) == 0 || len(body) > request.MaxBodyBytes || base64.StdEncoding.EncodeToString(body) != envelope.BodyBase64 {
		return envelope, nil, errors.New("invalid raw body encoding")
	}
	return envelope, body, nil
}
