package command

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"path/filepath"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/evidencemirror"
)

func mirrorClient(path string) (*client.Client, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("explicit absolute credential path required")
	}
	var cfg client.Config
	if e := supervisionInput(path, true, &cfg); e != nil {
		return nil, errors.New("private mirror credentials unavailable or invalid")
	}
	return client.New(cfg)
}
func (inv *invocation) runEvidenceMirror(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: world evidence mirror prepare|run|verify|status --state-dir PRIVATE_DIR --json")
	}
	verb := args[0]
	if verb != "prepare" && verb != "run" && verb != "verify" && verb != "status" {
		return errors.New("unknown mirror action")
	}
	f := flag.NewFlagSet("evidence mirror "+verb, flag.ContinueOnError)
	f.SetOutput(inv.stderr)
	path := f.String("state-dir", "", "one immutable plan and source cache in a private absolute directory")
	id := f.String("id", "", "bounded plan identity, required only by prepare")
	source := f.String("source", "", "private source evidence-read credentials")
	destination := f.String("destination", "", "private destination evidence-write/read credentials; verify only needs read")
	reference := f.String("reference", "", "exact digest, size and MIME JSON; prepare only")
	js := f.Bool("json", true, "JSON status and last-known progress")
	if e := inv.parseFlags(f, args[1:]); e != nil {
		return e
	}
	if f.NArg() != 0 || !*js || !filepath.IsAbs(*path) || filepath.Clean(*path) != *path {
		return errors.New("absolute private state-dir and JSON output required")
	}
	if verb == "prepare" && (*id == "" || *source == "" || *destination == "" || *reference == "") || verb != "prepare" && (*id != "" || *reference != "") || verb == "run" && (*source == "" || *destination == "") || verb == "verify" && (*source != "" || *destination == "") || verb == "status" && (*source != "" || *destination != "") {
		return errors.New("prepare needs id/source/destination/reference; run needs source/destination; verify needs destination; status needs only state-dir")
	}
	if verb == "status" {
		j, e := evidencemirror.Open(*path)
		if e != nil {
			return e
		}
		defer j.Close()
		s, e := j.Status(time.Now())
		if e != nil {
			return e
		}
		return json.NewEncoder(out).Encode(s)
	}
	ctx, cancel := context.WithTimeout(inv.ctx, 2*time.Minute)
	defer cancel()
	dst, e := mirrorClient(*destination)
	if e != nil {
		return e
	}
	var src *client.Client
	if *source != "" {
		src, e = mirrorClient(*source)
		if e != nil {
			return e
		}
	}
	if verb == "prepare" {
		var ref client.EvidenceReference
		if e = supervisionInput(*reference, false, &ref); e != nil {
			return e
		}
		state, e := client.PrepareEvidenceMirror(ctx, *id, *path, src, dst, ref)
		if e != nil {
			return e
		}
		return json.NewEncoder(out).Encode(evidencemirror.Status{State: state, Availability: "not_verified"})
	}
	var status evidencemirror.Status
	if verb == "run" {
		status, e = client.RunEvidenceMirror(ctx, *path, src, dst)
	} else {
		status, e = client.VerifyEvidenceMirror(ctx, *path, dst)
	}
	if status.Plan.ID != "" {
		if writeErr := json.NewEncoder(out).Encode(status); writeErr != nil {
			return writeErr
		}
	}
	return e
}
