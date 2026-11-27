package main

import (
	"bytes"
	"errors"
	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"time"
)

type pairTrust struct {
	Root        []byte                      `json:"root_public_key"`
	Revocations authority.SignedRevocations `json:"revocations"`
}

func (r *runtime) refreshTrust() error {
	if r.cfg.TrustPath == "" {
		return nil
	}
	var trust pairTrust
	if err := load(r.cfg.TrustPath, &trust); err != nil {
		return err
	}
	if r.memberVerifier == nil {
		g := r.cfg.Public.Spatial.Gas
		verifier, err := authority.NewVerifier(authority.Config{RootPublicKey: trust.Root, Tenant: g.Tenant, Site: g.Field.Site, Domain: "simulation", Audience: r.cfg.Client.Audience, Now: time.Now, Replay: authority.NewMemoryReplay(4096), MaxBodyBytes: 64 << 10})
		if err != nil {
			return err
		}
		r.memberVerifier = verifier
		r.trustedRoot = append([]byte(nil), trust.Root...)
	} else if !bytes.Equal(r.trustedRoot, trust.Root) {
		return errors.New("paired member trust root changed")
	}
	return r.memberVerifier.UpdateRevocations(trust.Revocations)
}
func (r *runtime) verifyMembers(records []authority.SignedRecord) error {
	if r.memberVerifier == nil {
		return errors.New("paired member verification not commissioned")
	}
	_, err := r.memberVerifier.VerifyRecords(records)
	return err
}
