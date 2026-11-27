package serviceorigin

import (
	"context"
	"testing"
)

func TestEmptyPublicationScopeIsAValidNoWriteLease(t *testing.T) {
	e := setup(t)
	cfg := e.cfg
	cfg.Scope.Publish = nil
	p, err := NewPublisher(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if err = p.Validate(context.Background()); err != nil {
		t.Fatal("read-only authority cannot validate", err)
	}
	if _, err = p.Publish(context.Background(), e.request); err == nil {
		t.Fatal("read-only scope signed publication")
	}
	if p.sequence != 0 {
		t.Fatal("denied no-write request consumed a version")
	}
}
