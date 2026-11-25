package main

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/gasreplica"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"time"
)

var errOriginalExpired = errors.New("original support expired")

type signedPending struct {
	body      []byte
	component world.Component
}
type publisher struct {
	cfg        client.Config
	api        *client.Client
	epoch, seq uint64
	pending    *signedPending
	last       *signedPending
}

func newPublisher(cfg client.Config) (*publisher, error) {
	c, e := client.New(cfg)
	return &publisher{cfg: cfg, api: c}, e
}
func (p *publisher) version(ctx context.Context) (world.Version, error) {
	if p.epoch == 0 {
		epoch, e := p.api.NextEpoch(ctx)
		if e != nil {
			return world.Version{}, e
		}
		p.epoch = epoch
	}
	p.seq++
	return world.Version{Epoch: p.epoch, Sequence: p.seq}, nil
}
func (p *publisher) prepare(c world.Component) (*signedPending, error) {
	signed, e := authority.SignRecord(p.cfg.Grant, ed25519.PrivateKey(p.cfg.PrivateKey), c, time.Now())
	if e != nil {
		return nil, e
	}
	if e = gasreplica.CheckSignedSize(signed); e != nil {
		return nil, e
	}
	raw, e := json.Marshal(signed)
	return &signedPending{raw, c}, e
}
func (p *publisher) send(ctx context.Context, pending *signedPending) error {
	if gasreplica.Remaining(pending.component, time.Now()) <= 0 {
		return errOriginalExpired
	}
	return p.api.Do(ctx, "publish", "POST", "/v1/components", pending.body, nil)
}
func (p *publisher) retry(ctx context.Context) error {
	if p.pending == nil {
		return nil
	}
	if gasreplica.Remaining(p.pending.component, time.Now()) <= 0 {
		p.pending = nil
		return errOriginalExpired
	}
	if e := p.send(ctx, p.pending); e != nil {
		return e
	}
	p.last = p.pending
	p.pending = nil
	return nil
}
