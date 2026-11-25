// Disposable lab helper. It uploads explicitly synthetic fixtures through real
// authenticated endpoints; it is not part of the product CLI or production trust.
package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"time"
)

func main() {
	if e := run(); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
func run() error {
	cfgPath := flag.String("credentials", "", "private lab client config")
	out := flag.String("out", "", "reference output")
	kind := flag.String("kind", "png", "png or filler")
	seed := flag.Uint("seed", 1729, "synthetic pixel seed")
	until := flag.Int64("until", 0, "absolute original deadline")
	revoke := flag.String("revoke-node", "", "lab node config to update while stopped")
	issuer := flag.String("issuer", "", "disposable lab issuer")
	bind := flag.String("bind-writer", "", "explicit destination audience under disposable issuer")
	flag.Parse()
	b, e := os.ReadFile(*cfgPath)
	if e != nil {
		return e
	}
	var cfg client.Config
	if e = json.Unmarshal(b, &cfg); e != nil {
		return e
	}
	if *bind != "" {
		raw, e := os.ReadFile(*issuer)
		if e != nil {
			return e
		}
		var issue struct {
			PrivateKey ed25519.PrivateKey `json:"private_key"`
		}
		if e = json.Unmarshal(raw, &issue); e != nil {
			return e
		}
		g := cfg.Grant.Grant
		pub, key, e := ed25519.GenerateKey(rand.Reader)
		if e != nil {
			return e
		}
		g.ID += "/mirror/" + *bind
		g.Principal = "lab/mirror-writer-" + *bind
		g.PublishAudience = *bind
		g.PublicKey = pub
		g.Scopes = []string{"evidence-write", "evidence-read"}
		g.Source.Device = "mirror-writer-" + *bind
		g.Source.Service = "mirror"
		g.Source.Stream = "evidence"
		cfg.Audience = *bind
		cfg.PrivateKey = key
		cfg.Grant, e = authority.SignGrant(g, issue.PrivateKey)
		if e != nil {
			return e
		}
		raw, _ = json.MarshalIndent(cfg, "", "  ")
		return os.WriteFile(*out, raw, 0600)
	}
	if *revoke != "" {
		raw, e := os.ReadFile(*issuer)
		if e != nil {
			return e
		}
		var issue struct {
			PrivateKey  ed25519.PrivateKey          `json:"private_key"`
			Revocations authority.SignedRevocations `json:"revocations"`
		}
		if e = json.Unmarshal(raw, &issue); e != nil {
			return e
		}
		rev := issue.Revocations.Revocations
		rev.Revision++
		rev.IssuedAtUnixMS = time.Now().Add(-time.Millisecond).UnixMilli()
		rev.ExpiresAtUnixMS = time.Now().Add(30 * time.Minute).UnixMilli()
		rev.GrantIDs = []string{cfg.Grant.Grant.ID}
		signed, e := authority.SignRevocations(rev, issue.PrivateKey)
		if e != nil {
			return e
		}
		raw, e = os.ReadFile(*revoke)
		if e != nil {
			return e
		}
		var node map[string]any
		if e = json.Unmarshal(raw, &node); e != nil {
			return e
		}
		node["revocations"] = signed
		raw, _ = json.MarshalIndent(node, "", "  ")
		return os.WriteFile(*revoke, raw, 0600)
	}
	c, e := client.New(cfg)
	if e != nil {
		return e
	}
	var data []byte
	mime := "image/png"
	if *kind == "png" {
		pic := image.NewNRGBA(image.Rect(0, 0, 256, 256))
		x := uint32(*seed)
		for y := 0; y < 256; y++ {
			for i := 0; i < 256; i++ {
				x = x*1664525 + 1013904223
				pic.SetNRGBA(i, y, color.NRGBA{uint8(x >> 24), uint8(x >> 16), uint8(x >> 8), 255})
			}
		}
		var buf bytes.Buffer
		if e = png.Encode(&buf, pic); e != nil {
			return e
		}
		data = buf.Bytes()
	} else if *kind == "filler" {
		data = bytes.Repeat([]byte("protected unrelated quota fixture\n"), 27000)
		mime = "application/octet-stream"
	} else {
		return fmt.Errorf("unknown synthetic fixture kind")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	receipt, e := c.UploadEvidence(ctx, data, mime, time.UnixMilli(*until))
	if e != nil {
		return e
	}
	reference := client.EvidenceReference{SHA256: receipt.SHA256, Size: receipt.Size, ContentType: receipt.ContentType}
	raw, _ := json.MarshalIndent(reference, "", "  ")
	if e = os.WriteFile(*out, raw, 0600); e != nil {
		return e
	}
	if e = os.WriteFile(filepath.Join(filepath.Dir(*out), filepath.Base(*out)+".bytes"), data, 0600); e != nil {
		return e
	}
	return json.NewEncoder(os.Stdout).Encode(receipt)
}
