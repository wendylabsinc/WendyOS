package command

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/evidence"
)

// Pin requests carry an absolute deadline in a retained file. Repeating the
// command cannot silently generate a new pin identity or extend its lifetime.
func (inv *invocation) runEvidence(args []string, output io.Writer) error {
	if len(args) > 0 {
		switch args[0] {
		case "usage":
			return inv.runEvidenceUsage(args[1:], output)
		case "mirror":
			return inv.runEvidenceMirror(args[1:], output)
		case "fetch":
			return inv.runEvidenceFetch(args[1:], output)
		case "cache":
			return inv.runEvidenceCache(args[1:], output)
		}
	}

	if len(args) == 0 || (args[0] != "pin" && args[0] != "unpin") {
		return errors.New("usage: world evidence usage --credentials FILE --json; world evidence pin --credentials FILE --request FILE --json; world evidence unpin --credentials FILE --id ID --json")
	}
	f := flag.NewFlagSet("evidence "+args[0], flag.ContinueOnError)
	f.SetOutput(inv.stderr)
	credentials := f.String("credentials", "", "private evidence-pin credentials for this capture node")
	requestFile := f.String("request", "", "immutable pin request JSON with ID, digests and absolute deadline")
	id := f.String("id", "", "owned pin ID to release")
	f.Bool("json", true, "emit JSON")
	if inv.parseFlags(f, args[1:]) != nil || f.NArg() != 0 || *credentials == "" {
		return errors.New("explicit credentials and valid evidence flags required")
	}
	if (args[0] == "pin" && (*requestFile == "" || *id != "")) || (args[0] == "unpin" && (*id == "" || *requestFile != "")) {
		return errors.New("pin requires --request; unpin requires --id")
	}
	var cfg client.Config
	if err := supervisionInput(*credentials, true, &cfg); err != nil {
		return errors.New("evidence credentials unavailable or invalid")
	}
	if cfg.Grant.Grant.Domain != "simulation" {
		return errors.New("evidence commands currently support simulation only")
	}
	kind := cfg.Grant.Grant.Kind
	if (kind != "operator" && kind != "app" && kind != "device") || !siteScope(cfg.Grant.Grant.Scopes, "evidence-pin") || cfg.Grant.Grant.PublishAudience != cfg.Audience {
		return errors.New("capture-node-bound evidence-pin authority required")
	}
	c, err := client.New(cfg)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(inv.ctx, 15*time.Second)
	defer cancel()
	var out evidence.PinResponse
	if args[0] == "pin" {
		var request evidence.PinRequest
		if err = supervisionInput(*requestFile, false, &request); err != nil {
			return errors.New("evidence pin request unavailable or invalid")
		}
		out, err = c.PinEvidence(ctx, request)
	} else {
		out, err = c.UnpinEvidence(ctx, evidence.UnpinRequest{ID: *id})
	}
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(out)
}

func (inv *invocation) runEvidenceUsage(args []string, output io.Writer) error {
	f := flag.NewFlagSet("evidence usage", flag.ContinueOnError)
	f.SetOutput(inv.stderr)
	credentials := f.String("credentials", "", "private evidence-read credentials for this node")
	f.Bool("json", true, "emit JSON")
	if err := inv.parseFlags(f, args); err != nil {
		return err
	}
	if f.NArg() != 0 || *credentials == "" {
		return errors.New("evidence usage requires explicit credentials")
	}
	var cfg client.Config
	if err := supervisionInput(*credentials, true, &cfg); err != nil {
		return errors.New("evidence credentials unavailable or invalid")
	}
	if cfg.Grant.Grant.Domain != "simulation" || !siteScope(cfg.Grant.Grant.Scopes, "evidence-read") {
		return errors.New("simulation evidence-read authority required")
	}
	c, err := client.New(cfg)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(inv.ctx, 15*time.Second)
	defer cancel()
	out, err := c.EvidenceUsage(ctx)
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(out)
}
