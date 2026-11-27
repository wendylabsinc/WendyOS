package client

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

// PreparedPublication owns the exact signed record. Its body is not exposed to
// callers, so an uncertain delivery can be retried without re-signing or changing
// the original component, proof deadline or grant. It is an in-process value;
// it does not imply a durable publication queue.
type PreparedPublication struct {
	body     []byte
	grant    [sha256.Size]byte
	audience string
}

func (c *Client) publicationGrant() [sha256.Size]byte {
	b, _ := json.Marshal(c.config.Grant)
	return sha256.Sum256(b)
}

func (c *Client) PreparePublication(component world.Component) (PreparedPublication, error) {
	record, err := authority.SignRecord(c.config.Grant, ed25519.PrivateKey(c.config.PrivateKey), component, time.Now())
	if err != nil {
		return PreparedPublication{}, err
	}
	body, err := json.Marshal(record)
	if err != nil {
		return PreparedPublication{}, err
	}
	if len(body) > 1<<20 {
		return PreparedPublication{}, errors.New("publication exceeds request bound")
	}
	return PreparedPublication{body: body, grant: c.publicationGrant(), audience: c.config.Audience}, nil
}

// PublishPrepared creates fresh transport authorization around unchanged origin
// bytes. The server still checks current authority, the proof and component
// freshness. Callers must discard expired live claims rather than renew them.
func (c *Client) PublishPrepared(ctx context.Context, prepared PreparedPublication) error {
	if len(prepared.body) == 0 || len(prepared.body) > 1<<20 || prepared.grant != c.publicationGrant() || prepared.audience != c.config.Audience {
		return errors.New("prepared publication does not belong to this publisher")
	}
	return c.Do(ctx, "publish", http.MethodPost, "/v1/components", prepared.body, nil)
}

// Transient reports retryable transport and server availability errors. In
// particular a URL wrapper alone is not enough: redirects, bad TLS trust and
// other permanent client errors must not become infinite retry loops.
func Transient(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	var status *HTTPStatusError
	if errors.As(err, &status) {
		return status.StatusCode == http.StatusRequestTimeout || status.StatusCode == http.StatusTooManyRequests || status.StatusCode >= 500
	}
	var wrapped *url.Error
	if errors.As(err, &wrapped) {
		return Transient(wrapped.Err)
	}
	var network net.Error
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.As(err, &network)
}
