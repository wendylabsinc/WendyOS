package command

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"syscall"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/selection"
)

func queryReadFile(path string, limit int64, private bool) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !stat.Mode().IsRegular() || (private && stat.Mode().Perm()&0077 != 0) {
		return nil, errors.New("read configuration must be a regular file; credentials must be private")
	}
	if stat.Size() > limit {
		return nil, errors.New("read configuration exceeds byte limit")
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("read configuration exceeds byte limit")
	}
	return data, nil
}

// runSelectedRead reads the current replica only. It never installs a package,
// changes replication membership, signs a task or grants physical execution.
func (inv *invocation) runSelectedRead(args []string, output io.Writer) error {
	if len(args) == 0 || (args[0] != "query" && args[0] != "watch") {
		return errors.New("usage: world query|watch --credentials FILE [--selection FILE] [--cursor TOKEN | --revision TOKEN --timeout-ms N] --json")
	}
	f := flag.NewFlagSet(args[0], flag.ContinueOnError)
	f.SetOutput(inv.stderr)
	credentials := f.String("credentials", "", "private reader credentials")
	selectors := f.String("selection", "", "JSON exact type/entity/source/frame selection; omitted selects the current replica")
	cursor := f.String("cursor", "", "query continuation token")
	revision := f.String("revision", "", "watch revision from a previous query or watch")
	timeout := f.Int("timeout-ms", 1000, "watch wait, 0 through 5000 milliseconds")
	count := f.Int("page-size", selection.DefaultPageSize, "maximum records in one page")
	size := f.Int("max-bytes", selection.DefaultPageBytes, "maximum encoded response bytes")
	f.Bool("json", true, "emit JSON")
	if err := inv.parseFlags(f, args[1:]); err != nil {
		return err
	}
	if *credentials == "" || f.NArg() != 0 {
		return errors.New("credentials required; no positional arguments")
	}
	badFlag := false
	f.Visit(func(flag *flag.Flag) {
		if (args[0] == "query" && (flag.Name == "revision" || flag.Name == "timeout-ms")) || (args[0] == "watch" && flag.Name == "cursor") {
			badFlag = true
		}
	})
	if badFlag {
		return errors.New("--cursor applies to query; --revision and --timeout-ms apply to watch")
	}
	var selected selection.Selection
	if *selectors != "" {
		data, err := queryReadFile(*selectors, selection.MaxRequestBytes, false)
		if err != nil {
			return err
		}
		if err := authority.DecodeStrictLimit(data, &selected, selection.MaxRequestBytes); err != nil {
			return fmt.Errorf("selection: %w", err)
		}
	}
	request, err := (selection.Request{Version: 1, Selection: selected, Cursor: *cursor, PageSize: *count, MaxBytes: *size}).Normalize()
	if err != nil {
		return err
	}
	watch := selection.WatchRequest{Version: 1, Selection: request.Selection, Revision: *revision, WaitMS: *timeout, PageSize: request.PageSize, MaxBytes: request.MaxBytes}
	if args[0] == "watch" {
		watch, err = watch.Normalize()
		if err != nil {
			return err
		}
	}
	data, err := queryReadFile(*credentials, authority.MaxEnvelopeBytes, true)
	if err != nil {
		return err
	}
	var cfg client.Config
	if err := authority.DecodeStrict(data, &cfg); err != nil {
		return fmt.Errorf("credentials: %w", err)
	}
	if cfg.Grant.Grant.Domain != "simulation" {
		return errors.New("selected read CLI currently supports simulation only")
	}
	c, err := client.New(cfg)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(inv.ctx, 15*time.Second)
	defer cancel()
	var out selection.Response
	if args[0] == "query" {
		out, err = c.QuerySelected(ctx, request)
	} else {
		out, err = c.WatchSelected(ctx, watch)
	}
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(out)
}
