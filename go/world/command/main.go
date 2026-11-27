// world provides private task, site, package and geographic tools.
package command

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/appsupport"
	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func (inv *invocation) run(args []string) error {
	if len(args) > 0 && args[0] == "area" {
		return inv.runArea(args[1:], inv.stdout)
	}
	if len(args) > 0 && args[0] == "replica" {
		return inv.runReplica(args[1:], inv.stdout)
	}
	if len(args) > 0 && (args[0] == "query" || args[0] == "watch") {
		return inv.runSelectedRead(args, inv.stdout)
	}
	if len(args) > 0 && args[0] == "zone" {
		return inv.runZone(args[1:], inv.stdout)
	}
	if len(args) > 0 && args[0] == "annotation" {
		return inv.runAnnotation(args[1:], inv.stdout)
	}
	if len(args) > 0 && args[0] == "evidence" {
		return inv.runEvidence(args[1:], inv.stdout)
	}
	if len(args) > 0 && args[0] == "package" {
		return inv.runPackage(args[1:], inv.stdout)
	}
	if len(args) > 0 && args[0] == "site" {
		return inv.runSite(args[1:], inv.stdout)
	}
	if len(args) > 0 && args[0] == "frame" {
		return inv.runFrame(args[1:], inv.stdout)
	}
	return inv.runTask(args, inv.stdout)
}

func (inv *invocation) runTask(args []string, output io.Writer) error {
	if len(args) >= 2 && args[0] == "task" {
		switch args[1] {
		case "history":
			return inv.runTaskHistory(args[2:], output)
		case "catalog", "status", "propose", "proposal":
			return inv.runProposalCommand(args[1:], output)
		}
	}
	if len(args) >= 2 && args[0] == "task" && args[1] == "supervise" {
		return inv.runSupervise(args[2:], output)
	}
	if len(args) < 2 || args[0] != "task" {
		return errors.New("usage: world task send|stop|list|supervise --credentials FILE [--spec FILE | --id ID] [--destination AUDIENCE | --relayed] [--json]; world frame convert|locate|project --site FILE [--request FILE | --sample FILE --at RFC3339] --json; world site publish|list|fetch --credentials FILE [--site FILE --run-id RUN | --trust FILE --sha256 DIGEST] --json; world package publish|fetch --credentials FILE [--site FILE --package FILE --run-id RUN | --trust FILE --sha256 DIGEST --out NEWDIR] --json")
	}
	f := flag.NewFlagSet("task "+args[1], flag.ContinueOnError)
	f.SetOutput(inv.stderr)
	credentials := f.String("credentials", "", "private operator credentials")
	specFile := f.String("spec", "", "signed mission specification")
	id := f.String("id", "", "task ID to stop")
	triggerFile := f.String("trigger-file", "", "optional hazard component to bind into signed mission")
	destination := f.String("destination", "", "final assignee audience for durable relay through this gateway")
	relayed := f.Bool("relayed", false, "list the gateway's durable relay entries and last-known signed statuses")
	f.Bool("json", true, "emit JSON")
	if err := inv.parseFlags(f, args[2:]); err != nil {
		return err
	}
	if *credentials == "" || f.NArg() != 0 {
		return errors.New("credentials required; unexpected positional arguments")
	}
	if (*relayed && args[1] != "list") || (*destination != "" && args[1] == "list") {
		return errors.New("--relayed applies to list; --destination applies to send or stop")
	}
	b, err := os.ReadFile(*credentials)
	if err != nil {
		return err
	}
	var cfg client.Config
	if err = authority.DecodeStrict(b, &cfg); err != nil {
		return fmt.Errorf("credentials: %w", err)
	}
	if cfg.Grant.Grant.Domain != "simulation" {
		return errors.New("this client supports simulation only")
	}
	c, err := client.New(cfg)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(inv.ctx, 15*time.Second)
	defer cancel()
	var out json.RawMessage
	if args[1] == "list" {
		path := "/v1/tasks"
		if *relayed {
			path = "/v1/task-relay"
		}
		err = c.Do(ctx, "read", "GET", path, nil, &out)
	} else {
		now := time.Now()
		operation, path, lifetime := "mission", "/v1/tasks", time.Minute
		var payload []byte
		switch args[1] {
		case "send":
			payload, err = os.ReadFile(*specFile)
			if err != nil {
				return err
			}
			var spec tasking.Spec
			if err = authority.DecodeStrict(payload, &spec); err != nil {
				return fmt.Errorf("mission spec: %w", err)
			}
			if *triggerFile != "" {
				if spec.Trigger != nil {
					return errors.New("spec already includes a trigger")
				}
				var component world.Component
				if err = appsupport.ReadJSON(*triggerFile, &component); err != nil {
					return fmt.Errorf("trigger: %w", err)
				}
				trigger, err := tasking.TriggerFromComponent(component)
				if err != nil {
					return err
				}
				spec.Trigger = trigger
				payload, err = json.Marshal(spec)
				if err != nil {
					return err
				}
			}
			now = time.Now()
			lifetime = time.UnixMilli(spec.NotAfterUnixMS).Sub(now) + time.Millisecond
			if spec.NotAfterUnixMS > cfg.Grant.Grant.NotAfterUnixMS {
				return errors.New("mission deadline exceeds operator grant")
			}
		case "stop":
			if *id == "" {
				return errors.New("stop requires --id")
			}
			operation, path = "stop", "/v1/tasks/stop"
			payload, _ = json.Marshal(map[string]string{"task_id": *id})
		default:
			return errors.New("unknown task command")
		}
		audience := cfg.Audience
		if *destination != "" {
			audience, path = *destination, "/v1/task-relay"
		}
		instruction, err := authority.SignInstruction(cfg.Grant, ed25519.PrivateKey(cfg.PrivateKey), audience, operation, payload, now, lifetime)
		if err != nil {
			return err
		}
		body, err := json.Marshal(instruction)
		if err != nil {
			return err
		}
		err = c.Do(ctx, operation, "POST", path, body, &out)
		if err != nil {
			return fmt.Errorf("submit %s: %w", operation, err)
		}
	}
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(output, string(out))
	return err
}
