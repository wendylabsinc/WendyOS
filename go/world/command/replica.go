package command

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/selection"
	"github.com/wendylabsinc/wendy/go/world/internal/sitepub"
	"github.com/wendylabsinc/wendy/go/world/internal/subscription"
	"io"
	"os"
	"path/filepath"
	"time"
)

func (inv *invocation) runReplica(args []string, output io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: world replica policy --tenant TENANT --site SITE --selection FILE [--out NEWFILE] --json; world replica status --credentials FILE --json")
	}
	switch args[0] {
	case "policy":
		f := flag.NewFlagSet("replica policy", flag.ContinueOnError)
		f.SetOutput(inv.stderr)
		tenant := f.String("tenant", "", "tenant identity")
		site := f.String("site", "", "site identity")
		source := f.String("selection", "", "exact types/entities/sources selection JSON")
		unknown := f.String("unknown-location", "", "regional storage requires retain; unknown locations are not assumed inside")
		out := f.String("out", "", "new immutable policy file; existing files are never overwritten")
		f.Bool("json", true, "emit JSON")
		if err := inv.parseFlags(f, args[1:]); err != nil {
			return err
		}
		if f.NArg() != 0 || *source == "" {
			return errors.New("selection file required; no positional arguments")
		}
		raw, err := queryReadFile(*source, subscription.MaxBytes, false)
		if err != nil {
			return err
		}
		var selected selection.Selection
		if err = authority.DecodeStrict(raw, &selected); err != nil {
			return err
		}
		p, err := (subscription.Policy{UnknownLocation: *unknown, Version: 1, Tenant: *tenant, Site: *site, Selection: selected, RequiredTypes: []string{sitepub.Type}}).Normalize()
		if err != nil {
			return err
		}
		written := ""
		if *out != "" {
			written, err = writeReplicaPolicy(*out, p)
			if err != nil {
				return err
			}
		}
		return json.NewEncoder(output).Encode(struct {
			Policy subscription.Policy `json:"policy"`
			SHA256 string              `json:"policy_sha256"`
			File   string              `json:"file,omitempty"`
		}{p, p.Digest(), written})
	case "status":
		f := flag.NewFlagSet("replica status", flag.ContinueOnError)
		f.SetOutput(inv.stderr)
		credentials := f.String("credentials", "", "private reader credentials")
		f.Bool("json", true, "emit JSON")
		if err := inv.parseFlags(f, args[1:]); err != nil {
			return err
		}
		if f.NArg() != 0 || *credentials == "" {
			return errors.New("credentials required; no positional arguments")
		}
		raw, err := queryReadFile(*credentials, authority.MaxEnvelopeBytes, true)
		if err != nil {
			return err
		}
		var cfg client.Config
		if err = authority.DecodeStrict(raw, &cfg); err != nil {
			return err
		}
		if cfg.Grant.Grant.Domain != "simulation" {
			return errors.New("replica status CLI supports simulation only")
		}
		c, err := client.New(cfg)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(inv.ctx, 15*time.Second)
		defer cancel()
		status, err := c.ReplicaStatus(ctx)
		if err != nil {
			return err
		}
		return json.NewEncoder(output).Encode(status)
	default:
		return errors.New("unknown replica command; use policy or status")
	}
}
func writeReplicaPolicy(path string, p subscription.Policy) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	directory := filepath.Dir(absolute)
	raw, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return "", err
	}
	raw = append(raw, '\n')
	f, err := os.CreateTemp(directory, ".replica-policy-*")
	if err != nil {
		return "", err
	}
	temporary := f.Name()
	defer os.Remove(temporary)
	defer f.Close()
	if _, err = f.Write(raw); err != nil {
		return "", err
	}
	if err = f.Sync(); err != nil {
		return "", err
	}
	if err = f.Close(); err != nil {
		return "", err
	}
	if err = os.Link(temporary, absolute); err != nil {
		return "", err
	}
	dir, err := os.Open(directory)
	if err != nil {
		return "", err
	}
	defer dir.Close()
	if err = dir.Sync(); err != nil {
		return "", err
	}
	return absolute, nil
}
