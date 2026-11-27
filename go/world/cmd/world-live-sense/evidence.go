package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	worldclient "github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/evidence"
	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
)

type evidenceClient interface {
	UploadRollingEvidence(context.Context, []byte, string, time.Time) (evidence.PutResponse, error)
	DownloadEvidence(context.Context, string, int64) ([]byte, evidence.GetResponse, error)
}

// Upload first so ordinary write-only publishers need no read permission.
// Another principal may already own the digest, or win allocation concurrently.
// Reuse requires a complete verified read and preserves its existing expiry.
func storeEvidence(ctx context.Context, client evidenceClient, ref sensing.EvidenceRef, data []byte, retainUntil time.Time) (int64, error) {
	if err := ref.Validate(); err != nil {
		return 0, err
	}
	sum := sha256.Sum256(data)
	if int64(len(data)) != ref.Size || hex.EncodeToString(sum[:]) != ref.SHA256 {
		return 0, errors.New("publication evidence bytes differ from reference")
	}
	if retainUntil.IsZero() || !time.Now().Before(retainUntil) {
		return 0, errors.New("publication evidence retention expired")
	}
	result, uploadErr := client.UploadRollingEvidence(ctx, data, ref.ContentType, retainUntil)
	if uploadErr == nil {
		if result.SHA256 != ref.SHA256 || result.Size != ref.Size || result.ContentType != ref.ContentType || !result.Complete || result.Received != ref.Size || result.RetainUntilUnixMS <= time.Now().UnixMilli() {
			return 0, errors.New("invalid publication evidence acknowledgement")
		}
		return result.RetainUntilUnixMS, nil
	}
	var status *worldclient.HTTPStatusError
	if errors.As(uploadErr, &status) && (status.StatusCode == 401 || status.StatusCode == 403) {
		return 0, uploadErr
	}
	got, meta, readErr := client.DownloadEvidence(ctx, ref.SHA256, ref.Size)
	if readErr != nil {
		// Keep the write failure authoritative for write-only callers. A failed
		// optional read must not turn an ownership conflict into a permission loss.
		return 0, uploadErr
	}
	if meta.SHA256 != ref.SHA256 || meta.Size != ref.Size || meta.ContentType != ref.ContentType || meta.RetainUntilUnixMS <= time.Now().UnixMilli() || !bytes.Equal(got, data) {
		return 0, errors.New("existing publication evidence differs or expired")
	}
	return meta.RetainUntilUnixMS, nil
}
