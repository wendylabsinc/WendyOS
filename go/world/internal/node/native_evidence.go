package node

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

const (
	NativeEvidencePath      = "/v1/native-evidence/get"
	MaxNativeEvidenceRange  = 48 << 10
	MaxNativeEvidenceObject = 8 << 20
)

// NativeEvidenceSource identifies the original native media owner. The HTTP
// gateway does not substitute a result publisher or a standalone evidence store.
type NativeEvidenceSource struct {
	Device        string `json:"device"`
	App           string `json:"app"`
	Service       string `json:"service"`
	ComponentType string `json:"component_type"`
	Stream        string `json:"stream"`
}

type NativeEvidenceRequest struct {
	Source      NativeEvidenceSource `json:"source"`
	SHA256      string               `json:"sha256"`
	Size        int64                `json:"size"`
	ContentType string               `json:"content_type"`
	Offset      int64                `json:"offset"`
	Length      int                  `json:"length"`
}

type NativeEvidenceResponse struct {
	Source            NativeEvidenceSource `json:"source"`
	SHA256            string               `json:"sha256"`
	Size              int64                `json:"size"`
	ContentType       string               `json:"content_type"`
	Offset            int64                `json:"offset"`
	Data              []byte               `json:"data"`
	RetainUntilUnixMS int64                `json:"retain_until_unix_ms"`
}

func (q NativeEvidenceRequest) Validate() error {
	s := q.Source
	if (world.Source{Device: s.Device, App: s.App, Service: s.Service, Stream: s.Stream}).Validate() != nil || s.ComponentType == "" || len(s.ComponentType) > 128 || strings.ContainsAny(s.ComponentType, "*?\x00\r\n") || q.Size < 1 || q.Size > MaxNativeEvidenceObject || q.Offset < 0 || q.Offset >= q.Size || q.Length < 1 || q.Length > MaxNativeEvidenceRange {
		return errors.New("invalid native evidence range")
	}
	b, err := hex.DecodeString(q.SHA256)
	if err != nil || len(b) != 32 || hex.EncodeToString(b) != q.SHA256 {
		return errors.New("invalid native evidence digest")
	}
	switch q.ContentType {
	case "image/png", "image/jpeg", "audio/wav":
	default:
		return errors.New("unsupported native evidence media")
	}
	return nil
}

// ValidateFor checks the exact range contract. The native callback must check
// retention against its authenticated physical clock before returning bytes.
// Clients check the complete object hash and stable retention across ranges.
func (r NativeEvidenceResponse) ValidateFor(q NativeEvidenceRequest) error {
	if q.Validate() != nil || r.Source != q.Source || r.SHA256 != q.SHA256 || r.Size != q.Size || r.ContentType != q.ContentType || r.Offset != q.Offset || int64(len(r.Data)) != min(int64(q.Length), q.Size-q.Offset) || r.RetainUntilUnixMS <= 0 || r.RetainUntilUnixMS > 253402300799999 {
		return errors.New("native evidence response mismatch")
	}
	return nil
}

// NewNativeEvidenceReadHandler reuses signed evidence-read authorization. It
// has no evidence store, replica, signing key or result freshness dependency.
func NewNativeEvidenceReadHandler(cfg ComponentReadConfig, verifier *authority.Verifier, read func(context.Context, NativeEvidenceRequest) (NativeEvidenceResponse, error)) (http.Handler, error) {
	if cfg.RequestTimeout == 0 {
		cfg.RequestTimeout = 5 * time.Second
	}
	if cfg.Tenant == "" || cfg.Site == "" || cfg.Audience == "" || cfg.RequestTimeout < time.Millisecond || cfg.RequestTimeout > 10*time.Second || read == nil || !verifier.MatchesScope(cfg.Tenant, cfg.Site, "simulation", cfg.Audience) {
		return nil, errors.New("explicit simulation evidence configuration required")
	}
	n := &Node{cfg: Config{Tenant: cfg.Tenant, Site: cfg.Site, Audience: cfg.Audience}, verifier: verifier}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.URL.Path != NativeEvidencePath || r.URL.RawPath != "" || r.URL.RawQuery != "" || r.URL.ForceQuery {
			http.NotFound(w, r)
			return
		}
		if !method(w, r, http.MethodPost) {
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8<<10))
		if err != nil {
			http.Error(w, "invalid evidence request", http.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), cfg.RequestTimeout)
		defer cancel()
		r = r.WithContext(ctx)
		if _, ok := n.authorize(w, r, "evidence-read", body); !ok {
			return
		}
		var q NativeEvidenceRequest
		if authority.DecodeStrictLimit(body, &q, 8<<10) != nil || q.Validate() != nil {
			http.Error(w, "invalid evidence request", http.StatusBadRequest)
			return
		}
		reply, err := read(ctx, q)
		if err != nil || ctx.Err() != nil || reply.ValidateFor(q) != nil {
			http.Error(w, "native evidence unavailable", http.StatusServiceUnavailable)
			return
		}
		reply.Data = bytes.Clone(reply.Data)
		writeJSON(w, http.StatusOK, reply)
	}), nil
}
