package command

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/proposals"
)

func (inv *invocation) runProposalCommand(args []string, output io.Writer) error {
	if len(args) == 0 {
		return errors.New("task catalog|status|propose|proposal list|get required")
	}
	command, rest := args[0], args[1:]
	if command == "proposal" {
		if len(rest) == 0 || (rest[0] != "list" && rest[0] != "get") {
			return errors.New("task proposal list|get required")
		}
		command, rest = "proposal "+rest[0], rest[1:]
	}
	f := flag.NewFlagSet("task "+command, flag.ContinueOnError)
	f.SetOutput(inv.stderr)
	credentials := f.String("credentials", "", "private simulation credentials")
	var id, proposalFile, after, origin string
	f.Bool("json", true, "emit JSON")
	if command == "proposal list" || command == "proposal get" {
		f.StringVar(&origin, "origin", "", "explicit origin audience in remote read-only inbox")
	}
	limit := 20
	switch command {
	case "status", "proposal get":
		f.StringVar(&id, "id", "", "retained ID")
	case "propose":
		f.StringVar(&proposalFile, "proposal", "", "non-executable proposal JSON")
	case "proposal list":
		f.IntVar(&limit, "limit", 20, "maximum summaries, 1 to 100")
		f.StringVar(&after, "after", "", "previous page next cursor")
	case "catalog":
	default:
		return errors.New("unknown proposal command")
	}
	if inv.parseFlags(f, rest) != nil || f.NArg() != 0 || *credentials == "" {
		return errors.New("explicit credentials and valid flags required")
	}
	var cfg client.Config
	if err := supervisionInput(*credentials, true, &cfg); err != nil {
		return errors.New("proposal credentials unavailable or invalid")
	}
	if cfg.Grant.Grant.Domain != "simulation" {
		return errors.New("proposal commands currently support simulation only")
	}
	c, err := client.New(cfg)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(inv.ctx, 15*time.Second)
	defer cancel()
	var out any
	switch command {
	case "catalog":
		out, err = c.TaskCatalog(ctx)
	case "status":
		out, err = c.TaskStatus(ctx, id)
	case "proposal list":
		if origin != "" {
			out, err = c.ProposalInbox(ctx, origin, limit, after)
		} else {
			out, err = c.Proposals(ctx, limit, after)
		}
	case "proposal get":
		if origin != "" {
			out, err = c.RemoteProposal(ctx, origin, id)
		} else {
			out, err = c.Proposal(ctx, id)
		}
	case "propose":
		var request proposals.Request
		if err = supervisionInput(proposalFile, false, &request); err != nil {
			return errors.New("proposal file unavailable or invalid")
		}
		if request.Tenant != cfg.Grant.Grant.Tenant || request.Site != cfg.Grant.Grant.Site {
			return errors.New("proposal must match credential tenant and site")
		}
		out, err = c.ProposeTask(ctx, request)
		if err != nil {
			return fmt.Errorf("proposal submission unconfirmed; inspect the same ID before retrying with its original content: %w", err)
		}
	}
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(out)
}
