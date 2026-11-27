package client

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/wendylabsinc/wendy/go/world/internal/annotations"
	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

type AnnotationResult struct {
	Result    world.Result    `json:"result"`
	Component world.Component `json:"component"`
}

// NextAnnotationEpoch allocates only through the narrow operator grant. Do not
// call while signed notes remain pending: those records cannot change epoch.
func (c *Client) NextAnnotationEpoch(ctx context.Context) (uint64, error) {
	var response struct {
		Epoch uint64 `json:"epoch"`
	}
	err := c.Do(ctx, "annotation-write", http.MethodPost, "/v1/annotations/epoch", []byte("{}"), &response)
	if err == nil && response.Epoch == 0 {
		err = errors.New("node returned zero annotation epoch")
	}
	return response.Epoch, err
}

// PrepareAnnotation produces the immutable bytes callers must durably retain
// before sending. Retry PublishAnnotation with these same bytes after failure.
func (c *Client) PrepareAnnotation(component world.Component) ([]byte, error) {
	note, err := annotations.Validate(component)
	if err != nil {
		return nil, err
	}
	if err = c.validateClockComponent(note); err != nil {
		return nil, err
	}
	r, err := authority.SignAnnotation(c.config.Grant, ed25519.PrivateKey(c.config.PrivateKey), component)
	if err != nil {
		return nil, err
	}
	return json.Marshal(r)
}

func (c *Client) PublishAnnotation(ctx context.Context, signedRecord []byte) (AnnotationResult, error) {
	return c.publishAnnotation(ctx, signedRecord, func(out any) error {
		return c.Do(ctx, "annotation-write", http.MethodPost, "/v1/annotations", signedRecord, out)
	})
}
func (c *Client) publishAnnotation(ctx context.Context, signedRecord []byte, send func(any) error) (AnnotationResult, error) {
	var result AnnotationResult
	var record authority.SignedRecord
	if err := authority.DecodeStrict(signedRecord, &record); err != nil {
		return result, err
	}
	err := send(&result)
	if err == nil {
		want, wantErr := json.Marshal(record.Component)
		got, gotErr := json.Marshal(result.Component)
		if wantErr != nil || gotErr != nil || !bytes.Equal(want, got) || (result.Result != world.Applied && result.Result != world.Duplicate) {
			err = errors.New("annotation acknowledgment differs from pending signed record")
		}
	}
	return result, err
}
