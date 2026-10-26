//go:build linux

package localmesh

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

type refillingControlLink struct {
	heldManifestControlLink
	write func(ControlMessage) error
}

func (l *refillingControlLink) WriteControl(m ControlMessage) error { return l.write(m) }

func TestCatalogueControlPriorityQueuesCannotStarveCancellation(t *testing.T) {
	p := &nodePeer{control: make(chan ControlMessage, 1), catalog: make(chan ControlMessage, 1)}
	p.control <- ControlMessage{Kind: "identity-request", Principal: "ready"}
	p.catalog <- ControlMessage{Kind: "catalog", Catalog: []byte("ready")}
	writes := 0
	budget := errors.New("writer ignored cancellation through sixteen ready frames")
	p.link = &refillingControlLink{write: func(m ControlMessage) error {
		writes++
		if writes == 16 {
			return budget
		}
		// Keep both priority classes ready without scheduling or timing. This
		// models a busy Node/catalogue producer while a link owner is stopping.
		if m.Kind == "catalog" {
			p.catalog <- m
		} else {
			p.control <- m
		}
		return nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := (&Node{}).writeControls(ctx, p); !errors.Is(err, context.Canceled) || writes != 0 {
		t.Fatalf("canceled owner continued priority writes: writes=%d err=%v", writes, err)
	}
}

func TestCatalogueFairnessPreservesQueuedRenewalGuardsAcrossBlockedWrite(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	old := newManifestTestAuthority(t, now).issue(t, now, nil, now.Add(time.Hour))
	current := newManifestTestAuthority(t, now).issue(t, now, nil, now.Add(2*time.Hour))
	allowOld := true
	verify := func(chain [][]byte, at time.Time) (Identity, error) {
		if id, err := current.Verify(chain, at); err == nil {
			return id, nil
		}
		if allowOld {
			return old.Verify(chain, at)
		}
		return Identity{}, errors.New("old issuer retired")
	}
	cache, err := OpenIdentityCache("", DefaultCacheLimits(), verify, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Put(old.Certificate.Certificate, now); err != nil {
		t.Fatal(err)
	}
	directory, err := NewDirectory(old.Device.Tenant(), 8, cache)
	if err != nil {
		t.Fatal(err)
	}
	manifest := routeManifest(old.Device, now)
	manifest.Revision = 1
	manifest.Expires = now.Add(time.Minute).UnixMilli()
	obsolete, err := SignManifest(manifest, old.Certificate.Certificate, old.Signer, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := directory.Accept(obsolete, now); err != nil {
		t.Fatal(err)
	}
	assertWarmRouteAuthorities(t, directory, now, obsolete)
	link := &heldManifestControlLink{entered: make(chan struct{}), release: make(chan struct{}), written: make(chan ControlMessage, 32)}
	p := &nodePeer{link: link, control: make(chan ControlMessage, 16), catalog: make(chan ControlMessage, 4)}
	p.control <- ControlMessage{Kind: "identity-request", Principal: "barrier"}
	n := &Node{cache: cache, directory: directory}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	done := make(chan error, 1)
	go func() { done <- n.writeControls(ctx, p) }()
	defer func() {
		cancel()
		select {
		case <-link.release:
		default:
			close(link.release)
		}
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Error("control writer did not cancel", err)
		}
	}()
	select {
	case <-link.entered:
	case <-ctx.Done():
		t.Fatal("writer did not reach held transport", ctx.Err())
	}
	p.control <- ControlMessage{Kind: "manifest", Manifest: &obsolete}
	p.control <- ControlMessage{Kind: "certificate", Certificate: DescribeCertificates(obsolete.Principal, old.Certificate.Certificate)}
	// No directory/cache lock may span the held transport write. Renew the
	// principal, replace the exact admitted signature, and retain old queue data.
	allowOld = false
	if _, err := cache.Put(current.Certificate.Certificate, now); err != nil {
		t.Fatal(err)
	}
	assertWarmRouteAuthorities(t, directory, now)
	admitted, err := SignManifest(manifest, current.Certificate.Certificate, current.Signer, now)
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := directory.Accept(admitted, now); err != nil || !changed {
		t.Fatal("current signature was not admitted", changed, err)
	}
	assertWarmRouteAuthorities(t, directory, now, admitted)
	p.control <- ControlMessage{Kind: "manifest", Manifest: &admitted}
	for i := range 8 {
		p.control <- ControlMessage{Kind: "identity-request", Principal: fmt.Sprintf("urgent-%d", i)}
	}
	p.control <- ControlMessage{Kind: "identity-request", Principal: "end"}
	for i := range 3 {
		p.catalog <- ControlMessage{Kind: "catalog", Catalog: []byte{byte(i)}}
	}
	close(link.release)
	urgent, catalogues, descriptors, manifests := 0, 0, 0, 0
	for {
		var message ControlMessage
		select {
		case message = <-link.written:
		case <-ctx.Done():
			t.Fatal("merged writer failed to progress", ctx.Err())
		}
		if message.Kind == "catalog" {
			if urgent > 4 || len(message.Catalog) != 1 || int(message.Catalog[0]) != catalogues {
				t.Fatalf("catalogue lost order/fairness after %d urgent writes: %+v", urgent, message)
			}
			catalogues++
			urgent = 0
			continue
		}
		urgent++
		if catalogues < 3 && urgent > 4 {
			t.Fatal("waiting catalogue starved behind urgent controls")
		}
		if message.Kind == "certificate" {
			descriptors++
			if message.Certificate == nil || message.Certificate.Chain == nil || message.Certificate.Chain.Hashes[0] != certificateHash(current.Certificate.Certificate[0]) {
				t.Fatal("fairness bypassed current descriptor normalization")
			}
		}
		if message.Kind == "manifest" {
			manifests++
			if message.Manifest == nil || !bytes.Equal(message.Manifest.Signature, admitted.Signature) {
				t.Fatal("fairness exported an obsolete queued signature")
			}
		}
		if message.Principal == "end" {
			break
		}
	}
	if catalogues != 3 || descriptors != 1 || manifests != 1 {
		t.Fatalf("missing current/fair traffic: catalogues=%d descriptors=%d manifests=%d", catalogues, descriptors, manifests)
	}
}
