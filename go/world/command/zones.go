package command

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/sitepub"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/internal/zonepub"
	"io"
	"time"
)

func (inv *invocation) runZone(args []string, output io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: world zone publish|list|fetch|delete --credentials FILE [--catalog FILE | --trust FILE --sha256 DIGEST --source FILE] --json")
	}
	verb := args[0]
	if verb != "publish" && verb != "list" && verb != "fetch" && verb != "delete" {
		return errors.New("unknown zone command")
	}
	f := flag.NewFlagSet("zone "+verb, flag.ContinueOnError)
	f.SetOutput(inv.stderr)
	credentials := f.String("credentials", "", "simulation operator or reader credentials")
	catalog := f.String("catalog", "", "zone catalog JSON")
	trust := f.String("trust", "", "pinned root and current revocations")
	digest := f.String("sha256", "", "exact catalog digest")
	sourceFile := f.String("source", "", "exact source identity JSON")
	ttl := f.Duration("ttl", 30*time.Minute, "publication or tombstone lifetime")
	jsonOut := f.Bool("json", true, "emit JSON")
	if err := inv.parseFlags(f, args[1:]); err != nil {
		return err
	}
	ttlSet := false
	f.Visit(func(v *flag.Flag) {
		if v.Name == "ttl" {
			ttlSet = true
		}
	})
	if f.NArg() != 0 || *credentials == "" || !*jsonOut {
		return errors.New("credentials and JSON output required")
	}
	if verb == "publish" {
		if *catalog == "" || *trust != "" || *digest != "" || *sourceFile != "" {
			return errors.New("publish requires only catalog and optional TTL")
		}
	} else {
		if *trust == "" || *catalog != "" {
			return errors.New("zone reads/deletes require pinned trust and no catalog")
		}
		if verb == "list" {
			if *digest != "" || *sourceFile != "" || ttlSet {
				return errors.New("list does not select a revision or accept TTL")
			}
		} else if !zonepub.ValidDigest(*digest) || *sourceFile == "" {
			return errors.New("fetch/delete requires exact digest and source")
		}
		if verb == "fetch" && ttlSet {
			return errors.New("fetch does not accept TTL")
		}
	}
	if *ttl < time.Millisecond || *ttl > authority.DefaultRecordLifetime {
		return errors.New("TTL must be in [1ms,24h]")
	}
	raw, err := readGeoFile(*credentials, authority.MaxEnvelopeBytes)
	if err != nil {
		return err
	}
	var cfg client.Config
	if err = authority.DecodeStrict(raw, &cfg); err != nil {
		return err
	}
	if cfg.Grant.Grant.Domain != "simulation" {
		return errors.New("zone commands support simulation only")
	}
	c, err := client.New(cfg)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(inv.ctx, 15*time.Second)
	defer cancel()
	if verb == "publish" || verb == "delete" {
		g := cfg.Grant.Grant
		if g.Kind != "operator" || !siteScope(g.Scopes, "publish") || !siteScope(g.Scopes, "site-publish") {
			return errors.New("zone publication requires operator publish and site-publish authority")
		}
	}
	if verb == "publish" {
		raw, err = readGeoFile(*catalog, zonepub.MaxBytes)
		if err != nil {
			return err
		}
		doc, _, err := zonepub.Decode(raw)
		if err != nil {
			return err
		}
		config := sitepub.Config{Tenant: cfg.Grant.Grant.Tenant, Site: cfg.Grant.Grant.Site, RunID: doc.RunID, Source: cfg.Grant.Grant.Source, Epoch: 1, Sequence: 1, Now: time.Now(), TTL: *ttl}
		if _, err = zonepub.Build(raw, config); err != nil {
			return err
		}
		config.Epoch, err = c.NextEpoch(ctx)
		if err != nil {
			return err
		}
		config.Now = time.Now()
		component, err := zonepub.Build(raw, config)
		if err != nil {
			return err
		}
		if err = c.Publish(ctx, component); err != nil {
			return err
		}
		p, _ := zonepub.Validate(component)
		return json.NewEncoder(output).Encode(struct {
			Publication *zonepub.Publication `json:"publication"`
			Source      world.Source         `json:"source"`
			Epoch       uint64               `json:"epoch"`
			Published   bool                 `json:"published"`
		}{p, config.Source, config.Epoch, true})
	}
	v, err := siteVerifier(*trust, cfg)
	if err != nil {
		return err
	}
	reply, err := c.Zones(ctx, v)
	if err != nil {
		return err
	}
	if verb == "list" {
		return json.NewEncoder(output).Encode(struct {
			zonepub.Reply
			Verified bool   `json:"verified"`
			Scope    string `json:"scope"`
		}{reply, true, "this_replica_only"})
	}
	var source world.Source
	raw, err = readGeoFile(*sourceFile, 4096)
	if err != nil {
		return err
	}
	if err = authority.DecodeStrict(raw, &source); err != nil {
		return err
	}
	selected, err := client.SelectZone(reply, *digest, source, time.Now())
	if err != nil {
		return err
	}
	if verb == "fetch" {
		return json.NewEncoder(output).Encode(selected)
	}
	if source != cfg.Grant.Grant.Source {
		return errors.New("cannot delete another source's revision")
	}
	if selected.Record.Component.Tombstone {
		return json.NewEncoder(output).Encode(selected)
	}
	epoch, err := c.NextEpoch(ctx)
	if err != nil {
		return err
	}
	component, err := zonepub.Delete(selected.Record.Component, epoch, time.Now(), *ttl)
	if err != nil {
		return err
	}
	if err = c.Publish(ctx, component); err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(struct {
		SHA256  string       `json:"sha256"`
		Source  world.Source `json:"source"`
		Deleted bool         `json:"deleted"`
		Epoch   uint64       `json:"epoch"`
	}{*digest, source, true, epoch})
}
