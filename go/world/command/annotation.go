package command

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
)

func (inv *invocation) runAnnotation(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: world annotation list|amend|send --credentials FILE; amend --original FILE [--predecessor FILE] --action correct|withdraw --text TEXT --position '[x,y,z]' --out NEWFILE; send --record FILE")
	}
	f := flag.NewFlagSet("annotation "+args[0], flag.ContinueOnError)
	f.SetOutput(inv.stderr)
	credentials := f.String("credentials", "", "private contributor or reader profile")
	original := f.String("original", "", "exact original signed note")
	prior := f.String("predecessor", "", "exact previous signed amendment")
	action := f.String("action", "correct", "correct or withdraw")
	text := f.String("text", "", "replacement text")
	position := f.String("position", "[0,0,0]", "integer millimetres [x,y,z]")
	dest := f.String("out", "", "new private signed outbox file")
	record := f.String("record", "", "exact saved signed bytes")
	f.Bool("json", true, "JSON output")
	if e := inv.parseFlags(f, args[1:]); e != nil {
		return e
	}
	if f.NArg() != 0 || *credentials == "" {
		return errors.New("credentials required; unexpected arguments")
	}
	raw, e := annotationRead(*credentials, authority.MaxEnvelopeBytes)
	if e != nil {
		return e
	}
	var cfg client.Config
	if e = authority.DecodeStrict(raw, &cfg); e != nil {
		return e
	}
	c, e := client.New(cfg)
	if e != nil {
		return e
	}
	ctx, cancel := context.WithTimeout(inv.ctx, 15*time.Second)
	defer cancel()
	switch args[0] {
	case "list":
		v, e := c.AnnotationViews(ctx)
		if e != nil {
			return e
		}
		return json.NewEncoder(out).Encode(v)
	case "send":
		raw, e := annotationRead(*record, authority.MaxEnvelopeBytes)
		if e != nil {
			return e
		}
		elapsed, e := annotationElapsedClock()
		if e != nil {
			return e
		}
		clock, e := c.ObserveAnnotationClock(ctx, elapsed)
		if e != nil {
			return e
		}
		v, e := c.PublishAnnotationWithClock(ctx, clock, raw)
		if e != nil {
			return e
		}
		return json.NewEncoder(out).Encode(v)
	case "amend":
		if *original == "" || *dest == "" || !filepath.IsAbs(*dest) {
			return errors.New("exact original and new absolute outbox path required")
		}
		originalBytes, e := annotationRead(*original, authority.MaxEnvelopeBytes)
		if e != nil {
			return e
		}
		var previous []byte
		if *prior != "" {
			previous, e = annotationRead(*prior, authority.MaxEnvelopeBytes)
			if e != nil {
				return e
			}
		}
		var rawCoords []json.RawMessage
		if json.Unmarshal([]byte(*position), &rawCoords) != nil || len(rawCoords) != 3 {
			return errors.New("position requires exactly three integer millimetres")
		}
		var coords [3]int64
		for i, raw := range rawCoords {
			if string(raw) == "null" || json.Unmarshal(raw, &coords[i]) != nil {
				return errors.New("position requires exactly three non-null integer millimetres")
			}
		}
		// Each prepare gets a fresh gateway sample; no wall-clock authoring fallback.
		elapsed, e := annotationElapsedClock()
		if e != nil {
			return e
		}
		clock, e := c.ObserveAnnotationClock(ctx, elapsed)
		if e != nil {
			return e
		}
		epoch, e := c.NextAnnotationEpochWithClock(ctx, clock)
		if e != nil {
			return e
		}
		evidence, e := clock.Evidence()
		if e != nil {
			return e
		}
		body, e := c.PrepareAnnotationAmendment(originalBytes, previous, *action, *text, [3]int64{coords[0], coords[1], coords[2]}, epoch, 1, evidence)
		if e != nil {
			return e
		}
		file, e := os.OpenFile(*dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			return e
		}
		ok := false
		defer func() {
			file.Close()
			if !ok {
				os.Remove(*dest)
			}
		}()
		if _, e = file.Write(body); e != nil {
			return e
		}
		if e = file.Sync(); e != nil {
			return e
		}
		if e = file.Close(); e != nil {
			return e
		}
		dir, e := os.Open(filepath.Dir(*dest))
		if e != nil {
			return e
		}
		e = dir.Sync()
		dir.Close()
		if e != nil {
			return e
		}
		ok = true
		return json.NewEncoder(out).Encode(map[string]any{"state": "prepared_not_sent", "out": *dest, "signed_bytes": len(body)})
	default:
		return errors.New("unknown annotation command")
	}
}

func annotationRead(path string, limit int) ([]byte, error) {
	f, e := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	i, e := f.Stat()
	if e != nil {
		return nil, e
	}
	st, ok := i.Sys().(*syscall.Stat_t)
	if !ok || !i.Mode().IsRegular() || i.Mode().Perm()&0077 != 0 || int(st.Uid) != os.Geteuid() || st.Nlink != 1 || i.Size() > int64(limit) {
		return nil, errors.New("private singly-linked regular input required")
	}
	raw, e := io.ReadAll(io.LimitReader(f, int64(limit)+1))
	if len(raw) > limit {
		return nil, errors.New("input exceeds bound")
	}
	return raw, e
}

// Keep a failed clock latched for the lifetime of this short command. No UTC
// fallback is acceptable for an authoring proof.
func annotationElapsedClock() (func() time.Duration, error) {
	if annotationBootTime() < 0 {
		return nil, errors.New("suspend-inclusive monotonic clock unavailable")
	}
	failed := false
	return func() time.Duration {
		if failed {
			return -1
		}
		value := annotationBootTime()
		if value < 0 {
			failed = true
		}
		return value
	}, nil
}
