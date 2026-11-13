//go:build linux

package localmesh

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"io"
	"testing"
	"time"
)

type heldManifestControlLink struct {
	entered, release chan struct{}
	written          chan ControlMessage
}

func (l *heldManifestControlLink) WriteControl(m ControlMessage) error {
	if m.Principal == "barrier" {
		close(l.entered)
		<-l.release
	}
	l.written <- m
	return nil
}
func (*heldManifestControlLink) Close() error { return nil }
func (*heldManifestControlLink) ReadControl() (ControlMessage, error) {
	return ControlMessage{}, io.EOF
}
func (*heldManifestControlLink) SendDatagram([]byte) error                       { return nil }
func (*heldManifestControlLink) ReceiveDatagram(context.Context) ([]byte, error) { return nil, io.EOF }

func TestQueuedManifestRenewalDropsOldWireAndKeepsLinkUsable(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	old := newManifestTestAuthority(t, now).issue(t, now, nil, now.Add(time.Hour))
	renewed := newManifestTestAuthority(t, now).issue(t, now, nil, now.Add(2*time.Hour))
	allowOld := true
	verify := func(chain [][]byte, at time.Time) (Identity, error) {
		if id, err := renewed.Verify(chain, at); err == nil {
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
	if _, err = cache.Put(old.Certificate.Certificate, now); err != nil {
		t.Fatal(err)
	}
	d, err := NewDirectory(old.Device.Tenant(), 10, cache)
	if err != nil {
		t.Fatal(err)
	}
	m := routeManifest(old.Device, now)
	wire, err := SignManifest(m, old.Certificate.Certificate, old.Signer, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.Accept(wire, now); err != nil {
		t.Fatal(err)
	}
	link := &heldManifestControlLink{entered: make(chan struct{}), release: make(chan struct{}), written: make(chan ControlMessage, 8)}
	p := &nodePeer{link: link, control: make(chan ControlMessage, 8)}
	p.control <- ControlMessage{Kind: "identity-request", Principal: "barrier"}
	p.control <- ControlMessage{Kind: "manifest", Manifest: &wire}
	p.control <- ControlMessage{Kind: "identity-request", Principal: "after-old"}
	n := &Node{directory: d}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- n.writeControls(ctx, p) }()
	defer func() {
		select {
		case <-link.release:
		default:
			close(link.release)
		}
	}()
	select {
	case <-link.entered:
	case <-time.After(time.Second):
		t.Fatal("writer did not reach held frame")
	}
	// No trust/directory lock is held over the transport write. Channel release
	// orders retirement before the worker can inspect the queued manifest.
	allowOld = false
	if _, err = cache.Put(renewed.Certificate.Certificate, now); err != nil {
		t.Fatal(err)
	}
	current, err := SignManifest(m, renewed.Certificate.Certificate, renewed.Signer, now)
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := d.Accept(current, now); err != nil || !changed {
		t.Fatal(changed, err)
	}
	p.control <- ControlMessage{Kind: "manifest", Manifest: &current}
	close(link.release)
	for i, want := range []string{"barrier", "after-old", "current"} {
		select {
		case got := <-link.written:
			if want == "current" {
				if got.Kind != "manifest" || got.Manifest == nil || string(got.Manifest.Signature) != string(current.Signature) {
					t.Fatalf("write%d did not carry renewed signature", i)
				}
			} else if got.Principal != want {
				t.Fatalf("obsolete queued manifest was written before %s", want)
			}
		case <-time.After(time.Second):
			t.Fatal("current control traffic did not progress")
		}
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("control worker did not cancel")
	}
}

func TestQueuedSameKeyRenewalUsesCurrentFetchableDescriptor(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	old := newManifestTestAuthority(t, now).issue(t, now, nil, now.Add(time.Hour))
	renewed := newManifestTestAuthority(t, now).issue(t, now, old.Signer.(*ecdsa.PrivateKey), now.Add(2*time.Hour))
	allowOld := true
	verify := func(chain [][]byte, at time.Time) (Identity, error) {
		if id, err := renewed.Verify(chain, at); err == nil {
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
	if _, err = cache.Put(old.Certificate.Certificate, now); err != nil {
		t.Fatal(err)
	}
	d, err := NewDirectory(old.Device.Tenant(), 10, cache)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := SignManifest(routeManifest(old.Device, now), old.Certificate.Certificate, old.Signer, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.Accept(wire, now); err != nil {
		t.Fatal(err)
	}
	sender := NewSynchronizer(d, cache)
	defer sender.Close()
	link := &heldManifestControlLink{entered: make(chan struct{}), release: make(chan struct{}), written: make(chan ControlMessage, 8)}
	p := &nodePeer{link: link, control: make(chan ControlMessage, 8)}
	p.control <- ControlMessage{Kind: "identity-request", Principal: "barrier"}
	for _, message := range sender.Record(wire, now) {
		p.control <- message
	}
	p.control <- ControlMessage{Kind: "identity-request", Principal: "end"}
	n := &Node{directory: d, cache: cache}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- n.writeControls(ctx, p) }()
	defer func() {
		select {
		case <-link.release:
		default:
			close(link.release)
		}
	}()
	select {
	case <-link.entered:
	case <-time.After(time.Second):
		t.Fatal("writer did not reach held frame")
	}
	allowOld = false
	if _, err = cache.Put(renewed.Certificate.Certificate, now); err != nil {
		t.Fatal(err)
	}
	close(link.release)
	var messages []ControlMessage
	for {
		var got ControlMessage
		select {
		case got = <-link.written:
		case <-time.After(time.Second):
			t.Fatal("same-key renewal blocked control progress")
		}
		if got.Principal == "barrier" {
			continue
		}
		if got.Principal == "end" {
			break
		}
		messages = append(messages, got)
	}
	if len(messages) != 2 || messages[0].Certificate == nil || messages[0].Certificate.Chain == nil || messages[0].Certificate.Chain.Hashes[0] != certificateHash(renewed.Certificate.Certificate[0]) {
		t.Fatal("queued descriptor still names the retired issuer chain")
	}
	coldCache, err := OpenIdentityCache("", DefaultCacheLimits(), renewed.Verify, now)
	if err != nil {
		t.Fatal(err)
	}
	cold, err := NewDirectory(renewed.Device.Tenant(), 10, coldCache)
	if err != nil {
		t.Fatal(err)
	}
	receiver := NewSynchronizer(cold, coldCache)
	defer receiver.Close()
	if changed := exchangeDirectoryMessages(t, sender, receiver, messages, now); len(changed) != 1 || len(cold.Snapshot(now)) != 1 {
		t.Fatal("same-key old signature did not reach cold receiver with current chain")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("control worker did not cancel")
	}
}
