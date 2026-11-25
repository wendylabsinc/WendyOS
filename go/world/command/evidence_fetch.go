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
	"github.com/wendylabsinc/wendy/go/world/internal/evidence"
	"github.com/wendylabsinc/wendy/go/world/internal/evidencecache"
)

type evidenceSourceFiles struct {
	Sources []struct {
		ID          string `json:"id"`
		Credentials string `json:"credentials"`
	} `json:"sources"`
}

func (inv *invocation) runEvidenceFetch(args []string, output io.Writer) error {
	f := flag.NewFlagSet("evidence fetch", flag.ContinueOnError)
	f.SetOutput(inv.stderr)
	sourcesPath := f.String("sources", "", "explicit source ID and private credential file list")
	referencePath := f.String("reference", "", "exact expected digest, size and content type")
	cacheDir := f.String("cache-dir", "", "private durable per-source cache")
	outPath := f.String("out", "", "new file in an existing private directory")
	cacheBytes := f.Int64("cache-bytes", 0, "total reservation ceiling, equally divided among sources")
	jsonOutput := f.Bool("json", true, "emit JSON")
	if err := inv.parseFlags(f, args); err != nil {
		return err
	}
	if f.NArg() != 0 || !*jsonOutput || *sourcesPath == "" || *referencePath == "" || *cacheDir == "" || !filepath.IsAbs(*outPath) {
		return errors.New("fetch requires sources, reference, cache-dir, absolute new out and JSON output")
	}
	var files evidenceSourceFiles
	if err := supervisionInput(*sourcesPath, true, &files); err != nil {
		return errors.New("private source list unavailable or invalid")
	}
	if len(files.Sources) < 1 || len(files.Sources) > 8 {
		return errors.New("source list must contain one to eight entries")
	}
	var ref client.EvidenceReference
	if err := supervisionInput(*referencePath, false, &ref); err != nil {
		return errors.New("evidence reference unavailable or invalid")
	}
	if err := ref.Validate(); err != nil {
		return err
	}
	opts := client.EvidenceResolveOptions{CacheDir: *cacheDir, CacheBytes: *cacheBytes}
	for _, s := range files.Sources {
		if !filepath.IsAbs(s.Credentials) {
			return errors.New("source credentials must use explicit absolute paths")
		}
		var cfg client.Config
		if err := supervisionInput(s.Credentials, true, &cfg); err != nil {
			return errors.New("source credentials unavailable or invalid")
		}
		c, err := client.New(cfg)
		if err != nil {
			return err
		}
		opts.Sources = append(opts.Sources, client.EvidenceSource{ID: s.ID, Client: c})
	}
	ctx, cancel := context.WithTimeout(inv.ctx, 2*time.Minute)
	defer cancel()
	result, err := client.ResolveEvidence(ctx, ref, opts)
	if err != nil {
		if len(result.Attempts) > 0 {
			if e := json.NewEncoder(output).Encode(result); e != nil {
				return e
			}
		}
		return err
	}
	// Stage bytes first, then recheck server access immediately before the atomic
	// export. A refreshed credential file is not automatically trusted or signed.
	var chosen *client.Client
	for _, s := range opts.Sources {
		if s.ID == result.Source {
			chosen = s.Client
			break
		}
	}
	err = evidencecache.Export(*outPath, result.Data, result.RetainUntilUnixMS, func() error {
		r, e := chosen.GetEvidence(ctx, evidence.GetRequest{SHA256: ref.SHA256, Offset: 0, Length: 1})
		if e != nil {
			return e
		}
		if r.SHA256 != ref.SHA256 || r.Size != ref.Size || r.ContentType != ref.ContentType || r.Offset != 0 || len(r.Data) != 1 || r.Data[0] != result.Data[0] || r.RetainUntilUnixMS != result.RetainUntilUnixMS {
			return errors.New("evidence changed before export")
		}
		result.Attempts[len(result.Attempts)-1].DownloadedBytes++
		return ctx.Err()
	})
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(struct {
		client.EvidenceResolution
		Output   string `json:"output"`
		Exported bool   `json:"exported"`
	}{result, *outPath, true})
}
func (inv *invocation) runEvidenceCache(args []string, output io.Writer) error {
	if len(args) == 0 || (args[0] != "list" && args[0] != "discard") {
		return errors.New("evidence cache requires list or discard")
	}
	f := flag.NewFlagSet("evidence cache "+args[0], flag.ContinueOnError)
	f.SetOutput(inv.stderr)
	path := f.String("cache-dir", "", "private evidence cache")
	id := f.String("source", "", "exact configured source ID")
	hash := f.String("sha256", "", "exact digest to discard from that source")
	js := f.Bool("json", true, "emit JSON")
	if err := inv.parseFlags(f, args[1:]); err != nil {
		return err
	}
	if f.NArg() != 0 || !*js || *path == "" || args[0] == "list" && (*id != "" || *hash != "") || args[0] == "discard" && (*id == "" || *hash == "") {
		return errors.New("cache-dir required; discard also requires source and sha256")
	}
	if args[0] == "list" {
		v, e := evidencecache.Inspect(*path)
		if e != nil {
			return e
		}
		return json.NewEncoder(output).Encode(v)
	}
	if e := evidencecache.Discard(*path, *id, *hash); e != nil {
		return e
	}
	return json.NewEncoder(output).Encode(struct {
		Source    string `json:"source"`
		SHA256    string `json:"sha256"`
		Discarded bool   `json:"discarded"`
	}{*id, *hash, true})
}
