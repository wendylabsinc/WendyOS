package command

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/selection"
)

type areaEnvelope struct {
	SHA256   string          `json:"sha256"`
	Snapshot json.RawMessage `json:"snapshot"`
}

func (inv *invocation) runArea(args []string, out io.Writer) error {
	if len(args) == 0 || (args[0] != "fetch" && args[0] != "show") {
		return errors.New("usage: world area fetch --name NAME --selection FILE --credentials FILE --out NEWFILE; world area show --file SNAPSHOT")
	}
	f := flag.NewFlagSet("area", flag.ContinueOnError)
	f.SetOutput(inv.stderr)
	name := f.String("name", "", "workspace display name")
	sel := f.String("selection", "", "region selection JSON")
	credentials := f.String("credentials", "", "private reader profile")
	dest := f.String("out", "", "new complete snapshot; never overwrite")
	input := f.String("file", "", "saved complete snapshot")
	pageSize := f.Int("page-size", 32, "records per page")
	f.Bool("json", true, "JSON output")
	if e := inv.parseFlags(f, args[1:]); e != nil {
		return e
	}
	if f.NArg() != 0 {
		return errors.New("unexpected arguments")
	}
	if args[0] == "show" {
		raw, e := queryReadFile(*input, 4<<20, true)
		if e != nil {
			return e
		}
		var envelope areaEnvelope
		if e = authority.DecodeStrictLimit(raw, &envelope, 4<<20); e != nil {
			return e
		}
		if selection.Digest(json.RawMessage(envelope.Snapshot)) != envelope.SHA256 {
			return errors.New("saved area checksum mismatch")
		}
		var saved client.AreaSnapshot
		if e = authority.DecodeStrictLimit(envelope.Snapshot, &saved, 2<<20); e != nil {
			return e
		}
		if e = client.ValidateAreaSnapshot(saved); e != nil {
			return e
		}
		// CLI exports are historical on every read, even on a clock rollback. There is
		// no cache TTL renewal and no claim that an unexpired row is currently verified.
		return json.NewEncoder(out).Encode(struct {
			Snapshot client.AreaSnapshot `json:"snapshot"`
			State    string              `json:"state"`
		}{saved, "historical complete read; original deadlines apply; current authority not checked"})
	}
	if *credentials == "" || *sel == "" || strings.TrimSpace(*name) == "" || !filepath.IsAbs(*dest) {
		return errors.New("credentials, region selection, name and absolute new output file required")
	}
	raw, e := queryReadFile(*sel, selection.MaxRequestBytes, false)
	if e != nil {
		return e
	}
	var s selection.Selection
	if e = authority.DecodeStrictLimit(raw, &s, selection.MaxRequestBytes); e != nil {
		return e
	}
	raw, e = queryReadFile(*credentials, authority.MaxEnvelopeBytes, true)
	if e != nil {
		return e
	}
	var cfg client.Config
	if e = authority.DecodeStrict(raw, &cfg); e != nil {
		return e
	}
	if cfg.Grant.Grant.Domain != "simulation" {
		return errors.New("area CLI supports simulation only")
	}
	c, e := client.New(cfg)
	if e != nil {
		return e
	}
	ctx, cancel := context.WithTimeout(inv.ctx, 30*time.Second)
	defer cancel()
	snapshot, e := c.ReadArea(ctx, *name, s, *pageSize)
	if e != nil {
		return e
	}
	if e = client.ValidateAreaSnapshot(snapshot); e != nil {
		return e
	}
	payload, e := json.Marshal(snapshot)
	if e != nil {
		return e
	}
	data, e := json.Marshal(areaEnvelope{selection.Digest(json.RawMessage(payload)), payload})
	if e != nil {
		return e
	}
	if e = writeAreaSnapshot(*dest, data); e != nil {
		return e
	}
	return json.NewEncoder(out).Encode(struct {
		Name, Path, SHA256 string
		Pages              int
		Historical         bool
	}{*name, *dest, selection.Digest(json.RawMessage(payload)), len(snapshot.Pages), true})
}
func writeAreaSnapshot(path string, data []byte) error {
	parent := filepath.Dir(path)
	st, e := os.Lstat(parent)
	if e != nil {
		return e
	}
	owner, ok := st.Sys().(*syscall.Stat_t)
	if !ok || !st.IsDir() || st.Mode().Perm()&0077 != 0 || owner.Uid != uint32(os.Geteuid()) {
		return errors.New("snapshot parent must be a private owned directory")
	}
	root, e := os.OpenRoot(parent)
	if e != nil {
		return e
	}
	defer root.Close()
	tmp := fmt.Sprintf(".area-%d-%d", os.Getpid(), time.Now().UnixNano())
	file, e := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if e != nil {
		return e
	}
	defer root.Remove(tmp)
	if _, e = file.Write(data); e == nil {
		e = file.Sync()
	}
	closeErr := file.Close()
	if e != nil {
		return e
	}
	if closeErr != nil {
		return closeErr
	}
	// Link publishes a fully synced file atomically without replacing any existing
	// destination. Removing the temporary link leaves a single-link private file.
	if e = root.Link(tmp, filepath.Base(path)); e != nil {
		return e
	}
	if e = root.Remove(tmp); e != nil {
		return e
	}
	dir, e := root.Open(".")
	if e != nil {
		return e
	}
	defer dir.Close()
	if e = dir.Sync(); e != nil {
		return errors.New("snapshot published but directory sync uncertain")
	}
	return nil
}
