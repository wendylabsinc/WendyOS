package node

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/evidence"
)

func (n *Node) evidenceRequest(w http.ResponseWriter, r *http.Request, operation string) ([]byte, authority.Identity, bool) {
	if n.cfg.Evidence == nil {
		http.Error(w, "evidence service unavailable", 404)
		return nil, authority.Identity{}, false
	}
	if !method(w, r, http.MethodPost) {
		return nil, authority.Identity{}, false
	}
	body, ok := readBody(w, r)
	if !ok {
		return nil, authority.Identity{}, false
	}
	id, ok := n.authorize(w, r, operation, body)
	return body, id, ok
}
func (n *Node) evidencePut(w http.ResponseWriter, r *http.Request) {
	body, id, ok := n.evidenceRequest(w, r, "evidence-write")
	if !ok {
		return
	}
	var req evidence.PutRequest
	if err := authority.DecodeStrictLimit(body, &req, MaxBodyBytes); err != nil {
		http.Error(w, "invalid evidence chunk", 400)
		return
	}
	result, err := n.cfg.Evidence.Put(id.Principal, req)
	if err != nil {
		status := 409
		if errors.Is(err, evidence.ErrQuota) {
			status = 507
		}
		http.Error(w, err.Error(), status)
		return
	}
	writeJSON(w, 200, result)
}
func (n *Node) evidenceGet(w http.ResponseWriter, r *http.Request) {
	body, _, ok := n.evidenceRequest(w, r, "evidence-read")
	if !ok {
		return
	}
	var req evidence.GetRequest
	if err := authority.DecodeStrict(body, &req); err != nil {
		http.Error(w, "invalid evidence range", 400)
		return
	}
	result, err := n.cfg.Evidence.Get(req)
	if err != nil {
		status := 409
		if errors.Is(err, evidence.ErrUnavailable) {
			status = 404
		}
		http.Error(w, err.Error(), status)
		return
	}
	writeJSON(w, 200, result)
}

func (n *Node) evidencePin(w http.ResponseWriter, r *http.Request) {
	body, id, ok := n.evidenceRequest(w, r, "evidence-pin")
	if !ok {
		return
	}
	var req evidence.PinRequest
	if err := authority.DecodeStrict(body, &req); err != nil {
		http.Error(w, "invalid evidence pin", 400)
		return
	}
	result, err := n.cfg.Evidence.Pin(id.Principal, req)
	evidencePinResult(w, result, err)
}

func (n *Node) evidenceUnpin(w http.ResponseWriter, r *http.Request) {
	body, id, ok := n.evidenceRequest(w, r, "evidence-pin")
	if !ok {
		return
	}
	var req evidence.UnpinRequest
	if err := authority.DecodeStrict(body, &req); err != nil {
		http.Error(w, "invalid evidence pin release", 400)
		return
	}
	result, err := n.cfg.Evidence.Unpin(id.Principal, req)
	evidencePinResult(w, result, err)
}

func evidencePinResult(w http.ResponseWriter, result evidence.PinResponse, err error) {
	if err != nil {
		status := http.StatusConflict
		if errors.Is(err, evidence.ErrUnavailable) {
			status = http.StatusNotFound
		} else if errors.Is(err, evidence.ErrQuota) {
			status = http.StatusInsufficientStorage
		}
		http.Error(w, err.Error(), status)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// Usage exposes tenant/site accounting to the same explicit evidence readers
// who can retrieve blobs. It does not disclose object IDs, owners or pin IDs.
func (n *Node) evidenceUsage(w http.ResponseWriter, r *http.Request) {
	body, _, ok := n.evidenceRequest(w, r, "evidence-read")
	if !ok {
		return
	}
	var empty map[string]json.RawMessage
	if err := authority.DecodeStrict(body, &empty); err != nil || empty == nil || len(empty) != 0 {
		http.Error(w, "usage requires an empty JSON object", 400)
		return
	}
	out, err := n.cfg.Evidence.Usage()
	if err != nil {
		http.Error(w, "evidence usage unavailable", 503)
		return
	}
	writeJSON(w, 200, out)
}
