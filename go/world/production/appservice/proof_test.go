package appservice

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/proofref"
	"github.com/wendylabsinc/wendy/go/world/production/serviceaccess"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
)

func TestHistoricalProofRequiresFenceAndExactReturnedBytes(t *testing.T) {
	// The broker treats these as opaque bytes. Actual proof cryptography and
	// history storage are exercised by proofarchive and agentmodule tests.
	original := []byte("original archived envelope")
	sum := sha256.Sum256(original)
	request := &appv1.GetRecordProofRequest{Source: brokerAccessQuery().Sources[0], Entity: "observation", Epoch: 3, Sequence: 8, EnvelopeSha256: hex.EncodeToString(sum[:]), RecordSha256: strings.Repeat("b", 64)}
	var mode atomic.Int32
	f := setup(t, func(cfg *Config) {
		cfg.GetRecordProof = func(ctx context.Context, scope serviceorigin.Scope, ref proofref.Reference, fence *Fence) (serviceaccess.RecordProof, error) {
			if ref.Key.Tenant != scope.Tenant || ref.Key.Site != scope.Site || ref.Key.Source.Device != scope.Device || ref.Key.Source.Service != request.Source.Service || ref.Version.Sequence != request.Sequence || ref.RecordSHA256 != request.RecordSha256 {
				return serviceaccess.RecordProof{}, ErrAdmission
			}
			out := serviceaccess.RecordProof{Envelope: bytes.Clone(original), RetainUntilUnixMS: 1800000000000}
			if mode.Load() == 0 {
				return out, nil
			}
			if err := fence.Commit(ctx, func(context.Context) error { return nil }); err != nil {
				return serviceaccess.RecordProof{}, err
			}
			if mode.Load() == 2 {
				out.Envelope[0]++
			}
			return out, nil
		}
	})
	mount := f.mount(t, f.scope)
	client, _ := dial(t, mount.SocketPath)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if out, err := client.GetRecordProof(ctx, request); err == nil || out != nil {
		t.Fatal("unfenced archive read escaped")
	}
	mode.Store(1)
	out, err := client.GetRecordProof(ctx, request)
	if err != nil || !out.Historical || !bytes.Equal(out.SignedRecordJson, original) || out.RetainUntilUnixMs != 1800000000000 {
		t.Fatal("historical exact bytes or label changed", err)
	}
	mode.Store(2)
	if out, err := client.GetRecordProof(ctx, request); err == nil || out != nil {
		t.Fatal("different envelope returned for exact hash")
	}
}
