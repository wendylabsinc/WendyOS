// world-evidence retrieves evidence with a separately scoped signed grant.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/appsupport"
	"github.com/wendylabsinc/wendy/go/world/internal/evidence"
)

func run(args []string) error {
	f := flag.NewFlagSet("world-evidence", flag.ContinueOnError)
	credentials := f.String("credentials", "", "private evidence reader credentials")
	digest := f.String("sha256", "", "evidence digest")
	output := f.String("out", "", "optional new private destination file")
	offset := f.Int64("offset", -1, "retrieve one chunk starting at offset")
	length := f.Int("length", evidence.MaxChunk, "single-chunk byte count")
	if err := f.Parse(args); err != nil {
		return err
	}
	if *digest == "" || f.NArg() != 0 {
		return errors.New("sha256 required")
	}
	_, c, err := appsupport.SimulationClient(*credentials)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if *offset >= 0 {
		if *output != "" {
			return errors.New("chunk mode emits JSON; out is for full verified downloads")
		}
		chunk, err := c.GetEvidence(ctx, evidence.GetRequest{SHA256: *digest, Offset: *offset, Length: *length})
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(chunk)
	}
	data, meta, err := c.DownloadEvidence(ctx, *digest, 8<<20)
	if err != nil {
		return err
	}
	if *output != "" {
		file, err := os.OpenFile(*output, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		_, err = file.Write(data)
		if err == nil {
			err = file.Sync()
		}
		closeErr := file.Close()
		if err != nil {
			os.Remove(*output)
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	meta.Data = nil
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"verified": true, "metadata": meta, "bytes": len(data)})
}
func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
