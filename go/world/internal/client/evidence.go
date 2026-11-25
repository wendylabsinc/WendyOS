package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/evidence"
)

func (c *Client) PutEvidence(ctx context.Context, r evidence.PutRequest) (evidence.PutResponse, error) {
	b, e := json.Marshal(r)
	if e != nil {
		return evidence.PutResponse{}, e
	}
	var out evidence.PutResponse
	e = c.Do(ctx, "evidence-write", "POST", "/v1/evidence/put", b, &out)
	return out, e
}
func (c *Client) GetEvidence(ctx context.Context, r evidence.GetRequest) (evidence.GetResponse, error) {
	b, e := json.Marshal(r)
	if e != nil {
		return evidence.GetResponse{}, e
	}
	var out evidence.GetResponse
	e = c.Do(ctx, "evidence-read", "POST", "/v1/evidence/get", b, &out)
	return out, e
}

// UploadEvidence resumes committed chunks under unchanged owner and metadata.
// RetainUntil may be zero to use the server's bounded default. Once allocated,
// the returned concrete expiry is carried on all further chunks.
func (c *Client) UploadEvidence(ctx context.Context, data []byte, contentType string, retainUntil time.Time) (evidence.PutResponse, error) {
	return c.uploadEvidence(ctx, data, contentType, retainUntil, "")
}

// UploadRollingEvidence explicitly permits quota eviction before the immutable
// retention deadline. A successful receipt does not promise future presence.
// Callers must not cache that deadline as a durable availability guarantee.
func (c *Client) UploadRollingEvidence(ctx context.Context, data []byte, contentType string, retainUntil time.Time) (evidence.PutResponse, error) {
	return c.uploadEvidence(ctx, data, contentType, retainUntil, "rolling")
}

func (c *Client) uploadEvidence(ctx context.Context, data []byte, contentType string, retainUntil time.Time, retentionClass string) (evidence.PutResponse, error) {
	if len(data) < 1 || len(data) > 64<<20 {
		return evidence.PutResponse{}, errors.New("evidence upload outside client bound")
	}
	digest := sha256.Sum256(data)
	hash := hex.EncodeToString(digest[:])
	until := int64(0)
	if !retainUntil.IsZero() {
		until = retainUntil.UnixMilli()
	}
	req := evidence.PutRequest{SHA256: hash, Size: int64(len(data)), ContentType: contentType, RetainUntilUnixMS: until, RetentionClass: retentionClass}
	out, err := c.PutEvidence(ctx, req)
	if err != nil {
		return out, err
	}
	for {
		if out.SHA256 != hash || out.Size != int64(len(data)) || out.ContentType != contentType || out.Received < 0 || out.Received > out.Size || out.Complete != (out.Received == out.Size) || out.RetainUntilUnixMS <= time.Now().UnixMilli() || (until != 0 && out.RetainUntilUnixMS != until) {
			return evidence.PutResponse{}, errors.New("invalid evidence upload acknowledgement")
		}
		if out.Complete {
			return out, nil
		}
		until = out.RetainUntilUnixMS
		req.RetainUntilUnixMS = until
		req.Offset = out.Received
		end := req.Offset + evidence.MaxChunk
		if end > req.Size {
			end = req.Size
		}
		req.Data = data[req.Offset:end]
		previous := out.Received
		out, err = c.PutEvidence(ctx, req)
		if err != nil {
			return out, err
		}
		if out.Received <= previous {
			return evidence.PutResponse{}, errors.New("evidence upload made no progress")
		}
	}
}

