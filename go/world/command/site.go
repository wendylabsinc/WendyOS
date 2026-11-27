package command

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/sitepub"
)

type siteTrust struct {
	RootPublicKey []byte                      `json:"root_public_key"`
	Revocations   authority.SignedRevocations `json:"revocations"`
}
type siteReply struct {
	Publications []authority.SignedRecord `json:"publications"`
}
type siteFetch struct {
	Publication  *sitepub.Publication   `json:"publication"`
	SignedRecord authority.SignedRecord `json:"signed_record"`
	Verified     bool                   `json:"verified"`
}

func siteScope(scopes []string, want string) bool {
	for _, scope := range scopes {
		if scope == want {
			return true
		}
	}
	return false
}
func siteVerifier(path string, cfg client.Config) (*authority.Verifier, error) {
	raw, err := readGeoFile(path, geoSiteLimit)
	if err != nil {
		return nil, err
	}
	var trust siteTrust
	if err = authority.DecodeStrictLimit(raw, &trust, geoSiteLimit); err != nil {
		return nil, err
	}
	verifier, err := authority.NewVerifier(authority.Config{RootPublicKey: ed25519.PublicKey(trust.RootPublicKey), Tenant: cfg.Grant.Grant.Tenant, Site: cfg.Grant.Grant.Site, Domain: "simulation", Audience: cfg.Audience, Now: time.Now, Replay: authority.NewMemoryReplay(10000)})
	if err != nil {
		return nil, err
	}
	if err = verifier.UpdateRevocations(trust.Revocations); err != nil {
		return nil, err
	}
	return verifier, nil
}
func verifiedSiteRecord(record authority.SignedRecord, verifier *authority.Verifier, now time.Time) (*sitepub.Publication, error) {
	identity, err := verifier.VerifyRecord(record)
	if err != nil {
		return nil, err
	}
	if identity.Kind != "operator" || !identity.Allows("site-publish") {
		return nil, errors.New("site origin lacks operator publication authority")
	}
	publication, err := sitepub.Validate(record.Component)
	if err != nil {
		return nil, err
	}
	c := record.Component
	if now.Before(c.Timing.SampleTime) || !now.Before(publication.ValidUntil) {
		return nil, errors.New("site publication is future or expired")
	}
	age := now.Sub(c.Timing.SampleTime)
	if c.Timing.ErrorBound > c.TTL || age > c.TTL-c.Timing.ErrorBound {
		return nil, errors.New("site publication expired")
	}
	if c.Timing.AgeHint >= c.TTL || age+c.Timing.ErrorBound >= c.TTL {
		return nil, errors.New("site publication expired")
	}
	return publication, nil
}

func (inv *invocation) runSite(args []string, output io.Writer) error {
	if len(args) == 0 || (args[0] != "publish" && args[0] != "list" && args[0] != "fetch") {
		return errors.New("usage: world site publish --credentials FILE --site FILE --run-id RUN [--ttl 30m] --json; world site list|fetch --credentials FILE --trust FILE [--sha256 DIGEST] --json")
	}
	f := flag.NewFlagSet("site "+args[0], flag.ContinueOnError)
	f.SetOutput(inv.stderr)
	credentials := f.String("credentials", "", "operator or viewer credentials")
	document := f.String("site", "", "geographic site document to publish")
	runID := f.String("run-id", "", "simulation run")
	ttl := f.Duration("ttl", 30*time.Minute, "publication lifetime")
	trustPath := f.String("trust", "", "pinned root and current signed revocations")
	digest := f.String("sha256", "", "exact publication digest to fetch")
	jsonOutput := f.Bool("json", true, "emit JSON")
	if err := inv.parseFlags(f, args[1:]); err != nil {
		return err
	}
	if f.NArg() != 0 || *credentials == "" || !*jsonOutput {
		return errors.New("site commands require credentials and JSON output without positional arguments")
	}
	explicitTTL := false
	f.Visit(func(flag *flag.Flag) {
		if flag.Name == "ttl" {
			explicitTTL = true
		}
	})
	if args[0] == "publish" {
		if *document == "" || *runID == "" || *digest != "" || *trustPath != "" || *ttl < time.Millisecond || *ttl > authority.DefaultRecordLifetime {
			return errors.New("publish requires --site and --run-id, TTL in [1ms,24h], and no read flags")
		}
	} else {
		if *trustPath == "" || *document != "" || *runID != "" || explicitTTL {
			return errors.New("site reads require --trust and do not accept publication flags")
		}
		if args[0] == "fetch" && !sensing.DigestValid(*digest) {
			return errors.New("fetch requires a lowercase SHA-256 digest")
		}
		if args[0] == "list" && *digest != "" {
			return errors.New("list does not accept --sha256")
		}
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
		return errors.New("site commands currently support simulation only")
	}
	c, err := client.New(cfg)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(inv.ctx, 15*time.Second)
	defer cancel()
	if args[0] == "publish" {
		g := cfg.Grant.Grant
		if g.Kind != "operator" || !siteScope(g.Scopes, "publish") || !siteScope(g.Scopes, "site-publish") {
			return errors.New("site publishing requires an operator with publish and site-publish grants")
		}
		raw, err = readGeoFile(*document, geoSiteLimit)
		if err != nil {
			return err
		}
		config := sitepub.Config{Tenant: g.Tenant, Site: g.Site, RunID: *runID, Source: g.Source, Epoch: 1, Sequence: 1, Now: time.Now(), TTL: *ttl}
		component, err := sitepub.Build(raw, config)
		if err != nil {
			return err
		}
		config.Epoch, err = c.NextEpoch(ctx)
		if err != nil {
			return err
		}
		config.Now = time.Now()
		component, err = sitepub.Build(raw, config)
		if err != nil {
			return err
		}
		if err = c.Publish(ctx, component); err != nil {
			return err
		}
		publication, err := sitepub.Validate(component)
		if err != nil {
			return err
		}
		return json.NewEncoder(output).Encode(struct {
			Publication *sitepub.Publication `json:"publication"`
			Epoch       uint64               `json:"epoch"`
			Published   bool                 `json:"published"`
		}{publication, config.Epoch, true})
	}
	verifier, err := siteVerifier(*trustPath, cfg)
	if err != nil {
		return fmt.Errorf("site trust: %w", err)
	}
	var rawReply json.RawMessage
	if err = c.Do(ctx, "read", http.MethodGet, "/v1/sites", nil, &rawReply); err != nil {
		return err
	}
	var reply siteReply
	if err = authority.DecodeStrictLimit(rawReply, &reply, geoSiteLimit); err != nil {
		return err
	}
	if reply.Publications == nil || len(reply.Publications) > 10000 {
		return errors.New("invalid site publication list")
	}
	var selected *siteFetch
	for _, record := range reply.Publications {
		publication, err := verifiedSiteRecord(record, verifier, time.Now())
		if err != nil {
			return fmt.Errorf("site origin rejected: %w", err)
		}
		if publication.SHA256 == *digest {
			if selected != nil && !bytes.Equal(selected.Publication.Document, publication.Document) {
				return errors.New("conflicting content under publication digest")
			}
			if selected == nil {
				selected = &siteFetch{Publication: publication, SignedRecord: record, Verified: true}
			}
		}
	}
	if args[0] == "fetch" {
		if selected == nil {
			return errors.New("requested site publication is unavailable")
		}
		return json.NewEncoder(output).Encode(selected)
	}
	return json.NewEncoder(output).Encode(struct {
		Publications []authority.SignedRecord `json:"publications"`
		Verified     bool                     `json:"verified"`
	}{reply.Publications, true})
}
