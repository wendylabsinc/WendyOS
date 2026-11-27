package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/cameraframe"
	"github.com/wendylabsinc/wendy/go/world/production/liveapp"
)

type receipt struct {
	Version        int    `json:"version"`
	Status         string `json:"status"`
	JPEGSHA256     string `json:"jpeg_sha256"`
	JPEGSize       int64  `json:"jpeg_size"`
	Width          int    `json:"width"`
	Height         int    `json:"height"`
	MetadataSHA256 string `json:"metadata_sha256"`
	MetadataSize   int    `json:"metadata_size"`
	Published      bool   `json:"published"`
}

func main() { os.Exit(run()) }

func run() int {
	f := flag.NewFlagSet("world-agent-native-camera", flag.ContinueOnError)
	component := f.String("component-type", "", "configured publication component type")
	stream := f.String("stream", "", "configured publication stream")
	out := f.String("output-dir", "", "new absolute output directory; parent must exist")
	timeout := f.Int("timeout-ms", 5000, "whole read timeout, 1..30000 milliseconds")
	if f.Parse(os.Args[1:]) != nil || f.NArg() != 0 || *component == "" || *stream == "" || !validOutput(*out) || *timeout < 1 || *timeout > 30000 {
		return 2
	}
	conn, err := liveapp.Dial(os.Getenv("WENDY_WORLD_SOCKET"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "explicit World service socket required")
		return 2
	}
	defer conn.Close()
	c, err := cameraframe.New(appv1.NewWorldAppServiceClient(conn), cameraframe.Config{ComponentType: *component, Stream: *stream, Timeout: time.Duration(*timeout) * time.Millisecond})
	if err != nil {
		fmt.Fprintln(os.Stderr, "native camera configuration refused")
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	frame, err := c.Read(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "native camera read failed:", err)
		return 1
	}
	if err = ctx.Err(); err != nil {
		fmt.Fprintln(os.Stderr, "native camera read canceled")
		return 1
	}
	r, err := writeFrame(*out, frame)
	if err != nil {
		fmt.Fprintln(os.Stderr, "native camera output failed; any partial files retained")
		return 1
	}
	if json.NewEncoder(os.Stdout).Encode(r) != nil {
		return 1
	}
	return 0
}

func validOutput(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && path != "/"
}

// A new directory and exclusive files prevent overwrite. A failed write keeps
// any partial files for inspection and emits no successful receipt.
func writeFrame(path string, f cameraframe.Frame) (receipt, error) {
	if !validOutput(path) {
		return receipt{}, errors.New("new absolute output directory required")
	}
	if err := f.Validate(); err != nil {
		return receipt{}, err
	}
	m, err := cameraframe.EncodeMetadata(f.Metadata)
	if err != nil {
		return receipt{}, err
	}
	if err = os.Mkdir(path, 0700); err != nil {
		return receipt{}, err
	}
	dir, err := os.OpenRoot(path)
	if err != nil {
		return receipt{}, err
	}
	defer dir.Close()
	for _, file := range []struct {
		name string
		data []byte
	}{{"frame.jpg", f.JPEG}, {"metadata.json", m}} {
		h, err := dir.OpenFile(file.name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return receipt{}, err
		}
		n, e := h.Write(file.data)
		if e == nil && n != len(file.data) {
			e = io.ErrShortWrite
		}
		if e == nil {
			e = h.Sync()
		}
		closed := h.Close()
		if e != nil {
			return receipt{}, e
		}
		if closed != nil {
			return receipt{}, closed
		}
	}
	sum := sha256.Sum256(m)
	return receipt{Version: 1, Status: "complete", JPEGSHA256: f.Metadata.JPEGSHA256, JPEGSize: f.Metadata.JPEGSize, Width: f.Metadata.Width, Height: f.Metadata.Height, MetadataSHA256: hex.EncodeToString(sum[:]), MetadataSize: len(m), Published: false}, nil
}