// PinEvidence requests an immutable, owner-scoped hold over complete local blobs.
// It requires a separate evidence-pin grant bound to this capture node. An old
// create retried after release returns a released receipt, never a new hold.
func (c *Client) PinEvidence(ctx context.Context, request evidence.PinRequest) (evidence.PinResponse, error) {
	var out evidence.PinResponse
	digests, err := pinDigests(request.SHA256)
	if err != nil || request.ID == "" || len(request.ID) > 256 || request.RetainUntilUnixMS <= time.Now().UnixMilli() {
		return out, errors.New("invalid evidence pin request")
	}
	request.SHA256 = digests
	body, err := json.Marshal(request)
	if err != nil {
		return out, err
	}
	if err = c.Do(ctx, "evidence-pin", "POST", "/v1/evidence/pin", body, &out); err != nil {
		return out, err
	}
	if out.ID != request.ID || out.RetainUntilUnixMS != request.RetainUntilUnixMS || !sameDigests(out.SHA256, digests) || (out.State != "active" && out.State != "released") {
		return out, errors.New("invalid evidence pin acknowledgement")
	}
	if out.State != "active" || out.RetainUntilUnixMS <= time.Now().UnixMilli() {
		return out, errors.New("evidence pin is no longer active")
	}
	return out, nil
}

func (c *Client) UnpinEvidence(ctx context.Context, request evidence.UnpinRequest) (evidence.PinResponse, error) {
	var out evidence.PinResponse
	if request.ID == "" || len(request.ID) > 256 {
		return out, errors.New("invalid evidence pin release")
	}
	body, err := json.Marshal(request)
	if err != nil {
		return out, err
	}
	if err = c.Do(ctx, "evidence-pin", "POST", "/v1/evidence/unpin", body, &out); err != nil {
		return out, err
	}
	digests, err := pinDigests(out.SHA256)
	if err != nil || out.ID != request.ID || out.State != "released" || out.RetainUntilUnixMS <= 0 || !sameDigests(out.SHA256, digests) {
		return out, errors.New("invalid evidence pin release acknowledgement")
	}
	return out, nil
}

func pinDigests(values []string) ([]string, error) {
	if len(values) == 0 || len(values) > evidence.MaxPinDigests {
		return nil, errors.New("invalid evidence pin digest count")
	}
	copy := append([]string(nil), values...)
	sort.Strings(copy)
	for i, d := range copy {
		decoded, err := hex.DecodeString(d)
		if err != nil || len(decoded) != sha256.Size || hex.EncodeToString(decoded) != d || (i > 0 && d == copy[i-1]) {
			return nil, errors.New("invalid evidence pin digest")
		}
	}
	return copy, nil
}

func sameDigests(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// DownloadEvidence validates stable metadata and the complete digest. Range
// calls can be persisted by a caller for cross-process resume; this helper is
// bounded in memory and returns only the completely verified blob.
func (c *Client) DownloadEvidence(ctx context.Context, digest string, maxBytes int64) ([]byte, evidence.GetResponse, error) {
	if maxBytes < 1 || maxBytes > 64<<20 {
		return nil, evidence.GetResponse{}, errors.New("invalid evidence download bound")
	}
	var data []byte
	var first evidence.GetResponse
	for offset := int64(0); ; {
		response, err := c.GetEvidence(ctx, evidence.GetRequest{SHA256: digest, Offset: offset, Length: evidence.MaxChunk})
		if err != nil {
			return nil, first, err
		}
		if response.SHA256 != digest || response.Size < 1 || response.Size > maxBytes || response.Offset != offset || len(response.Data) < 1 || len(response.Data) > evidence.MaxChunk || int64(len(response.Data)) > response.Size-offset || response.RetainUntilUnixMS <= time.Now().UnixMilli() {
			return nil, first, errors.New("invalid evidence chunk response")
		}
		if offset == 0 {
			first = response
			first.Data = nil
		} else if response.Size != first.Size || response.ContentType != first.ContentType || response.RetainUntilUnixMS != first.RetainUntilUnixMS {
			return nil, first, errors.New("evidence metadata changed during transfer")
		}
		data = append(data, response.Data...)
		offset += int64(len(response.Data))
		if offset == response.Size {
			sum := sha256.Sum256(data)
			if hex.EncodeToString(sum[:]) != digest {
				return nil, first, errors.New("evidence download digest mismatch")
			}
			return data, first, nil
		}
	}
}
