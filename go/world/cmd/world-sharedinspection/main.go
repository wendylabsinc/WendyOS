// world-sharedinspection commissions and proposes simulation-only inspections.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
)

func readJSON(path string, out any, limit int, private bool) error {
	f, e := os.Open(path)
	if e != nil {
		return e
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil {
		return e
	}
	if !st.Mode().IsRegular() || private && st.Mode().Perm()&0077 != 0 {
		return errors.New("regular private input required")
	}
	raw, e := io.ReadAll(io.LimitReader(f, int64(limit)+1))
	if e != nil {
		return e
	}
	return authority.DecodeStrictLimit(raw, out, limit)
}
func writeNew(path string, value any) error {
	raw, e := json.MarshalIndent(value, "", "  ")
	if e != nil {
		return e
	}
	f, e := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return e
	}
	_, e = f.Write(append(raw, '\n'))
	if e == nil {
		e = f.Sync()
	}
	closed := f.Close()
	if e != nil {
		return e
	}
	return closed
}
func run(args []string) error {
	if len(args) == 0 {
		return errors.New("commission, executor or propose subcommand required")
	}
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	switch args[0] {
	case "result":
		credentials := flags.String("credentials", "", "private reader credentials")
		evidence := flags.String("evidence-credentials", "", "separate evidence credentials")
		id := flags.String("id", "", "retained task ID")
		origin := flags.String("origin", "", "explicit final audience for retained relay history")
		out := flags.String("out", "", "private verified media directory")
		if e := flags.Parse(args[1:]); e != nil {
			return e
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		result, e := inspectionResult(ctx, *credentials, *evidence, *id, *origin, *out)
		if e != nil {
			return e
		}
		return json.NewEncoder(os.Stdout).Encode(result)
	case "catalog":
		path := flags.String("credentials", "", "existing read credentials")
		if e := flags.Parse(args[1:]); e != nil {
			return e
		}
		var cfg client.Config
		if e := readJSON(*path, &cfg, 64<<10, true); e != nil {
			return e
		}
		api, e := client.New(cfg)
		if e != nil {
			return e
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		value, e := api.TaskCatalog(ctx)
		if e != nil {
			return e
		}
		return json.NewEncoder(os.Stdout).Encode(value)
	case "commission":
		input := flags.String("input", "", "observed public commissioning JSON")
		office := flags.String("office-scene", "", "pinned existing scene.json")
		out := flags.String("out", "", "fresh absolute private output directory")
		if e := flags.Parse(args[1:]); e != nil {
			return e
		}
		if flags.NArg() != 0 || !filepath.IsAbs(*out) || filepath.Clean(*out) != *out || *out == "/" {
			return errors.New("fresh absolute output directory required")
		}
		var in commissionInput
		if e := readJSON(*input, &in, 64<<10, false); e != nil {
			return e
		}
		f, e := os.Open(*office)
		if e != nil {
			return e
		}
		raw, e := io.ReadAll(io.LimitReader(f, (1<<20)+1))
		_ = f.Close()
		if e != nil || len(raw) > 1<<20 {
			return errors.New("bounded office metadata required")
		}
		files, e := commission(in, raw, time.Now())
		if e != nil {
			return e
		}
		if in.OperatorURL != "" {
			files["ui.json"] = uiConfig(*out)
		}
		if e = os.Mkdir(*out, 0700); e != nil {
			return e
		}
		names := make([]string, 0, len(files))
		for name := range files {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			if e = writeNew(filepath.Join(*out, name), files[name]); e != nil {
				return e
			}
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"version": 1, "files": names, "mission_signed": false})
	case "executor":
		path := flags.String("config", "", "fixed executor launch JSON")
		if e := flags.Parse(args[1:]); e != nil {
			return e
		}
		var c launchConfig
		if e := readJSON(*path, &c, 8192, false); e != nil {
			return e
		}
		argv, e := executorArgs(c)
		if e != nil {
			return e
		}
		return syscall.Exec("/executor", argv, os.Environ())
	case "propose":
		credentials := flags.String("credentials", "", "separate proposer credentials")
		input := flags.String("mission-input", "", "selected reader mission_input JSON")
		output := flags.String("handoff", "", "new private handoff JSON")
		id := flags.String("id", "", "proposal ID")
		navigation := flags.String("navigation-directory", "", "optional commissioned planar files directory")
		readToken := flags.String("source-read-token-file", "", "read-only source token for current Atlas state")
		if e := flags.Parse(args[1:]); e != nil {
			return e
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return propose(ctx, *credentials, *input, *output, *id, *navigation, *readToken)
	default:
		return errors.New("unknown subcommand")
	}
}
func executorArgs(c launchConfig) ([]string, error) {
	if c.Version != 1 || !httpOrigin(c.SceneURL) || !text(c.RunID) || c.Frame.Validate() != nil {
		return nil, errors.New("explicit executor source binding required")
	}
	return []string{"/executor", "--credentials", "/config/executor.json", "--adapter", c.SceneURL, "--adapter-token-file", "/config/scene-control-token", "--adapter-read-token-file", "/config/scene-read-token", "--run-id", c.RunID, "--frame", c.Frame.ID, "--revision", c.Frame.Revision, "--planar-config", "/config/planar.json"}, nil
}
func main() {
	if e := run(os.Args[1:]); e != nil {
		if errors.Is(e, errInspectionUnavailable) {
			fmt.Fprintln(os.Stderr, "inspection_unavailable:", e)
			os.Exit(3)
		}
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
