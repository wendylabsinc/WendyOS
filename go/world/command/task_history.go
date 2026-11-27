package command

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"io"
	"time"
)

func (inv *invocation) runTaskHistory(args []string, out io.Writer) error {
	f := flag.NewFlagSet("task history", flag.ContinueOnError)
	f.SetOutput(inv.stderr)
	credentials := f.String("credentials", "", "reader credentials")
	origin := f.String("origin", "", "explicit assignee origin")
	id := f.String("id", "", "optional exact task ID")
	after := f.String("after", "", "retained scoped cursor")
	limit := f.Int("limit", 20, "at most 100 historical events")
	f.Bool("json", true, "JSON")
	if inv.parseFlags(f, args) != nil || f.NArg() != 0 || *credentials == "" || *origin == "" {
		return errors.New("usage: world task history --credentials FILE --origin AUDIENCE [--id TASK --after CURSOR --limit 20] --json")
	}
	var cfg client.Config
	if e := supervisionInput(*credentials, true, &cfg); e != nil {
		return e
	}
	c, e := client.New(cfg)
	if e != nil {
		return e
	}
	ctx, cancel := context.WithTimeout(inv.ctx, 10*time.Second)
	defer cancel()
	v, e := c.TaskHistory(ctx, *origin, *id, *limit, *after)
	if e != nil {
		return e
	}
	return json.NewEncoder(out).Encode(v)
}
