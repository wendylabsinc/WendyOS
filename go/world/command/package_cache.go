package command

import (
	"encoding/json"
	"errors"
	"flag"
	"io"

	"github.com/wendylabsinc/wendy/go/world/internal/packagecache"
)

type packageCacheResult struct {
	Hits            int   `json:"hits"`
	ResumedBytes    int64 `json:"resumed_bytes"`
	DownloadedBytes int64 `json:"downloaded_bytes"`
}

func (inv *invocation) runPackageCache(args []string, out io.Writer) error {
	if len(args) == 0 || (args[0] != "list" && args[0] != "discard") {
		return errors.New("usage: world package cache list --cache-dir DIR --json; world package cache discard --cache-dir DIR --sha256 BLOB_DIGEST --json")
	}
	f := flag.NewFlagSet("package cache "+args[0], flag.ContinueOnError)
	f.SetOutput(inv.stderr)
	directory := f.String("cache-dir", "", "private package cache directory")
	digest := f.String("sha256", "", "exact cached blob digest to discard")
	jsonOutput := f.Bool("json", true, "emit JSON")
	if err := inv.parseFlags(f, args[1:]); err != nil {
		return err
	}
	if *directory == "" || f.NArg() != 0 || !*jsonOutput || (args[0] == "list" && *digest != "") || (args[0] == "discard" && *digest == "") {
		return errors.New("cache commands require cache-dir, JSON output and an exact digest for discard")
	}
	if args[0] == "discard" {
		if err := packagecache.Discard(*directory, *digest); err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(struct {
			SHA256    string `json:"sha256"`
			Discarded bool   `json:"discarded"`
		}{*digest, true})
	}
	state, err := packagecache.Inspect(*directory)
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(state)
}
