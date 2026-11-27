package livefusion

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/fusioncapture"
	"github.com/wendylabsinc/wendy/go/world/production/serviceaccess"
	"google.golang.org/grpc"
)

func TestNativeMediaCacheFreshOriginalsPreserveLineageAndOwnedBytes(t *testing.T) {
	for _, mode := range []string{"native", "independent", "one-shot"} {
		t.Run(mode, func(t *testing.T) {
			f, api := setup(t, "complete")
			remoteFusionSources(t, f, "camera-b", "audio-1")
			create := New
			if mode == "native" {
				create = NewNative
			}
			s, err := create(f.cfg, api)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			step := func() (Result, error) {
				if mode == "one-shot" {
					return Run(context.Background(), f.cfg, api)
				}
				r, e := s.Step(context.Background())
				return r.Result, e
			}
			first, err := step()
			if err != nil || !first.LocallyVerified {
				t.Fatal("first publication", err)
			}
			// Caller-owned receipts cannot modify bytes retained by the session.
			first.Media[0].Data[0] ^= 1
			freshNativeCaptures(t, f)
			fresh, err := step()
			if err != nil || !fresh.LocallyVerified || f.published != 2 {
				t.Fatal("fresh publication", err)
			}
			for _, media := range fresh.Media {
				wantReads := 2
				if mode == "native" {
					wantReads = 1
				}
				if f.owners[media.Source.Stream] != wantReads || digest(media.Data) != media.Reference.SHA256 {
					t.Fatal("incorrect read count or altered cached bytes", media.Source.Stream, f.owners)
				}
				if (media.Source.Stream == "camera-b" || media.Source.Stream == "audio-1") && media.Source.Device != remoteFusionDevice {
					t.Fatal("remote media owner changed")
				}
			}
			for _, input := range fresh.Inputs {
				if !bytes.Equal(input.EnvelopeJSON, f.proofs[input.Reference.Key.Source.Stream]) {
					t.Fatal("cached media reused a capture proof")
				}
			}
			_, firstRecord, _ := decodeEnvelope(first.SignedRecordJSON)
			_, freshRecord, _ := decodeEnvelope(fresh.SignedRecordJSON)
			a, e1 := fusioncapture.ValidateReplicaPublication(firstRecord.Publication)
			b, e2 := fusioncapture.ValidateReplicaPublication(freshRecord.Publication)
			if e1 != nil || e2 != nil || !a.PhysicalValidUntil.Equal(b.PhysicalValidUntil) || bytes.Equal(first.RequestJSON, fresh.RequestJSON) {
				t.Fatal("original physical bounds changed or old request reused", e1, e2)
			}
			if mode == "native" && f.queries != 2 {
				t.Fatal("cache changed fresh admitted reads", f.queries)
			}
			if mode != "native" && s.mediaCache != nil {
				t.Fatal("independent mode acquired a persistent media cache")
			}
			if err := s.Close(); err != nil || s.mediaCache != nil {
				t.Fatal("close retained media", err)
			}
		})
	}
}

type shortMediaRetentionClient struct {
	appv1.WorldAppServiceClient
	fixture *fixtureServer
}

func (c shortMediaRetentionClient) GetEvidence(ctx context.Context, p *appv1.GetEvidenceRequest, opts ...grpc.CallOption) (*appv1.GetEvidenceReply, error) {
	r, err := c.WorldAppServiceClient.GetEvidence(ctx, p, opts...)
	if err == nil {
		w, _ := c.fixture.clock.Now()
		r.RetainUntilUnixMs = w.Earliest.Add(100 * time.Millisecond).UnixMilli()
	}
	return r, err
}

func TestNativeMediaCacheExpiredRetentionRefetchesAndCaptureExpiryStillWins(t *testing.T) {
	for _, mode := range []string{"media-retention", "capture-expiry"} {
		t.Run(mode, func(t *testing.T) {
			f, api := setup(t, "complete")
			if mode == "media-retention" {
				api = shortMediaRetentionClient{api, f}
			}
			s, err := NewNative(f.cfg, api)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if first, err := s.Step(context.Background()); err != nil || !first.LocallyVerified {
				t.Fatal("first", err)
			}
			freshNativeCaptures(t, f)
			if mode == "media-retention" {
				f.clock.advance(200 * time.Millisecond)
			} else {
				f.clock.advance(6 * time.Second)
			}
			out, err := s.Step(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if mode == "media-retention" {
				if !out.LocallyVerified || f.published != 2 {
					t.Fatal("expired media was not reacquired", out.Status)
				}
				for _, reads := range f.owners {
					if reads != 2 {
						t.Fatal("expired cache entry reused", f.owners)
					}
				}
			} else if out.LocallyVerified || f.published != 1 || out.Status != "insufficient_evidence" {
				t.Fatal("cached bytes extended capture lifetime", out.Status)
			}
		})
	}
}

func TestNativeMediaCacheExactOwnerReferenceAndBounds(t *testing.T) {
	data := []byte("exact immutable media")
	key := mediaCacheKey{source: serviceaccess.Source{Device: "device-a", App: "app", Service: "camera", ComponentType: "capture", Stream: "samples"}, ref: sensing.EvidenceRef{SHA256: digest(data), Size: int64(len(data)), ContentType: "image/png"}}
	c := &nativeMediaCache{}
	c.put(key, data, 1234)
	data[0] ^= 1
	for _, change := range []func(*mediaCacheKey){
		func(k *mediaCacheKey) { k.source.Device += "-other" },
		func(k *mediaCacheKey) { k.source.App += "-other" },
		func(k *mediaCacheKey) { k.source.Service += "-other" },
		func(k *mediaCacheKey) { k.source.ComponentType += "-other" },
		func(k *mediaCacheKey) { k.source.Stream += "-other" },
		func(k *mediaCacheKey) { k.ref.SHA256 = digest([]byte("other")) },
		func(k *mediaCacheKey) { k.ref.Size++ },
		func(k *mediaCacheKey) { k.ref.ContentType = "image/jpeg" },
	} {
		other := key
		change(&other)
		if _, _, ok := c.get(other); ok {
			t.Fatal("cache borrowed another owner or reference")
		}
	}
	got, until, ok := c.get(key)
	if !ok || until != 1234 || digest(got) != key.ref.SHA256 {
		t.Fatal("input mutation or read extended retention")
	}
	got[0] ^= 1
	again, until, _ := c.get(key)
	if digest(again) != key.ref.SHA256 || until != 1234 {
		t.Fatal("read aliases cache or changes retention")
	}
	for i := 0; i < maxNativeMediaEntries; i++ {
		next := key
		next.source.Stream = fmt.Sprint(i)
		c.put(next, again, 1234)
	}
	if len(c.entries) != maxNativeMediaEntries {
		t.Fatal("entry bound changed")
	}
	if _, _, ok := c.get(key); ok {
		t.Fatal("oldest entry was not evicted")
	}
	c = &nativeMediaCache{}
	large := make([]byte, 8<<20)
	for i := 0; i < 3; i++ {
		next := key
		next.source.Stream = fmt.Sprint(i)
		c.put(next, large, 1234)
	}
	if c.bytes != maxNativeMediaBytes || len(c.entries) != 2 {
		t.Fatal("byte bound changed", c.bytes, len(c.entries))
	}
}
